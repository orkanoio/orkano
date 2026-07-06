package install

import (
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/orkanoio/orkano/config"
)

// TestVendoredCertManagerStaysPinned guards the two local modifications to the
// vendored upstream cert-manager: every jetstack image is digest-pinned (no tag
// can sneak back in on a bump) and the namespace carries an explicit restricted
// PSA label (init enforces restricted cluster-wide).
func TestVendoredCertManagerStaysPinned(t *testing.T) {
	raw, err := config.StaticManifests.ReadFile("cert-manager/cert-manager.yaml")
	if err != nil {
		t.Fatalf("read vendored cert-manager: %v", err)
	}
	manifest := string(raw)

	// Every jetstack image reference must be by digest, never by tag.
	jetstackRef := regexp.MustCompile(`quay\.io/jetstack/[a-z-]+(@sha256:[0-9a-f]{64}|:[^"'\s]+)`)
	refs := jetstackRef.FindAllString(manifest, -1)
	if len(refs) < 4 {
		t.Fatalf("expected at least 4 jetstack image references, found %d", len(refs))
	}
	for _, ref := range refs {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("cert-manager image %q is not digest-pinned", ref)
		}
	}

	if !strings.Contains(manifest, "pod-security.kubernetes.io/enforce: restricted") {
		t.Error("cert-manager namespace must carry the restricted PSA label")
	}
}

// TestVendoredExternalSecretsStaysScoped guards the load-bearing properties
// of the vendored ESO render (ADR-0018): the scoping that keeps ESO's Secret
// reach inside orkano-apps, the digest pin, the restricted-PSA namespace, and
// the absence of the cluster-scoped kinds. A version bump that re-renders
// without hack/vendor-external-secrets.sh's values or patches fails here.
func TestVendoredExternalSecretsStaysScoped(t *testing.T) {
	raw, err := config.ExternalSecretsManifest.ReadFile("external-secrets/external-secrets.yaml")
	if err != nil {
		t.Fatalf("read vendored external-secrets: %v", err)
	}
	manifest := string(raw)

	// Every image reference must be digest-pinned, never by tag alone.
	esoRef := regexp.MustCompile(`image: ghcr\.io/external-secrets/[a-z-]+:[^\s@]+(@sha256:[0-9a-f]{64})?`)
	refs := esoRef.FindAllString(manifest, -1)
	if len(refs) != 3 {
		t.Fatalf("expected 3 external-secrets image references, found %d", len(refs))
	}
	for _, ref := range refs {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("external-secrets image %q is not digest-pinned", ref)
		}
	}

	if !strings.Contains(manifest, "pod-security.kubernetes.io/enforce: restricted") {
		t.Error("external-secrets namespace must carry the restricted PSA label")
	}

	// The controller must be confined to orkano-apps: cache scoping via
	// --namespace, cluster-scoped reconcilers off, and its RBAC a namespaced
	// Role there.
	for _, want := range []string{
		"- --namespace=orkano-apps",
		"- --enable-cluster-store-reconciler=false",
		"- --enable-cluster-external-secret-reconciler=false",
		"namespace: \"orkano-apps\"",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("vendored external-secrets missing %q", want)
		}
	}

	// No ClusterRole may grant any secrets access (the cert-controller's is
	// patched down to a name-pinned Role in external-secrets), and the
	// blanket serviceaccounts/token grant must stay off.
	for _, doc := range strings.Split(manifest, "\n---\n") {
		if !strings.Contains("\n"+doc+"\n", "\nkind: ClusterRole\n") {
			continue
		}
		if strings.Contains(doc, `"secrets"`) {
			t.Error("a ClusterRole in the vendored external-secrets render grants secrets")
		}
	}
	if strings.Contains(manifest, "serviceaccounts/token") {
		t.Error("vendored external-secrets must not grant serviceaccounts/token")
	}
	if !strings.Contains(manifest, "name: external-secrets-cert-controller-webhook-secret") ||
		!strings.Contains(manifest, `- "external-secrets-webhook"`) {
		t.Error("cert-controller webhook-secret Role (name-pinned) missing")
	}

	// The cluster-scoped kinds and PushSecret stay out (ADR-0018 3+6); the
	// two kinds the dashboard writes must be present.
	for _, banned := range []string{
		"name: clustersecretstores.external-secrets.io",
		"name: clusterexternalsecrets.external-secrets.io",
		"name: pushsecrets.external-secrets.io",
		"name: clusterpushsecrets.external-secrets.io",
	} {
		if strings.Contains(manifest, banned) {
			t.Errorf("vendored external-secrets must not ship %q", banned)
		}
	}
	for _, required := range []string{
		"name: secretstores.external-secrets.io",
		"name: externalsecrets.external-secrets.io",
	} {
		if !strings.Contains(manifest, required) {
			t.Errorf("vendored external-secrets missing CRD %q", required)
		}
	}
}

// TestVendoredExternalSecretsParses decodes every document so a bad render
// (a stray patch anchor, truncated write) cannot land as "valid YAML that
// kubectl rejects".
func TestVendoredExternalSecretsParses(t *testing.T) {
	raw, err := config.ExternalSecretsManifest.ReadFile("external-secrets/external-secrets.yaml")
	if err != nil {
		t.Fatalf("read vendored external-secrets: %v", err)
	}
	docs := 0
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("document %d does not parse: %v", docs, err)
		}
		if obj.Kind == "" || obj.APIVersion == "" || obj.Metadata.Name == "" {
			t.Errorf("document %d missing kind/apiVersion/name (kind=%q name=%q)", docs, obj.Kind, obj.Metadata.Name)
		}
		docs++
	}
	if docs < 30 {
		t.Fatalf("expected at least 30 documents, found %d", docs)
	}
}

// TestVendoredTraefikRedirect confirms the bundled-Traefik HTTP→HTTPS redirect
// is present and targets the websecure entrypoint (ADR-0006).
func TestVendoredTraefikRedirect(t *testing.T) {
	raw, err := config.StaticManifests.ReadFile("traefik/traefik-redirect.yaml")
	if err != nil {
		t.Fatalf("read traefik redirect: %v", err)
	}
	manifest := string(raw)
	for _, want := range []string{"kind: HelmChartConfig", "to: websecure", "scheme: https"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("traefik redirect missing %q", want)
		}
	}
}
