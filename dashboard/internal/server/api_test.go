package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
)

// getWithCookie issues a GET carrying the given cookies, mirroring post() for the
// read paths.
func getWithCookie(t *testing.T, s *Server, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.RemoteAddr = "10.0.0.1:5555"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// freshenStepUp stamps a session's reauth marker to now so a RequireStepUp-gated
// route admits it.
func freshenStepUp(t *testing.T, store *fakeStore, raw string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	sess, ok := store.sessions[auth.HashToken(raw)]
	if !ok {
		t.Fatalf("no session for the given token")
	}
	sess.ReauthAt = pgtype.Timestamptz{Time: fixedNow(), Valid: true}
}

// TestAPISkeletonTiers proves the two middleware tiers the M2.4 API mounts on:
// the session tier (GET) admits any valid session; the step-up tier (POST)
// additionally demands a fresh second factor. Both deny an unauthenticated
// request. The skeleton handler returns 501 once a request clears the gate, so a
// 501 proves the request reached the handler and a 401/403 proves it was stopped
// at the middleware.
func TestAPISkeletonTiers(t *testing.T) {
	store := newFakeStore()
	store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)
	uid := store.firstUserID()

	// No session → both tiers deny with 401.
	if rec := getReq(t, s, "/api/skeleton"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/skeleton no-session = %d, want 401", rec.Code)
	}
	if rec := post(t, s, "/api/skeleton", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/skeleton no-session = %d, want 401", rec.Code)
	}

	raw := mustSession(t, store, uid)
	sessionCk := &http.Cookie{Name: sessionCookie, Value: raw}

	// Valid session → the session tier (GET) reaches the handler (501).
	if rec := getWithCookie(t, s, "/api/skeleton", sessionCk); rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET /api/skeleton with session = %d, want 501", rec.Code)
	}

	// Valid session but no step-up → the step-up tier (POST) is forbidden.
	if rec := post(t, s, "/api/skeleton", nil, sessionCk); rec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/skeleton without step-up = %d, want 403", rec.Code)
	}

	// Fresh step-up → the step-up tier reaches the handler (501).
	freshenStepUp(t, store, raw)
	if rec := post(t, s, "/api/skeleton", nil, sessionCk); rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/skeleton with step-up = %d, want 501", rec.Code)
	}
}

// TestAPIUnknownPathIsJSON404 proves an unmatched /api path returns a JSON 404,
// not the SPA HTML shell — an API client must never receive HTML for a wrong
// endpoint. A non-/api unknown path still falls through to the SPA index.
func TestAPIUnknownPathIsJSON404(t *testing.T) {
	s := newTestServer(t, fakePinger{})

	rec := do(t, s, http.MethodGet, "/api/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/does-not-exist = %d, want 404", rec.Code)
	}
	if rec.Body.String() == indexBody {
		t.Fatal("unknown /api path served the SPA index instead of a JSON 404")
	}
	if got := decodeBody(t, rec)["error"]; got != "not_found" {
		t.Fatalf("error code = %v, want not_found", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("unknown /api Content-Type = %q, want application/json (must not be HTML)", ct)
	}

	// A non-/api unknown path is a client-side route → SPA index.
	if rec := do(t, s, http.MethodGet, "/apps/my-app"); rec.Code != http.StatusOK || rec.Body.String() != indexBody {
		t.Fatalf("non-/api path did not serve the SPA: status=%d", rec.Code)
	}
}

// TestAPISkeletonCoexistsWithAuth proves mounting the /api skeleton and the
// /api/* catch-all alongside the pre-existing /api/auth subtree does not shadow
// the auth routes (a chi radix-tree overlap regression guard).
func TestAPISkeletonCoexistsWithAuth(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)
	// /api/auth/status still resolves to the auth handler (200 + a state body),
	// not the /api/* 404 catch-all.
	rec := getReq(t, s, "/api/auth/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/status = %d, want 200", rec.Code)
	}
	if got := decodeBody(t, rec)["state"]; got != "needs_bootstrap" {
		t.Fatalf("auth status state = %v, want needs_bootstrap (catch-all shadowed it?)", got)
	}
}

// TestWriteK8sError pins the apimachinery-error → (status, code) mapping. Every
// branch returns a stable snake_code, never the apiserver's raw message.
func TestWriteK8sError(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	gr := schema.GroupResource{Group: "orkano.io", Resource: "apps"}
	gk := schema.GroupKind{Group: "orkano.io", Kind: "App"}

	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"not found", apierrors.NewNotFound(gr, "x"), http.StatusNotFound, "not_found"},
		{"already exists", apierrors.NewAlreadyExists(gr, "x"), http.StatusConflict, "already_exists"},
		{"conflict", apierrors.NewConflict(gr, "x", errors.New("stale")), http.StatusConflict, "conflict"},
		{"invalid", apierrors.NewInvalid(gk, "x", nil), http.StatusUnprocessableEntity, "invalid"},
		{"bad request", apierrors.NewBadRequest("malformed"), http.StatusBadRequest, "bad_request"},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("nope")), http.StatusForbidden, "forbidden"},
		{"unauthorized", apierrors.NewUnauthorized("no"), http.StatusUnauthorized, "unauthorized"},
		{"unavailable", apierrors.NewServiceUnavailable("down"), http.StatusServiceUnavailable, "unavailable"},
		{"server timeout", apierrors.NewServerTimeout(gr, "get", 1), http.StatusServiceUnavailable, "unavailable"},
		{"timeout", apierrors.NewTimeoutError("ctx deadline", 1), http.StatusServiceUnavailable, "unavailable"},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), http.StatusServiceUnavailable, "unavailable"},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.writeK8sError(rec, "test.action", tc.err)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if got := decodeBody(t, rec)["error"]; got != tc.wantBody {
				t.Fatalf("error code = %v, want %q", got, tc.wantBody)
			}
		})
	}
}
