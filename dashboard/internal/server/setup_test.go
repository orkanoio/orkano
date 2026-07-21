package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/internal/db"
)

// --- fakeStore settings methods (migration 00007) ---

func (f *fakeStore) GetSetting(_ context.Context, key string) (db.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settingsErr != nil {
		return db.Setting{}, f.settingsErr
	}
	s, ok := f.settings[key]
	if !ok {
		return db.Setting{}, pgx.ErrNoRows
	}
	return s, nil
}

func (f *fakeStore) UpsertSetting(_ context.Context, arg db.UpsertSettingParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settingsErr != nil {
		return f.settingsErr
	}
	f.settings[arg.Key] = db.Setting{Key: arg.Key, Value: arg.Value}
	return nil
}

func (f *fakeStore) ListSettings(context.Context) ([]db.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	out := make([]db.Setting, 0, len(f.settings))
	for _, s := range f.settings {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeStore) setSetting(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[key] = db.Setting{Key: key, Value: value}
}

// --- harness ---

// setupServer builds a server for the wizard endpoints: a fake K8s client
// seeded with objs, and a mutate hook for the setup-specific config knobs
// (WebhookURL, OIDC, OIDCValidator). It returns the K8s client too so tests can
// assert on Secret writes.
func setupServer(t *testing.T, store *fakeStore, mutate func(*Config), objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		Build()
	cfg := Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            &fakePodStreamer{},
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
		// Discovery succeeds by default; tests override to fail.
		OIDCValidator: func(context.Context, func(string) string) error { return nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, k8s
}

func decodeSetupStatus(t *testing.T, body []byte) setupStatusResponse {
	t.Helper()
	var resp setupStatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode setup status: %v", err)
	}
	return resp
}

func checkByID(t *testing.T, resp setupStatusResponse, id string) setupCheckJSON {
	t.Helper()
	for _, c := range resp.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q missing from response (got %+v)", id, resp.Checks)
	return setupCheckJSON{}
}

// oidcPlaceholder is the empty orkano-oidc Secret the install pre-creates; the
// wizard's value-blind UPDATE needs it to exist (github_test's helper).
func oidcPlaceholder() *corev1.Secret { return placeholderSecret(oidcSecretName) }

// validOIDCBody is a wizard OIDC form that passes LoadConfig.
func validOIDCBody() map[string]string {
	return map[string]string{
		"issuer":        "https://idp.example.com",
		"clientId":      "orkano-dashboard",
		"clientSecret":  "s3cret-value",
		"allowedEmails": "ops@example.com, dev@example.com",
	}
}

// --- setup status ---

// TestSetupStatusFresh: a brand-new install (no settings, no webhook URL, no
// OIDC, no Domains) reports every step unmet, with github.app-connected BLOCKED
// behind the missing webhook URL and domains.tls-ready not applicable.
func TestSetupStatusFresh(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, nil, readyNode("node-a", nil))

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", rec.Code, rec.Body)
	}
	resp := decodeSetupStatus(t, rec.Body.Bytes())

	// Dependency (registration) order is the wizard's walk order.
	wantOrder := []string{
		checkNodesReady, checkAccessModeChosen, checkAdminBootstrapped, checkOIDCConfigured,
		checkWebhookURLConfigured, checkGitHubAppConnected, checkVaultConnected,
		checkDomainTLSReady, checkFirstAppDeployed,
	}
	if len(resp.Checks) != len(wantOrder) {
		t.Fatalf("got %d checks, want %d", len(resp.Checks), len(wantOrder))
	}
	for i, id := range wantOrder {
		if resp.Checks[i].ID != id {
			t.Fatalf("check order[%d]: got %q, want %q", i, resp.Checks[i].ID, id)
		}
	}

	for id, outcome := range map[string]string{
		checkNodesReady:           "pass", // the cluster under a fresh install has its node
		checkAccessModeChosen:     "fail",
		checkAdminBootstrapped:    "pass", // the authed session implies a confirmed admin
		checkOIDCConfigured:       "fail",
		checkWebhookURLConfigured: "fail",
		checkGitHubAppConnected:   "blocked",
		checkDomainTLSReady:       "skip",
		checkFirstAppDeployed:     "fail",
		// The fake client's scheme knows the ESO kinds, so the list answers
		// empty rather than NoMatch: "installed, nothing connected" = fail.
		// The dedicated vault-check test covers the real NoMatch skip.
		checkVaultConnected: "fail",
	} {
		if got := checkByID(t, resp, id).Outcome; got != outcome {
			t.Errorf("%s outcome: got %q, want %q", id, got, outcome)
		}
	}
	// The remediation survives the JSON projection on an actionable outcome.
	if checkByID(t, resp, checkAccessModeChosen).Remediation == "" {
		t.Error("access-mode check lost its remediation in the JSON projection")
	}
	blocked := checkByID(t, resp, checkGitHubAppConnected)
	if len(blocked.Blockers) != 1 || blocked.Blockers[0] != checkWebhookURLConfigured {
		t.Errorf("github.app-connected blockers: got %v", blocked.Blockers)
	}
	if resp.AccessMode != "" || resp.WebhookURLConfigured || resp.OIDCEnabled ||
		resp.OIDCPendingRestart || resp.GitHub.Connected {
		t.Errorf("fresh install state drifted: %+v", resp)
	}
	if resp.PublicURLConfigured || resp.OIDCRedirectURL != "" {
		t.Errorf("no PublicURL: redirect URL must be request-derived, got %+v", resp)
	}
}

// TestSetupStatusPinnedRedirectURL: with ORKANO_PUBLIC_URL set, the status
// carries the exact callback URL a connect will register, so the wizard shows
// the authoritative value before the admin registers it at the IdP.
func TestSetupStatusPinnedRedirectURL(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, func(cfg *Config) {
		cfg.PublicURL = "https://dash.example.com"
	})

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	if !resp.PublicURLConfigured || resp.OIDCRedirectURL != "https://dash.example.com"+oidcCallbackPath {
		t.Fatalf("pinned redirect URL drifted: %+v", resp)
	}
}

// TestSetupStatusConfigured: with the webhook URL set, GitHub connected (the
// callback's settings marker), an access mode chosen, OIDC live, and a Domain
// carrying a ready certificate, every check passes.
func TestSetupStatusConfigured(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	store.setSetting(settingAccessMode, "tailscale")
	store.setSetting(settingGitHubAppSlug, "orkano-test")
	store.setSetting(settingGitHubAppID, "42")
	store.setSetting(settingGitHubConnectedAt, "2026-06-28T10:00:00Z")
	// The steady state after a restart: the marker predates process start
	// (fixedNow), so no restart is pending even though OIDC is live.
	store.setSetting(settingOIDCConfiguredAt, "2026-06-28T10:00:00Z")

	domain := &orkanov1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "app-example-com", Namespace: appsNamespace},
		Spec:       orkanov1alpha1.DomainSpec{Host: "app.example.com", AppRef: orkanov1alpha1.LocalObjectRef{Name: "web"}},
		Status: orkanov1alpha1.DomainStatus{
			Conditions: []metav1.Condition{{
				Type: orkanov1alpha1.ConditionCertificateReady, Status: metav1.ConditionTrue,
				Reason: "Ready", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	vaultStore := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1",
		"kind":       "SecretStore",
		"metadata":   map[string]any{"name": "team-vault", "namespace": appsNamespace},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}}
	runningApp := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/web"}},
			Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
		Status: orkanov1alpha1.AppStatus{
			Conditions: []metav1.Condition{{
				Type: orkanov1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Available", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	s, _ := setupServer(t, store, func(cfg *Config) {
		cfg.WebhookURL = "https://hooks.example.com/webhook"
		cfg.OIDC = &fakeOIDC{}
	}, domain, vaultStore, readyNode("node-a", nil), runningApp)

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (%s)", rec.Code, rec.Body)
	}
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	for _, c := range resp.Checks {
		if c.Outcome != "pass" {
			t.Errorf("%s: got %q (%s), want pass", c.ID, c.Outcome, c.Message)
		}
	}
	if resp.AccessMode != "tailscale" || !resp.WebhookURLConfigured || !resp.OIDCEnabled {
		t.Errorf("configured state drifted: %+v", resp)
	}
	if resp.OIDCPendingRestart {
		t.Error("a marker predating process start must not report a pending restart")
	}
	if !resp.GitHub.Connected || resp.GitHub.AppSlug != "orkano-test" || resp.GitHub.AppID != "42" {
		t.Errorf("github state drifted: %+v", resp.GitHub)
	}
}

// TestSetupStatusClusterDegraded: a NotReady node fails cluster.nodes-ready
// with honest counts, and created-but-unready apps fail apps.first-app-deployed
// without claiming an empty install.
func TestSetupStatusClusterDegraded(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	stuckApp := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/web"}},
			Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
	s, _ := setupServer(t, store, nil, readyNode("node-a", nil), notReadyNode("node-b"), stuckApp)

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	nodes := checkByID(t, resp, checkNodesReady)
	if nodes.Outcome != "fail" || !strings.Contains(nodes.Message, "1 of 2") {
		t.Fatalf("nodes-ready = %+v, want fail with 1-of-2 counts", nodes)
	}
	firstApp := checkByID(t, resp, checkFirstAppDeployed)
	if firstApp.Outcome != "fail" || !strings.Contains(firstApp.Message, "none Ready yet") {
		t.Fatalf("first-app = %+v, want fail naming unready apps", firstApp)
	}
}

// TestSetupStatusNodesUnreadable: an empty node list can only mean the read
// itself is broken (a cluster always has its own node), so the check reports a
// probe ERROR — unknown never counts as healthy OR unhealthy.
func TestSetupStatusNodesUnreadable(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, nil)

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	nodes := checkByID(t, resp, checkNodesReady)
	if nodes.Outcome != "error" {
		t.Fatalf("nodes-ready with no readable nodes = %+v, want a probe error", nodes)
	}
}

// TestSetupStatusOIDCRotationPending: OIDC is live but the Secret was rewritten
// AFTER this process started (a wizard rotation) — the restart prompt must
// return, and the check message must say so.
func TestSetupStatusOIDCRotationPending(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	// One minute after fixedNow (= process start).
	store.setSetting(settingOIDCConfiguredAt, "2026-06-28T12:01:00Z")
	s, _ := setupServer(t, store, func(cfg *Config) {
		cfg.OIDC = &fakeOIDC{}
	})

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	if !resp.OIDCPendingRestart || !resp.OIDCEnabled {
		t.Fatalf("rotation state: %+v", resp)
	}
	c := checkByID(t, resp, checkOIDCConfigured)
	if c.Outcome != "pass" || !strings.Contains(c.Message, "awaits a dashboard restart") {
		t.Fatalf("oidc check after rotation: %+v", c)
	}
}

// TestSetupStatusOIDCPendingRestart: the wizard wrote orkano-oidc but this
// process still runs without it — the status surfaces the restart, and the
// check message says so.
func TestSetupStatusOIDCPendingRestart(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	store.setSetting(settingOIDCConfiguredAt, "2026-07-02T10:00:00Z")
	s, _ := setupServer(t, store, nil)

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	if !resp.OIDCPendingRestart || resp.OIDCEnabled {
		t.Fatalf("pending-restart state: %+v", resp)
	}
	c := checkByID(t, resp, checkOIDCConfigured)
	if c.Outcome != "fail" || c.Message != "OIDC credentials written; restart the dashboard to activate them" {
		t.Fatalf("oidc check: %+v", c)
	}
}

// TestSetupStatusDomainListError: a viewer-client failure is a probe ERROR
// (unknown never counts as hardened), not a fail and not a 500.
func TestSetupStatusDomainListError(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, func(cfg *Config) {
		cfg.ViewerClient = failingListClient{Client: cfg.ViewerClient}
	})

	rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	resp := decodeSetupStatus(t, rec.Body.Bytes())
	if got := checkByID(t, resp, checkDomainTLSReady).Outcome; got != "error" {
		t.Fatalf("domains.tls-ready outcome: got %q, want error", got)
	}
}

// failingListClient fails every List — the domains probe's error leg.
type failingListClient struct{ client.Client }

func (f failingListClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("viewer down")
}

// TestSetupStatusRequiresSession: no cookie → 401, and the settings-unavailable
// path is a 503.
func TestSetupStatusRequiresSession(t *testing.T) {
	store := newFakeStore()
	s, _ := setupServer(t, store, nil)
	if rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}

	ck := authedSession(t, store)
	store.settingsErr = errors.New("db down")
	if rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings unavailable: got %d, want 503", rec.Code)
	}
}

// --- access mode ---

func TestSetAccessMode(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, nil)

	rec := apiReq(t, s, http.MethodPost, "/api/setup/access-mode", map[string]string{"mode": "proxy"}, ck)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set access mode: got %d (%s)", rec.Code, rec.Body)
	}
	if got, _ := store.GetSetting(context.Background(), settingAccessMode); got.Value != "proxy" {
		t.Fatalf("access mode not persisted: %+v", got)
	}
	if e := lastAudit(t, store, "setup.access_mode"); e.Target != "proxy" || e.Outcome != "success" {
		t.Fatalf("audit entry drifted: %+v", e)
	}

	// Re-choosing overwrites.
	if rec := apiReq(t, s, http.MethodPost, "/api/setup/access-mode", map[string]string{"mode": "public"}, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("overwrite access mode: got %d", rec.Code)
	}
	if got, _ := store.GetSetting(context.Background(), settingAccessMode); got.Value != "public" {
		t.Fatalf("access mode not overwritten: %+v", got)
	}
}

// TestSetAccessModeUnavailable: a DB failure is a 503 with a failure audit
// entry, and nothing persisted.
func TestSetAccessModeUnavailable(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, nil)

	store.settingsErr = errors.New("db down")
	rec := apiReq(t, s, http.MethodPost, "/api/setup/access-mode", map[string]string{"mode": "proxy"}, ck)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("db down: got %d, want 503 (%s)", rec.Code, rec.Body)
	}
	if e := lastAudit(t, store, "setup.access_mode"); e.Target != "proxy" || e.Outcome != "failure" {
		t.Fatalf("audit entry drifted: %+v", e)
	}
	store.settingsErr = nil
	if _, err := store.GetSetting(context.Background(), settingAccessMode); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("failed write was persisted: %v", err)
	}
}

func TestSetAccessModeRejectsUnknown(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s, _ := setupServer(t, store, nil)

	for _, mode := range []string{"", "internet", "PROXY", "proxy,public"} {
		rec := apiReq(t, s, http.MethodPost, "/api/setup/access-mode", map[string]string{"mode": mode}, ck)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("mode %q: got %d, want 400", mode, rec.Code)
		}
	}
	if _, err := store.GetSetting(context.Background(), settingAccessMode); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rejected mode was persisted: %v", err)
	}
}

// --- OIDC connect ---

func TestSetupOIDCRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store) // session, but no fresh second factor
	s, _ := setupServer(t, store, nil, oidcPlaceholder())

	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", validOIDCBody(), ck)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without step-up: got %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

func TestSetupOIDCWritesSecret(t *testing.T) {
	store := newFakeStore()
	ck := steppedUpSession(t, store)
	s, k8s := setupServer(t, store, nil, oidcPlaceholder())

	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", validOIDCBody(), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("oidc connect: got %d (%s)", rec.Code, rec.Body)
	}
	var resp struct {
		RedirectURL     string `json:"redirectUrl"`
		RestartRequired bool   `json:"restartRequired"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.RestartRequired || resp.RedirectURL != "http://example.com"+oidcCallbackPath {
		t.Fatalf("response drifted: %+v", resp)
	}

	var secret corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: systemNamespace, Name: oidcSecretName}, &secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	want := map[string]string{
		"ORKANO_OIDC_ISSUER":         "https://idp.example.com",
		"ORKANO_OIDC_CLIENT_ID":      "orkano-dashboard",
		"ORKANO_OIDC_CLIENT_SECRET":  "s3cret-value",
		"ORKANO_OIDC_REDIRECT_URL":   "http://example.com" + oidcCallbackPath,
		"ORKANO_OIDC_ALLOWED_EMAILS": "ops@example.com, dev@example.com",
	}
	if len(secret.Data) != len(want) {
		t.Fatalf("secret keys: got %v", keysOf(secret.Data))
	}
	for k, v := range want {
		if string(secret.Data[k]) != v {
			t.Errorf("secret[%s]: got %q, want %q", k, secret.Data[k], v)
		}
	}

	// The pending-restart marker is set, and the audit entry carries the issuer
	// and allowlist COUNTS — never the client secret (INV-03).
	if got, err := store.GetSetting(context.Background(), settingOIDCConfiguredAt); err != nil || got.Value == "" {
		t.Fatalf("configured marker missing: %v", err)
	}
	entry := lastAudit(t, store, "setup.oidc_configure")
	if entry.Target != "https://idp.example.com" || entry.Outcome != "success" {
		t.Fatalf("audit entry drifted: %+v", entry)
	}
	detail := string(entry.Detail)
	if !json.Valid(entry.Detail) || detail == "" {
		t.Fatalf("audit detail not JSON: %q", detail)
	}
	for _, forbidden := range []string{"s3cret-value", "ops@example.com"} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("audit detail leaks %q: %s", forbidden, detail)
		}
	}
}

// TestSetupOIDCWritesOptionalKeys: non-empty scopes/groupsClaim land in the
// Secret (empty ones are omitted so the loader's defaults apply — pinned by
// TestSetupOIDCWritesSecret's exact key count).
func TestSetupOIDCWritesOptionalKeys(t *testing.T) {
	store := newFakeStore()
	ck := steppedUpSession(t, store)
	s, k8s := setupServer(t, store, nil, oidcPlaceholder())

	body := validOIDCBody()
	body["scopes"] = "openid profile groups"
	body["groupsClaim"] = "roles"
	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", body, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("oidc connect: got %d (%s)", rec.Code, rec.Body)
	}

	var secret corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: systemNamespace, Name: oidcSecretName}, &secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if len(secret.Data) != 7 {
		t.Fatalf("secret keys: got %v, want 7", keysOf(secret.Data))
	}
	if got := string(secret.Data["ORKANO_OIDC_SCOPES"]); got != "openid profile groups" {
		t.Errorf("scopes: got %q", got)
	}
	if got := string(secret.Data["ORKANO_OIDC_GROUPS_CLAIM"]); got != "roles" {
		t.Errorf("groups claim: got %q", got)
	}
}

func TestSetupOIDCInvalidConfig(t *testing.T) {
	store := newFakeStore()
	ck := steppedUpSession(t, store)
	s, k8s := setupServer(t, store, nil, oidcPlaceholder())

	body := validOIDCBody()
	delete(body, "clientId") // required by LoadConfig
	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", body, ck)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid config: got %d (%s)", rec.Code, rec.Body)
	}
	assertNoOIDCWrite(t, k8s, store)

	// No allowlist at all is fail-closed (would disable OIDC on restart).
	body = validOIDCBody()
	delete(body, "allowedEmails")
	if rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", body, ck); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing allowlist: got %d", rec.Code)
	}
}

func TestSetupOIDCDiscoveryFailure(t *testing.T) {
	store := newFakeStore()
	ck := steppedUpSession(t, store)
	s, k8s := setupServer(t, store, func(cfg *Config) {
		cfg.OIDCValidator = func(context.Context, func(string) string) error {
			return errors.New("issuer unreachable")
		}
	}, oidcPlaceholder())

	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", validOIDCBody(), ck)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("discovery failure: got %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeBody(t, rec)["error"]; got != "oidc_discovery_failed" {
		t.Fatalf("error code: got %q", got)
	}
	assertNoOIDCWrite(t, k8s, store)
	if e := lastAudit(t, store, "setup.oidc_configure"); e.Target != "https://idp.example.com" || e.Outcome != "failure" {
		t.Fatalf("audit entry drifted: %+v", e)
	}
}

// TestSetupOIDCMissingPlaceholder: the grant is update-only, so a missing
// placeholder (a broken install) surfaces as not_found rather than silently
// creating an object the RBAC pin would refuse anyway.
func TestSetupOIDCMissingPlaceholder(t *testing.T) {
	store := newFakeStore()
	ck := steppedUpSession(t, store)
	s, _ := setupServer(t, store, nil) // no placeholder seeded

	rec := apiReq(t, s, http.MethodPost, "/api/setup/oidc", validOIDCBody(), ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing placeholder: got %d (%s)", rec.Code, rec.Body)
	}
	if got, err := store.GetSetting(context.Background(), settingOIDCConfiguredAt); err == nil {
		t.Fatalf("marker set despite failed write: %+v", got)
	}
}

// --- GitHub connect marker (the callback's settings write) ---

// TestGitHubCallbackRecordsSettings drives the real callback happy path (over
// the github_test harness) and asserts the wizard's connect markers landed.
func TestGitHubCallbackRecordsSettings(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{creds: testCreds()}, true)

	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])

	cb := getWithCookies(t, s, "/api/github/app/callback?state="+state+"&code=c0de", flow)
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != "/?github=connected" {
		t.Fatalf("callback: got %d -> %q", cb.Code, cb.Header().Get("Location"))
	}

	for key, want := range map[string]string{
		settingGitHubAppSlug: "orkano-acme",
		settingGitHubAppID:   "424242",
	} {
		if got, err := store.GetSetting(context.Background(), key); err != nil || got.Value != want {
			t.Errorf("setting %s: got %+v, %v; want %q", key, got, err, want)
		}
	}
	if got, err := store.GetSetting(context.Background(), settingGitHubConnectedAt); err != nil || got.Value == "" {
		t.Errorf("connected-at marker missing: %v", err)
	}
}

// TestGitHubCallbackSettingsFailureDoesNotFailConnect: the Secrets are already
// written, so a marker failure must not break the redirect.
func TestGitHubCallbackSettingsFailureDoesNotFailConnect(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{creds: testCreds()}, true)

	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])

	store.settingsErr = errors.New("db down")
	cb := getWithCookies(t, s, "/api/github/app/callback?state="+state+"&code=c0de", flow)
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != "/?github=connected" {
		t.Fatalf("callback with settings failure: got %d -> %q", cb.Code, cb.Header().Get("Location"))
	}
}

// --- small helpers ---

func assertNoOIDCWrite(t *testing.T, k8s client.Client, store *fakeStore) {
	t.Helper()
	var secret corev1.Secret
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: systemNamespace, Name: oidcSecretName}, &secret)
	if err == nil && len(secret.Data) != 0 {
		t.Fatalf("secret was written despite the refusal: %v", keysOf(secret.Data))
	}
	if _, err := store.GetSetting(context.Background(), settingOIDCConfiguredAt); err == nil {
		t.Fatalf("configured marker set despite the refusal")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lastAudit returns the most recent audit entry for an action.
func lastAudit(t *testing.T, store *fakeStore, action string) db.AppendAuditEntryParams {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := len(store.audit) - 1; i >= 0; i-- {
		if store.audit[i].Action == action {
			return store.audit[i]
		}
	}
	t.Fatalf("no audit entry for action %q (have %+v)", action, store.audit)
	return db.AppendAuditEntryParams{}
}

// TestSetupStatusVaultCheck pins the secrets.vault-connected branches the
// shared status tests cannot reach: ESO absent (NoMatch → skip, the optional
// path) and a connected store that is not Ready (fail with guidance).
func TestSetupStatusVaultCheck(t *testing.T) {
	t.Run("eso absent skips", func(t *testing.T) {
		store := newFakeStore()
		ck := authedSession(t, store)
		noMatch := interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "SecretStoreList" {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "external-secrets.io", Kind: "SecretStore"}}
				}
				return cl.List(ctx, list, opts...)
			},
		}
		k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(noMatch).Build()
		s := serverWith(t, store, k8s)

		rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
		resp := decodeSetupStatus(t, rec.Body.Bytes())
		c := checkByID(t, resp, checkVaultConnected)
		if c.Outcome != "skip" || !strings.Contains(c.Message, "--secrets-vault") {
			t.Fatalf("got %q (%s), want skip pointing at --secrets-vault", c.Outcome, c.Message)
		}
	})

	t.Run("store not ready fails", func(t *testing.T) {
		store := newFakeStore()
		ck := authedSession(t, store)
		broken := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "SecretStore",
			"metadata":   map[string]any{"name": "team-vault", "namespace": appsNamespace},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "False"}},
			},
		}}
		s, _ := setupServer(t, store, nil, broken)

		rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
		resp := decodeSetupStatus(t, rec.Body.Bytes())
		c := checkByID(t, resp, checkVaultConnected)
		if c.Outcome != "fail" || !strings.Contains(c.Message, "0 of 1 store(s) Ready") {
			t.Fatalf("got %q (%s), want fail counting the unready store", c.Outcome, c.Message)
		}
	})

	// Every connected store must be Ready — one healthy store must not hide a
	// second one's expired credential behind a Done badge (review-caught).
	t.Run("partial ready fails", func(t *testing.T) {
		store := newFakeStore()
		ck := authedSession(t, store)
		mk := func(name, ready string) *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "external-secrets.io/v1",
				"kind":       "SecretStore",
				"metadata":   map[string]any{"name": name, "namespace": appsNamespace},
				"status": map[string]any{
					"conditions": []any{map[string]any{"type": "Ready", "status": ready}},
				},
			}}
		}
		s, _ := setupServer(t, store, nil, mk("vault-a", "True"), mk("vault-b", "False"))

		rec := apiReq(t, s, http.MethodGet, "/api/setup/status", nil, ck)
		resp := decodeSetupStatus(t, rec.Body.Bytes())
		c := checkByID(t, resp, checkVaultConnected)
		if c.Outcome != "fail" || !strings.Contains(c.Message, "1 of 2") {
			t.Fatalf("got %q (%s), want fail with the 1-of-2 count", c.Outcome, c.Message)
		}
	})
}
