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
	for _, name := range []string{"operator-deployment.yaml", "receiver.yaml", "platform-issuer.yaml", "migration-job.yaml"} {
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

func TestRenderComponentsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"bad version", Config{Version: "1.0.0 ; rm -rf /"}},
		{"bad email", Config{Version: "1.0.0", ACMEEmail: "not-an-email\ninjected: true"}},
		{"bad repo", Config{Version: "1.0.0", RepoAllowlist: []string{`bad"repo`}}},
		{"repo without slash", Config{Version: "1.0.0", RepoAllowlist: []string{"noslash"}}},
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
		"platform-issuer.yaml",
		"migration-job.yaml",
		"components-platform-postgres.yaml", // static set still written
	} {
		if _, ok := n.files[path.Join(base, name)]; !ok {
			t.Errorf("expected %s to be deployed", name)
		}
	}
}
