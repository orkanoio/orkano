package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// TestViewerConfigSetsImpersonation pins the impersonation: the fixed viewer
// group is the load-bearing identity, the username rides along for the audit
// trail, and the base config is never mutated.
func TestViewerConfigSetsImpersonation(t *testing.T) {
	base := &rest.Config{Host: "https://api.example"}
	cfg := viewerConfig(base, "alice")

	if base.Impersonate.UserName != "" || len(base.Impersonate.Groups) != 0 {
		t.Fatalf("viewerConfig mutated the base config: %+v", base.Impersonate)
	}
	if cfg.Impersonate.UserName != "alice" {
		t.Fatalf("Impersonate.UserName = %q, want alice", cfg.Impersonate.UserName)
	}
	if len(cfg.Impersonate.Groups) != 1 || cfg.Impersonate.Groups[0] != ViewerGroup {
		t.Fatalf("Impersonate.Groups = %v, want [%s]", cfg.Impersonate.Groups, ViewerGroup)
	}
}

// TestViewerImpersonationHeadersOnWire proves the impersonation config actually
// serializes to Impersonate-User / Impersonate-Group headers — the wire contract
// the apiserver authorizes against.
func TestViewerImpersonationHeadersOnWire(t *testing.T) {
	var gotUser, gotGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("Impersonate-User")
		gotGroup = r.Header.Get("Impersonate-Group")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt, err := rest.TransportFor(viewerConfig(&rest.Config{Host: srv.URL}, "alice"))
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

	if gotUser != "alice" {
		t.Fatalf("Impersonate-User = %q, want alice", gotUser)
	}
	if gotGroup != ViewerGroup {
		t.Fatalf("Impersonate-Group = %q, want %q", gotGroup, ViewerGroup)
	}
}

// TestReadsUseViewerClient proves a read view routes through the viewer client,
// not the SA client: the two clients are seeded with disjoint objects, and a GET
// resolves against the viewer client's set only.
func TestReadsUseViewerClient(t *testing.T) {
	store := newFakeStore()
	saClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{}).
		WithObjects(seedApp("sa-only")).
		Build()
	viewer := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{}).
		WithObjects(seedApp("viewer-only")).
		Build()
	s := serverWithViewer(t, store, saClient, func(string) (client.Client, error) { return viewer, nil })
	ck := authedSession(t, store)

	// The viewer client's object resolves; the SA-only object is invisible to a
	// read view (proving reads do not use the SA client).
	if rec := apiReq(t, s, http.MethodGet, "/api/apps/viewer-only", nil, ck); rec.Code != http.StatusOK {
		t.Fatalf("read of viewer-client object = %d, want 200", rec.Code)
	}
	if rec := apiReq(t, s, http.MethodGet, "/api/apps/sa-only", nil, ck); rec.Code != http.StatusNotFound {
		t.Fatalf("read must not see the SA-only object = %d, want 404", rec.Code)
	}
}

// TestViewerClientBuildFailureIs500 proves a factory error surfaces as a 500, not
// a panic or a silent fall-through to the SA client.
func TestViewerClientBuildFailureIs500(t *testing.T) {
	store := newFakeStore()
	saClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := serverWithViewer(t, store, saClient, func(string) (client.Client, error) {
		return nil, errors.New("build failed")
	})
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodGet, "/api/apps", nil, ck); rec.Code != http.StatusInternalServerError {
		t.Fatalf("viewer build failure = %d, want 500", rec.Code)
	}
}

// TestWritesUseSAClient proves the symmetric half of the read/write split: every
// write path uses the SA client, never the viewer. The viewer factory always
// errors here, so any write that touched it would 500 — yet create/update/env/
// delete all succeed.
func TestWritesUseSAClient(t *testing.T) {
	store := newFakeStore()
	saClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{}).
		WithObjects(seedApp("demo")).
		Build()
	s := serverWithViewer(t, store, saClient, func(string) (client.Client, error) {
		return nil, errors.New("viewer unavailable")
	})
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
