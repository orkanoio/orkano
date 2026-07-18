package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func qty(t *testing.T, s string) *resource.Quantity {
	t.Helper()
	q := resource.MustParse(s)
	return &q
}

func seedPostgres(t *testing.T, name, size string) *orkanov1alpha1.Postgres {
	t.Helper()
	return &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec:       orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, size)},
		Status: orkanov1alpha1.PostgresStatus{
			SecretName: name,
			Conditions: []metav1.Condition{{
				Type:               orkanov1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Available",
				LastTransitionTime: metav1.NewTime(fixedNow()),
			}},
		},
	}
}

func getPostgres(t *testing.T, s *Server, name string) (orkanov1alpha1.Postgres, error) {
	t.Helper()
	var p orkanov1alpha1.Postgres
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &p)
	return p, err
}

func TestCreatePostgres(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	body := postgresCreateRequest{Name: "api-db", Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPost, "/api/postgres", body, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getPostgres(t, s, "api-db")
	if err != nil {
		t.Fatalf("created postgres not found: %v", err)
	}
	if got.Spec.Version != "16" || got.Spec.StorageSize.String() != "10Gi" {
		t.Fatalf("spec not stored: %+v", got.Spec)
	}
	// The dashboard writes spec only — the operator-owned status stays empty.
	if got.Status.SecretName != "" || len(got.Status.Conditions) != 0 {
		t.Fatalf("dashboard wrote status: %+v", got.Status)
	}
	assertAudited(t, store, "postgres.create", "success")
}

func TestCreatePostgresCannotEnablePgwebWithoutStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	body := postgresCreateRequest{
		Name: "api-db",
		Spec: orkanov1alpha1.PostgresSpec{
			Version: "16", StorageSize: qty(t, "10Gi"),
			Pgweb: &orkanov1alpha1.PgwebSpec{Enabled: true},
		},
	}
	rec := apiReq(t, s, http.MethodPost, "/api/postgres", body, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "pgweb_requires_step_up" {
		t.Fatalf("create with Pgweb = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := getPostgres(t, s, "api-db"); !apierrors.IsNotFound(err) {
		t.Fatalf("Postgres was created despite rejected Pgweb enable: %v", err)
	}
}

func TestCreatePostgresConflict(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	body := postgresCreateRequest{Name: "api-db", Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPost, "/api/postgres", body, ck)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "already_exists" {
		t.Fatalf("error = %v, want already_exists", got)
	}
	assertAudited(t, store, "postgres.create", "failure")
}

func TestCreatePostgresInvalidName(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	body := postgresCreateRequest{Name: "Bad_Name!", Spec: orkanov1alpha1.PostgresSpec{Version: "16"}}
	rec := apiReq(t, s, http.MethodPost, "/api/postgres", body, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "invalid_name" {
		t.Fatalf("error = %v, want invalid_name", got)
	}
}

func TestCreatePostgresRequiresSession(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)

	body := postgresCreateRequest{Name: "api-db", Spec: orkanov1alpha1.PostgresSpec{Version: "16"}}
	if rec := apiReq(t, s, http.MethodPost, "/api/postgres", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", rec.Code)
	}
}

func TestGetPostgres(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/postgres/api-db", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["name"] != "api-db" {
		t.Fatalf("name = %v", body["name"])
	}
	// Status surfaced read-only (the connection-Secret name, never a value).
	status, _ := body["status"].(map[string]any)
	if status["secretName"] != "api-db" {
		t.Fatalf("status not surfaced: %v", body["status"])
	}
}

func TestListPostgresNamespacePinned(t *testing.T) {
	store := newFakeStore()
	elsewhere := seedPostgres(t, "elsewhere", "10Gi")
	elsewhere.Namespace = "other-ns"
	s := apiServer(t, store, seedPostgres(t, "a", "10Gi"), seedPostgres(t, "b", "10Gi"), elsewhere)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/postgres", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (orkano-apps only)", len(items))
	}
}

func TestListPostgresEmpty(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/postgres", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty list = %d, want 200", rec.Code)
	}
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatal("items missing or null, want []")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

func TestUpdatePostgresGrowStorage(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	body := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "20Gi")}}
	rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", body, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("grow = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getPostgres(t, s, "api-db")
	if got.Spec.StorageSize.String() != "20Gi" {
		t.Fatalf("storage = %s, want 20Gi", got.Spec.StorageSize)
	}
	assertAudited(t, store, "postgres.update", "success")
}

// TestUpdatePostgresEqualSizeAllowed proves an equal-size update is not mistaken
// for a shrink (the grow-only guard is Cmp < 0, so equal passes through).
func TestUpdatePostgresEqualSizeAllowed(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	body := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "10Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", body, ck); rec.Code != http.StatusOK {
		t.Fatalf("equal-size update = %d, want 200", rec.Code)
	}
}

func TestUpdatePostgresNotFoundAudited(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	body := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "10Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/ghost", body, ck); rec.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", rec.Code)
	}
	assertAudited(t, store, "postgres.update", "failure")
}

// TestUpdatePostgresShrinkForbidden proves the dashboard refuses a storage
// shrink client-side (the apiserver does not — only the reconciler would, with a
// worse UX), and nothing is written.
func TestUpdatePostgresShrinkForbidden(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "20Gi"))
	ck := authedSession(t, store)

	body := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", body, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("shrink = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "storage_shrink_forbidden" {
		t.Fatalf("error = %v, want storage_shrink_forbidden", got)
	}
	got, _ := getPostgres(t, s, "api-db")
	if got.Spec.StorageSize.String() != "20Gi" {
		t.Fatalf("storage changed despite the refused shrink: %s", got.Spec.StorageSize)
	}
}

// TestUpdatePostgresOmittedStoragePreserved proves an update that omits
// storageSize keeps the current size rather than letting the schema default
// re-shrink it to 10Gi.
func TestUpdatePostgresOmittedStoragePreserved(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "20Gi"))
	ck := authedSession(t, store)

	body := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16"}} // no storageSize
	rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", body, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getPostgres(t, s, "api-db")
	if got.Spec.StorageSize == nil || got.Spec.StorageSize.String() != "20Gi" {
		t.Fatalf("omitted storage was not preserved: %v", got.Spec.StorageSize)
	}
}

func TestUpdatePostgresPreservesPgweb(t *testing.T) {
	store := newFakeStore()
	pg := seedPostgres(t, "api-db", "10Gi")
	pg.Spec.Pgweb = &orkanov1alpha1.PgwebSpec{Enabled: true}
	s := apiServer(t, store, pg)
	ck := authedSession(t, store)

	grow := postgresUpdateRequest{Spec: orkanov1alpha1.PostgresSpec{Version: "16", StorageSize: qty(t, "20Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", grow, ck); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getPostgres(t, s, "api-db")
	if !got.PgwebEnabled() {
		t.Fatal("ordinary Postgres update disabled Pgweb")
	}

	grow.Spec.Pgweb = &orkanov1alpha1.PgwebSpec{Enabled: false}
	rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db", grow, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "use_pgweb_endpoint" {
		t.Fatalf("ordinary Pgweb change = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestUpdatePgwebRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db/pgweb", pgwebUpdateRequest{Enabled: true}, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("enable without step-up = %d, want 403", rec.Code)
	}
	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db/pgweb", pgwebUpdateRequest{Enabled: true}, ck); rec.Code != http.StatusOK {
		t.Fatalf("enable with step-up = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getPostgres(t, s, "api-db")
	if !got.PgwebEnabled() {
		t.Fatal("Pgweb was not enabled")
	}
	assertAudited(t, store, "postgres.pgweb.enable", "success")

	if rec := apiReq(t, s, http.MethodPut, "/api/postgres/api-db/pgweb", pgwebUpdateRequest{Enabled: false}, ck); rec.Code != http.StatusOK {
		t.Fatalf("disable with step-up = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ = getPostgres(t, s, "api-db")
	if got.Spec.Pgweb != nil {
		t.Fatalf("Pgweb spec after disable = %+v, want nil", got.Spec.Pgweb)
	}
	assertAudited(t, store, "postgres.pgweb.disable", "success")
}

func TestPgwebProxyUsesSessionAndDoesNotForwardCredentials(t *testing.T) {
	store := newFakeStore()
	pg := seedPostgres(t, "api-db", "10Gi")
	pg.Spec.Pgweb = &orkanov1alpha1.PgwebSpec{Enabled: true}
	pg.Status.PgwebServiceName = "api-db-pgweb"
	pg.Status.Conditions = append(pg.Status.Conditions, metav1.Condition{
		Type: orkanov1alpha1.ConditionPgwebReady, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.Now(),
	})
	s := apiServer(t, store, pg)
	s.cfg.PgwebTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "http" || req.URL.Host != "api-db-pgweb.orkano-apps.svc.cluster.local:8081" || req.URL.Path != "/api/postgres/api-db/pgweb/" {
			t.Errorf("upstream URL = %s", req.URL.String())
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Forwarded") != "" || req.Header.Get("X-Forwarded-For") != "" {
			t.Errorf("browser credentials or forwarding headers leaked upstream: %v", req.Header)
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "text/html")
		headers.Set("Set-Cookie", "pgweb=secret")
		headers.Set("WWW-Authenticate", "Basic")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader("<h1>Pgweb</h1>")),
			Request:    req,
		}, nil
	})
	ck := authedSession(t, store)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/postgres/api-db/pgweb/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.AddCookie(ck)
	req.Header.Set("Authorization", "Bearer browser-secret")
	req.Header.Set("Forwarded", "for=attacker")
	req.Header.Set("X-Forwarded-For", "attacker")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pgweb") {
		t.Fatalf("proxy = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("upstream auth headers leaked to browser: %v", rec.Header())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("proxy security headers = %v", rec.Header())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src http://example.com/api/postgres/api-db/pgweb/") || strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("proxy CSP does not confine browser API calls to Pgweb: %q", csp)
	}
	assertAudited(t, store, "postgres.pgweb.open", "success")

	unauthorized := apiReq(t, s, http.MethodGet, "/api/postgres/api-db/pgweb/", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("open without dashboard session = %d, want 401", unauthorized.Code)
	}
}

func TestDeletePostgresRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedPostgres(t, "api-db", "10Gi"))
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodDelete, "/api/postgres/api-db", nil, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("delete without step-up = %d, want 403", rec.Code)
	}
	if _, err := getPostgres(t, s, "api-db"); err != nil {
		t.Fatalf("postgres deleted despite 403: %v", err)
	}

	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodDelete, "/api/postgres/api-db", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with step-up = %d, want 204", rec.Code)
	}
	if _, err := getPostgres(t, s, "api-db"); !apierrors.IsNotFound(err) {
		t.Fatalf("postgres not deleted: err=%v", err)
	}
	assertAudited(t, store, "postgres.delete", "success")
}

func TestDeletePostgresNotFoundAudited(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	if rec := apiReq(t, s, http.MethodDelete, "/api/postgres/ghost", nil, ck); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
	assertAudited(t, store, "postgres.delete", "failure")
}
