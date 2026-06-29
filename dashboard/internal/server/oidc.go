package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/dashboard/internal/oidc"
	"github.com/orkanoio/orkano/internal/db"
)

// maxUsernameLen mirrors the users.username CHECK upper bound (00003): the JIT
// display username is capped to it (the subject can be longer).
const maxUsernameLen = 254

// OIDCAuthenticator is the slice of dashboard/internal/oidc.Authenticator the
// handlers use. It is an interface so the handler tests can drive the flow with a
// fake, no live IdP; *oidc.Authenticator is the production implementation.
type OIDCAuthenticator interface {
	// AuthCodeURL builds the IdP authorization redirect bound to state, nonce, and
	// the PKCE verifier; reauth forces prompt=login for the step-up path.
	AuthCodeURL(state, nonce, verifier string, reauth bool) string
	// Exchange swaps a code for a verified Identity (signature, issuer, audience,
	// expiry, nonce all checked); it does NOT apply the allowlist.
	Exchange(ctx context.Context, code, nonce, verifier string) (*oidc.Identity, error)
	// Authorize reports whether a verified Identity passes the allowlist.
	Authorize(id *oidc.Identity) bool
}

const (
	// oidcCookie is the short-lived sealed flow cookie carrying the state, nonce,
	// and PKCE verifier across the IdP round-trip.
	oidcCookie = "orkano_oidc"
	// oidcFlowTTL bounds that round-trip (redirect out, authenticate, redirect
	// back) generously — a user may take a minute to log in at the IdP.
	oidcFlowTTL = 10 * time.Minute
)

// SSO error codes appended to the SPA redirect on a failed callback. A fixed set,
// never reflected user input, so the redirect stays a safe relative path.
const (
	ssoDisabled      = "disabled"
	ssoNoFlow        = "no_flow"
	ssoStateMismatch = "state_mismatch"
	ssoExchange      = "exchange_failed"
	ssoNotAllowed    = "not_allowed"
	ssoInternal      = "internal_error"
	ssoIdP           = "idp_error"
	ssoNoSession     = "no_session"
	ssoNotOIDC       = "not_oidc"
)

// oidcFlow is the JSON sealed into the flow cookie. It carries no secret beyond
// the per-flow random values; an absolute expiry is checked server-side. For a
// step-up re-auth it ALSO carries the initiating session's hash + user id: the
// SameSite=Strict session cookie is NOT sent on the cross-site IdP→callback
// navigation, so the callback cannot read the live session and must learn it from
// here (sealed, Lax) instead.
type oidcFlow struct {
	State       string `json:"state"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	Reauth      bool   `json:"reauth,omitempty"`
	UID         int64  `json:"uid,omitempty"`
	SessionHash string `json:"sess,omitempty"`
	Expires     int64  `json:"exp"`
}

// handleOIDCLogin starts the authorization-code flow: mint per-flow secrets, seal
// them into the flow cookie, and redirect to the IdP. A top-level browser
// navigation, so every exit is a redirect (never JSON the user would see raw).
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDC == nil {
		s.redirectSSOError(w, r, ssoDisabled)
		return
	}

	// ?stepup=1 re-authenticates an EXISTING OIDC session for a destructive action
	// (the password+TOTP step-up endpoint refuses an OIDC session). It needs a live
	// OIDC session, whose hash+id it carries in the flow cookie so the callback can
	// find it without the Strict session cookie (not sent cross-site).
	reauth := r.URL.Query().Get("stepup") == "1"
	flow := oidcFlow{Reauth: reauth}
	if reauth {
		user, _, ok := s.resolveSession(r)
		if !ok {
			s.redirectSSOError(w, r, ssoNoSession)
			return
		}
		if !user.OIDC {
			// A local admin steps up with password+TOTP, not an OIDC round-trip.
			s.redirectSSOError(w, r, ssoNotOIDC)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			s.redirectSSOError(w, r, ssoNoSession)
			return
		}
		flow.UID = user.ID
		flow.SessionHash = auth.HashToken(c.Value)
	}

	state, nonce, verifier, err := oidc.NewFlowSecrets()
	if err != nil {
		s.log.Error("oidc flow secrets failed", "err", err)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	flow.State, flow.Nonce, flow.Verifier = state, nonce, verifier
	if !s.setOIDCCookie(w, r, flow) {
		return
	}
	target := s.cfg.OIDC.AuthCodeURL(state, nonce, verifier, reauth)
	//nolint:gosec // G710: the URL is the admin-configured IdP authorization endpoint plus server-generated state/nonce/PKCE; `reauth` only toggles prompt=login — no request-controlled redirect target
	http.Redirect(w, r, target, http.StatusFound)
}

// handleOIDCCallback completes the flow: validate state, exchange + verify the
// token, apply the allowlist, JIT-provision the identity, and mint a session. The
// flow cookie is single-use — cleared on entry regardless of outcome.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDC == nil {
		s.redirectSSOError(w, r, ssoDisabled)
		return
	}
	ctx := r.Context()
	flow, ok := s.readOIDCCookie(r)
	// Clear on entry regardless of outcome (incl. an IdP-reported error below) so
	// the same browser cannot resubmit the flow. A captured-cookie replay is
	// additionally defeated by the IdP's one-time authorization code: the second
	// Exchange of a spent code fails (ssoExchange).
	s.clearOIDCCookie(w, r)

	// Attribute a failed callback to the flow the user actually started, so a
	// step-up denial isn't logged as a login (INV-08). Unknown when the cookie is
	// absent — default to login.
	action := "auth.oidc_login"
	if ok && flow.Reauth {
		action = "auth.oidc_stepup"
	}

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// The IdP itself reported a failure (access_denied, etc.). Don't reflect it.
		s.audit(ctx, "anonymous", action, "", "failure", r)
		s.redirectSSOError(w, r, ssoIdP)
		return
	}
	if !ok {
		s.audit(ctx, "anonymous", action, "", "failure", r)
		s.redirectSSOError(w, r, ssoNoFlow)
		return
	}
	// state binds the callback to the cookie we set (CSRF). Constant-time, length
	// guard so two empties can't match (subtle.ConstantTimeCompare("","")==1).
	state := q.Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		s.audit(ctx, "anonymous", action, "", "failure", r)
		s.redirectSSOError(w, r, ssoStateMismatch)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.audit(ctx, "anonymous", action, "", "failure", r)
		s.redirectSSOError(w, r, ssoExchange)
		return
	}

	id, err := s.cfg.OIDC.Exchange(ctx, code, flow.Nonce, flow.Verifier)
	if err != nil {
		s.log.Warn("oidc exchange failed", "err", err)
		s.audit(ctx, "anonymous", action, "", "failure", r)
		s.redirectSSOError(w, r, ssoExchange)
		return
	}
	if !s.cfg.OIDC.Authorize(id) {
		// Verified but not on the allowlist: audit by the claimed identity so a
		// denied sign-in/step-up is attributable (INV-08), then refuse.
		s.audit(ctx, oidcActor(id), action, oidcActor(id), "denied", r)
		s.redirectSSOError(w, r, ssoNotAllowed)
		return
	}

	if flow.Reauth {
		s.completeOIDCStepUp(w, r, flow, id)
		return
	}

	uid, username, err := s.resolveOIDCUser(ctx, id)
	if err != nil {
		s.log.Error("oidc provision failed", "err", err)
		s.audit(ctx, oidcActor(id), "auth.oidc_login", oidcActor(id), "failure", r)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	raw, err := s.mintSession(ctx, uid)
	if err != nil {
		s.log.Error("mint session failed", "err", err)
		s.audit(ctx, username, "auth.oidc_login", username, "failure", r)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	s.setSessionCookie(w, r, raw)
	s.audit(ctx, username, "auth.oidc_login", username, "success", r)
	http.Redirect(w, r, "/", http.StatusFound)
}

// completeOIDCStepUp marks the initiating session's reauth_at after a fresh IdP
// re-authentication — the OIDC analog of the password+TOTP step-up. It binds the
// re-verified identity to the SAME session and user that started the flow: the
// session (by hash from the sealed cookie) must still be live, belong to the flow
// UID, and resolve to a user whose stored subject equals the just-verified one.
func (s *Server) completeOIDCStepUp(w http.ResponseWriter, r *http.Request, flow oidcFlow, id *oidc.Identity) {
	ctx := r.Context()
	if flow.SessionHash == "" {
		s.audit(ctx, oidcActor(id), "auth.oidc_stepup", oidcActor(id), "failure", r)
		s.redirectSSOError(w, r, ssoNoSession)
		return
	}
	sess, err := s.cfg.Store.GetSession(ctx, flow.SessionHash)
	if err != nil {
		// Expired or revoked mid-flow — nothing to step up. A revoked session
		// arriving here is worth recording (INV-08).
		s.audit(ctx, oidcActor(id), "auth.oidc_stepup", oidcActor(id), "failure", r)
		s.redirectSSOError(w, r, ssoNoSession)
		return
	}
	if sess.UserID != flow.UID {
		// An internal binding invariant violation (the sealed cookie is
		// server-generated, so this is unreachable normally).
		s.audit(ctx, oidcActor(id), "auth.oidc_stepup", oidcActor(id), "failure", r)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	user, err := s.cfg.Store.GetUserByID(ctx, sess.UserID)
	if err != nil {
		s.log.Error("oidc stepup user lookup failed", "err", err)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	// The re-verified subject must be the SAME identity the session belongs to, or
	// a different IdP account (even an allowlisted one) could step up someone
	// else's session. Constant-time + empty guard.
	if !user.OidcSubject.Valid || id.Subject == "" ||
		subtle.ConstantTimeCompare([]byte(user.OidcSubject.String), []byte(id.Subject)) != 1 {
		s.audit(ctx, user.Username, "auth.oidc_stepup", user.Username, "failure", r)
		s.redirectSSOError(w, r, ssoNotAllowed)
		return
	}
	// If the session were revoked in the window between GetSession and here, this
	// UPDATE is a benign no-op (0 rows): the next resolveSession fails anyway, so a
	// stale "success" audit is the only effect — not an escalation.
	if err := s.cfg.Store.MarkSessionReauth(ctx, flow.SessionHash); err != nil {
		s.log.Error("mark session reauth failed", "err", err)
		s.audit(ctx, user.Username, "auth.oidc_stepup", user.Username, "failure", r)
		s.redirectSSOError(w, r, ssoInternal)
		return
	}
	s.audit(ctx, user.Username, "auth.oidc_stepup", user.Username, "success", r)
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveOIDCUser returns the user id + username for a verified identity,
// just-in-time provisioning a credential-less anchor on first sign-in. The
// lookup-or-create keys on (issuer, subject); a create that loses a concurrent
// race re-reads and succeeds.
func (s *Server) resolveOIDCUser(ctx context.Context, id *oidc.Identity) (int64, string, error) {
	key := db.GetUserByOIDCParams{Issuer: pgText(id.Issuer), Subject: pgText(id.Subject)}
	if u, err := s.cfg.Store.GetUserByOIDC(ctx, key); err == nil {
		return u.ID, u.Username, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", err
	}

	// First sign-in: provision. username is the IdP email for display/audit, or the
	// subject when no email claim was returned (a group-only allowlist). It is only
	// a display field — the (issuer, subject) key above is the identity — so cap it
	// to the column limit (a 255-char subject would otherwise fail the 254-char
	// username CHECK and turn a valid login into an internal error).
	username := id.Email
	if username == "" {
		username = id.Subject
	}
	if len(username) > maxUsernameLen {
		username = username[:maxUsernameLen]
	}
	created, err := s.cfg.Store.CreateOIDCUser(ctx, db.CreateOIDCUserParams{
		Username: username, Issuer: pgText(id.Issuer), Subject: pgText(id.Subject),
	})
	if err == nil {
		return created.ID, created.Username, nil
	}
	// A concurrent first sign-in for the same (issuer, subject) wins the unique
	// index; re-read returns its row. Any other error — e.g. a username collision
	// with a DIFFERENT identity (the IdP email equals another row's username) —
	// surfaces; identity confusion is impossible because the re-read keys on
	// (issuer, subject).
	if u, e := s.cfg.Store.GetUserByOIDC(ctx, key); e == nil {
		return u.ID, u.Username, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		s.log.Warn("oidc provision: username already claimed by a different identity", "username", username)
	}
	return 0, "", err
}

// oidcActor names the audit actor for a verified-but-maybe-unauthorized identity:
// the email if present, else the subject.
func oidcActor(id *oidc.Identity) string {
	if id == nil {
		return "anonymous"
	}
	if id.Email != "" {
		return id.Email
	}
	return id.Subject
}

func (s *Server) redirectSSOError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?sso_error="+code, http.StatusFound)
}

// --- flow cookie ---

// setOIDCCookie seals the flow values into the cookie. Unlike the session and
// challenge cookies (SameSite=Strict), this one is SameSite=Lax ON PURPOSE: the
// IdP→callback hop is a cross-site top-level GET, which a Strict cookie would not
// be sent on, breaking the flow (ADR-0016).
func (s *Server) setOIDCCookie(w http.ResponseWriter, r *http.Request, flow oidcFlow) bool {
	flow.Expires = s.now().Add(oidcFlowTTL).Unix()
	payload, err := json.Marshal(flow)
	if err != nil {
		s.log.Error("marshal oidc flow failed", "err", err)
		s.redirectSSOError(w, r, ssoInternal)
		return false
	}
	sealed, err := s.cfg.Cipher.Seal(string(payload))
	if err != nil {
		s.log.Error("seal oidc flow failed", "err", err)
		s.redirectSSOError(w, r, ssoInternal)
		return false
	}
	//nolint:gosec // G124: HttpOnly always set; SameSite=Lax is REQUIRED for the cross-site IdP callback; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookie,
		Value:    sealed,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oidcFlowTTL / time.Second),
	})
	return true
}

// readOIDCCookie opens and validates the flow cookie: it must decrypt (AEAD
// rejects tampering) and not be expired.
func (s *Server) readOIDCCookie(r *http.Request) (oidcFlow, bool) {
	c, err := r.Cookie(oidcCookie)
	if err != nil || c.Value == "" {
		return oidcFlow{}, false
	}
	plain, err := s.cfg.Cipher.Open(c.Value)
	if err != nil {
		return oidcFlow{}, false
	}
	var flow oidcFlow
	if err := json.Unmarshal([]byte(plain), &flow); err != nil {
		return oidcFlow{}, false
	}
	if flow.State == "" || s.now().Unix() >= flow.Expires {
		return oidcFlow{}, false
	}
	return flow, true
}

// clearOIDCCookie expires the flow cookie, mirroring its SameSite=Lax attribute
// (the shared clearCookie sends Strict, which the Strict session/challenge
// cookies want but this one does not). Browsers delete by name+path regardless,
// but matching the attributes keeps the intent honest.
func (s *Server) clearOIDCCookie(w http.ResponseWriter, r *http.Request) {
	//nolint:gosec // G124: HttpOnly+SameSite=Lax mirror the live flow cookie; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// pgText wraps a non-NULL text value for a pgtype.Text query parameter.
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
