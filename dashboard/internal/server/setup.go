package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/oidc"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/db"
)

// The onboarding wizard's server side (M2.6): a setup-status endpoint that runs
// a registry of dashboard-observable checks — the wizard face of the shared
// check framework (api/check + internal/checks, PLANNING's "one framework,
// three faces") — plus the two write steps the wizard performs itself: record
// the access-mode choice, and write the orkano-oidc Secret. The GitHub App
// connect step reuses the existing manifest flow (github.go); its callback
// records connect-state in the settings table, because the dashboard is
// value-blind on the credential Secret it writes and cannot derive
// "connected" from the cluster.

// Settings keys (the settings table, migration 00007). Values are non-secret
// pointers and choices only (INV-03).
const (
	settingAccessMode        = "access_mode"
	settingGitHubAppSlug     = "github_app_slug"
	settingGitHubAppID       = "github_app_id"
	settingGitHubConnectedAt = "github_connected_at"
	settingOIDCConfiguredAt  = "oidc_configured_at"
)

// Wizard check IDs. PERMANENT once shipped (api/check contract): they appear in
// the setup-status JSON the SPA consumes and may later join doctor output.
const (
	checkAdminBootstrapped    = "auth.admin-bootstrapped"
	checkOIDCConfigured       = "auth.oidc-configured"
	checkWebhookURLConfigured = "github.webhook-url-configured"
	checkGitHubAppConnected   = "github.app-connected"
	checkDomainTLSReady       = "domains.tls-ready"
	checkAccessModeChosen     = "setup.access-mode-chosen"
)

// accessModes is the fixed vocabulary of ADR-0004's exposure paths. The wizard
// records the operator's choice; the exposure itself is performed outside the
// dashboard (the dashboard ships ClusterIP-only and holds no grant to expose
// itself, INV-05).
var accessModes = map[string]bool{
	"proxy":     true,
	"tailscale": true,
	"iap":       true,
	"public":    true,
}

// oidcSecretName is the Secret dashboard.yaml.tmpl mounts via per-key refs;
// the install pre-creates an empty placeholder so the wizard's value-blind
// UPDATE (no create — it cannot be resourceNames-pinned) has an object to
// replace. MUST stay byte-identical to internal/install (secretOIDC) and the
// template's secretKeyRef names.
const oidcSecretName = "orkano-oidc" //nolint:gosec // G101: a Secret object name, not a credential.

// oidcDiscoveryTimeout bounds the live issuer-discovery probe the OIDC connect
// step performs, well inside main's 30s WriteTimeout.
const oidcDiscoveryTimeout = 10 * time.Second

// --- setup status ---

// setupCheckJSON mirrors internal/checks' JSON projection (format.go); the
// wizard walks these in the returned (dependency) order.
type setupCheckJSON struct {
	ID          string   `json:"id"`
	Severity    string   `json:"severity"`
	Summary     string   `json:"summary,omitempty"`
	Outcome     string   `json:"outcome"`
	Message     string   `json:"message,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
}

type setupStatusResponse struct {
	Checks []setupCheckJSON `json:"checks"`
	// The raw state the wizard renders alongside the check outcomes.
	AccessMode           string `json:"accessMode"`
	WebhookURLConfigured bool   `json:"webhookUrlConfigured"`
	// PublicURLConfigured: whether ORKANO_PUBLIC_URL pins the dashboard's base
	// URL. When false, the OIDC redirect URL the connect step writes derives
	// from the request Host — correct for the access path in use, but worth a
	// warning in the UI before it lands persistently in the Secret.
	PublicURLConfigured bool `json:"publicUrlConfigured"`
	// OIDCRedirectURL is the exact callback URL the connect step will register
	// when ORKANO_PUBLIC_URL is set (empty otherwise — the client derives it
	// from its own origin then). Exposed so the wizard shows the admin the
	// server-authoritative value BEFORE they register it at the IdP: an admin
	// on a port-forward would otherwise register their localhost origin while
	// the server writes the pinned one, breaking every sign-in with a
	// redirect_uri_mismatch discovered only later.
	OIDCRedirectURL string `json:"oidcRedirectUrl"`
	OIDCEnabled     bool   `json:"oidcEnabled"`
	// OIDCPendingRestart: orkano-oidc was written but this process has not
	// loaded it — either OIDC is off entirely (initial connect) or the write
	// postdates process start (credential rotation). The UI shows the rollout
	// command either way.
	OIDCPendingRestart bool             `json:"oidcPendingRestart"`
	GitHub             setupGitHubState `json:"github"`
}

type setupGitHubState struct {
	Connected   bool   `json:"connected"`
	AppSlug     string `json:"appSlug,omitempty"`
	AppID       string `json:"appId,omitempty"`
	ConnectedAt string `json:"connectedAt,omitempty"`
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	settings, err := s.loadSettings(ctx)
	if err != nil {
		s.log.Error("load settings failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}

	oidcPending := s.oidcRestartPending(settings)

	reg := checks.New()
	for _, c := range s.setupChecks(settings, oidcPending) {
		if err := reg.Register(c); err != nil {
			// A malformed static check is a programming error; surface it loudly.
			s.log.Error("register setup check failed", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
	}
	run, err := reg.Run(ctx)
	if err != nil {
		s.log.Error("setup check run failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	resp := setupStatusResponse{
		Checks:               make([]setupCheckJSON, 0, len(run.Results)),
		AccessMode:           settings[settingAccessMode],
		WebhookURLConfigured: s.cfg.WebhookURL != "",
		PublicURLConfigured:  s.cfg.PublicURL != "",
		OIDCRedirectURL:      s.pinnedOIDCRedirectURL(),
		OIDCEnabled:          s.cfg.OIDC != nil,
		OIDCPendingRestart:   oidcPending,
		GitHub: setupGitHubState{
			Connected:   settings[settingGitHubConnectedAt] != "",
			AppSlug:     settings[settingGitHubAppSlug],
			AppID:       settings[settingGitHubAppID],
			ConnectedAt: settings[settingGitHubConnectedAt],
		},
	}
	for _, res := range run.Results {
		resp.Checks = append(resp.Checks, setupCheckJSON{
			ID:          res.ID,
			Severity:    string(res.Severity),
			Summary:     res.Summary,
			Outcome:     string(res.Outcome),
			Message:     res.Message,
			Blockers:    res.Blockers,
			Remediation: res.Remediation,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// loadSettings reads the whole (small) settings table into a map. A key that
// was never written is simply absent.
func (s *Server) loadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.cfg.Store.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, row := range rows {
		m[row.Key] = row.Value
	}
	return m, nil
}

// pinnedOIDCRedirectURL is the callback URL a connect will register when the
// public URL is pinned; empty when it would be request-derived.
func (s *Server) pinnedOIDCRedirectURL() string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	return s.cfg.PublicURL + oidcCallbackPath
}

// oidcRestartPending reports whether the orkano-oidc Secret holds newer
// configuration than this process loaded: OIDC off with the write marker set
// (initial connect), or the marker postdating process start (rotation while
// running). An unparseable marker on a live authenticator is treated as
// not-pending rather than nagging forever.
func (s *Server) oidcRestartPending(settings map[string]string) bool {
	marker := settings[settingOIDCConfiguredAt]
	if marker == "" {
		return false
	}
	if s.cfg.OIDC == nil {
		return true
	}
	ts, err := time.Parse(time.RFC3339, marker)
	return err == nil && ts.After(s.started)
}

// setupChecks composes the wizard's check set over the already-loaded settings
// map (one DB read per status call, not one per check). Registration order is
// the wizard's step order; Requires edges express real dependencies.
func (s *Server) setupChecks(settings map[string]string, oidcPending bool) []check.Check {
	return []check.Check{
		{
			ID:       checkAccessModeChosen,
			Severity: check.SeverityWarning,
			Summary:  "an access mode for reaching the dashboard has been chosen",
			Remediation: "choose how the dashboard is reached (orkano proxy, Tailscale, " +
				"identity-aware proxy, or public with enforced SSO) in the setup wizard",
			Probe: func(context.Context) (check.Result, error) {
				if mode := settings[settingAccessMode]; mode != "" {
					return check.Result{Status: check.StatusPass, Message: "access mode: " + mode}, nil
				}
				return check.Result{Status: check.StatusFail, Message: "no access mode chosen yet"}, nil
			},
		},
		{
			ID:          checkAdminBootstrapped,
			Severity:    check.SeverityCritical,
			Summary:     "a local admin with a confirmed second factor exists",
			Remediation: "redeem the install token printed by orkano init to create the admin account",
			Probe: func(ctx context.Context) (check.Result, error) {
				n, err := s.cfg.Store.CountConfirmedAdmins(ctx)
				if err != nil {
					return check.Result{}, fmt.Errorf("counting confirmed admins: %w", err)
				}
				if n > 0 {
					return check.Result{Status: check.StatusPass, Message: "local admin enrolled"}, nil
				}
				return check.Result{Status: check.StatusFail, Message: "no confirmed admin yet"}, nil
			},
		},
		{
			ID:       checkOIDCConfigured,
			Severity: check.SeverityInfo,
			Summary:  "single sign-on via an OIDC identity provider is active",
			Remediation: "connect an identity provider in the setup wizard " +
				"(the local admin remains as break-glass)",
			Probe: func(context.Context) (check.Result, error) {
				if s.cfg.OIDC != nil {
					msg := "OIDC sign-in enabled"
					if oidcPending {
						msg += "; an updated configuration awaits a dashboard restart"
					}
					return check.Result{Status: check.StatusPass, Message: msg}, nil
				}
				if oidcPending {
					return check.Result{
						Status:  check.StatusFail,
						Message: "OIDC credentials written; restart the dashboard to activate them",
					}, nil
				}
				return check.Result{Status: check.StatusFail, Message: "no identity provider connected (optional)"}, nil
			},
		},
		{
			ID:       checkWebhookURLConfigured,
			Severity: check.SeverityCritical,
			Summary:  "the receiver's public webhook URL is configured",
			Remediation: "re-run orkano init with --receiver-host, or set ORKANO_WEBHOOK_URL " +
				"on the orkano-dashboard Deployment",
			Probe: func(context.Context) (check.Result, error) {
				if s.cfg.WebhookURL != "" {
					return check.Result{Status: check.StatusPass, Message: s.cfg.WebhookURL}, nil
				}
				return check.Result{Status: check.StatusFail, Message: "no webhook URL configured"}, nil
			},
		},
		{
			ID:          checkGitHubAppConnected,
			Severity:    check.SeverityCritical,
			Summary:     "a GitHub App delivers push webhooks to this install",
			Remediation: "connect GitHub from the setup wizard (one click creates the App via the manifest flow)",
			Requires:    []string{checkWebhookURLConfigured},
			Probe: func(context.Context) (check.Result, error) {
				if settings[settingGitHubConnectedAt] != "" {
					msg := "GitHub App connected"
					if slug := settings[settingGitHubAppSlug]; slug != "" {
						msg += ": " + slug
					}
					return check.Result{Status: check.StatusPass, Message: msg}, nil
				}
				return check.Result{Status: check.StatusFail, Message: "no GitHub App connected yet"}, nil
			},
		},
		{
			ID:       checkDomainTLSReady,
			Severity: check.SeverityInfo,
			Summary:  "at least one Domain has a ready TLS certificate",
			Remediation: "add a Domain from an app screen and point its DNS at the cluster; " +
				"certificates issue via the orkano-platform issuer (Let's Encrypt staging " +
				"unless init ran with --acme-prod)",
			Probe: func(ctx context.Context) (check.Result, error) {
				var list orkanov1alpha1.DomainList
				if err := s.cfg.ViewerClient.List(ctx, &list, client.InNamespace(appsNamespace)); err != nil {
					return check.Result{}, fmt.Errorf("listing domains: %w", err)
				}
				if len(list.Items) == 0 {
					return check.Result{
						Status:  check.StatusSkip,
						Message: "no Domains yet — not applicable until an app has a custom domain",
					}, nil
				}
				ready := 0
				for _, d := range list.Items {
					for _, c := range d.Status.Conditions {
						if c.Type == orkanov1alpha1.ConditionCertificateReady && c.Status == "True" {
							ready++
							break
						}
					}
				}
				if ready > 0 {
					return check.Result{
						Status:  check.StatusPass,
						Message: fmt.Sprintf("%d of %d domain(s) have a ready certificate", ready, len(list.Items)),
					}, nil
				}
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("%d domain(s), none with a ready certificate yet", len(list.Items)),
				}, nil
			},
		},
	}
}

// --- access mode ---

type accessModeRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleSetAccessMode(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req accessModeRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !accessModes[req.Mode] {
		writeJSONError(w, http.StatusBadRequest, "invalid_access_mode")
		return
	}

	err := s.cfg.Store.UpsertSetting(r.Context(), upsertSettingParams(settingAccessMode, req.Mode))
	s.auditResult(r, user, "setup.access_mode", req.Mode, err)
	if err != nil {
		s.log.Error("record access mode failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- OIDC connect ---

// setupOIDCRequest carries the wizard's OIDC connect form. The redirect URL is
// NOT accepted from the client: the server derives the only correct value
// (dashboard base URL + the callback path) itself and returns it, so the admin
// registers exactly that at the IdP.
type setupOIDCRequest struct {
	Issuer        string `json:"issuer"`
	ClientID      string `json:"clientId"`
	ClientSecret  string `json:"clientSecret"`
	AllowedEmails string `json:"allowedEmails,omitempty"`
	AllowedGroups string `json:"allowedGroups,omitempty"`
	Scopes        string `json:"scopes,omitempty"`
	GroupsClaim   string `json:"groupsClaim,omitempty"`
}

// oidcCallbackPath is the dashboard's own OIDC callback (auth.go routes).
const oidcCallbackPath = "/api/auth/oidc/callback"

func (s *Server) handleSetupOIDC(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := userFromContext(ctx)
	var req setupOIDCRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}

	redirectURL := s.dashboardBaseURL(r) + oidcCallbackPath
	env := map[string]string{
		oidc.EnvIssuer:        strings.TrimSpace(req.Issuer),
		oidc.EnvClientID:      strings.TrimSpace(req.ClientID),
		oidc.EnvClientSecret:  req.ClientSecret,
		oidc.EnvRedirectURL:   redirectURL,
		oidc.EnvAllowedEmails: strings.TrimSpace(req.AllowedEmails),
		oidc.EnvAllowedGroups: strings.TrimSpace(req.AllowedGroups),
		oidc.EnvScopes:        strings.TrimSpace(req.Scopes),
		oidc.EnvGroupsClaim:   strings.TrimSpace(req.GroupsClaim),
	}
	getenv := func(k string) string { return env[k] }

	// Fail-closed field validation first (pure, instant), then a live discovery
	// probe against the issuer — "probe capabilities, never read configs": a
	// config the restarted dashboard could not load must not be written.
	if _, err := oidc.LoadConfig(getenv); err != nil {
		s.auditResult(r, user, "setup.oidc_configure", env[oidc.EnvIssuer], err)
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid_oidc_config")
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
	defer cancel()
	if err := s.cfg.OIDCValidator(probeCtx, getenv); err != nil {
		s.log.Warn("oidc discovery probe failed", "err", err)
		s.auditResult(r, user, "setup.oidc_configure", env[oidc.EnvIssuer], err)
		writeJSONError(w, http.StatusUnprocessableEntity, "oidc_discovery_failed")
		return
	}

	// Value-blind whole-object UPDATE of the pre-created placeholder (the
	// dashboard's orkano-system grant is update-only, resourceNames-pinned; it
	// can rotate this Secret but never read it back — INV-01/INV-07). Optional
	// keys are omitted so the loader's defaults apply on restart.
	data := make(map[string][]byte, len(env))
	for k, v := range env {
		if v != "" {
			data[k] = []byte(v)
		}
	}
	if err := s.blindUpdateSecret(ctx, oidcSecretName, data); err != nil {
		s.log.Error("oidc secret write failed", "err", err)
		s.auditResult(r, user, "setup.oidc_configure", env[oidc.EnvIssuer], err)
		s.writeK8sError(w, "update oidc secret", err)
		return
	}

	// Best-effort marker: the status endpoint uses it to show "restart pending".
	// The Secret write already succeeded, so a failed marker must not fail the
	// connect — the wizard would re-show the form, and a re-submit is idempotent.
	if err := s.cfg.Store.UpsertSetting(ctx, upsertSettingParams(settingOIDCConfiguredAt, s.now().UTC().Format(time.RFC3339))); err != nil {
		s.log.Warn("record oidc configured marker failed", "err", err)
	}

	// Audit the issuer and allowlist SIZES only — never the client secret, and
	// not the member lists (INV-03/INV-08 non-secret detail).
	s.auditDetail(ctx, actorName(user), "setup.oidc_configure", env[oidc.EnvIssuer], "success", r,
		map[string]any{
			"allowed_emails": countList(env[oidc.EnvAllowedEmails]),
			"allowed_groups": countList(env[oidc.EnvAllowedGroups]),
		})
	writeJSON(w, http.StatusOK, map[string]any{
		"redirectUrl":     redirectURL,
		"restartRequired": true,
	})
}

// countList counts the non-empty entries of a comma-separated env-style list.
func countList(s string) int {
	n := 0
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// recordGitHubConnected persists the connect marker after a successful manifest
// exchange (called from the GitHub callback). Best-effort: the Secrets are
// already written, so a marker failure must not fail the connect — it only
// costs the wizard's "connected" badge until the next successful connect.
func (s *Server) recordGitHubConnected(ctx context.Context, creds *GitHubAppCredentials) {
	// Ordered, with the connected-at marker LAST: it is the key the status
	// endpoint derives "connected" from, so a partial write must leave the
	// wizard showing not-connected rather than connected-with-missing-detail.
	for _, p := range []db.UpsertSettingParams{
		upsertSettingParams(settingGitHubAppSlug, creds.Slug),
		upsertSettingParams(settingGitHubAppID, strconv.FormatInt(creds.ID, 10)),
		upsertSettingParams(settingGitHubConnectedAt, s.now().UTC().Format(time.RFC3339)),
	} {
		if err := s.cfg.Store.UpsertSetting(ctx, p); err != nil {
			s.log.Warn("record github connect marker failed", "key", p.Key, "err", err)
		}
	}
}

// upsertSettingParams builds the generated params struct — a shorthand for the
// call sites above.
func upsertSettingParams(key, value string) db.UpsertSettingParams {
	return db.UpsertSettingParams{Key: key, Value: value}
}
