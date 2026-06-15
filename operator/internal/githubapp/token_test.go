package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testAppID          = 424242
	testInstallationID = 99
)

// fixedTime is a deterministic clock so the stub's expiry and the cache math
// are reproducible.
var fixedTime = time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

// githubStub records every access-token request, verifies the App JWT against
// the test key, and answers with a fresh token whose expiry is one hour past
// the request's observed clock.
type githubStub struct {
	t   *testing.T
	pub *rsa.PublicKey
	now func() time.Time

	mu       sync.Mutex
	requests int
	paths    []string
	lastJWT  jwtClaims

	status int // overrides 201 when non-zero
}

func (g *githubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests++
	g.paths = append(g.paths, r.URL.Path)
	g.mu.Unlock()

	wantPath := fmt.Sprintf("/app/installations/%d/access_tokens", testInstallationID)
	if r.Method != http.MethodPost {
		g.t.Errorf("method = %s, want POST", r.Method)
	}
	if r.URL.Path != wantPath && !strings.HasPrefix(r.URL.Path, "/app/installations/") {
		g.t.Errorf("path = %s, want %s", r.URL.Path, wantPath)
	}
	if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
		g.t.Errorf("Accept = %q", got)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != githubAPIVersion {
		g.t.Errorf("X-GitHub-Api-Version = %q", got)
	}

	// These run in the server goroutine, so they must use Errorf, never Fatalf
	// (Fatal/FailNow off the test goroutine is forbidden); a 500 lets the
	// caller's InstallationToken return a clean error the test goroutine sees.
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		g.t.Errorf("Authorization = %q, want a Bearer token", authz)
		http.Error(w, "missing bearer token", http.StatusInternalServerError)
		return
	}
	claims, ok := g.verifyJWT(strings.TrimPrefix(authz, "Bearer "))
	if !ok {
		http.Error(w, "invalid app jwt", http.StatusInternalServerError)
		return
	}
	g.mu.Lock()
	g.lastJWT = claims
	g.mu.Unlock()

	if g.status != 0 {
		http.Error(w, `{"message":"Bad credentials"}`, g.status)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      fmt.Sprintf("ghs_test_%d", g.count()),
		"expires_at": g.now().Add(time.Hour).Format(time.RFC3339),
	})
}

func (g *githubStub) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

func (g *githubStub) recordedPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.paths...)
}

// verifyJWT proves buildAppJWT produced a signature this key can verify and
// returns the decoded claims. It runs in the server goroutine, so failures use
// Errorf and signal via ok=false — never Fatalf.
func (g *githubStub) verifyJWT(token string) (jwtClaims, bool) {
	g.t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		g.t.Errorf("JWT has %d segments, want 3", len(parts))
		return jwtClaims{}, false
	}

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		g.t.Errorf("decoding JWT header: %v", err)
		return jwtClaims{}, false
	}
	if !strings.Contains(string(header), `"RS256"`) {
		g.t.Errorf("JWT header = %s, want alg RS256", header)
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		g.t.Errorf("decoding JWT signature: %v", err)
		return jwtClaims{}, false
	}
	if err := rsa.VerifyPKCS1v15(g.pub, crypto.SHA256, digest[:], sig); err != nil {
		g.t.Errorf("JWT signature does not verify against the App key: %v", err)
		return jwtClaims{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		g.t.Errorf("decoding JWT claims: %v", err)
		return jwtClaims{}, false
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		g.t.Errorf("unmarshaling JWT claims: %v", err)
		return jwtClaims{}, false
	}
	return claims, true
}

// newSource wires a TokenSource to a stub server and a fake Secret. A nil
// secret means no object exists (the not-found path).
func newSource(t *testing.T, key *rsa.PrivateKey, secret *corev1.Secret, status int) (*TokenSource, *githubStub) {
	t.Helper()
	stub := &githubStub{t: t, pub: &key.PublicKey, now: func() time.Time { return fixedTime }, status: status}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if secret != nil {
		builder = builder.WithObjects(secret)
	}

	return &TokenSource{
		Reader:  builder.Build(),
		BaseURL: srv.URL,
		Now:     func() time.Time { return fixedTime },
	}, stub
}

func pkcs1Secret(t *testing.T, key *rsa.PrivateKey, appID string) *corev1.Secret {
	t.Helper()
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return secretWith(map[string][]byte{AppIDKey: []byte(appID), PrivateKeyKey: keyPEM})
}

func secretWith(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SecretNamespace, Name: DefaultSecretName},
		Data:       data,
	}
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return key
}

func TestInstallationTokenMintsCachesAndRefreshes(t *testing.T) {
	key := mustKey(t)
	src, stub := newSource(t, key, pkcs1Secret(t, key, strconv.Itoa(testAppID)), 0)
	ctx := context.Background()

	first, err := src.InstallationToken(ctx, testInstallationID)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if first != "ghs_test_1" {
		t.Errorf("first token = %q, want ghs_test_1", first)
	}

	// The App JWT must name this App and stay inside GitHub's 10-minute ceiling.
	if stub.lastJWT.Iss != strconv.Itoa(testAppID) {
		t.Errorf("JWT iss = %q, want %d", stub.lastJWT.Iss, testAppID)
	}
	if span := stub.lastJWT.Exp - stub.lastJWT.Iat; span <= 0 || span > 600 {
		t.Errorf("JWT exp-iat = %ds, want (0, 600]", span)
	}
	if stub.lastJWT.Iat > fixedTime.Unix() {
		t.Errorf("JWT iat = %d is not backdated relative to now %d", stub.lastJWT.Iat, fixedTime.Unix())
	}

	// Second call inside the lifetime is served from cache — no new request.
	second, err := src.InstallationToken(ctx, testInstallationID)
	if err != nil {
		t.Fatalf("cached mint: %v", err)
	}
	if second != first {
		t.Errorf("cached token = %q, want %q", second, first)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("request count = %d after a cached read, want 1", n)
	}

	// Advance past (expiry - guard) and the next call re-mints.
	src.Now = func() time.Time { return fixedTime.Add(time.Hour - time.Minute) }
	stub.now = src.Now
	third, err := src.InstallationToken(ctx, testInstallationID)
	if err != nil {
		t.Fatalf("refresh mint: %v", err)
	}
	if third == first {
		t.Errorf("token after refresh = %q, want a fresh value", third)
	}
	if n := stub.count(); n != 2 {
		t.Fatalf("request count = %d after refresh, want 2", n)
	}
}

func TestInstallationTokenPerInstallationCache(t *testing.T) {
	key := mustKey(t)
	src, stub := newSource(t, key, pkcs1Secret(t, key, strconv.Itoa(testAppID)), 0)
	ctx := context.Background()

	if _, err := src.InstallationToken(ctx, 100); err != nil {
		t.Fatalf("installation 100: %v", err)
	}
	if _, err := src.InstallationToken(ctx, 200); err != nil {
		t.Fatalf("installation 200: %v", err)
	}
	// Re-read 100 from cache; only the two distinct installations cost a mint.
	if _, err := src.InstallationToken(ctx, 100); err != nil {
		t.Fatalf("installation 100 again: %v", err)
	}
	if n := stub.count(); n != 2 {
		t.Fatalf("request count = %d, want 2 (one per installation)", n)
	}
	// Each installation must have hit its own endpoint — not the same one twice.
	got := stub.recordedPaths()
	sort.Strings(got)
	if want := "/app/installations/100/access_tokens,/app/installations/200/access_tokens"; strings.Join(got, ",") != want {
		t.Errorf("request paths = %v, want one mint to each installation endpoint", got)
	}
}

// A 201 carrying an empty token or no expiry must error and must not be cached:
// the defensive guards in requestInstallationToken exist for a GitHub schema
// change, so they get regression coverage.
func TestInstallationTokenMalformed201NotCached(t *testing.T) {
	key := mustKey(t)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}

	for _, tc := range []struct{ name, body string }{
		{name: "empty token", body: `{"token":"","expires_at":"2026-06-16T13:00:00Z"}`},
		{name: "missing expiry", body: `{"token":"ghs_x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			src := &TokenSource{
				Reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(pkcs1Secret(t, key, "1")).Build(),
				BaseURL: srv.URL,
				Now:     func() time.Time { return fixedTime },
			}
			if _, err := src.InstallationToken(context.Background(), testInstallationID); err == nil {
				t.Fatal("expected an error for a malformed 201 body")
			}
			if _, err := src.InstallationToken(context.Background(), testInstallationID); err == nil {
				t.Fatal("expected a second error (malformed responses are not cached)")
			}
			if n := requests.Load(); n != 2 {
				t.Fatalf("request count = %d, want 2 (no caching of malformed responses)", n)
			}
		})
	}
}

func TestInstallationTokenGitHubErrorNotCached(t *testing.T) {
	key := mustKey(t)
	src, stub := newSource(t, key, pkcs1Secret(t, key, strconv.Itoa(testAppID)), http.StatusUnauthorized)
	ctx := context.Background()

	if _, err := src.InstallationToken(ctx, testInstallationID); err == nil {
		t.Fatal("expected an error when GitHub answers 401")
	}
	// A failed mint must not poison the cache: the next call tries again.
	if _, err := src.InstallationToken(ctx, testInstallationID); err == nil {
		t.Fatal("expected a second error")
	}
	if n := stub.count(); n != 2 {
		t.Fatalf("request count = %d, want 2 (no caching of failures)", n)
	}
}

func TestInstallationTokenInvalidID(t *testing.T) {
	key := mustKey(t)
	src, stub := newSource(t, key, pkcs1Secret(t, key, strconv.Itoa(testAppID)), 0)

	if _, err := src.InstallationToken(context.Background(), 0); err == nil {
		t.Fatal("expected an error for a non-positive installation id")
	}
	if n := stub.count(); n != 0 {
		t.Fatalf("request count = %d, want 0 (rejected before any HTTP)", n)
	}
}

func TestInstallationTokenCredentialErrors(t *testing.T) {
	key := mustKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling PKCS#8: %v", err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	validKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	for _, tc := range []struct {
		name    string
		secret  *corev1.Secret
		wantErr bool
	}{
		{name: "missing secret", secret: nil, wantErr: true},
		{name: "missing app-id", secret: secretWith(map[string][]byte{PrivateKeyKey: validKeyPEM}), wantErr: true},
		{name: "empty app-id", secret: secretWith(map[string][]byte{AppIDKey: {}, PrivateKeyKey: validKeyPEM}), wantErr: true},
		{name: "non-numeric app-id", secret: secretWith(map[string][]byte{AppIDKey: []byte("not-a-number"), PrivateKeyKey: validKeyPEM}), wantErr: true},
		{name: "missing private key", secret: secretWith(map[string][]byte{AppIDKey: []byte("1")}), wantErr: true},
		{name: "garbage private key", secret: secretWith(map[string][]byte{AppIDKey: []byte("1"), PrivateKeyKey: []byte("not pem")}), wantErr: true},
		{name: "pkcs1 ok", secret: secretWith(map[string][]byte{AppIDKey: []byte("1"), PrivateKeyKey: validKeyPEM}), wantErr: false},
		{name: "pkcs8 ok", secret: secretWith(map[string][]byte{AppIDKey: []byte("1"), PrivateKeyKey: pkcs8PEM}), wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := newSource(t, key, tc.secret, 0)
			_, err := src.InstallationToken(context.Background(), testInstallationID)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Guard the not-found path explicitly: a missing Secret surfaces as a wrapped
// NotFound, never a panic.
func TestInstallationTokenSecretNotFoundIsWrapped(t *testing.T) {
	key := mustKey(t)
	src, _ := newSource(t, key, nil, 0)
	_, err := src.InstallationToken(context.Background(), testInstallationID)
	if !errors.IsNotFound(err) {
		t.Fatalf("error = %v, want a wrapped NotFound", err)
	}
}

func TestSecretRefDefault(t *testing.T) {
	var s TokenSource
	if got := s.secretRef().String(); got != SecretNamespace+"/"+DefaultSecretName {
		t.Errorf("default secretRef = %q", got)
	}
	custom := TokenSource{SecretRef: types.NamespacedName{Namespace: "other", Name: "creds"}}
	if got := custom.secretRef().String(); got != "other/creds" {
		t.Errorf("custom secretRef = %q", got)
	}
}
