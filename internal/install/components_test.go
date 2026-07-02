package install

import (
	"context"
	"path"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderByName renders the components and returns them keyed by manifest name.
func renderByName(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	files, err := renderComponents(cfg)
	if err != nil {
		t.Fatalf("renderComponents: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.name] = string(f.content)
		assertValidYAML(t, f.name, f.content)
	}
	return out
}

// assertValidYAML parses every document in a (possibly multi-doc) manifest, so a
// template that produces malformed YAML fails loudly.
func assertValidYAML(t *testing.T, name string, content []byte) {
	t.Helper()
	for i, doc := range strings.Split(string(content), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("%s doc %d is not valid YAML: %v", name, i, err)
		}
	}
}

func TestRenderComponentsEmptyVersionRendersNothing(t *testing.T) {
	files, err := renderComponents(Config{})
	if err != nil {
		t.Fatalf("renderComponents: %v", err)
	}
	if files != nil {
		t.Fatalf("expected no component manifests without a version, got %d", len(files))
	}
}

func TestRenderComponentsVersionTagsFirstPartyImages(t *testing.T) {
	m := renderByName(t, Config{Version: "1.4.2"})
	for _, name := range []string{"operator-deployment.yaml", "receiver.yaml", "dashboard.yaml", "platform-issuer.yaml", "migration-job.yaml"} {
		if _, ok := m[name]; !ok {
			t.Errorf("expected %s to be rendered", name)
		}
	}
	if !strings.Contains(m["operator-deployment.yaml"], "ghcr.io/orkanoio/orkano-operator:1.4.2") {
		t.Error("operator deployment should use the version-tagged operator image")
	}
	if !strings.Contains(m["migration-job.yaml"], "ghcr.io/orkanoio/orkano-operator:1.4.2") {
		t.Error("migration job should reuse the version-tagged operator image")
	}
	if !strings.Contains(m["receiver.yaml"], "ghcr.io/orkanoio/orkano-receiver:1.4.2") {
		t.Error("receiver deployment should use the version-tagged receiver image")
	}
	if !strings.Contains(m["dashboard.yaml"], "ghcr.io/orkanoio/orkano-dashboard:1.4.2") {
		t.Error("dashboard deployment should use the version-tagged dashboard image")
	}
}

func TestRenderComponentsDashboard(t *testing.T) {
	d := renderByName(t, Config{Version: "1.0.0"})["dashboard.yaml"]
	if !strings.Contains(d, "serviceAccountName: orkano-dashboard") {
		t.Error("dashboard should run under the orkano-dashboard ServiceAccount")
	}
	if !strings.Contains(d, "name: orkano-dashboard-db") {
		t.Error("dashboard should read its DSN from the orkano-dashboard-db Secret")
	}
	// The TOTP-seed encryption key and the install-token hash are injected via
	// secretKeyRef (no secrets-read RBAC needed, ADR-0013).
	for _, want := range []string{
		"name: ORKANO_DASHBOARD_ENC_KEY",
		"name: orkano-dashboard-enc-key",
		"name: ORKANO_BOOTSTRAP_TOKEN_SHA256",
		"name: orkano-bootstrap-token",
		"key: token-sha256",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("dashboard manifest missing %q", want)
		}
	}
	// OIDC config (ADR-0016) is injected via EXPLICIT per-key optional
	// secretKeyRefs — deliberately NOT envFrom: the orkano-oidc Secret is
	// dashboard-SA-writable (the wizard grant), and envFrom would let a
	// hostile write inject arbitrary env into the pod (e.g. redirect the
	// GitHub manifest exchange). Naming the keys pins the injectable surface
	// to exactly the OIDC configuration.
	if strings.Contains(d, "envFrom:") {
		t.Error("dashboard must not envFrom the dashboard-writable orkano-oidc Secret")
	}
	for _, key := range []string{
		"ORKANO_OIDC_ISSUER", "ORKANO_OIDC_CLIENT_ID", "ORKANO_OIDC_CLIENT_SECRET",
		"ORKANO_OIDC_REDIRECT_URL", "ORKANO_OIDC_ALLOWED_EMAILS",
		"ORKANO_OIDC_ALLOWED_GROUPS", "ORKANO_OIDC_SCOPES", "ORKANO_OIDC_GROUPS_CLAIM",
	} {
		if !strings.Contains(d, "key: "+key) {
			t.Errorf("dashboard manifest missing OIDC key ref %q", key)
		}
	}
	for _, want := range []string{"name: orkano-oidc", "optional: true"} {
		if !strings.Contains(d, want) {
			t.Errorf("dashboard manifest missing OIDC secretKeyRef %q", want)
		}
	}
	// INV-05: ClusterIP only, never an Ingress; never a public Service type.
	if strings.Contains(d, "kind: Ingress") {
		t.Error("dashboard must not render an Ingress (INV-05, ClusterIP only)")
	}
	if strings.Contains(d, "type: NodePort") || strings.Contains(d, "type: LoadBalancer") {
		t.Error("dashboard Service must stay ClusterIP (INV-05)")
	}
}

// TestRenderComponentsDashboardWebhookURL: init's --receiver-host threads the
// receiver's public webhook endpoint into the dashboard env (the GitHub App
// manifest flow needs it); without the flag the variable is absent and the
// wizard's GitHub step shows the remediation instead.
func TestRenderComponentsDashboardWebhookURL(t *testing.T) {
	without := renderByName(t, Config{Version: "1.0.0"})["dashboard.yaml"]
	if strings.Contains(without, "ORKANO_WEBHOOK_URL") {
		t.Error("no webhook URL env should render without --receiver-host")
	}

	with := renderByName(t, Config{Version: "1.0.0", ReceiverHost: "hooks.example.com"})["dashboard.yaml"]
	if !strings.Contains(with, "name: ORKANO_WEBHOOK_URL") ||
		!strings.Contains(with, `value: "https://hooks.example.com/webhook"`) {
		t.Errorf("dashboard should carry the receiver webhook URL, got:\n%s", with)
	}
}

func TestRenderComponentsACMEServerAndEmail(t *testing.T) {
	// Match the server directive line, not the whole manifest (a comment mentions
	// "staging by default", which would fool a substring check).
	staging := renderByName(t, Config{Version: "1.0.0"})["platform-issuer.yaml"]
	if !strings.Contains(staging, "server: "+acmeStagingServer) {
		t.Error("default issuer should use the Let's Encrypt staging server")
	}
	if strings.Contains(staging, "email:") {
		t.Error("no email line should render without --acme-email")
	}

	prod := renderByName(t, Config{Version: "1.0.0", ACMEProd: true, ACMEEmail: "ops@example.com"})["platform-issuer.yaml"]
	if !strings.Contains(prod, "server: "+acmeProdServer) {
		t.Error("--acme-prod should select the production ACME server")
	}
	if !strings.Contains(prod, "email: ops@example.com") {
		t.Error("--acme-email should render an email line")
	}
}

func TestRenderComponentsAllowlist(t *testing.T) {
	m := renderByName(t, Config{Version: "1.0.0", RepoAllowlist: []string{"orkanoio/orkano", "acme/widgets"}})
	if !strings.Contains(m["receiver.yaml"], `value: "orkanoio/orkano,acme/widgets"`) {
		t.Error("receiver should carry the comma-joined repo allowlist")
	}
}

func TestRenderComponentsReceiverIngressOptional(t *testing.T) {
	// Without --receiver-host the Ingress is skipped entirely (an empty host would
	// render an invalid Ingress); the receiver stays ClusterIP-only.
	without := renderByName(t, Config{Version: "1.0.0"})
	if _, ok := without["receiver-ingress.yaml"]; ok {
		t.Error("receiver Ingress should not render without --receiver-host")
	}

	with := renderByName(t, Config{Version: "1.0.0", ReceiverHost: "hooks.example.com"})
	ing, ok := with["receiver-ingress.yaml"]
	if !ok {
		t.Fatal("receiver Ingress should render with --receiver-host")
	}
	for _, want := range []string{
		"host: hooks.example.com",
		"- hooks.example.com",
		"cert-manager.io/cluster-issuer: orkano-platform",
		"secretName: orkano-receiver-tls",
		"ingressClassName: traefik",
		"name: orkano-receiver",
	} {
		if !strings.Contains(ing, want) {
			t.Errorf("receiver Ingress missing %q", want)
		}
	}
}

func TestRenderComponentsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"bad version", Config{Version: "1.0.0 ; rm -rf /"}},
		{"bad email", Config{Version: "1.0.0", ACMEEmail: "not-an-email\ninjected: true"}},
		{"bad repo", Config{Version: "1.0.0", RepoAllowlist: []string{`bad"repo`}}},
		{"repo without slash", Config{Version: "1.0.0", RepoAllowlist: []string{"noslash"}}},
		{"bad receiver host", Config{Version: "1.0.0", ReceiverHost: "bad host\ninjected: true"}},
		{"receiver host with scheme", Config{Version: "1.0.0", ReceiverHost: "https://hooks.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := renderComponents(tc.cfg); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestApplyWritesComponentsWhenVersioned(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	for _, name := range []string{
		"operator-deployment.yaml",
		"receiver.yaml",
		"dashboard.yaml",
		"platform-issuer.yaml",
		"migration-job.yaml",
		"components-platform-postgres.yaml", // static set still written
	} {
		if _, ok := n.files[path.Join(base, name)]; !ok {
			t.Errorf("expected %s to be deployed", name)
		}
	}
	// The optional receiver Ingress is the one conditional file: absent here
	// (no ReceiverHost), present in TestApplyWritesReceiverIngressWhenHostSet.
	if _, ok := n.files[path.Join(base, "receiver-ingress.yaml")]; ok {
		t.Error("receiver Ingress should not deploy without a receiver host")
	}
}

func TestApplyWritesReceiverIngressWhenHostSet(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0", ReceiverHost: "hooks.example.com"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ing, ok := n.files[path.Join(DefaultAutoDeployDir, manifestSubdir, "receiver-ingress.yaml")]
	if !ok {
		t.Fatal("receiver Ingress was not deployed with a receiver host")
	}
	if !strings.Contains(ing, "host: hooks.example.com") {
		t.Errorf("deployed receiver Ingress missing the host:\n%s", ing)
	}
}
