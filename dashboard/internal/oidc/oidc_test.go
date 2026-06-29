package oidc_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/dashboard/internal/oidc"
)

// envMap backs a getenv func from a literal map (absent key = "").
type envMap map[string]string

func (m envMap) get(k string) string { return m[k] }

func baseEnv(issuer string) envMap {
	return envMap{
		oidc.EnvIssuer:        issuer,
		oidc.EnvClientID:      "orkano",
		oidc.EnvClientSecret:  "shh",
		oidc.EnvRedirectURL:   "https://orkano.example/api/auth/oidc/callback",
		oidc.EnvAllowedEmails: "alice@example.com",
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("empty issuer is not-configured", func(t *testing.T) {
		_, err := oidc.LoadConfig(envMap{}.get)
		if !errors.Is(err, oidc.ErrNotConfigured) {
			t.Fatalf("want ErrNotConfigured, got %v", err)
		}
	})

	t.Run("issuer with a missing required field errors", func(t *testing.T) {
		for _, miss := range []string{oidc.EnvClientID, oidc.EnvClientSecret, oidc.EnvRedirectURL} {
			env := baseEnv("https://idp.example")
			delete(env, miss)
			_, err := oidc.LoadConfig(env.get)
			if err == nil || errors.Is(err, oidc.ErrNotConfigured) {
				t.Fatalf("missing %s: want a validation error, got %v", miss, err)
			}
		}
	})

	t.Run("no allowlist is fail-closed", func(t *testing.T) {
		env := baseEnv("https://idp.example")
		delete(env, oidc.EnvAllowedEmails)
		_, err := oidc.LoadConfig(env.get)
		if err == nil || errors.Is(err, oidc.ErrNotConfigured) {
			t.Fatalf("want allowlist validation error, got %v", err)
		}
	})

	t.Run("relative redirect URL errors", func(t *testing.T) {
		env := baseEnv("https://idp.example")
		env[oidc.EnvRedirectURL] = "/api/auth/oidc/callback"
		if _, err := oidc.LoadConfig(env.get); err == nil {
			t.Fatal("want absolute-URL validation error")
		}
	})

	t.Run("valid config normalizes", func(t *testing.T) {
		env := baseEnv("https://idp.example")
		env[oidc.EnvAllowedEmails] = " Alice@Example.com , bob@example.com , alice@example.com "
		env[oidc.EnvAllowedGroups] = "platform, , platform"
		env[oidc.EnvScopes] = "email groups"
		cfg, err := oidc.LoadConfig(env.get)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		// Emails lowercased + deduped, preserving order.
		if got := strings.Join(cfg.AllowedEmails, ","); got != "alice@example.com,bob@example.com" {
			t.Fatalf("emails: %q", got)
		}
		if got := strings.Join(cfg.AllowedGroups, ","); got != "platform" {
			t.Fatalf("groups: %q", got)
		}
		// openid is forced on even when the admin omits it.
		if cfg.Scopes[0] != "openid" || !contains(cfg.Scopes, "email") || !contains(cfg.Scopes, "groups") {
			t.Fatalf("scopes: %v", cfg.Scopes)
		}
		if cfg.GroupsClaim != "groups" {
			t.Fatalf("default groups claim: %q", cfg.GroupsClaim)
		}
	})
}

func TestAuthorize(t *testing.T) {
	stub := newIDPStub(t)
	defer stub.Close()
	env := baseEnv(stub.URL)
	env[oidc.EnvAllowedGroups] = "platform-admins"
	a, err := oidc.New(context.Background(), env.get)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name string
		id   oidc.Identity
		want bool
	}{
		{"verified email on the list", oidc.Identity{Email: "Alice@Example.com", EmailVerified: true}, true},
		{"unverified email denied", oidc.Identity{Email: "alice@example.com", EmailVerified: false}, false},
		{"email not on the list", oidc.Identity{Email: "mallory@example.com", EmailVerified: true}, false},
		{"allowed group", oidc.Identity{Groups: []string{"other", "platform-admins"}}, true},
		{"group not on the list", oidc.Identity{Groups: []string{"randoms"}}, false},
		{"nothing matches", oidc.Identity{Email: "x@y.z"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Authorize(&tc.id); got != tc.want {
				t.Fatalf("Authorize(%+v) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
	if a.Authorize(nil) {
		t.Fatal("nil identity must not authorize")
	}
}

func TestNewDisabledAndMisconfigured(t *testing.T) {
	if _, err := oidc.New(context.Background(), envMap{}.get); !errors.Is(err, oidc.ErrNotConfigured) {
		t.Fatalf("empty issuer: want ErrNotConfigured, got %v", err)
	}
	// A configured issuer that fails discovery is an ordinary error (not
	// ErrNotConfigured) — main logs it and keeps OIDC disabled.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no discovery here", http.StatusNotFound)
	}))
	defer dead.Close()
	_, err := oidc.New(context.Background(), baseEnv(dead.URL).get)
	if err == nil || errors.Is(err, oidc.ErrNotConfigured) {
		t.Fatalf("unreachable discovery: want a real error, got %v", err)
	}
}

func TestNewFlowSecrets(t *testing.T) {
	s1, n1, v1, err := oidc.NewFlowSecrets()
	if err != nil {
		t.Fatalf("NewFlowSecrets: %v", err)
	}
	s2, n2, v2, _ := oidc.NewFlowSecrets()
	for _, v := range []string{s1, n1, v1} {
		if v == "" {
			t.Fatal("flow secret must be non-empty")
		}
	}
	if s1 == s2 || n1 == n2 || v1 == v2 {
		t.Fatal("flow secrets must differ across calls")
	}
}

func TestExchangeVerifiesAndExtractsClaims(t *testing.T) {
	stub := newIDPStub(t)
	defer stub.Close()
	env := baseEnv(stub.URL)
	env[oidc.EnvAllowedGroups] = "platform"
	a, err := oidc.New(context.Background(), env.get)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now()
	base := map[string]any{
		"iss": stub.URL, "aud": "orkano", "sub": "user-123",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": "the-nonce", "email": "alice@example.com",
		"email_verified": true, "name": "Alice", "groups": []any{"platform"},
	}

	t.Run("happy path", func(t *testing.T) {
		stub.setIDToken(t, base)
		id, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if id.Subject != "user-123" || id.Email != "alice@example.com" || !id.EmailVerified {
			t.Fatalf("identity: %+v", id)
		}
		if id.Issuer != stub.URL || id.Name != "Alice" || !contains(id.Groups, "platform") {
			t.Fatalf("identity: %+v", id)
		}
		if !a.Authorize(id) {
			t.Fatal("the verified identity should pass the allowlist")
		}
	})

	t.Run("nonce mismatch is rejected", func(t *testing.T) {
		stub.setIDToken(t, base)
		if _, err := a.Exchange(context.Background(), "code", "WRONG-nonce", "verifier"); err == nil {
			t.Fatal("want a nonce-mismatch error")
		}
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		claims := cloneClaims(base)
		claims["aud"] = "someone-else"
		stub.setIDToken(t, claims)
		if _, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier"); err == nil {
			t.Fatal("want an audience verification error")
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		claims := cloneClaims(base)
		claims["exp"] = now.Add(-time.Hour).Unix()
		stub.setIDToken(t, claims)
		if _, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier"); err == nil {
			t.Fatal("want an expiry verification error")
		}
	})

	t.Run("string email_verified is honored", func(t *testing.T) {
		claims := cloneClaims(base)
		claims["email_verified"] = "true"
		stub.setIDToken(t, claims)
		id, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier")
		if err != nil || !id.EmailVerified {
			t.Fatalf("string email_verified: %+v, err %v", id, err)
		}
	})

	t.Run("numeric email_verified is honored", func(t *testing.T) {
		claims := cloneClaims(base)
		claims["email_verified"] = 1
		stub.setIDToken(t, claims)
		id, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier")
		if err != nil || !id.EmailVerified {
			t.Fatalf("numeric email_verified: %+v, err %v", id, err)
		}
	})

	// The signature is the package's most critical invariant: a token signed by a
	// key NOT in the IdP's JWKS must be rejected.
	t.Run("token signed by a wrong key is rejected", func(t *testing.T) {
		wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa key: %v", err)
		}
		stub.idToken = signIDToken(t, wrongKey, base)
		if _, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier"); err == nil {
			t.Fatal("want a signature verification error")
		}
	})

	// A token carrying no nonce claim must always be rejected — including (the
	// load-bearing case) when the caller's nonce is also empty, which a naive
	// constant-time compare would let through.
	t.Run("absent nonce claim is rejected", func(t *testing.T) {
		claims := cloneClaims(base)
		delete(claims, "nonce")
		stub.setIDToken(t, claims)
		if _, err := a.Exchange(context.Background(), "code", "the-nonce", "verifier"); err == nil {
			t.Fatal("absent nonce + caller nonce: want rejection")
		}
		stub.setIDToken(t, claims)
		if _, err := a.Exchange(context.Background(), "code", "", "verifier"); err == nil {
			t.Fatal("absent nonce + empty caller nonce: want rejection (the empty-compare bypass)")
		}
	})
}

// --- a minimal in-process OIDC provider for the verify path ---

type idpStub struct {
	*httptest.Server
	key     *rsa.PrivateKey
	idToken string
}

const stubKID = "orkano-test-key"

func newIDPStub(t *testing.T) *idpStub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	s := &idpStub{key: key}
	mux := http.NewServeMux()
	s.Server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 s.URL,
			"authorization_endpoint": s.URL + "/auth",
			"token_endpoint":         s.URL + "/token",
			"jwks_uri":               s.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": stubKID,
			"n": b64(key.N.Bytes()),
			"e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": s.idToken,
		})
	})
	return s
}

// setIDToken signs the claims (with the stub's own key, matching its JWKS) into
// the id_token the /token endpoint returns next.
func (s *idpStub) setIDToken(t *testing.T, claims map[string]any) {
	t.Helper()
	s.idToken = signIDToken(t, s.key, claims)
}

// signIDToken builds a real RS256-signed JWT. Signing with a key OTHER than the
// stub's drives the wrong-key rejection test (the JWKS won't match).
func signIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := b64(mustJSON(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": stubKID}))
	payload := b64(mustJSON(t, claims))
	signing := header + "." + payload
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign id_token: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func cloneClaims(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
