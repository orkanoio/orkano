package server

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/internal/db"
)

// Auth-flow tunables (ADR-0003).
const (
	// maxAuthBodyBytes bounds an auth request body; these payloads are tiny.
	maxAuthBodyBytes = 64 << 10
	// maxFailedLogins is the consecutive-failure threshold that locks an account.
	maxFailedLogins = 5
	// lockoutWindow is how long an account stays locked once the threshold trips.
	lockoutWindow = 15 * time.Minute
	// enrollChallengeTTL bounds the TOTP-enrollment challenge after redeem.
	enrollChallengeTTL = 10 * time.Minute
	// totpChallengeTTL bounds the second-factor challenge after a password login.
	totpChallengeTTL = 5 * time.Minute
	// recoveryCodeCount is how many single-use recovery codes a redeem mints.
	recoveryCodeCount = 10
	// totpIssuer labels the otpauth:// URL the authenticator app shows.
	totpIssuer = "Orkano"
)

// Challenge-cookie names and stages. A challenge cookie is a short-lived sealed
// blob proving the caller passed the prior step (redeem or password) so the
// second-factor step knows which user it is completing — without minting a real
// session until both factors are satisfied.
const (
	challengeCookie = "orkano_challenge"
	stageEnroll     = "enroll"
	stageTOTP       = "totp"
)

// challenge is the JSON sealed into the challenge cookie. It carries no secret —
// just the user id, the stage, and an absolute expiry checked server-side (the
// cookie MaxAge is advisory; a tampered cookie fails the AEAD open).
type challenge struct {
	UID     int64  `json:"uid"`
	Stage   string `json:"stage"`
	Expires int64  `json:"exp"`
}

// mountAuthRoutes registers the bootstrap-auth API under /api/auth. It must be
// called before the SPA catch-all so chi matches these ahead of "/*".
func (s *Server) mountAuthRoutes(r chi.Router) {
	r.Route("/api/auth", func(ar chi.Router) {
		// status is read-only and the SPA polls it; keep it OUT of the per-IP rate
		// limiter so a polling client never 429s itself. The limiter guards only the
		// credential-bearing endpoints below.
		ar.Get("/status", s.handleAuthStatus)

		ar.Group(func(lr chi.Router) {
			lr.Use(s.rl.middleware)
			lr.Post("/redeem", s.handleRedeem)
			lr.Post("/totp/confirm", s.handleConfirmTOTP)
			lr.Post("/login", s.handleLogin)
			lr.Post("/login/totp", s.handleLoginTOTP)

			lr.Group(func(pr chi.Router) {
				pr.Use(s.RequireSession)
				pr.Post("/logout", s.handleLogout)
				pr.Post("/stepup", s.handleStepUp)
			})
		})
	})
}

// --- 1. status ---

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.cfg.Store.CountConfirmedAdmins(ctx)
	if err != nil {
		s.log.Error("count admins failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"state": "needs_bootstrap"})
		return
	}
	if user, _, ok := s.resolveSession(r); ok {
		writeJSON(w, http.StatusOK, map[string]string{"state": "authenticated", "username": user.Username})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "needs_login"})
}

// --- 2. redeem ---

type redeemRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req redeemRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	n, err := s.cfg.Store.CountConfirmedAdmins(ctx)
	if err != nil {
		s.log.Error("count admins failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if n > 0 {
		s.audit(ctx, "anonymous", "bootstrap.redeem", "", "failure", r)
		writeJSONError(w, http.StatusConflict, "already_bootstrapped")
		return
	}

	// Structural validation runs BEFORE the token comparison so a 400 never
	// implies the token was correct (a status-code oracle would leak token
	// validity). It also bounds the username before it can land in an audit row.
	if req.Username == "" || len(req.Username) > 254 {
		writeJSONError(w, http.StatusBadRequest, "invalid_username")
		return
	}
	if err := auth.ValidatePasswordPolicy(req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, "weak_password")
		return
	}

	// Constant-time compare the hashed presented token against the stored hash.
	if !hmac.Equal([]byte(auth.HashToken(req.Token)), []byte(s.cfg.BootstrapTokenHash)) {
		s.audit(ctx, "anonymous", "bootstrap.redeem", req.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_token")
		return
	}

	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	secret, otpauthURL, err := auth.GenerateTOTP(totpIssuer, req.Username)
	if err != nil {
		s.log.Error("generate totp failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	sealedSecret, err := s.cfg.Cipher.Seal(secret)
	if err != nil {
		s.log.Error("seal totp secret failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	plainCodes, codeHashes, err := auth.GenerateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		s.log.Error("generate recovery codes failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	user, err := s.cfg.Store.CreateAdmin(ctx, CreateAdminParams{
		Username:           req.Username,
		PasswordHash:       passwordHash,
		SealedTOTPSecret:   sealedSecret,
		RecoveryCodeHashes: codeHashes,
	})
	if err != nil {
		s.log.Error("create admin failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	if !s.setChallengeCookie(w, r, challenge{UID: user.ID, Stage: stageEnroll}, enrollChallengeTTL) {
		return
	}
	s.audit(ctx, req.Username, "bootstrap.redeem", req.Username, "success", r)
	writeJSON(w, http.StatusOK, map[string]any{
		"otpauthUrl":    otpauthURL,
		"recoveryCodes": plainCodes,
	})
}

// --- 3. confirm TOTP enrollment ---

type codeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req codeRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	ch, ok := s.readChallenge(r, stageEnroll)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "no_challenge")
		return
	}
	user, err := s.cfg.Store.GetUserByID(ctx, ch.UID)
	if err != nil {
		// The user row vanished (e.g. swept by a concurrent redeem). Audit the
		// failure (INV-08) — the username is unknown here, so the actor is anonymous.
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("user lookup failed", "err", err)
		}
		s.audit(ctx, "anonymous", "bootstrap.confirm_totp", "", "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if user.TotpConfirmedAt.Valid {
		// Already confirmed — the enrollment window is spent (a replayed/spent
		// challenge). Audit it (INV-08).
		s.audit(ctx, user.Username, "bootstrap.confirm_totp", user.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	secret, err := s.cfg.Cipher.Open(user.TotpSecret)
	if err != nil {
		s.log.Error("open totp secret failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !auth.ValidateTOTP(secret, req.Code) {
		s.audit(ctx, user.Username, "bootstrap.confirm_totp", user.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_code")
		return
	}
	if err := s.cfg.Store.ConfirmUserTOTP(ctx, user.ID); err != nil {
		// The single-confirmed-admin unique index (migration 00005) is the atomic
		// backstop against a concurrent-redeem race: the second confirmer trips a
		// 23505 and gets a clean 409, not a 500. The first confirmer wins.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			s.audit(ctx, user.Username, "bootstrap.confirm_totp", user.Username, "failure", r)
			writeJSONError(w, http.StatusConflict, "already_bootstrapped")
			return
		}
		s.log.Error("confirm totp failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	clearCookie(w, r, challengeCookie)
	raw, err := s.mintSession(ctx, user.ID)
	if err != nil {
		s.log.Error("mint session failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	s.setSessionCookie(w, r, raw)
	s.audit(ctx, user.Username, "bootstrap.confirm_totp", user.Username, "success", r)
	writeJSON(w, http.StatusOK, map[string]string{"state": "authenticated", "username": user.Username})
}

// --- 4. login (first factor) ---

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req loginRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	user, err := s.cfg.Store.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("user lookup failed", "err", err)
		}
		// No such user. Still run one bcrypt comparison against a dummy hash so
		// the response timing for "no such user" matches "wrong password" — the
		// username must not leak through a timing oracle. Do NOT recordFailedLogin
		// (there is no row to count, and counting non-existent users is pointless).
		_ = auth.VerifyPassword(dummyPasswordHash, req.Password)
		s.audit(ctx, req.Username, "auth.login", req.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}

	if user.LockedUntil.Valid && user.LockedUntil.Time.After(s.now()) {
		s.audit(ctx, user.Username, "auth.login", user.Username, "locked", r)
		writeJSONError(w, http.StatusLocked, "account_locked")
		return
	}

	// ALWAYS verify the password (never short-circuit on the unconfirmed flag, or
	// the password check's absence would be observable). pwOK gates everything
	// below; the response is the same generic 401 for a wrong password and for a
	// correct password on an unconfirmed (enrollment-abandoned) user.
	pwOK := auth.VerifyPassword(user.PasswordHash, req.Password) == nil

	if !pwOK || !user.TotpConfirmedAt.Valid {
		// Only count a failed attempt against a CONFIRMED user with a wrong
		// password. An unconfirmed user must never have failed_logins bumped or be
		// locked: otherwise an attacker could lock the admin during the enrollment
		// window so they are locked out the instant they confirm (a bootstrap DoS).
		if user.TotpConfirmedAt.Valid && !pwOK {
			s.recordFailedLogin(ctx, user.ID)
		}
		s.audit(ctx, user.Username, "auth.login", user.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}

	// First factor proved; do NOT reset the counter yet — the second factor must
	// also pass. Hand out a short-lived challenge so login/totp knows the user.
	if !s.setChallengeCookie(w, r, challenge{UID: user.ID, Stage: stageTOTP}, totpChallengeTTL) {
		return
	}
	s.audit(ctx, user.Username, "auth.login", user.Username, "success", r)
	writeJSON(w, http.StatusOK, map[string]string{"state": "totp_required"})
}

// --- 5. login (second factor) ---

type loginTOTPRequest struct {
	Code         string `json:"code"`
	RecoveryCode string `json:"recoveryCode"`
}

func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req loginTOTPRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	ch, ok := s.readChallenge(r, stageTOTP)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "no_challenge")
		return
	}
	user, err := s.cfg.Store.GetUserByID(ctx, ch.UID)
	if err != nil || !user.TotpConfirmedAt.Valid {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if !s.secondFactorOK(ctx, user, req) {
		s.recordFailedLogin(ctx, user.ID)
		s.audit(ctx, user.Username, "auth.login_totp", user.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}

	if err := s.cfg.Store.ResetFailedLogins(ctx, user.ID); err != nil {
		s.log.Warn("reset failed logins failed", "err", err)
	}
	clearCookie(w, r, challengeCookie)
	raw, err := s.mintSession(ctx, user.ID)
	if err != nil {
		s.log.Error("mint session failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	s.setSessionCookie(w, r, raw)
	s.audit(ctx, user.Username, "auth.login_totp", user.Username, "success", r)
	writeJSON(w, http.StatusOK, map[string]string{"state": "authenticated", "username": user.Username})
}

// secondFactorOK validates either a live TOTP code or a single-use recovery
// code. A recovery code, once consumed, cannot be replayed (the query stamps
// used_at atomically).
func (s *Server) secondFactorOK(ctx context.Context, user db.GetUserByIDRow, req loginTOTPRequest) bool {
	if req.Code != "" {
		secret, err := s.cfg.Cipher.Open(user.TotpSecret)
		if err != nil {
			s.log.Error("open totp secret failed", "err", err)
			return false
		}
		return auth.ValidateTOTP(secret, req.Code)
	}
	if req.RecoveryCode != "" {
		_, err := s.cfg.Store.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{
			UserID:   user.ID,
			CodeHash: auth.HashRecoveryCode(req.RecoveryCode),
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.log.Error("consume recovery code failed", "err", err)
			}
			return false
		}
		return true
	}
	return false
}

// --- 6. logout ---

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := userFromContext(ctx)
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.cfg.Store.DeleteSession(ctx, auth.HashToken(c.Value)); err != nil {
			s.log.Warn("delete session failed", "err", err)
		}
	}
	clearCookie(w, r, sessionCookie)
	s.audit(ctx, actorName(user), "auth.logout", actorName(user), "success", r)
	w.WriteHeader(http.StatusNoContent)
}

// --- 7. step-up ---

type stepUpRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleStepUp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := userFromContext(ctx)
	var req stepUpRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	full, err := s.cfg.Store.GetUserByID(ctx, user.ID)
	if err != nil {
		s.log.Error("user lookup failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	secret, err := s.cfg.Cipher.Open(full.TotpSecret)
	if err != nil {
		s.log.Error("open totp secret failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	// A valid TOTP is required; if a password is supplied it must also verify.
	if !auth.ValidateTOTP(secret, req.Code) ||
		(req.Password != "" && auth.VerifyPassword(full.PasswordHash, req.Password) != nil) {
		s.audit(ctx, user.Username, "auth.stepup", user.Username, "failure", r)
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}

	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.cfg.Store.MarkSessionReauth(ctx, auth.HashToken(c.Value)); err != nil {
			s.log.Error("mark session reauth failed", "err", err)
			// The second factor was just re-proved but the marker did not persist —
			// audit the failure (INV-08) so the gap is visible.
			s.audit(ctx, user.Username, "auth.stepup", user.Username, "failure", r)
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
	}
	s.audit(ctx, user.Username, "auth.stepup", user.Username, "success", r)
	w.WriteHeader(http.StatusNoContent)
}

// --- shared helpers ---

// dummyPasswordHash is a valid bcrypt hash of a fixed throwaway password,
// computed once at init. handleLogin runs a bcrypt comparison against it when the
// username does not exist so the response time matches the real wrong-password
// path (a confirmed user runs bcrypt too) — closing the user-enumeration timing
// oracle. It is never a real credential; nothing ever logs in with it.
var dummyPasswordHash = func() string {
	h, err := auth.HashPassword("orkano-dummy-password-timing-equalizer")
	if err != nil {
		// HashPassword only fails on a bcrypt internal error; a startup panic is
		// correct since the login path would otherwise leak timing.
		panic("server: precompute dummy password hash: " + err.Error())
	}
	return h
}()

// recordFailedLogin bumps the consecutive-failure counter and locks the account
// once it crosses the threshold. Best-effort: a counter write failure must not
// turn a bad-credentials 401 into a 500.
func (s *Server) recordFailedLogin(ctx context.Context, userID int64) {
	count, err := s.cfg.Store.IncrementFailedLogins(ctx, userID)
	if err != nil {
		s.log.Warn("increment failed logins failed", "err", err)
		return
	}
	if count >= maxFailedLogins {
		if err := s.cfg.Store.LockUser(ctx, db.LockUserParams{
			UserID:      userID,
			LockedUntil: pgtype.Timestamptz{Time: s.now().Add(lockoutWindow), Valid: true},
		}); err != nil {
			s.log.Warn("lock user failed", "err", err)
		}
	}
}

func actorName(u *sessionUser) string {
	if u == nil {
		return "anonymous"
	}
	return u.Username
}

// --- challenge cookies ---

func (s *Server) setChallengeCookie(w http.ResponseWriter, r *http.Request, ch challenge, ttl time.Duration) bool {
	ch.Expires = s.now().Add(ttl).Unix()
	payload, err := json.Marshal(ch)
	if err != nil {
		s.log.Error("marshal challenge failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return false
	}
	sealed, err := s.cfg.Cipher.Seal(string(payload))
	if err != nil {
		s.log.Error("seal challenge failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return false
	}
	//nolint:gosec // G124: HttpOnly+SameSite=Strict always set; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     challengeCookie,
		Value:    sealed,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl / time.Second),
	})
	return true
}

// readChallenge opens and validates the challenge cookie: it must decrypt (AEAD
// rejects tampering), match the expected stage, and not be expired.
func (s *Server) readChallenge(r *http.Request, stage string) (challenge, bool) {
	c, err := r.Cookie(challengeCookie)
	if err != nil || c.Value == "" {
		return challenge{}, false
	}
	plain, err := s.cfg.Cipher.Open(c.Value)
	if err != nil {
		return challenge{}, false
	}
	var ch challenge
	if err := json.Unmarshal([]byte(plain), &ch); err != nil {
		return challenge{}, false
	}
	if ch.Stage != stage || s.now().Unix() >= ch.Expires {
		return challenge{}, false
	}
	return ch, true
}

// --- audit ---

// audit appends one audit entry, best-effort, with the client IP as the only
// detail. A write failure is logged, never surfaced to the caller.
func (s *Server) audit(ctx context.Context, actor, action, target, outcome string, r *http.Request) {
	s.auditDetail(ctx, actor, action, target, outcome, r, nil)
}

// auditDetail appends one audit entry with optional extra fields merged into the
// jsonb detail alongside the client IP. extra must carry only non-secret metadata
// — e.g. which env-var NAMES changed — NEVER a password, code, token, seed, or
// secret value (INV-03/INV-08).
func (s *Server) auditDetail(ctx context.Context, actor, action, target, outcome string, r *http.Request, extra map[string]any) {
	if actor == "" {
		actor = "anonymous"
	}
	detail := map[string]any{"ip": clientIP(r)}
	for k, v := range extra {
		detail[k] = v
	}
	payload, _ := json.Marshal(detail)
	if err := s.cfg.Store.AppendAuditEntry(ctx, db.AppendAuditEntryParams{
		Actor:   actor,
		Action:  action,
		Target:  target,
		Outcome: outcome,
		Detail:  payload,
	}); err != nil {
		s.log.Warn("audit append failed", "action", action, "err", err)
	}
}

// --- JSON helpers ---

// decodeJSON reads a bounded auth-request body into dst. It writes a 400 and
// returns false on a read/parse error so the caller can simply return.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONLimit(w, r, dst, maxAuthBodyBytes)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
