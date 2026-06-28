package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// viewerScheme builds a fake client carrying the orkano scheme, seeded with objs
// — used as a distinct viewer (read) client in the routing tests.
func viewerScheme(t *testing.T, objs ...orkanov1alpha1.App) *fake.ClientBuilder {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{})
	for i := range objs {
		b = b.WithObjects(&objs[i])
	}
	return b
}

// TestViewerConfigSetsImpersonation pins the FIXED impersonation identity: the
// resourceNames-pinned user + group, and the base config is never mutated.
func TestViewerConfigSetsImpersonation(t *testing.T) {
	base := &rest.Config{Host: "https://api.example"}
	cfg := viewerConfig(base)

	if base.Impersonate.UserName != "" || len(base.Impersonate.Groups) != 0 {
		t.Fatalf("viewerConfig mutated the base config: %+v", base.Impersonate)
	}
	if cfg.Impersonate.UserName != ViewerUser {
		t.Fatalf("Impersonate.UserName = %q, want %q", cfg.Impersonate.UserName, ViewerUser)
	}
	if len(cfg.Impersonate.Groups) != 1 || cfg.Impersonate.Groups[0] != ViewerGroup {
		t.Fatalf("Impersonate.Groups = %v, want [%s]", cfg.Impersonate.Groups, ViewerGroup)
	}
}

// TestViewerImpersonationHeadersOnWire proves the config serializes to the
// Impersonate-User / Impersonate-Group headers the apiserver authorizes against.
func TestViewerImpersonationHeadersOnWire(t *testing.T) {
	var gotUser, gotGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("Impersonate-User")
		gotGroup = r.Header.Get("Impersonate-Group")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt, err := rest.TransportFor(viewerConfig(&rest.Config{Host: srv.URL}))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/readyz", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	if gotUser != ViewerUser {
		t.Fatalf("Impersonate-User = %q, want %q", gotUser, ViewerUser)
	}
	if gotGroup != ViewerGroup {
		t.Fatalf("Impersonate-Group = %q, want %q", gotGroup, ViewerGroup)
	}
}

// TestReadsUseViewerClient proves a read view routes through the viewer client,
// not the SA client: the two are seeded with disjoint objects and a GET resolves
// against the viewer client's set only.
func TestReadsUseViewerClient(t *testing.T) {
	store := newFakeStore()
	saClient := viewerScheme(t, *seedApp("sa-only")).Build()
	viewer := viewerScheme(t, *seedApp("viewer-only")).Build()
	s := serverWithViewer(t, store, saClient, viewer)
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodGet, "/api/apps/viewer-only", nil, ck); rec.Code != http.StatusOK {
		t.Fatalf("read of viewer-client object = %d, want 200", rec.Code)
	}
	if rec := apiReq(t, s, http.MethodGet, "/api/apps/sa-only", nil, ck); rec.Code != http.StatusNotFound {
		t.Fatalf("read must not see the SA-only object = %d, want 404", rec.Code)
	}
}

// TestWritesUseSAClient proves the symmetric half: every write path uses the SA
// client, never the viewer. The viewer client here is empty, so any write that
// read through it would fail — yet create/update/env/delete all succeed via the
// SA client (which holds "demo").
func TestWritesUseSAClient(t *testing.T) {
	store := newFakeStore()
	saClient := viewerScheme(t, *seedApp("demo")).Build()
	viewer := viewerScheme(t).Build() // empty
	s := serverWithViewer(t, store, saClient, viewer)
	ck := steppedUpSession(t, store) // satisfies the session + step-up tiers

	if rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "new", Spec: webAppSpec()}, ck); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s) — did it touch the viewer client?", rec.Code, rec.Body.String())
	}
	if rec := apiReq(t, s, http.MethodPut, "/api/apps/demo", appUpdateRequest{Spec: webAppSpec()}, ck); rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env", setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck); rec.Code != http.StatusOK {
		t.Fatalf("env = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := apiReq(t, s, http.MethodDelete, "/api/apps/demo", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", rec.Code, rec.Body.String())
	}
}
