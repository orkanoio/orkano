package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func seedMongo(t *testing.T, name, size string) *orkanov1alpha1.Mongo {
	t.Helper()
	return &orkanov1alpha1.Mongo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec:       orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, size)},
		Status: orkanov1alpha1.MongoStatus{
			SecretName: name,
			Conditions: []metav1.Condition{{
				Type: orkanov1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.NewTime(fixedNow()),
			}},
		},
	}
}

func getMongo(t *testing.T, s *Server, name string) (orkanov1alpha1.Mongo, error) {
	t.Helper()
	var mongo orkanov1alpha1.Mongo
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &mongo)
	return mongo, err
}

func TestCreateMongo(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	body := mongoCreateRequest{Name: "document-db", Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPost, "/api/mongo", body, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getMongo(t, s, "document-db")
	if err != nil || got.Spec.Version != "8.0" || got.Spec.StorageSize.String() != "10Gi" {
		t.Fatalf("created Mongo = %+v, err=%v", got, err)
	}
	if got.Status.SecretName != "" || len(got.Status.Conditions) != 0 {
		t.Fatalf("dashboard wrote operator-owned status: %+v", got.Status)
	}
	assertAudited(t, store, "mongo.create", "success")
}

func TestCreateMongoCannotEnableExpressWithoutStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	body := mongoCreateRequest{
		Name: "document-db",
		Spec: orkanov1alpha1.MongoSpec{
			Version:      "8.0",
			StorageSize:  qty(t, "10Gi"),
			MongoExpress: &orkanov1alpha1.MongoExpressSpec{Enabled: true},
		},
	}
	rec := apiReq(t, s, http.MethodPost, "/api/mongo", body, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "mongo_express_requires_step_up" {
		t.Fatalf("create with Mongo Express = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := getMongo(t, s, "document-db"); !apierrors.IsNotFound(err) {
		t.Fatalf("Mongo was created despite rejected Mongo Express enable: %v", err)
	}
}

func TestMongoReadAndList(t *testing.T) {
	store := newFakeStore()
	elsewhere := seedMongo(t, "elsewhere", "10Gi")
	elsewhere.Namespace = "other-ns"
	s := apiServer(t, store, seedMongo(t, "documents", "20Gi"), elsewhere)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/mongo/documents", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	status, _ := body["status"].(map[string]any)
	if status["secretName"] != "documents" {
		t.Fatalf("status not surfaced: %v", status)
	}

	rec = apiReq(t, s, http.MethodGet, "/api/mongo", nil, ck)
	items, _ := decodeBody(t, rec)["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list = %d items=%d, want namespace-pinned one", rec.Code, len(items))
	}
}

func TestUpdateMongoGrowAndRejectShrink(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedMongo(t, "documents", "10Gi"))
	ck := authedSession(t, store)

	grow := mongoUpdateRequest{Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "20Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", grow, ck); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d (%s)", rec.Code, rec.Body.String())
	}
	shrink := mongoUpdateRequest{Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", shrink, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "storage_shrink_forbidden" {
		t.Fatalf("shrink = %d (%s), want 400 storage_shrink_forbidden", rec.Code, rec.Body.String())
	}
	got, _ := getMongo(t, s, "documents")
	if got.Spec.StorageSize.String() != "20Gi" {
		t.Fatalf("storage changed despite refused shrink: %s", got.Spec.StorageSize)
	}
}

func TestUpdateMongoPreservesMongoExpress(t *testing.T) {
	store := newFakeStore()
	mongo := seedMongo(t, "documents", "10Gi")
	mongo.Spec.MongoExpress = &orkanov1alpha1.MongoExpressSpec{Enabled: true}
	s := apiServer(t, store, mongo)
	ck := authedSession(t, store)

	grow := mongoUpdateRequest{Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "20Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", grow, ck); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getMongo(t, s, "documents")
	if !got.MongoExpressEnabled() {
		t.Fatal("ordinary Mongo update disabled Mongo Express")
	}

	grow.Spec.MongoExpress = &orkanov1alpha1.MongoExpressSpec{Enabled: false}
	rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", grow, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "use_mongo_express_endpoint" {
		t.Fatalf("ordinary Mongo Express change = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateMongoExpressRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedMongo(t, "documents", "10Gi"))
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents/express", mongoExpressUpdateRequest{Enabled: true}, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("enable without step-up = %d, want 403", rec.Code)
	}
	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents/express", mongoExpressUpdateRequest{Enabled: true}, ck); rec.Code != http.StatusOK {
		t.Fatalf("enable with step-up = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getMongo(t, s, "documents")
	if !got.MongoExpressEnabled() {
		t.Fatal("Mongo Express was not enabled")
	}
	assertAudited(t, store, "mongo.express.enable", "success")

	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents/express", mongoExpressUpdateRequest{Enabled: false}, ck); rec.Code != http.StatusOK {
		t.Fatalf("disable with step-up = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ = getMongo(t, s, "documents")
	if got.Spec.MongoExpress != nil {
		t.Fatalf("Mongo Express spec after disable = %+v, want nil", got.Spec.MongoExpress)
	}
	assertAudited(t, store, "mongo.express.disable", "success")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestMongoExpressProxyUsesSessionAndDoesNotForwardCredentials(t *testing.T) {
	store := newFakeStore()
	mongo := seedMongo(t, "documents", "10Gi")
	mongo.Spec.MongoExpress = &orkanov1alpha1.MongoExpressSpec{Enabled: true}
	mongo.Status.MongoExpressServiceName = "documents-mongo-express"
	mongo.Status.Conditions = append(mongo.Status.Conditions, metav1.Condition{
		Type: orkanov1alpha1.ConditionMongoExpressReady, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.Now(),
	})
	s := apiServer(t, store, mongo)
	s.cfg.MongoExpressTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "http" || req.URL.Host != "documents-mongo-express.orkano-apps.svc.cluster.local:8081" || req.URL.Path != "/api/mongo/documents/express/" {
			t.Errorf("upstream URL = %s", req.URL.String())
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Forwarded") != "" || req.Header.Get("X-Forwarded-For") != "" {
			t.Errorf("browser credentials or forwarding headers leaked upstream: %v", req.Header)
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "text/html")
		headers.Set("Set-Cookie", "express=secret")
		headers.Set("WWW-Authenticate", "Basic")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader("<h1>Mongo Express</h1>")),
			Request:    req,
		}, nil
	})
	ck := authedSession(t, store)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/mongo/documents/express/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.AddCookie(ck)
	req.Header.Set("Authorization", "Bearer browser-secret")
	req.Header.Set("Forwarded", "for=attacker")
	req.Header.Set("X-Forwarded-For", "attacker")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Mongo Express") {
		t.Fatalf("proxy = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("upstream auth headers leaked to browser: %v", rec.Header())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("proxy security headers = %v", rec.Header())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src http://example.com/api/mongo/documents/express/") || strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("proxy CSP does not confine browser API calls to Mongo Express: %q", csp)
	}
	assertAudited(t, store, "mongo.express.open", "success")

	unauthorized := apiReq(t, s, http.MethodGet, "/api/mongo/documents/express/", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("open without dashboard session = %d, want 401", unauthorized.Code)
	}
}

func TestDeleteMongoRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedMongo(t, "documents", "10Gi"))
	ck := authedSession(t, store)
	if rec := apiReq(t, s, http.MethodDelete, "/api/mongo/documents", nil, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("delete without step-up = %d, want 403", rec.Code)
	}
	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodDelete, "/api/mongo/documents", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with step-up = %d, want 204", rec.Code)
	}
	if _, err := getMongo(t, s, "documents"); !apierrors.IsNotFound(err) {
		t.Fatalf("Mongo not deleted: %v", err)
	}
	assertAudited(t, store, "mongo.delete", "success")
}

func TestCrossKindResourceNamesRejected(t *testing.T) {
	tests := []struct {
		name         string
		seed         client.Object
		path         string
		body         any
		action       string
		existingKind string
	}{
		{"app-blocked-by-postgres", seedPostgres(t, "shared", "10Gi"), "/api/apps", appCreateRequest{Name: "shared", Spec: webAppSpec()}, "app.create", "Postgres"},
		{"postgres-blocked-by-app", seedApp("shared"), "/api/postgres", postgresCreateRequest{Name: "shared", Spec: orkanov1alpha1.PostgresSpec{Version: "16"}}, "postgres.create", "App"},
		{"mongo-blocked-by-postgres", seedPostgres(t, "shared", "10Gi"), "/api/mongo", mongoCreateRequest{Name: "shared", Spec: orkanov1alpha1.MongoSpec{Version: "8.0"}}, "mongo.create", "Postgres"},
		{"postgres-blocked-by-mongo", seedMongo(t, "shared", "10Gi"), "/api/postgres", postgresCreateRequest{Name: "shared", Spec: orkanov1alpha1.PostgresSpec{Version: "16"}}, "postgres.create", "Mongo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			s := apiServer(t, store, tc.seed)
			ck := authedSession(t, store)
			rec := apiReq(t, s, http.MethodPost, tc.path, tc.body, ck)
			if rec.Code != http.StatusConflict {
				t.Fatalf("create = %d (%s), want 409", rec.Code, rec.Body.String())
			}
			body := decodeBody(t, rec)
			if body["error"] != "name_in_use" || body["existingKind"] != tc.existingKind {
				t.Fatalf("body = %v, want name_in_use existingKind=%s", body, tc.existingKind)
			}
			assertAudited(t, store, tc.action, "failure")
		})
	}
}
