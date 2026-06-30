package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/dashboard/internal/server"
	"github.com/orkanoio/orkano/internal/db"
)

// migratedPostgres starts a throwaway Postgres, applies the migrations, and
// returns its superuser DSN. Skips cleanly when no container runtime is reachable.
func migratedPostgres(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	pg, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("orkano"),
		postgres.WithUsername("orkano"),
		postgres.WithPassword("orkano-test"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, pg)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dsn
}

// newServer builds the real chi server over a pool + a (fake) K8s client. The
// viewer client is the same fake; impersonation has no effect against a fake
// client, and this suite proves the DB/auth/HTTP paths, not RBAC (that is
// rbac_matrix_test's job).
func newServer(t *testing.T, pool *pgxpool.Pool, k8s client.Client) *server.Server {
	t.Helper()
	srv, err := server.New(server.Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            server.NewPodLogStreamer(k8sfake.NewSimpleClientset()),
		DB:                 pool,
		Store:              server.NewStore(pool),
		Cipher:             mustIntegrationCipher(t),
		BootstrapTokenHash: auth.HashToken(installToken),
		SPA:                integrationSPA(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

// connectAs opens a pool authenticated as a specific role by swapping the
// credentials in the superuser DSN (pgxpool connects lazily, so a permission
// failure surfaces on the first query).
func connectAs(t *testing.T, dsn, user, password string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// integrationScheme carries the orkano + core types so the fake K8s client can
// serve the App/catalog handlers.
func integrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := orkanov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("orkano scheme: %v", err)
	}
	return scheme
}

// integrationSPA is a minimal embedded-app stand-in (the server requires a SPA
// with an index.html).
func integrationSPA() fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>orkano</title>")}}
}

// Mirrors internal/db/setup_test.go: a multi-arch index digest so the image
// resolves on CI amd64 and local arm64.
const postgresImage = "postgres:17-alpine@sha256:979c4379dd698aba0b890599a6104e082035f98ef31d9b9291ec22f2b13059ca"

const installToken = "bootstrap-install-token-integration"

// TestBootstrapAuthFullFlow drives the entire bootstrap-auth handshake over the
// real chi server backed by the real generated queries and a Postgres container:
// redeem -> confirm TOTP -> (fresh client) login -> login/totp -> a
// RequireSession-protected probe -> stepup -> logout. It proves the transactional
// redeem and every query work end to end, and that the audit log accrues rows.
// Skipped cleanly when no container runtime is reachable.
func TestBootstrapAuthFullFlow(t *testing.T) {
	ctx := context.Background()
	dsn := migratedPostgres(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	srv := newServer(t, pool, fake.NewClientBuilder().Build())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// A cookie jar models a single browser through the enrollment leg.
	enrollClient := mustClient(t)

	// --- redeem ---
	var redeemResp struct {
		OtpauthURL    string   `json:"otpauthUrl"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	postJSON(t, enrollClient, ts.URL+"/api/auth/redeem", map[string]string{
		"token":    installToken,
		"username": "admin",
		"password": "correct-horse-battery",
	}, http.StatusOK, &redeemResp)
	secret := secretFromOtpauth(t, redeemResp.OtpauthURL)
	if len(redeemResp.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(redeemResp.RecoveryCodes))
	}

	// --- confirm TOTP ---
	code := mustCode(t, secret)
	postJSON(t, enrollClient, ts.URL+"/api/auth/totp/confirm", map[string]string{"code": code}, http.StatusOK, nil)

	// --- login on a FRESH client (no enrollment cookies) ---
	loginClient := mustClient(t)
	var loginResp struct {
		State string `json:"state"`
	}
	postJSON(t, loginClient, ts.URL+"/api/auth/login", map[string]string{
		"username": "admin", "password": "correct-horse-battery",
	}, http.StatusOK, &loginResp)
	if loginResp.State != "totp_required" {
		t.Fatalf("login state = %q, want totp_required", loginResp.State)
	}

	postJSON(t, loginClient, ts.URL+"/api/auth/login/totp", map[string]string{"code": mustCode(t, secret)}, http.StatusOK, nil)

	// --- access a RequireSession probe (status reports authenticated) ---
	var statusResp struct {
		State    string `json:"state"`
		Username string `json:"username"`
	}
	getJSON(t, loginClient, ts.URL+"/api/auth/status", http.StatusOK, &statusResp)
	if statusResp.State != "authenticated" || statusResp.Username != "admin" {
		t.Fatalf("status = %+v, want authenticated/admin", statusResp)
	}

	// --- stepup ---
	postJSON(t, loginClient, ts.URL+"/api/auth/stepup", map[string]string{"code": mustCode(t, secret)}, http.StatusNoContent, nil)

	// --- logout ---
	postJSON(t, loginClient, ts.URL+"/api/auth/logout", nil, http.StatusNoContent, nil)
	// After logout the session is gone: status falls back to needs_login.
	getJSON(t, loginClient, ts.URL+"/api/auth/status", http.StatusOK, &statusResp)
	if statusResp.State != "needs_login" {
		t.Fatalf("post-logout status = %q, want needs_login", statusResp.State)
	}

	// --- audit accrued rows, none leaking the password ---
	var auditCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount == 0 {
		t.Fatal("expected audit_log rows after the full flow")
	}
	var leak int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE detail::text LIKE '%correct-horse-battery%'").Scan(&leak); err != nil {
		t.Fatalf("scan leak query: %v", err)
	}
	if leak != 0 {
		t.Fatal("audit_log detail leaked the password")
	}
}

// maxFailedLoginsForTest mirrors server.maxFailedLogins (unexported); the
// lockout sub-test fires this many wrong-password logins then expects a 423.
const maxFailedLoginsForTest = 5

// TestBootstrapAuthRecoveryAndLockout drives the real queries for two security
// properties the fake cannot fully prove: (a) a recovery-code login succeeds once
// and a replay of the same code fails (single-use via the real ConsumeRecoveryCode
// UPDATE), and (b) account lockout — maxFailedLogins wrong-password logins against
// the confirmed admin and the next returns 423. Skipped without a container runtime.
func TestBootstrapAuthRecoveryAndLockout(t *testing.T) {
	ctx := context.Background()
	dsn := migratedPostgres(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	srv := newServer(t, pool, fake.NewClientBuilder().Build())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// --- enrollment ---
	enrollClient := mustClient(t)
	var redeemResp struct {
		OtpauthURL    string   `json:"otpauthUrl"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	postJSON(t, enrollClient, ts.URL+"/api/auth/redeem", map[string]string{
		"token": installToken, "username": "admin", "password": "correct-horse-battery",
	}, http.StatusOK, &redeemResp)
	secret := secretFromOtpauth(t, redeemResp.OtpauthURL)
	if len(redeemResp.RecoveryCodes) == 0 {
		t.Fatal("redeem returned no recovery codes")
	}
	recoveryCode := redeemResp.RecoveryCodes[0]
	postJSON(t, enrollClient, ts.URL+"/api/auth/totp/confirm",
		map[string]string{"code": mustCode(t, secret)}, http.StatusOK, nil)

	// --- (a) recovery-code login is single-use ---
	rc1 := mustClient(t)
	postJSON(t, rc1, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "correct-horse-battery"}, http.StatusOK, nil)
	postJSON(t, rc1, ts.URL+"/api/auth/login/totp",
		map[string]string{"recoveryCode": recoveryCode}, http.StatusOK, nil)

	rc2 := mustClient(t)
	postJSON(t, rc2, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "correct-horse-battery"}, http.StatusOK, nil)
	// Replaying the spent code fails (single-use through the real UPDATE).
	postJSON(t, rc2, ts.URL+"/api/auth/login/totp",
		map[string]string{"recoveryCode": recoveryCode}, http.StatusUnauthorized, nil)

	// --- (b) lockout after maxFailedLogins wrong passwords ---
	// Section (a)'s spent-recovery-code replay recorded one second-factor failure
	// on the shared admin; clear it so the threshold is measured from zero.
	if _, err := pool.Exec(ctx, "UPDATE users SET failed_logins = 0, locked_until = NULL"); err != nil {
		t.Fatalf("reset failure counter: %v", err)
	}
	lockClient := mustClient(t)
	for i := 0; i < maxFailedLoginsForTest; i++ {
		postJSON(t, lockClient, ts.URL+"/api/auth/login",
			map[string]string{"username": "admin", "password": "wrong-and-long-x"}, http.StatusUnauthorized, nil)
	}
	// The next attempt — even with the correct password — is locked out (423).
	postJSON(t, lockClient, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "correct-horse-battery"}, http.StatusLocked, nil)
}

// TestAppCatalogAPIUnderDashboardRole drives a representative M2.4 flow — create
// an App, write a secret env var, read the deploy timeline and the audit log —
// over the real chi server whose Store runs as the least-privilege
// orkano_dashboard role. It proves that role's INSERT+SELECT grants on
// deploy_history and audit_log are sufficient end to end (INV-08), against a fake
// K8s client. Skipped without a container runtime.
func TestAppCatalogAPIUnderDashboardRole(t *testing.T) {
	ctx := context.Background()
	dsn := migratedPostgres(t)
	const dashPw = "dash-integration-pw-1"
	if err := db.SetupRoles(ctx, dsn, db.RolePasswords{
		Receiver: "recv-pw-x", Dispatcher: "disp-pw-x", Dashboard: dashPw,
	}); err != nil {
		t.Fatalf("setup roles: %v", err)
	}
	rolePool := connectAs(t, dsn, db.DashboardRole, dashPw)

	k8s := fake.NewClientBuilder().
		WithScheme(integrationScheme(t)).
		WithStatusSubresource(&orkanov1alpha1.App{}).
		Build()
	srv := newServer(t, rolePool, k8s)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Bootstrap an admin and obtain an authenticated session (all under the role).
	c := mustClient(t)
	var redeemResp struct {
		OtpauthURL string `json:"otpauthUrl"`
	}
	postJSON(t, c, ts.URL+"/api/auth/redeem", map[string]string{
		"token": installToken, "username": "admin", "password": "correct-horse-battery",
	}, http.StatusOK, &redeemResp)
	secret := secretFromOtpauth(t, redeemResp.OtpauthURL)
	postJSON(t, c, ts.URL+"/api/auth/totp/confirm", map[string]string{"code": mustCode(t, secret)}, http.StatusOK, nil)

	// 1. Create an App (session tier) — records a deploy + audits, both under the role.
	appSpec := map[string]any{
		"source": map[string]any{"github": map[string]any{"repo": "orkanoio/demo"}},
		"build":  map[string]any{"strategy": "Dockerfile"},
	}
	postJSON(t, c, ts.URL+"/api/apps", map[string]any{"name": "demo", "spec": appSpec}, http.StatusCreated, nil)

	// The App really landed in the (fake) cluster, readable via the viewer client.
	var appResp struct {
		Name string `json:"name"`
	}
	getJSON(t, c, ts.URL+"/api/apps/demo", http.StatusOK, &appResp)
	if appResp.Name != "demo" {
		t.Fatalf("get app = %+v", appResp)
	}

	// 2. Step up, then write a secret env var (step-up tier).
	postJSON(t, c, ts.URL+"/api/auth/stepup", map[string]string{"code": mustCode(t, secret)}, http.StatusNoContent, nil)
	putJSON(t, c, ts.URL+"/api/apps/demo/env",
		map[string]any{"secrets": map[string]string{"API_KEY": "a-secret-value"}}, http.StatusOK, nil)

	// 3. The deploy timeline shows the create — proves RecordDeploy INSERT +
	// ListAppDeploys SELECT under the role. The proof is content-based by
	// necessity: recordDeploy is best-effort (a missing INSERT grant is a logged
	// 42501, not an HTTP error), so the row's presence here IS the evidence the
	// role can both write and read it.
	var deploys struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	getJSON(t, c, ts.URL+"/api/apps/demo/deploys", http.StatusOK, &deploys)
	if len(deploys.Items) == 0 || deploys.Items[0].Status != "created" {
		t.Fatalf("deploys = %+v, want a 'created' row", deploys.Items)
	}

	// 4. The audit log shows app.create + env.update — proves AppendAuditEntry
	// INSERT + ListAuditEntries SELECT under the role (INV-08), and that the secret
	// value never reached the audit detail (INV-03).
	var audit struct {
		Items []struct {
			Action string          `json:"action"`
			Detail json.RawMessage `json:"detail"`
		} `json:"items"`
	}
	getJSON(t, c, ts.URL+"/api/audit", http.StatusOK, &audit)
	actions := map[string]bool{}
	for _, e := range audit.Items {
		actions[e.Action] = true
		if strings.Contains(string(e.Detail), "a-secret-value") {
			t.Fatal("audit detail leaked the secret value (INV-03)")
		}
	}
	for _, want := range []string{"app.create", "env.update"} {
		if !actions[want] {
			t.Fatalf("audit log missing action %q (have %v)", want, actions)
		}
	}
}

func mustIntegrationCipher(t *testing.T) *auth.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	c, err := auth.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func mustClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

func mustCode(t *testing.T, secret string) string {
	t.Helper()
	// The server runs on the real clock (default Now), so generate against now.
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("gen code: %v", err)
	}
	return code
}

func postJSON(t *testing.T, c *http.Client, url string, body any, wantStatus int, out any) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	doReq(t, c, req, wantStatus, out)
}

func putJSON(t *testing.T, c *http.Client, url string, body any, wantStatus int, out any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, url, strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	doReq(t, c, req, wantStatus, out)
}

func getJSON(t *testing.T, c *http.Client, url string, wantStatus int, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	doReq(t, c, req, wantStatus, out)
}

func doReq(t *testing.T, c *http.Client, req *http.Request, wantStatus int, out any) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s = %d, want %d", req.Method, req.URL.Path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s response: %v", req.URL.Path, err)
		}
	}
}

// secretFromOtpauth extracts the base32 seed from an otpauth:// URL.
func secretFromOtpauth(t *testing.T, otpauthURL string) string {
	t.Helper()
	const marker = "secret="
	i := strings.Index(otpauthURL, marker)
	if i < 0 {
		t.Fatalf("otpauth url missing secret: %q", otpauthURL)
	}
	rest := otpauthURL[i+len(marker):]
	if amp := strings.IndexByte(rest, '&'); amp >= 0 {
		rest = rest[:amp]
	}
	return rest
}
