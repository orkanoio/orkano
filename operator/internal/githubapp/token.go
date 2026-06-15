// Package githubapp mints short-lived GitHub App installation access tokens
// for the operator's dispatcher (INV-07). The App private key lives only as a
// Kubernetes Secret in orkano-system, readable by the operator alone; this
// package reads it per mint (so key rotation just works, like the registry
// resolver's per-call CA read), signs a ~minutes-long App JWT, exchanges it
// for an installation token GitHub caps at one hour, and caches that token in
// memory until just before it expires. Nothing is ever written to disk or the
// database.
//
// The JWT and the token exchange are hand-rolled against GitHub's documented
// API rather than pulling in a GitHub client library: the artifact is a fixed
// three-claim RS256 payload and a single POST, so stdlib crypto keeps the
// dependency surface (and the audit surface) minimal.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SecretNamespace and DefaultSecretName identify the Kubernetes Secret that
	// carries the GitHub App credentials. The operator's RBAC pins its secrets
	// `get` to this exact name via resourceNames, so it can read no other
	// Secret in its own namespace (INV-07). M1.5's component deploy creates it.
	SecretNamespace   = "orkano-system"
	DefaultSecretName = "orkano-github-app" //nolint:gosec // G101: a Secret object name, not a credential value.

	// AppIDKey and PrivateKeyKey are the data keys inside that Secret: the
	// non-secret App id and the PEM-encoded RSA private key.
	AppIDKey      = "app-id"
	PrivateKeyKey = "private-key.pem"

	defaultBaseURL   = "https://api.github.com"
	githubAPIVersion = "2022-11-28"

	// jwtBackdate softens clock skew against GitHub (their own recommendation).
	// GitHub validates exp-iat, so the effective window is jwtTTL+jwtBackdate =
	// 9 min — inside the 10-minute ceiling, but do NOT raise jwtTTL past 8 min
	// without lowering jwtBackdate, or exp-iat reaches the 600 s GitHub rejects.
	jwtBackdate = 60 * time.Second
	jwtTTL      = 8 * time.Minute

	// expiryGuard re-mints a cached installation token this far before it
	// actually expires, so a token never dies mid-request during a chain of
	// dispatcher API calls.
	expiryGuard = 2 * time.Minute

	httpTimeout      = 15 * time.Second
	maxResponseBytes = 1 << 20 // 1 MiB cap on the token response body.
)

// TokenSource mints and caches GitHub App installation access tokens. The zero
// value is usable once Reader is set: SecretRef, BaseURL, HTTPClient, and Now
// fall back to production defaults. Safe for concurrent use.
type TokenSource struct {
	// Reader fetches the credential Secret. Use the manager's uncached
	// APIReader: the Secret is read only on a cache miss (~once per hour per
	// installation), so an informer would buy nothing and cost a list/watch
	// grant the operator deliberately does not hold.
	Reader client.Reader

	// SecretRef locates the credential Secret; the zero value resolves to
	// orkano-system/orkano-github-app.
	SecretRef types.NamespacedName

	// BaseURL is the GitHub API root; empty means https://api.github.com.
	// Overridden by tests and by GitHub Enterprise installs.
	BaseURL string

	// HTTPClient is the client used for the token exchange; nil means a fresh
	// client with a fixed timeout.
	HTTPClient *http.Client

	// Now is the clock, injectable for tests; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// InstallationToken returns a valid installation access token for the given
// installation, minting a fresh one when the cache is empty or the cached
// token is within expiryGuard of expiry. The returned string is a live GitHub
// credential: log it nowhere, persist it nowhere.
func (s *TokenSource) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", fmt.Errorf("installation id must be positive, got %d", installationID)
	}

	// The mint path is serialized under the lock — including the HTTP exchange.
	// Mints are rare (once per token lifetime) and the operator is
	// single-tenant, so the simplicity beats a singleflight; the cost is that a
	// slow GitHub stalls concurrent callers for at most httpTimeout.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[int64]cachedToken{}
	}
	if tok, ok := s.cache[installationID]; ok && s.now().Before(tok.expires.Add(-expiryGuard)) {
		return tok.token, nil
	}

	creds, err := s.loadCredentials(ctx)
	if err != nil {
		return "", err
	}
	jwt, err := buildAppJWT(creds.appID, creds.key, s.now())
	if err != nil {
		return "", err
	}
	token, expires, err := s.requestInstallationToken(ctx, jwt, installationID)
	if err != nil {
		return "", err
	}
	s.cache[installationID] = cachedToken{token: token, expires: expires}
	return token, nil
}

type appCredentials struct {
	appID int64
	key   *rsa.PrivateKey
}

func (s *TokenSource) loadCredentials(ctx context.Context) (appCredentials, error) {
	ref := s.secretRef()
	var secret corev1.Secret
	if err := s.Reader.Get(ctx, ref, &secret); err != nil {
		return appCredentials{}, fmt.Errorf("reading GitHub App secret %s: %w", ref, err)
	}

	rawID, ok := secret.Data[AppIDKey]
	if !ok || len(rawID) == 0 {
		return appCredentials{}, fmt.Errorf("GitHub App secret %s is missing the %q key", ref, AppIDKey)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(rawID)), 10, 64)
	if err != nil {
		return appCredentials{}, fmt.Errorf("GitHub App secret %s: %q is not a numeric app id: %w", ref, AppIDKey, err)
	}

	pemBytes, ok := secret.Data[PrivateKeyKey]
	if !ok || len(pemBytes) == 0 {
		return appCredentials{}, fmt.Errorf("GitHub App secret %s is missing the %q key", ref, PrivateKeyKey)
	}
	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return appCredentials{}, fmt.Errorf("GitHub App secret %s: %w", ref, err)
	}
	return appCredentials{appID: appID, key: key}, nil
}

// parseRSAPrivateKey accepts GitHub's native PKCS#1 ("BEGIN RSA PRIVATE KEY")
// and the PKCS#8 form some tooling re-encodes to.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key (tried PKCS#1 and PKCS#8): %w", err)
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want an RSA key", keyAny)
	}
	return key, nil
}

type jwtClaims struct {
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

// buildAppJWT signs the App-level JWT GitHub requires to mint installation
// tokens: RS256 over base64url(header).base64url(claims), issuer = App id.
func buildAppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	claims := jwtClaims{
		Iat: now.Add(-jwtBackdate).Unix(),
		Exp: now.Add(jwtTTL).Unix(),
		Iss: strconv.FormatInt(appID, 10),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling App JWT claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing App JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *TokenSource) requestInstallationToken(ctx context.Context, jwt string, installationID int64) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(s.baseURL(), "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("building installation-token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("requesting installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading installation-token response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		// The body here is an error message, never a token; surfacing it aids
		// debugging (bad installation id, revoked key) without leaking secrets.
		return "", time.Time{}, fmt.Errorf("GitHub answered %s minting a token for installation %d: %s",
			resp.Status, installationID, truncate(strings.TrimSpace(string(body)), 256))
	}

	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding installation-token response: %w", err)
	}
	if parsed.Token == "" {
		return "", time.Time{}, errors.New("GitHub returned an empty installation token")
	}
	if parsed.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("GitHub returned an installation token without an expiry")
	}
	return parsed.Token, parsed.ExpiresAt, nil
}

func (s *TokenSource) secretRef() types.NamespacedName {
	if s.SecretRef.Name != "" {
		return s.SecretRef
	}
	return types.NamespacedName{Namespace: SecretNamespace, Name: DefaultSecretName}
}

func (s *TokenSource) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return defaultBaseURL
}

func (s *TokenSource) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{
		Timeout: httpTimeout,
		// The mint carries the App JWT in Authorization; refuse to follow
		// redirects rather than risk forwarding that credential to a redirect
		// target (Go strips Authorization only across hosts). A redirect
		// instead surfaces as a non-201 status the caller turns into an error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// One mint per token lifetime, so close the connection instead of
		// stranding an idle keepalive and its goroutine (mirrors registry.Resolver).
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

func (s *TokenSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
