package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/internal/db"
)

// Session lifetimes (ADR-0003).
const (
	// sessionTTL is the hard lifetime cap on a session, also the cookie MaxAge.
	sessionTTL = 12 * time.Hour
	// stepUpFreshness bounds how recently the second factor must have been
	// re-proved for a step-up-gated action to proceed.
	stepUpFreshness = 5 * time.Minute
)

// sessionCookie is the opaque-session cookie name. The challenge cookies used
// mid-flow are named separately so they never collide with a live session.
const sessionCookie = "orkano_session"

// ctxKey is the private type for request-context values so no other package can
// collide with our keys.
type ctxKey int

const userCtxKey ctxKey = iota

// sessionUser is the authenticated identity stashed on the request context by
// RequireSession. It carries only the id and username — never the password hash
// or TOTP seed.
type sessionUser struct {
	ID       int64
	Username string
}

// mintSession creates a server-side session for userID and returns the raw
// token to set as the cookie value. Only the token's hash is stored (ADR-0003).
func (s *Server) mintSession(ctx context.Context, userID int64) (string, error) {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	if err := s.cfg.Store.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: hash,
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(sessionTTL), Valid: true},
	}); err != nil {
		return "", err
	}
	return raw, nil
}

// setSessionCookie sets the session cookie. Secure is on when the request
// arrived over TLS (directly or via a TLS-terminating proxy that sets
// X-Forwarded-Proto), so a TLS deployment gets Secure while `orkano proxy` over
// http://localhost still works.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, raw string) {
	//nolint:gosec // G124: HttpOnly+SameSite=Strict always set; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// clearCookie expires a cookie by name. It mirrors the live cookie's security
// attributes (HttpOnly, SameSite=Strict, conditional Secure) so the browser
// matches and actually drops it.
func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	//nolint:gosec // G124: HttpOnly+SameSite=Strict always set; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// resolveSession reads the session cookie, validates it (unexpired row, a
// confirmed admin), slides the idle clock, and returns the identity plus the
// session row. A missing/invalid/expired cookie returns (nil, _, ok=false) with
// no error so callers can render the public state.
func (s *Server) resolveSession(r *http.Request) (*sessionUser, *db.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil, false
	}
	ctx := r.Context()
	hash := auth.HashToken(c.Value)
	sess, err := s.cfg.Store.GetSession(ctx, hash)
	if err != nil {
		// pgx.ErrNoRows = no live session; anything else is a backend error we
		// also can't authenticate on, so both deny.
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("session lookup failed", "err", err)
		}
		return nil, nil, false
	}
	user, err := s.cfg.Store.GetUserByID(ctx, sess.UserID)
	if err != nil || !user.TotpConfirmedAt.Valid {
		return nil, nil, false
	}
	// Best-effort idle-clock slide; failure must not deny an otherwise valid
	// session.
	if err := s.cfg.Store.TouchSession(ctx, hash); err != nil {
		s.log.Warn("touch session failed", "err", err)
	}
	return &sessionUser{ID: user.ID, Username: user.Username}, &sess, true
}

// RequireSession is middleware that admits only requests carrying a valid
// session cookie, stashing the authenticated identity on the request context.
func (s *Server) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := s.resolveSession(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireStepUp is middleware that admits only requests on a session whose
// second factor was re-proved within stepUpFreshness. M2.4 mounts it ahead of
// destructive routes; it is exported and intentionally not attached to any route
// yet.
func (s *Server) RequireStepUp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, sess, ok := s.resolveSession(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !sess.ReauthAt.Valid || s.now().Sub(sess.ReauthAt.Time) > stepUpFreshness {
			writeJSONError(w, http.StatusForbidden, "step_up_required")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userFromContext pulls the authenticated identity RequireSession stashed. The
// bool is false when called off a route that did not pass RequireSession.
func userFromContext(ctx context.Context) (*sessionUser, bool) {
	u, ok := ctx.Value(userCtxKey).(*sessionUser)
	return u, ok
}
