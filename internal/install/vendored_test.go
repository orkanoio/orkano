package install

import (
	"regexp"
	"strings"
	"testing"

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
