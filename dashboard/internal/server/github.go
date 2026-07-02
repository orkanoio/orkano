package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The GitHub App manifest flow (ADR-0003 onboarding, M2.6). The wizard hands a
// manifest to GitHub, the admin clicks "Create", GitHub redirects back with a
// single-use code, and the dashboard exchanges it for the App credentials and
// writes them — value-blind — into two Kubernetes Secrets the operator and the
// receiver read. The dashboard never reads those Secrets back (its orkano-system
// grant is update-only, resourceNames-pinned), so a compromised dashboard can
// rotate the App credential but never exfiltrate the private key (INV-01/INV-07).
//
// The mechanics were proven by spike 3 (hack/spikes/03-github-app-manifest).

const (
	// These Secret coordinates are a cross-component contract. The operator reads
	// the App credential to mint installation tokens; the receiver reads the
	// webhook secret to verify signatures. They live in orkano-system, NOT the
	// dashboard's orkano-apps namespace, so the manifest flow needs the
	// orkano-system update grant (config/rbac/dashboard.yaml).
	//
	// They MUST stay byte-identical to their authorities:
	//   githubAppSecretName / *Key  → operator/internal/githubapp (Default*/...Key)
	//   webhookSecretName / ...Key  → internal/install secretWebhook + its "secret" key
	//   systemNamespace             → githubapp.SecretNamespace
	// Renaming any of these without updating the reader breaks the deploy path.
	githubAppSecretName    = "orkano-github-app"     //nolint:gosec // G101: a Secret object name, not a credential.
	githubAppIDKey         = "app-id"                //
	githubAppPrivateKeyKey = "private-key.pem"       //
	webhookSecretName      = "orkano-webhook-secret" //nolint:gosec // G101: a Secret object name, not a credential.
	webhookSecretKey       = "secret"                //
	systemNamespace        = "orkano-system"

	// githubCookie is the short-lived sealed flow cookie carrying the CSRF state
	// (round-tripped through GitHub) and the initiating admin's username for the
	// callback's audit attribution. SameSite=Lax like the OIDC flow cookie: the
	// GitHub→callback hop is a cross-site top-level GET a Strict cookie would not
	// be sent on.
	githubCookie = "orkano_github"
	// githubFlowTTL bounds the round-trip generously (the admin may spend a minute
	// on GitHub's app-creation screen).
	githubFlowTTL = 15 * time.Minute

	// defaultGitHubBaseURL is the github.com host the App-creation form posts to;
	// overridden via Config.GitHubBaseURL for GitHub Enterprise.
	defaultGitHubBaseURL = "https://github.com"

	// githubManifestPath / githubCallbackPath are the dashboard's own endpoints.
	githubCallbackPath = "/api/github/app/callback"
)

// SSO-style error codes appended to the SPA redirect on a failed callback. A
// fixed set, never reflected user input, so the redirect stays a safe relative
// path (mirrors oidc.go's sso* codes).
const (
	ghNoFlow        = "no_flow"
	ghStateMismatch = "state_mismatch"
	ghNoCode        = "no_code"
	ghExchange      = "exchange_failed"
	ghWrite         = "write_failed"
)

// orgNameRe validates the optional ?org= parameter, which lands in the GitHub
// form URL PATH. A GitHub login is alphanumeric plus single hyphens, ≤39 chars,
// no leading/trailing hyphen — pinning it here keeps a hostile value out of the
// path (it is also url.PathEscape'd, belt and braces).
var orgNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// appNameRe validates the optional ?name= parameter. The name only ever lands in
// the JSON manifest (json.Marshal escapes it) and on GitHub's screen, so the
// bound is conservative-but-friendly; the admin can still edit it on GitHub.
var appNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,33}$`)

// defaultAppName seeds the manifest's App name; GitHub App names are globally
// unique, so the admin edits it on GitHub's screen (or passes ?name=).
const defaultAppName = "orkano"

// GitHubAppCredentials is the slice of GitHub's manifest-conversion response the
// dashboard persists. The PEM and WebhookSecret are live credentials — never
// logged, never written to the database (INV-03/INV-07), only to the two
// Kubernetes Secrets.
type GitHubAppCredentials struct {
	ID            int64
	Slug          string
	PEM           string
	WebhookSecret string
}

// ManifestExchanger swaps the single-use manifest code GitHub appends to the
// callback for the App's credentials. It is an interface so the handler tests
// drive the flow with a fake, no live GitHub; NewGitHubExchanger is production.
type ManifestExchanger interface {
	Exchange(ctx context.Context, code string) (*GitHubAppCredentials, error)
}

// githubFlow is the JSON sealed into the flow cookie. It carries no secret — just
// the per-flow CSRF state and the initiating admin's username for audit, plus an
// absolute expiry checked server-side.
type githubFlow struct {
	State   string `json:"state"`
	Actor   string `json:"actor"`
	Expires int64  `json:"exp"`
}

// manifest is the GitHub App manifest (the documented manifest-flow schema). It
// is marshaled to JSON and POSTed to GitHub by the browser via the form the SPA
// builds from the start endpoint's response.
type manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     hookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	CallbackURLs       []string          `json:"callback_urls"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type hookAttributes struct {
	URL string `json:"url"`
}

// handleGitHubManifest starts the manifest flow: build the manifest (webhook URL
// from config, redirect URL from the request/PublicURL), seal a CSRF state into
// the flow cookie, and return the GitHub form-POST URL plus the manifest JSON for
// the SPA to auto-submit. RequireSession-gated — only an authenticated admin
// starts it.
func (s *Server) handleGitHubManifest(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	// The receiver's public webhook endpoint is install-specific and must be
	// configured before a usable manifest can be generated (GitHub signs webhooks
	// with the App secret and delivers them here). Fail clean if it is unset.
	if s.cfg.WebhookURL == "" {
		writeJSONError(w, http.StatusConflict, "webhook_url_not_configured")
		return
	}

	org := r.URL.Query().Get("org")
	if org != "" && !orgNameRe.MatchString(org) {
		writeJSONError(w, http.StatusBadRequest, "invalid_org")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = defaultAppName
	} else if !appNameRe.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}

	state, err := randomState()
	if err != nil {
		s.log.Error("github flow state failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	redirectURL := s.dashboardBaseURL(r) + githubCallbackPath
	m := manifest{
		Name:               name,
		URL:                "https://github.com/orkanoio/orkano",
		HookAttributes:     hookAttributes{URL: s.cfg.WebhookURL},
		RedirectURL:        redirectURL,
		CallbackURLs:       []string{redirectURL},
		Public:             false,
		DefaultPermissions: map[string]string{"contents": "read", "metadata": "read"},
		DefaultEvents:      []string{"push"},
	}
	body, err := json.Marshal(m)
	if err != nil {
		s.log.Error("marshal manifest failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	if !s.setGitHubCookie(w, r, githubFlow{State: state, Actor: actorName(user)}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"postUrl":  s.newAppURL(org, state),
		"manifest": string(body),
	})
}

// handleGitHubCallback completes the flow: validate state against the flow
// cookie, exchange the code for credentials, and write them value-blind into the
// two Secrets. A top-level browser navigation (the SameSite=Strict session cookie
// is not sent), so every exit is a redirect to the SPA, never raw JSON. The flow
// cookie is single-use — cleared on entry regardless of outcome.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	flow, ok := s.readGitHubCookie(r)
	s.clearGitHubCookie(w, r)

	actor := "anonymous"
	if ok {
		actor = flow.Actor
	}
	q := r.URL.Query()

	// GitHub itself reported a problem (e.g. the admin cancelled on the App-creation
	// screen). Don't reflect the message; treat it as a failed exchange (mirrors the
	// OIDC callback's IdP-error branch).
	if e := q.Get("error"); e != "" {
		s.audit(ctx, actor, "github.app_connect", "", "failure", r)
		s.redirectGitHubError(w, r, ghExchange)
		return
	}
	if !ok {
		s.audit(ctx, actor, "github.app_connect", "", "failure", r)
		s.redirectGitHubError(w, r, ghNoFlow)
		return
	}
	// state binds the callback to the cookie we set (CSRF). Constant-time, with an
	// empty guard so two empties cannot match (subtle.ConstantTimeCompare("","")==1).
	state := q.Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		s.audit(ctx, actor, "github.app_connect", "", "failure", r)
		s.redirectGitHubError(w, r, ghStateMismatch)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.audit(ctx, actor, "github.app_connect", "", "failure", r)
		s.redirectGitHubError(w, r, ghNoCode)
		return
	}

	creds, err := s.cfg.GitHub.Exchange(ctx, code)
	if err != nil {
		s.log.Warn("github manifest exchange failed", "err", err)
		s.audit(ctx, actor, "github.app_connect", "", "failure", r)
		s.redirectGitHubError(w, r, ghExchange)
		return
	}

	if err := s.writeGitHubSecrets(ctx, creds); err != nil {
		s.log.Error("github credential write failed", "err", err)
		s.auditDetail(ctx, actor, "github.app_connect", creds.Slug, "failure", r, nil)
		s.redirectGitHubError(w, r, ghWrite)
		return
	}

	// Audit success with the App slug as the non-secret target (INV-08); the PEM
	// and webhook secret are never recorded (INV-03). The settings marker feeds
	// the wizard's "connected" state — best-effort, the connect already succeeded.
	s.recordGitHubConnected(ctx, creds)
	s.auditDetail(ctx, actor, "github.app_connect", creds.Slug, "success", r,
		map[string]any{"app_id": creds.ID})
	http.Redirect(w, r, "/?github=connected", http.StatusFound)
}

// writeGitHubSecrets writes the App credential and webhook secret value-blind.
// Both are blind UPDATEs (the dashboard's orkano-system grant is update-only,
// resourceNames-pinned, no get/create/delete): the install pre-creates an empty
// orkano-github-app placeholder and a generated orkano-webhook-secret, so both
// objects already exist. A NotFound therefore means a broken install, surfaced as
// an error rather than silently swallowed. The whole-object Update with no
// resourceVersion replaces the data without a read-modify-write (the only way to
// write a Secret the dashboard is forbidden to read).
func (s *Server) writeGitHubSecrets(ctx context.Context, creds *GitHubAppCredentials) error {
	if err := s.blindUpdateSecret(ctx, githubAppSecretName, map[string][]byte{
		githubAppIDKey:         []byte(strconv.FormatInt(creds.ID, 10)),
		githubAppPrivateKeyKey: []byte(creds.PEM),
	}); err != nil {
		return fmt.Errorf("write %s: %w", githubAppSecretName, err)
	}
	if err := s.blindUpdateSecret(ctx, webhookSecretName, map[string][]byte{
		webhookSecretKey: []byte(creds.WebhookSecret),
	}); err != nil {
		return fmt.Errorf("write %s: %w", webhookSecretName, err)
	}
	return nil
}

func (s *Server) blindUpdateSecret(ctx context.Context, name string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	return s.cfg.K8s.Update(ctx, secret)
}

// --- flow cookie (mirrors oidc.go's Lax sealed cookie) ---

func (s *Server) setGitHubCookie(w http.ResponseWriter, r *http.Request, flow githubFlow) bool {
	flow.Expires = s.now().Add(githubFlowTTL).Unix()
	payload, err := json.Marshal(flow)
	if err != nil {
		s.log.Error("marshal github flow failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return false
	}
	sealed, err := s.cfg.Cipher.Seal(string(payload))
	if err != nil {
		s.log.Error("seal github flow failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return false
	}
	//nolint:gosec // G124: HttpOnly always set; SameSite=Lax is REQUIRED for the cross-site GitHub callback; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     githubCookie,
		Value:    sealed,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(githubFlowTTL / time.Second),
	})
	return true
}

func (s *Server) readGitHubCookie(r *http.Request) (githubFlow, bool) {
	c, err := r.Cookie(githubCookie)
	if err != nil || c.Value == "" {
		return githubFlow{}, false
	}
	plain, err := s.cfg.Cipher.Open(c.Value)
	if err != nil {
		return githubFlow{}, false
	}
	var flow githubFlow
	if err := json.Unmarshal([]byte(plain), &flow); err != nil {
		return githubFlow{}, false
	}
	if flow.State == "" || s.now().Unix() >= flow.Expires {
		return githubFlow{}, false
	}
	return flow, true
}

func (s *Server) clearGitHubCookie(w http.ResponseWriter, r *http.Request) {
	//nolint:gosec // G124: HttpOnly+SameSite=Lax mirror the live flow cookie; Secure is conditional on TLS so ClusterIP http access (orkano proxy, INV-05) still works
	http.SetCookie(w, &http.Cookie{
		Name:     githubCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) redirectGitHubError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?github_error="+code, http.StatusFound)
}

// --- URL helpers ---

// dashboardBaseURL returns the dashboard's external base URL for the manifest
// redirect: the explicitly-configured PublicURL when set, else derived from the
// request (scheme + Host). The dashboard is ClusterIP-only and reached over a
// trusted path (orkano proxy / Tailscale / IAP, INV-05), so the request Host is
// trustworthy there; PublicURL pins it for the public-with-SSO mode.
func (s *Server) dashboardBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// newAppURL is the GitHub App-creation form action: an org form when org is set,
// else the personal-account form. state is round-tripped by GitHub onto the
// callback and verified against the flow cookie.
func (s *Server) newAppURL(org, state string) string {
	base := s.githubBaseURL()
	path := base + "/settings/apps/new"
	if org != "" {
		path = base + "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return path + "?state=" + url.QueryEscape(state)
}

func (s *Server) githubBaseURL() string {
	if s.cfg.GitHubBaseURL != "" {
		return s.cfg.GitHubBaseURL
	}
	return defaultGitHubBaseURL
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// --- the production exchanger ---

const (
	// githubAPIVersion is GitHub's stable API-version pin. It mirrors the operator's
	// githubapp.githubAPIVersion (separate module, same value) — bump both together.
	githubAPIVersion     = "2022-11-28"
	defaultGitHubAPIBase = "https://api.github.com"
	// exchangeTimeout bounds the single conversion POST. Kept at the OIDC exchanger's
	// budget so the callback's exchange + the two Secret writes finish inside main's
	// 30s server WriteTimeout (the redirect must reach the browser).
	exchangeTimeout        = 15 * time.Second
	maxConversionRespBytes = 1 << 20 // 1 MiB cap on the conversion response body.
)

// githubExchanger POSTs the manifest code to GitHub's unauthenticated conversion
// endpoint and parses the credentials out. Hand-rolled stdlib HTTP, mirroring the
// operator's githubapp/registry minimal-client philosophy — the call is one POST.
type githubExchanger struct {
	apiBaseURL string
	client     *http.Client
}

// NewGitHubExchanger builds the production exchanger. An empty apiBaseURL
// defaults to api.github.com; GitHub Enterprise passes its API root.
func NewGitHubExchanger(apiBaseURL string) ManifestExchanger {
	if apiBaseURL == "" {
		apiBaseURL = defaultGitHubAPIBase
	}
	return &githubExchanger{
		apiBaseURL: apiBaseURL,
		client: &http.Client{
			Timeout: exchangeTimeout,
			// The conversion carries no credential, but a redirect on it is anomalous;
			// surface it as a non-201 error rather than following it. Close connections
			// (one-shot call) instead of stranding an idle keepalive.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{DisableKeepAlives: true},
		},
	}
}

type conversionResponse struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

func (e *githubExchanger) Exchange(ctx context.Context, code string) (*GitHubAppCredentials, error) {
	// The host comes from server config (apiBaseURL); code is url.PathEscape'd into
	// the path component only, so it can never redirect the request to another host
	// — gosec's SSRF taint flag is a false positive here.
	u := fmt.Sprintf("%s/app-manifests/%s/conversions", e.apiBaseURL, url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil) //nolint:gosec // G704: apiBaseURL is server config, code is PathEscaped into the path only — not the host
	if err != nil {
		return nil, fmt.Errorf("building conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := e.client.Do(req) //nolint:gosec // G704: see the request construction above — the host is server config, not request-controlled
	if err != nil {
		return nil, fmt.Errorf("requesting manifest conversion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConversionRespBytes))
	if err != nil {
		return nil, fmt.Errorf("reading conversion response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		// The body here is an error message, never credentials; the code is single
		// use and expires after ~1h, so a stale code lands here.
		return nil, fmt.Errorf("GitHub answered %s to the manifest conversion: %s",
			resp.Status, truncate(string(body), 256))
	}

	var conv conversionResponse
	if err := json.Unmarshal(body, &conv); err != nil {
		return nil, fmt.Errorf("decoding conversion response: %w", err)
	}
	if conv.ID <= 0 || conv.PEM == "" || conv.WebhookSecret == "" {
		return nil, errors.New("conversion response is missing the id, pem, or webhook secret")
	}
	return &GitHubAppCredentials{
		ID:            conv.ID,
		Slug:          conv.Slug,
		PEM:           conv.PEM,
		WebhookSecret: conv.WebhookSecret,
	}, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
