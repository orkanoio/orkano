package install

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/config"
)

// The chart mirrors the embedded manifest set as verbatim copies (ADR-0019
// decision 1) — these tests are the drift guard that keeps the two install
// paths one artifact set. go:embed cannot reach ../../charts, so the chart
// side reads from disk (the nodeprep TestEmbeddedProfileMatchesConfig
// pattern).
const chartRoot = "../../charts/orkano"

// chartStaticSources maps every chart manifest file (relative to
// charts/orkano/) to its path inside config.StaticManifests. Adding a file to
// either side without updating this map fails the coverage test below — a new
// config/ manifest must be a deliberate chart decision, never a silent gap.
// The k3s-only traefik redirect is the one deliberate exclusion; the ESO set
// lives in its own embed FS and is paired separately.
var chartStaticSources = map[string]string{
	"crds/orkano.io_apps.yaml":       "crd/orkano.io_apps.yaml",
	"crds/orkano.io_builds.yaml":     "crd/orkano.io_builds.yaml",
	"crds/orkano.io_domains.yaml":    "crd/orkano.io_domains.yaml",
	"crds/orkano.io_mongoes.yaml":    "crd/orkano.io_mongoes.yaml",
	"crds/orkano.io_postgreses.yaml": "crd/orkano.io_postgreses.yaml",

	"static/namespaces/namespaces.yaml": "namespaces/namespaces.yaml",

	"static/rbac/dashboard-impersonate.yaml": "rbac/dashboard-impersonate.yaml",
	"static/rbac/dashboard.yaml":             "rbac/dashboard.yaml",
	"static/rbac/human-roles.yaml":           "rbac/human-roles.yaml",
	"static/rbac/operator.yaml":              "rbac/operator.yaml",
	"static/rbac/serviceaccounts.yaml":       "rbac/serviceaccounts.yaml",

	"static/netpol/orkano-builds.yaml":   "netpol/orkano-builds.yaml",
	"static/netpol/orkano-receiver.yaml": "netpol/orkano-receiver.yaml",
	"static/netpol/orkano-system.yaml":   "netpol/orkano-system.yaml",

	"static/registry/internal-ca.yaml": "registry/internal-ca.yaml",
	"static/registry/registry.yaml":    "registry/registry.yaml",

	"static/buildkit/buildkitd-config.yaml": "buildkit/buildkitd-config.yaml",

	"static/components/platform-postgres.yaml": "components/platform-postgres.yaml",

	"static/cert-manager/cert-manager.yaml": "cert-manager/cert-manager.yaml",
}

const (
	chartESOFile   = "static/external-secrets/external-secrets.yaml"
	embedESOFile   = "external-secrets/external-secrets.yaml"
	excludedPrefix = "traefik/"
)

func TestChartMirrorsEmbeddedManifests(t *testing.T) {
	for chartPath, embedPath := range chartStaticSources {
		chartRaw, err := os.ReadFile(filepath.Join(chartRoot, chartPath))
		if err != nil {
			t.Errorf("read chart file %s: %v", chartPath, err)
			continue
		}
		embedRaw, err := config.StaticManifests.ReadFile(embedPath)
		if err != nil {
			t.Errorf("read embedded manifest %s: %v", embedPath, err)
			continue
		}
		if !bytes.Equal(chartRaw, embedRaw) {
			t.Errorf("%s drifted from config/%s — the chart carries verbatim copies; edit both sides together", chartPath, embedPath)
		}
	}

	chartESO, err := os.ReadFile(filepath.Join(chartRoot, chartESOFile))
	if err != nil {
		t.Fatalf("read chart ESO manifest: %v", err)
	}
	embedESO, err := config.ExternalSecretsManifest.ReadFile(embedESOFile)
	if err != nil {
		t.Fatalf("read embedded ESO manifest: %v", err)
	}
	if !bytes.Equal(chartESO, embedESO) {
		t.Errorf("%s drifted from config/%s", chartESOFile, embedESOFile)
	}
}

// TestChartCoversEveryEmbeddedManifest fails when a manifest exists on one
// side without a pairing decision on the other: a new embedded file must be
// added to the chart (or excluded here with a reason), and a chart manifest
// must trace back to an embedded source.
func TestChartCoversEveryEmbeddedManifest(t *testing.T) {
	paired := make(map[string]bool, len(chartStaticSources))
	for _, embedPath := range chartStaticSources {
		paired[embedPath] = true
	}

	err := fs.WalkDir(config.StaticManifests, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		if strings.HasPrefix(p, excludedPrefix) {
			return nil // k3s-only Traefik redirect, deliberately not in the chart
		}
		if !paired[p] {
			t.Errorf("embedded manifest %s has no chart counterpart — add it to charts/orkano + chartStaticSources, or exclude it here with a reason", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded manifests: %v", err)
	}

	for _, dir := range []string{"crds", "static"} {
		root := filepath.Join(chartRoot, dir)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(chartRoot, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if _, ok := chartStaticSources[rel]; !ok && rel != chartESOFile {
				t.Errorf("chart file %s has no embedded source in chartStaticSources — chart manifests must be verbatim copies of config/", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk chart %s: %v", dir, err)
		}
	}
}

// TestChartTemplatesLoadEveryStaticDir guards the loader side: a static/
// subdirectory that no template references would package files Helm never
// renders — copied but silently not installed.
func TestChartTemplatesLoadEveryStaticDir(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(chartRoot, "templates"))
	if err != nil {
		t.Fatalf("read chart templates dir: %v", err)
	}
	// Only .Files loader lines count — a dir named in a template comment must
	// not satisfy the guard.
	var loaders strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(chartRoot, "templates", e.Name()))
		if err != nil {
			t.Fatalf("read template %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, ".Files.") {
				loaders.WriteString(line)
				loaders.WriteString("\n")
			}
		}
	}
	text := loaders.String()

	staticDirs, err := os.ReadDir(filepath.Join(chartRoot, "static"))
	if err != nil {
		t.Fatalf("read chart static dir: %v", err)
	}
	for _, dir := range staticDirs {
		if !dir.IsDir() {
			continue
		}
		if !strings.Contains(text, dir.Name()) {
			t.Errorf("static/%s is packaged but no template references it — its manifests would never render", dir.Name())
		}
	}
}
