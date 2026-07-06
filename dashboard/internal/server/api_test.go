package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/auth"
)

// --- M2.4 CRUD test harness ---

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := orkanov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add orkano scheme: %v", err)
	}
	// The ESO kinds the vault handlers touch as unstructured (ADR-0018). Only
	// the FAKE client needs scheme entries — the real client routes
	// unstructured through the RESTMapper without them.
	scheme.AddKnownTypeWithName(secretStoreGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(secretStoreListGVK, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(externalSecretGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(externalSecretListGVK, &unstructured.UnstructuredList{})
	return scheme
}

// apiServer builds a server whose K8s client carries the orkano scheme and is
// seeded with objs, for the App/Domain/Postgres CRUD handler tests. App and
// Domain carry a status subresource so the fake client's Update preserves the
// operator-owned status the way the real apiserver does.
func apiServer(t *testing.T, store *fakeStore, objs ...client.Object) *Server {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{}, &orkanov1alpha1.Postgres{}).
		WithObjects(objs...).
		Build()
	return serverWith(t, store, k8s)
}

// serverWith builds a server over a caller-supplied K8s client — for tests that
// need a fake client with interceptors (e.g. to fail a specific write).
func serverWith(t *testing.T, store *fakeStore, k8s client.Client) *Server {
	t.Helper()
	// By default the viewer (read) client is the same fake as the SA (write)
	// client, so read tests see the seeded objects; TestReadsUseViewerClient
	// overrides it with a distinct client to prove reads route through it.
	return serverWithViewer(t, store, k8s, k8s)
}

func serverWithViewer(t *testing.T, store *fakeStore, k8s, viewer client.Client) *Server {
	t.Helper()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       viewer,
		PodLogs:            &fakePodStreamer{},
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// apiReq issues an arbitrary-method JSON request carrying the given cookies.
func apiReq(t *testing.T, s *Server, method, target string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, rdr)
	req.RemoteAddr = "10.0.0.1:5555"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// authedSession seeds a confirmed admin + a live session and returns its cookie.
func authedSession(t *testing.T, store *fakeStore) *http.Cookie {
	t.Helper()
	store.confirmedUser(t, "admin", "correct-horse-battery")
	raw := mustSession(t, store, store.firstUserID())
	return &http.Cookie{Name: sessionCookie, Value: raw}
}

// steppedUpSession is authedSession with a fresh second-factor marker, so it
// clears the RequireStepUp-gated routes (delete, secret rotation).
func steppedUpSession(t *testing.T, store *fakeStore) *http.Cookie {
	t.Helper()
	ck := authedSession(t, store)
	freshenStepUp(t, store, ck.Value)
	return ck
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

// --- api.go unit tests ---

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

// TestAPIAuthRoutesNotShadowed proves mounting the /api/apps + /api/domains
// subtrees and the /api/* JSON-404 catch-all alongside the pre-existing
// /api/auth subtree does not shadow the auth routes (a chi radix-tree overlap
// guard).
func TestAPIAuthRoutesNotShadowed(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
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
		{"no kind match", &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: []string{"v1alpha1"}}, http.StatusServiceUnavailable, "cluster_not_ready"},
		// The dynamic RESTMapper's first-ever discovery miss surfaces the no-match
		// WRAPPED (apiutil.ErrResourceDiscoveryFailed unwraps to it); pin that
		// meta.IsNoMatchError matches through errors.Is, not a bare type assert.
		{"no kind match wrapped", fmt.Errorf("get apps: %w", &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: []string{"v1alpha1"}}), http.StatusServiceUnavailable, "cluster_not_ready"},
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
