package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func getEnvSecret(t *testing.T, s *Server, app string) (corev1.Secret, error) {
	t.Helper()
	var sec corev1.Secret
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: envSecretName(app)}, &sec)
	return sec, err
}

func TestSetEnvCreatesSecretAndRefs(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "k-value", "DB_PASS": "p-value"}}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("set env = %d (%s)", rec.Code, rec.Body.String())
	}

	// Secret written with the values, owned by the App so it cascades on delete.
	sec, err := getEnvSecret(t, s, "demo")
	if err != nil {
		t.Fatalf("env secret not created: %v", err)
	}
	if string(sec.Data["API_KEY"]) != "k-value" || string(sec.Data["DB_PASS"]) != "p-value" {
		t.Fatalf("secret data = %v", sec.Data)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "demo" {
		t.Fatalf("env secret not owned by the app: %+v", sec.OwnerReferences)
	}

	// spec.env references the per-app Secret by key.
	app, _ := getApp(t, s, "demo")
	refs := map[string]string{}
	for _, e := range app.Spec.Env {
		if e.SecretRef != nil {
			refs[e.Name] = e.SecretRef.Name + "/" + e.SecretRef.Key
		}
	}
	if refs["API_KEY"] != "demo-env/API_KEY" || refs["DB_PASS"] != "demo-env/DB_PASS" {
		t.Fatalf("spec.env refs = %v", refs)
	}

	// INV-03: neither the response body nor the audit detail carries a value.
	if strings.Contains(rec.Body.String(), "k-value") || strings.Contains(rec.Body.String(), "p-value") {
		t.Fatalf("response leaked a secret value: %s", rec.Body.String())
	}
	assertEnvAudited(t, store)
}

// assertEnvAudited confirms a success env.update audit row carries the key NAMES
// but never the seeded values.
func assertEnvAudited(t *testing.T, store *fakeStore) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, e := range store.audit {
		if e.Action != "env.update" || e.Outcome != "success" {
			continue
		}
		d := string(e.Detail)
		if !strings.Contains(d, "API_KEY") || !strings.Contains(d, "DB_PASS") {
			t.Fatalf("audit detail missing key names: %s", d)
		}
		if strings.Contains(d, "k-value") || strings.Contains(d, "p-value") {
			t.Fatalf("audit detail leaked a secret value: %s", d)
		}
		return
	}
	t.Fatalf("no env.update success audit entry: %+v", store.audit)
}

func TestSetEnvRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := authedSession(t, store) // a session, but no fresh second factor

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("set env without step-up = %d, want 403", rec.Code)
	}
	if _, err := getEnvSecret(t, s, "demo"); err == nil {
		t.Fatal("secret written despite 403")
	}
}

// TestSetEnvOverwritesWholeSet proves the value-blind whole-object write: a second
// PUT replaces the Secret entirely (a dropped key is gone, since the dashboard
// cannot read-modify-write), and spec.env tracks it.
func TestSetEnvOverwritesWholeSet(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := steppedUpSession(t, store)

	if rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v1", "OLD": "x"}}, ck); rec.Code != http.StatusOK {
		t.Fatalf("first set = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v2"}}, ck); rec.Code != http.StatusOK {
		t.Fatalf("second set = %d (%s)", rec.Code, rec.Body.String())
	}

	sec, err := getEnvSecret(t, s, "demo")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["API_KEY"]) != "v2" {
		t.Fatalf("API_KEY = %q, want v2", sec.Data["API_KEY"])
	}
	if _, ok := sec.Data["OLD"]; ok {
		t.Fatal("whole-object overwrite should have dropped OLD")
	}
	app, _ := getApp(t, s, "demo")
	for _, e := range app.Spec.Env {
		if e.Name == "OLD" {
			t.Fatal("spec.env still references the dropped OLD var")
		}
	}
}

// TestSetEnvPreservesOtherEnv proves the editor touches only its own secretRefs:
// plaintext vars and refs to other Secrets (e.g. a Postgres connection Secret)
// survive, while a stale managed ref is replaced.
func TestSetEnvPreservesOtherEnv(t *testing.T) {
	store := newFakeStore()
	app := seedApp("demo")
	app.Spec.Env = []orkanov1alpha1.EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "DATABASE_URL", SecretRef: &orkanov1alpha1.SecretKeyRef{Name: "api-db", Key: "uri"}},
		{Name: "STALE", SecretRef: &orkanov1alpha1.SecretKeyRef{Name: "demo-env", Key: "STALE"}},
	}
	s := apiServer(t, store, app)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("set env = %d (%s)", rec.Code, rec.Body.String())
	}

	got, _ := getApp(t, s, "demo")
	byName := map[string]orkanov1alpha1.EnvVar{}
	for _, e := range got.Spec.Env {
		byName[e.Name] = e
	}
	if byName["LOG_LEVEL"].Value != "debug" {
		t.Fatal("plaintext env var not preserved")
	}
	if byName["DATABASE_URL"].SecretRef == nil || byName["DATABASE_URL"].SecretRef.Name != "api-db" {
		t.Fatal("foreign secret ref not preserved")
	}
	if _, ok := byName["STALE"]; ok {
		t.Fatal("stale managed ref not removed")
	}
	if byName["API_KEY"].SecretRef == nil || byName["API_KEY"].SecretRef.Name != "demo-env" {
		t.Fatal("new managed ref not added")
	}
}

func TestSetEnvInvalidName(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedApp("demo"))
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"bad-name": "v"}}, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid env name = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "invalid_env" {
		t.Fatalf("error = %v, want invalid_env", got)
	}
	if _, err := getEnvSecret(t, s, "demo"); err == nil {
		t.Fatal("secret written despite an invalid name")
	}
	// A client-side rejection never reaches the apiserver, so it is not audited.
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.audit) != 0 {
		t.Fatalf("client-side rejection should not audit: %+v", store.audit)
	}
}

// TestSetEnvReplacesPlaintextOnNameCollision proves a secret key that collides
// with an existing plaintext var REPLACES it (one entry, now secretRef-backed) —
// not appends a duplicate listType=map key the apiserver would reject.
func TestSetEnvReplacesPlaintextOnNameCollision(t *testing.T) {
	store := newFakeStore()
	app := seedApp("demo")
	app.Spec.Env = []orkanov1alpha1.EnvVar{{Name: "API_KEY", Value: "plaintext"}}
	s := apiServer(t, store, app)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "now-secret"}}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("collision set = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getApp(t, s, "demo")
	var count int
	var apiKey orkanov1alpha1.EnvVar
	for _, e := range got.Spec.Env {
		if e.Name == "API_KEY" {
			count++
			apiKey = e
		}
	}
	if count != 1 {
		t.Fatalf("API_KEY appears %d times, want exactly 1 (no duplicate map key)", count)
	}
	if apiKey.SecretRef == nil || apiKey.Value != "" {
		t.Fatalf("API_KEY should now be a secretRef, got %+v", apiKey)
	}
}

// TestSetEnvAppNameTooLong proves a derived Secret name over the object-name limit
// is a clean 400 before any write.
func TestSetEnvAppNameTooLong(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	longName := strings.Repeat("a", 250) // 250 + len("-env") = 254 > 253
	rec := apiReq(t, s, http.MethodPut, "/api/apps/"+longName+"/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long app name = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "app_name_too_long" {
		t.Fatalf("error = %v, want app_name_too_long", got)
	}
}

// TestSetEnvLimitExceeded proves the env count is bounded BEFORE the Secret write,
// so an over-limit set leaves nothing written.
func TestSetEnvLimitExceeded(t *testing.T) {
	store := newFakeStore()
	app := seedApp("demo")
	app.Spec.Env = nil
	for i := 0; i < maxEnvVars; i++ {
		app.Spec.Env = append(app.Spec.Env, orkanov1alpha1.EnvVar{Name: fmt.Sprintf("VAR_%d", i), Value: "x"})
	}
	s := apiServer(t, store, app)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("env over limit = %d, want 400", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "env_limit_exceeded" {
		t.Fatalf("error = %v, want env_limit_exceeded", got)
	}
	if _, err := getEnvSecret(t, s, "demo"); err == nil {
		t.Fatal("secret written despite exceeding the env limit")
	}
}

// TestSetEnvEmptyClears proves an empty set clears the managed secret env (empty
// Secret + all managed refs removed) while leaving plaintext vars in place.
func TestSetEnvEmptyClears(t *testing.T) {
	store := newFakeStore()
	app := seedApp("demo")
	app.Spec.Env = []orkanov1alpha1.EnvVar{
		{Name: "KEEP", Value: "x"},
		{Name: "GONE", SecretRef: &orkanov1alpha1.SecretKeyRef{Name: "demo-env", Key: "GONE"}},
	}
	s := apiServer(t, store, app)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env", setEnvRequest{Secrets: map[string]string{}}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := getApp(t, s, "demo")
	var keep bool
	for _, e := range got.Spec.Env {
		if e.Name == "GONE" {
			t.Fatal("managed ref not cleared")
		}
		if e.Name == "KEEP" {
			keep = true
		}
	}
	if !keep {
		t.Fatal("plaintext var dropped on clear")
	}
	sec, err := getEnvSecret(t, s, "demo")
	if err != nil {
		t.Fatalf("empty secret not written: %v", err)
	}
	if len(sec.Data) != 0 {
		t.Fatalf("secret should be empty, got %v", sec.Data)
	}
}

// TestSetEnvSpecUpdateFailureAfterSecretWritten proves the value-blind ordering:
// the Secret is written before the spec Update, so when the Update fails the
// values are present (recoverable) and the failed attempt is audited.
func TestSetEnvSpecUpdateFailureAfterSecretWritten(t *testing.T) {
	store := newFakeStore()
	failAppUpdate := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*orkanov1alpha1.App); ok {
				return apierrors.NewConflict(schema.GroupResource{Group: "orkano.io", Resource: "apps"}, obj.GetName(), errors.New("stale"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Domain{}).
		WithObjects(seedApp("demo")).
		WithInterceptorFuncs(failAppUpdate).
		Build()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/demo/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusConflict {
		t.Fatalf("spec update conflict = %d, want 409", rec.Code)
	}
	if _, err := getEnvSecret(t, s, "demo"); err != nil {
		t.Fatalf("Secret should be written before the failed spec update: %v", err)
	}
	assertAudited(t, store, "env.update", "failure")
}

func TestSetEnvAppNotFound(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/apps/ghost/env",
		setEnvRequest{Secrets: map[string]string{"API_KEY": "v"}}, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing app = %d, want 404", rec.Code)
	}
	assertAudited(t, store, "env.update", "failure")
}
