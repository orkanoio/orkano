// Package oidc is the dashboard's OIDC sign-in adapter (ADR-0016): it loads the
// env-sourced configuration, discovers the IdP, and turns an authorization-code
// round-trip into a verified, allowlist-checked Identity. It deliberately holds
// no session or storage logic — the server package owns the cookies, the JIT
// provisioning, and the audit trail; this package only speaks to the IdP.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Env var names (ADR-0016). The Deployment sources them from an optional Secret
// `orkano-oidc`; an absent issuer leaves OIDC cleanly disabled.
const (
	EnvIssuer        = "ORKANO_OIDC_ISSUER"
	EnvClientID      = "ORKANO_OIDC_CLIENT_ID"
	EnvClientSecret  = "ORKANO_OIDC_CLIENT_SECRET" //nolint:gosec // G101: env var name, not a credential
	EnvRedirectURL   = "ORKANO_OIDC_REDIRECT_URL"
	EnvAllowedEmails = "ORKANO_OIDC_ALLOWED_EMAILS"
	EnvAllowedGroups = "ORKANO_OIDC_ALLOWED_GROUPS"
	EnvScopes        = "ORKANO_OIDC_SCOPES"
	EnvGroupsClaim   = "ORKANO_OIDC_GROUPS_CLAIM"
)

// defaultGroupsClaim is the ID-token claim groups are read from when
// ORKANO_OIDC_GROUPS_CLAIM is unset; "groups" is the de-facto convention
// (Keycloak/Authentik/Okta).
const defaultGroupsClaim = "groups"

// discoveryTimeout bounds the IdP HTTP calls (discovery, JWKS, token exchange) so
// a slow or wedged IdP never hangs startup or a callback.
const discoveryTimeout = 15 * time.Second

// defaultScopes are requested when ORKANO_OIDC_SCOPES is unset. "openid" is
// always forced on regardless (an OIDC request without it is not OIDC).
var defaultScopes = []string{gooidc.ScopeOpenID, "profile", "email"}

// ErrNotConfigured signals that no OIDC issuer is set, i.e. OIDC is intentionally
// off. New returns (nil, ErrNotConfigured) so main can log info-level and carry
// on with OIDC disabled — distinct from a misconfiguration, which is an ordinary
// error that also disables OIDC but is logged loudly.
var ErrNotConfigured = errors.New("oidc: not configured")

// Config is the resolved OIDC configuration.
type Config struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	AllowedEmails []string // lowercased, deduped
	AllowedGroups []string // trimmed, deduped; matched case-sensitively (IdP-defined)
	Scopes        []string
	GroupsClaim   string
}

// Identity is the verified subset of an ID token the dashboard acts on. Subject +
// Issuer are the durable key; the rest drives the allowlist and the display
// username.
type Identity struct {
	Subject       string
	Issuer        string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// Authenticator wraps the discovered provider, the ID-token verifier, and the
// OAuth2 client. It is a startup singleton; the fixed identity model means it
// carries no per-request state.
type Authenticator struct {
	cfg      Config
	verifier *gooidc.IDTokenVerifier
	oauth    oauth2.Config
	client   *http.Client
}

// New loads the configuration, discovers the IdP, and builds an Authenticator.
// Returns (nil, ErrNotConfigured) when no issuer is set (OIDC off), or (nil, err)
// on a misconfiguration or unreachable IdP — in both cases the caller keeps the
// dashboard running with OIDC disabled, so a bad OIDC Secret never locks out the
// break-glass local admin (ADR-0016).
func New(ctx context.Context, getenv func(string) string) (*Authenticator, error) {
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: discoveryTimeout}
	dctx := gooidc.ClientContext(ctx, client)
	provider, err := gooidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover issuer %q: %w", cfg.Issuer, err)
	}

	return &Authenticator{
		cfg:      cfg,
		verifier: provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       cfg.Scopes,
		},
		client: client,
	}, nil
}

// LoadConfig reads and validates the ORKANO_OIDC_* variables. It is pure (no
// network) so configuration validity is unit-testable. An empty issuer yields
// ErrNotConfigured; an issuer with any required field missing — including an
// empty allowlist — is a validation error (fail-closed: OIDC never admits an
// entire IdP directory by omission).
func LoadConfig(getenv func(string) string) (Config, error) {
	issuer := strings.TrimSpace(getenv(EnvIssuer))
	if issuer == "" {
		return Config{}, ErrNotConfigured
	}
	// Validate the issuer is an absolute http(s) URL up front (go-oidc would error
	// anyway, but a clear config message beats a library one). http is permitted
	// for an in-cluster IdP / tests; an external IdP should use https.
	if u, err := url.Parse(issuer); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("oidc: %s must be an absolute http(s) URL, got %q", EnvIssuer, issuer)
	}

	cfg := Config{
		Issuer:        issuer,
		ClientID:      strings.TrimSpace(getenv(EnvClientID)),
		ClientSecret:  strings.TrimSpace(getenv(EnvClientSecret)),
		RedirectURL:   strings.TrimSpace(getenv(EnvRedirectURL)),
		AllowedEmails: normalizeList(getenv(EnvAllowedEmails), strings.ToLower),
		AllowedGroups: normalizeList(getenv(EnvAllowedGroups), nil),
		GroupsClaim:   strings.TrimSpace(getenv(EnvGroupsClaim)),
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = defaultGroupsClaim
	}
	cfg.Scopes = parseScopes(getenv(EnvScopes))

	switch {
	case cfg.ClientID == "":
		return Config{}, fmt.Errorf("oidc: %s is set but %s is empty", EnvIssuer, EnvClientID)
	case cfg.ClientSecret == "":
		return Config{}, fmt.Errorf("oidc: %s is set but %s is empty", EnvIssuer, EnvClientSecret)
	case cfg.RedirectURL == "":
		return Config{}, fmt.Errorf("oidc: %s is set but %s is empty", EnvIssuer, EnvRedirectURL)
	}
	if u, err := url.Parse(cfg.RedirectURL); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("oidc: %s must be an absolute http(s) URL, got %q", EnvRedirectURL, cfg.RedirectURL)
	}
	if len(cfg.AllowedEmails) == 0 && len(cfg.AllowedGroups) == 0 {
		return Config{}, fmt.Errorf("oidc: refusing to enable without an allowlist — set %s and/or %s", EnvAllowedEmails, EnvAllowedGroups)
	}
	return cfg, nil
}

// AuthCodeURL builds the IdP authorization URL for the code flow, binding the
// request to state (CSRF), nonce (replay), and a PKCE challenge. reauth adds
// prompt=login so the IdP forces a fresh authentication — the OIDC step-up path.
func (a *Authenticator) AuthCodeURL(state, nonce, verifier string, reauth bool) string {
	opts := []oauth2.AuthCodeOption{
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	}
	if reauth {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
	}
	return a.oauth.AuthCodeURL(state, opts...)
}

// Exchange swaps an authorization code for tokens, verifies the ID token
// (signature via the IdP JWKS, issuer, audience, expiry), checks the nonce, and
// returns the claims as an Identity. It does NOT apply the allowlist — that is
// Authorize, so a caller can audit a verified-but-unauthorized identity by name.
func (a *Authenticator) Exchange(ctx context.Context, code, nonce, verifier string) (*Identity, error) {
	ctx = gooidc.ClientContext(ctx, a.client)
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oidc: code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("oidc: token response carried no id_token")
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	// Nonce binds this token to the authorization request we started; a missing or
	// mismatched nonce is a replay. go-oidc deliberately does NOT check the nonce,
	// so it is ours to enforce. The explicit empty guards are load-bearing:
	// subtle.ConstantTimeCompare("", "") returns 1, so without them an empty nonce
	// on BOTH sides (a token with no nonce claim + an empty caller nonce) would
	// pass — fail closed instead.
	if nonce == "" || idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, errors.New("oidc: id_token nonce mismatch")
	}
	if idToken.Subject == "" {
		return nil, errors.New("oidc: id_token has no subject")
	}

	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("oidc: parse claims: %w", err)
	}
	return &Identity{
		Subject:       idToken.Subject,
		Issuer:        a.cfg.Issuer,
		Email:         stringClaim(raw["email"]),
		EmailVerified: boolClaim(raw["email_verified"]),
		Name:          stringClaim(raw["name"]),
		Groups:        stringSliceClaim(raw[a.cfg.GroupsClaim]),
	}, nil
}

// Authorize reports whether a verified identity passes the allowlist: a verified
// email on the email list, or membership in an allowed group. An email is honored
// only when the IdP asserts email_verified (ADR-0016) — IdPs that omit it must be
// allowlisted by group instead.
func (a *Authenticator) Authorize(id *Identity) bool {
	if id == nil {
		return false
	}
	if id.EmailVerified && id.Email != "" {
		email := strings.ToLower(strings.TrimSpace(id.Email))
		for _, allowed := range a.cfg.AllowedEmails {
			if email == allowed {
				return true
			}
		}
	}
	for _, g := range id.Groups {
		for _, allowed := range a.cfg.AllowedGroups {
			if g == allowed {
				return true
			}
		}
	}
	return false
}

// Config returns a copy of the resolved configuration (for the redirect URL and
// for tests). Slices are cloned so a caller cannot mutate the authenticator.
func (a *Authenticator) Config() Config {
	c := a.cfg
	c.AllowedEmails = append([]string(nil), a.cfg.AllowedEmails...)
	c.AllowedGroups = append([]string(nil), a.cfg.AllowedGroups...)
	c.Scopes = append([]string(nil), a.cfg.Scopes...)
	return c
}

// flowSecretBytes is the entropy of the state and nonce values: 32 bytes = 256
// bits, far past any guessing budget.
const flowSecretBytes = 32

// NewFlowSecrets mints the per-flow state, nonce, and PKCE verifier. The handler
// seals all three into the short-lived flow cookie and replays them on callback.
func NewFlowSecrets() (state, nonce, verifier string, err error) {
	state, err = randToken()
	if err != nil {
		return "", "", "", err
	}
	nonce, err = randToken()
	if err != nil {
		return "", "", "", err
	}
	// oauth2.GenerateVerifier produces an RFC 7636-compliant code verifier.
	return state, nonce, oauth2.GenerateVerifier(), nil
}

func randToken() (string, error) {
	b := make([]byte, flowSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- claim + list helpers ---

func parseScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), defaultScopes...)
	}
	// Accept either space- or comma-separated scopes.
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	out := []string{gooidc.ScopeOpenID}
	seen := map[string]bool{gooidc.ScopeOpenID: true}
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// normalizeList splits a comma-separated value, trims, optionally maps (e.g.
// lowercasing emails), drops empties, and dedupes — preserving first-seen order.
func normalizeList(raw string, mapFn func(string) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		if mapFn != nil {
			v = mapFn(v)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func stringClaim(v any) string {
	s, _ := v.(string)
	return s
}

// boolClaim reads a boolean claim that may arrive as a real bool, the strings
// "true"/"false", or a JSON number 1/0 — IdPs serialize email_verified all three
// ways (Cognito/Okta emit the number). Anything else is false (fail closed).
func boolClaim(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	case float64: // encoding/json unmarshals every JSON number into float64
		return t == 1
	default:
		return false
	}
}

// stringSliceClaim reads a groups-style claim that may be a JSON array of strings
// or a single string.
func stringSliceClaim(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
