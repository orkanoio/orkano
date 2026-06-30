package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
)

const testWebhookURL = "https://hooks.orkano.example/webhook"

// fakeExchanger drives the manifest callback without a live GitHub.
type fakeExchanger struct {
	creds   *GitHubAppCredentials
	err     error
	gotCode string
}

func (f *fakeExchanger) Exchange(_ context.Context, code string) (*GitHubAppCredentials, error) {
	f.gotCode = code
	if f.err != nil {
		return nil, f.err
	}
	return f.creds, nil
}

// placeholderSecret is the empty Secret the install pre-creates so the dashboard
// can blind-UPDATE it (its orkano-system grant is update-only, no create).
func placeholderSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace},
		Type:       corev1.SecretTypeOpaque,
	}
}

// githubServer builds a server wired for the manifest flow. Unless seedSecrets is
// false it pre-seeds the two orkano-system placeholder Secrets (mirroring the
// install) so the value-blind UPDATE lands.
func githubServer(t *testing.T, store *fakeStore, ex ManifestExchanger, seedSecrets bool) *Server {
	t.Helper()
	builder := fakeclient.NewClientBuilder().WithScheme(testScheme(t))
	if seedSecrets {
		builder = builder.WithObjects(placeholderSecret(githubAppSecretName), placeholderSecret(webhookSecretName))
	}
	k8s := builder.Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            &fakePodStreamer{},
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
		GitHub:             ex,
		WebhookURL:         testWebhookURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func testCreds() *GitHubAppCredentials {
	return &GitHubAppCredentials{
		ID:            424242,
		Slug:          "orkano-acme",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nMIIfake\n-----END RSA PRIVATE KEY-----\n",
		WebhookSecret: "gh-generated-webhook-secret",
	}
}

// startManifest runs the authenticated start endpoint and returns the parsed
// JSON body plus the flow cookie it set.
func startManifest(t *testing.T, s *Server, store *fakeStore, query string) (map[string]string, *http.Cookie) {
	t.Helper()
	ck := authedSession(t, store)
	rec := getWithCookies(t, s, "/api/github/app/manifest"+query, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest start = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode manifest body: %v", err)
	}
	flow := cookieNamed(rec, githubCookie)
	if flow == nil {
		t.Fatal("start set no flow cookie")
	}
	return body, flow
}

// stateFromPostURL extracts the state GitHub will round-trip back.
func stateFromPostURL(t *testing.T, postURL string) string {
	t.Helper()
	u, err := url.Parse(postURL)
	if err != nil {
		t.Fatalf("parse postUrl %q: %v", postURL, err)
	}
	return u.Query().Get("state")
}

func TestGitHubManifestRequiresSession(t *testing.T) {
	s := githubServer(t, newFakeStore(), &fakeExchanger{}, true)
	rec := getReq(t, s, "/api/github/app/manifest")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session start = %d, want 401", rec.Code)
	}
}

func TestGitHubManifestRequiresWebhookURL(t *testing.T) {
	store := newFakeStore()
	k8s := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, err := New(Config{
		K8s: k8s, ViewerClient: k8s, PodLogs: &fakePodStreamer{}, DB: fakePinger{},
		Store: store, Cipher: testCipherInstance, BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA: testSPA(), Now: fixedNow, GitHub: &fakeExchanger{}, // WebhookURL deliberately empty
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ck := authedSession(t, store)
	rec := getWithCookies(t, s, "/api/github/app/manifest", ck)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no webhook URL = %d, want 409", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "webhook_url_not_configured" {
		t.Fatalf("error = %v, want webhook_url_not_configured", got)
	}
}

func TestGitHubManifestStart(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{}, true)
	body, flow := startManifest(t, s, store, "")

	// postUrl points at the personal-account form and carries the state, which is
	// also sealed in the flow cookie.
	if !strings.HasPrefix(body["postUrl"], "https://github.com/settings/apps/new?state=") {
		t.Fatalf("postUrl = %q, want personal-account form", body["postUrl"])
	}
	if flow.Value == "" || flow.SameSite != http.SameSiteLaxMode || !flow.HttpOnly {
		t.Fatalf("flow cookie = %+v, want a sealed Lax HttpOnly cookie", flow)
	}

	var m manifest
	if err := json.Unmarshal([]byte(body["manifest"]), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Name != defaultAppName {
		t.Errorf("manifest name = %q, want %q", m.Name, defaultAppName)
	}
	if m.HookAttributes.URL != testWebhookURL {
		t.Errorf("hook url = %q, want %q", m.HookAttributes.URL, testWebhookURL)
	}
	if !strings.HasSuffix(m.RedirectURL, githubCallbackPath) {
		t.Errorf("redirect url = %q, want suffix %q", m.RedirectURL, githubCallbackPath)
	}
	if len(m.CallbackURLs) != 1 || m.CallbackURLs[0] != m.RedirectURL {
		t.Errorf("callback urls = %v, want [%q]", m.CallbackURLs, m.RedirectURL)
	}
	if m.Public {
		t.Error("manifest should request a private App")
	}
	if m.DefaultPermissions["contents"] != "read" || m.DefaultPermissions["metadata"] != "read" {
		t.Errorf("permissions = %v, want contents+metadata read", m.DefaultPermissions)
	}
	if len(m.DefaultEvents) != 1 || m.DefaultEvents[0] != "push" {
		t.Errorf("events = %v, want [push]", m.DefaultEvents)
	}
}

func TestGitHubManifestStartOrgAndName(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{}, true)
	body, _ := startManifest(t, s, store, "?org=acme-co&name=My+Orkano")

	if !strings.HasPrefix(body["postUrl"], "https://github.com/organizations/acme-co/settings/apps/new?state=") {
		t.Fatalf("postUrl = %q, want org form for acme-co", body["postUrl"])
	}
	var m manifest
	if err := json.Unmarshal([]byte(body["manifest"]), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Name != "My Orkano" {
		t.Errorf("manifest name = %q, want %q", m.Name, "My Orkano")
	}
}

func TestGitHubManifestStartRejectsBadInput(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{}, true)
	ck := authedSession(t, store)
	for _, tc := range []struct {
		name, query, wantErr string
	}{
		{"bad org", "?org=-bad-", "invalid_org"},
		{"org path injection", "?org=a/b", "invalid_org"},
		{"bad name", "?name=" + url.QueryEscape("woah\nnewline"), "invalid_name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getWithCookies(t, s, "/api/github/app/manifest"+tc.query, ck)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400", tc.name, rec.Code)
			}
			if got := decodeBody(t, rec)["error"]; got != tc.wantErr {
				t.Fatalf("error = %v, want %v", got, tc.wantErr)
			}
		})
	}
}

func TestGitHubCallbackSuccess(t *testing.T) {
	store := newFakeStore()
	ex := &fakeExchanger{creds: testCreds()}
	s := githubServer(t, store, ex, true)

	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])

	rec := getWithCookies(t, s, "/api/github/app/callback?code=the-code&state="+url.QueryEscape(state), flow)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?github=connected" {
		t.Fatalf("redirect = %q, want /?github=connected", loc)
	}
	if ex.gotCode != "the-code" {
		t.Fatalf("exchanged code = %q, want the-code", ex.gotCode)
	}
	// The flow cookie is single-use — cleared on the callback.
	if c := cookieNamed(rec, githubCookie); c == nil || c.MaxAge != -1 {
		t.Fatalf("callback did not clear the flow cookie: %+v", c)
	}

	// Both credentials landed value-blind in orkano-system.
	appData := getSecretData(t, s, githubAppSecretName)
	if string(appData[githubAppIDKey]) != "424242" {
		t.Errorf("app-id = %q, want 424242", appData[githubAppIDKey])
	}
	if string(appData[githubAppPrivateKeyKey]) != testCreds().PEM {
		t.Errorf("private key not written verbatim")
	}
	hookData := getSecretData(t, s, webhookSecretName)
	if string(hookData[webhookSecretKey]) != testCreds().WebhookSecret {
		t.Errorf("webhook secret = %q, want %q", hookData[webhookSecretKey], testCreds().WebhookSecret)
	}

	assertAudited(t, store, "github.app_connect", "success")
}

// TestGitHubCallbackNoFlow rejects a callback with no flow cookie (a stray or
// forged redirect) — no exchange, no write, audited as a failure.
func TestGitHubCallbackNoFlow(t *testing.T) {
	store := newFakeStore()
	ex := &fakeExchanger{creds: testCreds()}
	s := githubServer(t, store, ex, true)
	rec := getWithCookies(t, s, "/api/github/app/callback?code=x&state=y") // no cookie
	assertGitHubError(t, rec, "no_flow")
	if ex.gotCode != "" {
		t.Fatal("a flowless callback must never reach the exchanger")
	}
	assertAudited(t, store, "github.app_connect", "failure")
}

// TestGitHubCallbackStateMismatch / NoCode / Exchange / Write exercise the paths
// that need a valid flow cookie (so they share the start-driven setup).
func TestGitHubCallbackStateMismatch(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{creds: testCreds()}, true)
	_, flow := startManifest(t, s, store, "")
	rec := getWithCookies(t, s, "/api/github/app/callback?code=x&state=wrong-state", flow)
	assertGitHubError(t, rec, "state_mismatch")
}

// TestGitHubCallbackGitHubError handles a GitHub-reported error on the callback
// (e.g. the admin cancelled): no exchange, audited failure, generic redirect.
func TestGitHubCallbackGitHubError(t *testing.T) {
	store := newFakeStore()
	ex := &fakeExchanger{creds: testCreds()}
	s := githubServer(t, store, ex, true)
	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])
	rec := getWithCookies(t, s, "/api/github/app/callback?error=access_denied&state="+url.QueryEscape(state), flow)
	assertGitHubError(t, rec, "exchange_failed")
	if ex.gotCode != "" {
		t.Fatal("a GitHub-error callback must never reach the exchanger")
	}
	assertAudited(t, store, "github.app_connect", "failure")
}

func TestGitHubCallbackNoCode(t *testing.T) {
	store := newFakeStore()
	s := githubServer(t, store, &fakeExchanger{creds: testCreds()}, true)
	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])
	rec := getWithCookies(t, s, "/api/github/app/callback?state="+url.QueryEscape(state), flow)
	assertGitHubError(t, rec, "no_code")
}

func TestGitHubCallbackExchangeError(t *testing.T) {
	store := newFakeStore()
	ex := &fakeExchanger{err: errors.New("github said no")}
	s := githubServer(t, store, ex, true)
	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])
	rec := getWithCookies(t, s, "/api/github/app/callback?code=c&state="+url.QueryEscape(state), flow)
	assertGitHubError(t, rec, "exchange_failed")
	assertAudited(t, store, "github.app_connect", "failure")
}

// TestGitHubCallbackWriteError proves a missing placeholder Secret (a broken
// install — the dashboard has update-only, no create) surfaces as a write error
// rather than a silent success.
func TestGitHubCallbackWriteError(t *testing.T) {
	store := newFakeStore()
	ex := &fakeExchanger{creds: testCreds()}
	s := githubServer(t, store, ex, false) // no placeholder Secrets seeded
	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])
	rec := getWithCookies(t, s, "/api/github/app/callback?code=c&state="+url.QueryEscape(state), flow)
	assertGitHubError(t, rec, "write_failed")
	assertAudited(t, store, "github.app_connect", "failure")
}

// TestGitHubCredentialWriteNeverReadsSecret proves the write is value-blind: the
// handler never GETs (or lists) the credential Secrets, only UPDATEs them.
func TestGitHubCredentialWriteNeverReadsSecret(t *testing.T) {
	store := newFakeStore()
	failRead := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				t.Errorf("the credential write must never GET a Secret (value-blind), got Get(%s)", key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				t.Error("the credential write must never LIST Secrets (value-blind)")
			}
			return c.List(ctx, list, opts...)
		},
	}
	k8s := fakeclient.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(placeholderSecret(githubAppSecretName), placeholderSecret(webhookSecretName)).
		WithInterceptorFuncs(failRead).
		Build()
	s, err := New(Config{
		K8s: k8s, ViewerClient: k8s, PodLogs: &fakePodStreamer{}, DB: fakePinger{},
		Store: store, Cipher: testCipherInstance, BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA: testSPA(), Now: fixedNow, GitHub: &fakeExchanger{creds: testCreds()}, WebhookURL: testWebhookURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, flow := startManifest(t, s, store, "")
	state := stateFromPostURL(t, body["postUrl"])
	rec := getWithCookies(t, s, "/api/github/app/callback?code=c&state="+url.QueryEscape(state), flow)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/?github=connected" {
		t.Fatalf("callback = %d %q, want a connected redirect", rec.Code, rec.Header().Get("Location"))
	}
}

// --- the production exchanger against an httptest GitHub ---

func TestGitHubExchangerSuccess(t *testing.T) {
	var gotPath, gotMethod, gotAPIVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAPIVersion = r.URL.EscapedPath(), r.Method, r.Header.Get("X-GitHub-Api-Version")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"slug":"s","pem":"PEMDATA","webhook_secret":"whsec","client_id":"cid"}`))
	}))
	defer srv.Close()

	ex := NewGitHubExchanger(srv.URL)
	creds, err := ex.Exchange(context.Background(), "the code/with-slash")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/app-manifests/the%20code%2Fwith-slash/conversions" {
		t.Errorf("path = %q, want the code path-escaped", gotPath)
	}
	if gotAPIVersion != githubAPIVersion {
		t.Errorf("api version header = %q, want %q", gotAPIVersion, githubAPIVersion)
	}
	if creds.ID != 99 || creds.PEM != "PEMDATA" || creds.WebhookSecret != "whsec" || creds.Slug != "s" {
		t.Errorf("creds = %+v, unexpected", creds)
	}
}

func TestGitHubExchangerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-201", http.StatusUnprocessableEntity, `{"message":"code expired"}`},
		{"missing pem", http.StatusCreated, `{"id":1,"webhook_secret":"w"}`},
		{"missing webhook secret", http.StatusCreated, `{"id":1,"pem":"p"}`},
		{"bad id", http.StatusCreated, `{"id":0,"pem":"p","webhook_secret":"w"}`},
		{"garbage", http.StatusCreated, `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := NewGitHubExchanger(srv.URL).Exchange(context.Background(), "c"); err == nil {
				t.Fatalf("%s: expected an error", tc.name)
			}
		})
	}
}

func TestNewGitHubExchangerDefaultsBase(t *testing.T) {
	// A nil Config.GitHub must be defaulted by New (not left nil), so the manifest
	// routes never nil-panic.
	s := newTestServer(t, fakePinger{})
	if s.cfg.GitHub == nil {
		t.Fatal("New left Config.GitHub nil; expected a default exchanger")
	}
}

// --- helpers ---

func assertGitHubError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302 (body %s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/?github_error="+code {
		t.Fatalf("redirect = %q, want /?github_error=%s", loc, code)
	}
}

func getSecretData(t *testing.T, s *Server, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: systemNamespace, Name: name}, &sec); err != nil {
		t.Fatalf("get secret %s: %v", name, err)
	}
	return sec.Data
}
