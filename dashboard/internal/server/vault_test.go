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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func storeRequest(name string) map[string]any {
	return map[string]any{
		"name": name,
		"vault": map[string]any{
			"server": "https://vault.internal.example:8200",
			"path":   "secret",
		},
		"token": "s.scoped-vault-token",
	}
}

func seedStore(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	req := secretStoreWriteRequest{Name: name}
	req.Vault.Server = "https://vault.internal.example:8200"
	req.Vault.Path = "secret"
	req.Vault.Version = "v2"
	store := secretStoreObject(&req)
	if err := c.Create(context.Background(), store); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

func TestSecretStoreConnectFlow(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/secretstores", storeRequest("team-vault"), ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create store = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// The response is value-blind — the token must never be echoed.
	if strings.Contains(rec.Body.String(), "s.scoped-vault-token") {
		t.Fatal("store response echoed the credential")
	}

	// The written SecretStore carries the dashboard-owned shape: auth pinned to
	// the sibling credentials Secret, never a caller-chosen ref.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(secretStoreGVK)
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "team-vault"}, got); err != nil {
		t.Fatalf("get store: %v", err)
	}
	refName, _, _ := unstructured.NestedString(got.Object, "spec", "provider", "vault", "auth", "tokenSecretRef", "name")
	if refName != "team-vault-credentials" {
		t.Fatalf("tokenSecretRef.name = %q, want team-vault-credentials", refName)
	}
	version, _, _ := unstructured.NestedString(got.Object, "spec", "provider", "vault", "version")
	if version != "v2" {
		t.Fatalf("version = %q, want default v2", version)
	}

	// The credentials Secret exists, holds the token, and is OWNED by the
	// store so deletion cascades (the dashboard has no secrets delete).
	var sec corev1.Secret
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "team-vault-credentials"}, &sec); err != nil {
		t.Fatalf("get credentials: %v", err)
	}
	if string(sec.Data["token"]) != "s.scoped-vault-token" {
		t.Fatal("credentials Secret does not hold the token")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "SecretStore" || sec.OwnerReferences[0].UID != got.GetUID() {
		t.Fatalf("credentials Secret not owned by the store: %+v", sec.OwnerReferences)
	}

	// List reflects it, without values.
	rec = apiReq(t, s, http.MethodGet, "/api/secretstores", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"team-vault"`) || strings.Contains(rec.Body.String(), "scoped-vault-token") {
		t.Fatalf("list body wrong: %s", rec.Body.String())
	}

	// Delete removes the store (the Secret cascades via its ownerRef in a real
	// cluster; envtest/fake run no GC).
	rec = apiReq(t, s, http.MethodDelete, "/api/secretstores/team-vault", nil, ck)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	// Every vault write lands in the audit log (INV-08).
	assertAudited(t, store, "secretstore.create", "success")
	assertAudited(t, store, "secretstore.delete", "success")
}

// TestSecretStoreCreateRefusesTakenCredentialsName pins the review-found
// clobber: a Postgres named <store>-credentials owns the connection Secret of
// that name (ADR-0014), and a connect must never blind-overwrite it — the
// detectable case 409s up front, anything else squatting on the name trips
// the strict create and rolls the store back.
func TestSecretStoreCreateRefusesTakenCredentialsName(t *testing.T) {
	store := newFakeStore()
	pg := &orkanov1alpha1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: "team-vault-credentials", Namespace: appsNamespace}}
	s := apiServer(t, store, pg)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/secretstores", storeRequest("team-vault"), ck)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["error"] != "credentials_name_taken" {
		t.Fatalf("connect over a Postgres connection Secret = %d %s, want 409 credentials_name_taken", rec.Code, rec.Body.String())
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(secretStoreGVK)
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "team-vault"}, got); err == nil {
		t.Fatal("SecretStore was created despite the taken credentials name")
	}

	// The same name occupied by an arbitrary foreign Secret (no Postgres to
	// see): the strict create trips AlreadyExists, the store rolls back, and
	// the foreign Secret's data is untouched.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-credentials", Namespace: appsNamespace},
		Data:       map[string][]byte{"uri": []byte("postgres://keep-me")},
	}
	s2 := apiServer(t, store, foreign)
	rec = apiReq(t, s2, http.MethodPost, "/api/secretstores", storeRequest("acme"), ck)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["error"] != "credentials_name_taken" {
		t.Fatalf("connect over a foreign Secret = %d %s, want 409 credentials_name_taken", rec.Code, rec.Body.String())
	}
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(secretStoreGVK)
	if err := s2.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "acme"}, got); err == nil {
		t.Fatal("SecretStore not rolled back after the credentials-write refusal")
	}
	var sec corev1.Secret
	if err := s2.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "acme-credentials"}, &sec); err != nil {
		t.Fatalf("foreign secret gone: %v", err)
	}
	if string(sec.Data["uri"]) != "postgres://keep-me" {
		t.Fatal("foreign Secret data was clobbered by the connect")
	}

	// Rotation against a taken name refuses too (a kubectl-authored store
	// could sit at a name whose credentials Secret belongs to a Postgres).
	s3 := apiServer(t, store, pg)
	seedStore(t, s3.cfg.K8s, "team-vault")
	req := storeRequest("team-vault")
	rec = apiReq(t, s3, http.MethodPut, "/api/secretstores/team-vault", req, ck)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["error"] != "credentials_name_taken" {
		t.Fatalf("rotation over a Postgres connection Secret = %d %s, want 409", rec.Code, rec.Body.String())
	}
}

// TestCreatePostgresRefusesESOClaimedNames is the mirror guard: the catalog
// names its connection Secret after the object, so a name an ESO sync target
// or a store's credentials Secret claims is refused up front (the operator's
// AlreadyOwnedError → ProvisionFailed is the backstop).
func TestCreatePostgresRefusesESOClaimedNames(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	seedStore(t, s.cfg.K8s, "team-vault")
	es := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": externalSecretGVK.GroupVersion().String(),
		"kind":       externalSecretGVK.Kind,
		"metadata":   map[string]any{"name": "api-stripe", "namespace": appsNamespace},
	}}
	if err := s.cfg.K8s.Create(context.Background(), es); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, name := range []string{"api-stripe", "team-vault-credentials"} {
		body := map[string]any{"name": name, "spec": map[string]any{}}
		rec := apiReq(t, s, http.MethodPost, "/api/postgres", body, ck)
		if rec.Code != http.StatusConflict || decodeBody(t, rec)["error"] != "name_conflict" {
			t.Errorf("create postgres %q = %d %s, want 409 name_conflict", name, rec.Code, rec.Body.String())
		}
	}

	// An unclaimed name still creates.
	rec := apiReq(t, s, http.MethodPost, "/api/postgres", map[string]any{"name": "clean-db", "spec": map[string]any{}}, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create postgres clean-db = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestSecretStoreWritesRequireStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/secretstores"},
		{http.MethodPut, "/api/secretstores/team-vault"},
		{http.MethodDelete, "/api/secretstores/team-vault"},
		{http.MethodPost, "/api/externalsecrets"},
		{http.MethodDelete, "/api/externalsecrets/api-stripe"},
	} {
		rec := apiReq(t, s, tc.method, tc.path, storeRequest("team-vault"), ck)
		if rec.Code != http.StatusForbidden || decodeBody(t, rec)["error"] != "step_up_required" {
			t.Errorf("%s %s = %d %s, want 403 step_up_required", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestSecretStoreCreateValidation(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	cases := []struct {
		name string
		mut  func(m map[string]any)
		want string
	}{
		{"bad name", func(m map[string]any) { m["name"] = "Not_A_Name" }, "invalid_name"},
		{"reserved credentials suffix", func(m map[string]any) { m["name"] = "x-credentials" }, "invalid_name"},
		{"reserved env suffix", func(m map[string]any) { m["name"] = "x-env" }, "invalid_name"},
		{"http server", func(m map[string]any) {
			m["vault"].(map[string]any)["server"] = "http://vault.internal:8200"
		}, "vault_server_must_be_https"},
		{"empty path", func(m map[string]any) { m["vault"].(map[string]any)["path"] = "" }, "invalid_vault_path"},
		{"bad version", func(m map[string]any) { m["vault"].(map[string]any)["version"] = "v3" }, "invalid_vault_version"},
		{"missing token", func(m map[string]any) { m["token"] = "" }, "missing_token"},
	}
	for _, tc := range cases {
		req := storeRequest("team-vault")
		tc.mut(req)
		rec := apiReq(t, s, http.MethodPost, "/api/secretstores", req, ck)
		if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != tc.want {
			t.Errorf("%s: got %d %s, want 400 %s", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestSecretStoreUpdateRotation(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)
	seeded := seedStore(t, s.cfg.K8s, "team-vault")
	if err := s.writeCredentialsSecret(context.Background(), seeded, "old-token", false); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}

	// Spec-only update: empty token keeps the current credential.
	req := storeRequest("team-vault")
	req["token"] = ""
	req["vault"].(map[string]any)["server"] = "https://vault2.internal.example:8200"
	rec := apiReq(t, s, http.MethodPut, "/api/secretstores/team-vault", req, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	var sec corev1.Secret
	key := client.ObjectKey{Namespace: appsNamespace, Name: "team-vault-credentials"}
	if err := s.cfg.K8s.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("get credentials: %v", err)
	}
	if string(sec.Data["token"]) != "old-token" {
		t.Fatal("spec-only update must not touch the credential")
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(secretStoreGVK)
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "team-vault"}, got); err != nil {
		t.Fatalf("get store: %v", err)
	}
	server, _, _ := unstructured.NestedString(got.Object, "spec", "provider", "vault", "server")
	if server != "https://vault2.internal.example:8200" {
		t.Fatalf("server = %q after update", server)
	}

	// Rotation: a token replaces the credential.
	req["token"] = "new-token"
	rec = apiReq(t, s, http.MethodPut, "/api/secretstores/team-vault", req, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}
	if err := s.cfg.K8s.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("get credentials: %v", err)
	}
	if string(sec.Data["token"]) != "new-token" {
		t.Fatal("rotation did not replace the credential")
	}

	assertAudited(t, store, "secretstore.update", "success")

	// Updating a missing store is a 404, not an upsert.
	rec = apiReq(t, s, http.MethodPut, "/api/secretstores/nope", storeRequest("nope"), ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", rec.Code)
	}
}

func TestExternalSecretCreateShape(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)
	seedStore(t, s.cfg.K8s, "team-vault")

	body := map[string]any{
		"name":      "api-stripe",
		"storeName": "team-vault",
		"keys": []map[string]any{
			{"secretKey": "STRIPE_KEY", "remoteKey": "apps/api/stripe"},
		},
	}
	rec := apiReq(t, s, http.MethodPost, "/api/externalsecrets", body, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(externalSecretGVK)
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: "api-stripe"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// The dashboard-owned shape: Owner policy + target.name == metadata.name +
	// namespaced-store ref + the defaulted refresh interval.
	policy, _, _ := unstructured.NestedString(got.Object, "spec", "target", "creationPolicy")
	target, _, _ := unstructured.NestedString(got.Object, "spec", "target", "name")
	kind, _, _ := unstructured.NestedString(got.Object, "spec", "secretStoreRef", "kind")
	interval, _, _ := unstructured.NestedString(got.Object, "spec", "refreshInterval")
	if policy != "Owner" || target != "api-stripe" || kind != "SecretStore" || interval != defaultRefreshInterval {
		t.Fatalf("wrong shape: policy=%q target=%q kind=%q interval=%q", policy, target, kind, interval)
	}

	// The delete leg.
	rec = apiReq(t, s, http.MethodDelete, "/api/externalsecrets/api-stripe", nil, ck)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	assertAudited(t, store, "externalsecret.create", "success")
	assertAudited(t, store, "externalsecret.delete", "success")
}

func TestExternalSecretCreateValidation(t *testing.T) {
	store := newFakeStore()
	pg := &orkanov1alpha1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: "api-db", Namespace: appsNamespace}}
	s := apiServer(t, store, pg)
	ck := steppedUpSession(t, store)
	seedStore(t, s.cfg.K8s, "team-vault")

	base := func() map[string]any {
		return map[string]any{
			"name":      "api-stripe",
			"storeName": "team-vault",
			"keys": []map[string]any{
				{"secretKey": "STRIPE_KEY", "remoteKey": "apps/api/stripe"},
			},
		}
	}
	cases := []struct {
		name   string
		mut    func(m map[string]any)
		status int
		want   string
	}{
		{"credentials suffix", func(m map[string]any) { m["name"] = "team-vault-credentials" }, 400, "reserved_name"},
		{"env suffix", func(m map[string]any) { m["name"] = "api-env" }, 400, "reserved_name"},
		{"postgres collision", func(m map[string]any) { m["name"] = "api-db" }, 409, "name_conflict"},
		{"unknown store", func(m map[string]any) { m["storeName"] = "nope" }, 400, "unknown_store"},
		{"bad interval", func(m map[string]any) { m["refreshInterval"] = "yearly" }, 400, "invalid_refresh_interval"},
		{"no keys", func(m map[string]any) { m["keys"] = []map[string]any{} }, 400, "invalid_keys"},
		{"bad env name", func(m map[string]any) {
			m["keys"] = []map[string]any{{"secretKey": "not valid", "remoteKey": "x"}}
		}, 400, "invalid_keys"},
		{"dotted key rejected by the EnvVar pattern", func(m map[string]any) {
			m["keys"] = []map[string]any{{"secretKey": "my.key", "remoteKey": "x"}}
		}, 400, "invalid_keys"},
		{"duplicate key", func(m map[string]any) {
			m["keys"] = []map[string]any{
				{"secretKey": "A", "remoteKey": "x"},
				{"secretKey": "A", "remoteKey": "y"},
			}
		}, 400, "invalid_keys"},
		{"empty remote", func(m map[string]any) {
			m["keys"] = []map[string]any{{"secretKey": "A", "remoteKey": ""}}
		}, 400, "invalid_keys"},
	}
	for _, tc := range cases {
		req := base()
		tc.mut(req)
		rec := apiReq(t, s, http.MethodPost, "/api/externalsecrets", req, ck)
		if rec.Code != tc.status || decodeBody(t, rec)["error"] != tc.want {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.status, tc.want)
		}
	}
}

func TestExternalSecretListReadsStatus(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	es := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": externalSecretGVK.GroupVersion().String(),
		"kind":       externalSecretGVK.Kind,
		"metadata":   map[string]any{"name": "api-stripe", "namespace": appsNamespace},
		"spec": map[string]any{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]any{"kind": "SecretStore", "name": "team-vault"},
			"target":          map[string]any{"name": "api-stripe", "creationPolicy": "Owner"},
			"data": []any{
				map[string]any{"secretKey": "STRIPE_KEY", "remoteRef": map[string]any{"key": "apps/api/stripe"}},
			},
		},
		"status": map[string]any{
			"refreshTime": "2026-07-06T10:00:00Z",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "False", "reason": "SecretSyncedError", "message": "boom"},
			},
		},
	}}
	if err := s.cfg.K8s.Create(context.Background(), es); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := apiReq(t, s, http.MethodGet, "/api/externalsecrets", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"ready":"False"`, `"reason":"SecretSyncedError"`, `"storeName":"team-vault"`, `"remoteKey":"apps/api/stripe"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list body missing %s: %s", want, body)
		}
	}
}

// TestVaultRoutesWithoutESO pins the missing-CRD mapping: a cluster that never
// opted in answers with secrets_vault_not_installed (actionable: re-run init
// with --secrets-vault), never the self-healing cluster_not_ready.
func TestVaultRoutesWithoutESO(t *testing.T) {
	store := newFakeStore()
	noMatch := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			if _, ok := list.(*unstructured.UnstructuredList); ok {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "external-secrets.io", Kind: "SecretStore"}}
			}
			return nil
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(noMatch).Build()
	s := serverWith(t, store, k8s)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/secretstores", nil, ck)
	if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["error"] != "secrets_vault_not_installed" {
		t.Fatalf("list without ESO = %d %s, want 503 secrets_vault_not_installed", rec.Code, rec.Body.String())
	}
}

// TestVaultReadsUseViewerClient proves the list routes go through the
// impersonating viewer, not the SA client (ADR-0015).
func TestVaultReadsUseViewerClient(t *testing.T) {
	store := newFakeStore()
	viewer := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	sa := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := serverWithViewer(t, store, sa, viewer)
	ck := authedSession(t, store)

	seedStore(t, viewer, "viewer-only")

	rec := apiReq(t, s, http.MethodGet, "/api/secretstores", nil, ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "viewer-only") {
		t.Fatalf("viewer-seeded store not listed: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSecretStoreAuditNeverCarriesToken (INV-03): the vault write paths audit
// object names only — no entry may ever carry a submitted credential. The
// INV-03 whole-codebase audit proposed this as the vault analog of the env
// editor's assertEnvAudited guard.
func TestSecretStoreAuditNeverCarriesToken(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	if rec := apiReq(t, s, http.MethodPost, "/api/secretstores", storeRequest("team-vault"), ck); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	rotate := storeRequest("team-vault")
	rotate["token"] = "s.rotated-token"
	if rec := apiReq(t, s, http.MethodPut, "/api/secretstores/team-vault", rotate, ck); rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.audit) < 2 {
		t.Fatalf("expected audit entries for create+rotate, got %d", len(store.audit))
	}
	for _, e := range store.audit {
		blob := fmt.Sprintf("%+v", e)
		for _, secret := range []string{"s.scoped-vault-token", "s.rotated-token"} {
			if strings.Contains(blob, secret) {
				t.Errorf("audit entry %s carries a credential", e.Action)
			}
		}
	}
}

// TestSecretStoreCreateRollbackDoubleFailure exercises the worst connect path:
// the credentials write fails AND the compensating store delete fails. The
// original write error must surface (never masked by the rollback), and no
// audit entry may carry the token.
func TestSecretStoreCreateRollbackDoubleFailure(t *testing.T) {
	store := newFakeStore()
	failing := interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewInternalError(errors.New("secret write boom"))
			}
			return cl.Create(ctx, obj, opts...)
		},
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return apierrors.NewInternalError(errors.New("rollback boom"))
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(failing).Build()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/secretstores", storeRequest("team-vault"), ck)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("double failure = %d %s, want the credentials-write 500", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, e := range store.audit {
		if strings.Contains(fmt.Sprintf("%+v", e), "s.scoped-vault-token") {
			t.Errorf("audit entry %s carries the credential", e.Action)
		}
	}
}
