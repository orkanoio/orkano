package server

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// webAppSpec is the minimal valid App spec the CRUD tests round-trip. The fake
// client does not run CEL, so a structurally-complete spec is enough here; the
// apiserver's validation is proven in the operator's envtest suite.
func webAppSpec() orkanov1alpha1.AppSpec {
	return orkanov1alpha1.AppSpec{
		Source: orkanov1alpha1.Source{GitHub: orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"}},
		Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
	}
}

// seedApp is a stored App carrying operator-owned status, so a read surfaces it
// and an update must be shown to preserve it.
func seedApp(name string) *orkanov1alpha1.App {
	return &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec:       webAppSpec(),
		Status: orkanov1alpha1.AppStatus{
			Image: "registry/demo@sha256:abc",
			Conditions: []metav1.Condition{{
				Type:               orkanov1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Available",
				LastTransitionTime: metav1.NewTime(fixedNow()),
			}},
		},
	}
}

func getApp(t *testing.T, s *Server, name string) (orkanov1alpha1.App, error) {
	t.Helper()
	var app orkanov1alpha1.App
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &app)
	return app, err
}

func TestCreateApp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getApp(t, s, "demo")
	if err != nil {
		t.Fatalf("created app not found: %v", err)
	}
	if got.Spec.Source.GitHub.Repo != "orkanoio/demo" {
		t.Fatalf("spec not stored: %+v", got.Spec)
	}
	// The dashboard writes spec only — the operator-owned status stays empty.
	if len(got.Status.Conditions) != 0 || got.Status.Image != "" {
		t.Fatalf("dashboard wrote status: %+v", got.Status)
	}
	assertAudited(t, store, "app.create", "success")
}

func TestCreateAppInvalidName(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "Bad_Name!", Spec: webAppSpec()}, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "invalid_name" {
		t.Fatalf("error = %v, want invalid_name", got)
	}
	// A rejected create never reaches the apiserver, so it is not audited.
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.audit) != 0 {
		t.Fatalf("client-side rejection should not audit: %+v", store.audit)
	}
}

// TestCreateAppConflict proves a duplicate name surfaces as a 409 (the most
// likely user-facing error from a naming UI) and audits the failed attempt.
func TestCreateAppConflict(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "already_exists" {
		t.Fatalf("error = %v, want already_exists", got)
	}
	assertAudited(t, store, "app.create", "failure")
}

func TestCreateAppRequiresSession(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", rec.Code)
	}
}

// TestCreateAppRejectsStatusField proves DisallowUnknownFields blocks an attempt
// to inject the operator-owned status through the create body.
func TestCreateAppRejectsStatusField(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	body := map[string]any{
		"name":   "demo",
		"spec":   webAppSpec(),
		"status": map[string]any{"image": "evil@sha256:bad"},
	}
	rec := apiReq(t, s, http.MethodPost, "/api/apps", body, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status injection = %d, want 400", rec.Code)
	}
}

func TestGetApp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/demo", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["name"] != "demo" || body["namespace"] != appsNamespace {
		t.Fatalf("identity = %v", body)
	}
	status, _ := body["status"].(map[string]any)
	if status["image"] != "registry/demo@sha256:abc" {
		t.Fatalf("status not surfaced read-only: %v", body["status"])
	}
}

func TestGetAppNotFound(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/ghost", nil, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing app = %d, want 404", rec.Code)
	}
}

// TestListAppsNamespacePinned proves List returns only orkano-apps objects — the
// only namespace the dashboard Role grants — never apps elsewhere.
func TestListAppsNamespacePinned(t *testing.T) {
	store := newFakeStore()
	elsewhere := seedApp("elsewhere")
	elsewhere.Namespace = "other-ns"
	s := apiServer(t, store, seedApp("a"), seedApp("b"), elsewhere)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (orkano-apps only)", len(items))
	}
}

// TestListAppsEmpty proves an empty list returns 200 with items:[] (not null,
// not 404) — the slice is preallocated to length 0 so JSON renders [].
func TestListAppsEmpty(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty list = %d, want 200", rec.Code)
	}
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatalf("items missing or null, want []")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

func TestUpdateApp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := authedSession(t, store)

	newSpec := webAppSpec()
	newSpec.Source.GitHub.Repo = "orkanoio/changed"
	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo", appUpdateRequest{Spec: newSpec}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getApp(t, s, "demo")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Spec.Source.GitHub.Repo != "orkanoio/changed" {
		t.Fatalf("spec not updated: %+v", got.Spec)
	}
	// The spec-only update must not disturb the operator-owned status.
	if len(got.Status.Conditions) == 0 || got.Status.Image != "registry/demo@sha256:abc" {
		t.Fatalf("update clobbered status: %+v", got.Status)
	}
	assertAudited(t, store, "app.update", "success")
}

func TestUpdateAppNotFoundAudited(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/ghost", appUpdateRequest{Spec: webAppSpec()}, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", rec.Code)
	}
	// The intent was a mutation, so the failed attempt is audited (INV-08).
	assertAudited(t, store, "app.update", "failure")
}

func TestDeleteAppRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := authedSession(t, store)

	// A plain session (no fresh second factor) is forbidden, and the object stays.
	if rec := apiReq(t, s, http.MethodDelete, "/api/apps/demo", nil, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("delete without step-up = %d, want 403", rec.Code)
	}
	if _, err := getApp(t, s, "demo"); err != nil {
		t.Fatalf("app deleted despite 403: %v", err)
	}

	// With a fresh step-up the delete succeeds.
	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodDelete, "/api/apps/demo", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with step-up = %d, want 204", rec.Code)
	}
	if _, err := getApp(t, s, "demo"); !apierrors.IsNotFound(err) {
		t.Fatalf("app not deleted: err=%v", err)
	}
	assertAudited(t, store, "app.delete", "success")
}

// TestDeleteAppNotFoundAudited proves a delete of a missing app is a 404 and the
// failed attempt is still audited (INV-08).
func TestDeleteAppNotFoundAudited(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodDelete, "/api/apps/ghost", nil, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
	assertAudited(t, store, "app.delete", "failure")
}
