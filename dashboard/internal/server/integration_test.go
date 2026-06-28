package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/dashboard/internal/server"
	"github.com/orkanoio/orkano/internal/db"
)

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

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	cipher := mustIntegrationCipher(t)
	srv, err := server.New(server.Config{
		K8s:                fake.NewClientBuilder().Build(),
		DB:                 pool,
		Store:              server.NewStore(pool),
		Cipher:             cipher,
		BootstrapTokenHash: auth.HashToken(installToken),
		SPA:                integrationSPA(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	srv, err := server.New(server.Config{
		K8s:                fake.NewClientBuilder().Build(),
		DB:                 pool,
		Store:              server.NewStore(pool),
		Cipher:             mustIntegrationCipher(t),
		BootstrapTokenHash: auth.HashToken(installToken),
		SPA:                integrationSPA(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
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
