package install

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/orkanoio/orkano/config"
	"github.com/orkanoio/orkano/internal/platformsecrets"
	"github.com/orkanoio/orkano/internal/repoallowlist"
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

// bootstrapIdentity is the shared name of the chart-only seeders'
// ServiceAccount, Role and RoleBinding, and of the Secret seeder Job.
const bootstrapIdentity = "orkano-bootstrap"

// chartTemplateComment matches a {{- /* ... */ -}} block so a header comment
// does not make the document carrying it look templated.
var chartTemplateComment = regexp.MustCompile(`(?s)\{\{-? */\*.*?\*/ *-?\}\}`)

// TestChartBootstrapJobIsChartOnly pins the one chart-only workload. `helm
// template` under make verify-chart only proves it parses; the content that
// matters is the Role, whose create-only grant is the whole least-privilege
// argument (a silently added get/update/patch would make generate-once a
// convention again instead of something the grant makes unrepresentable).
func TestChartBootstrapJobIsChartOnly(t *testing.T) {
	const rel = "templates/bootstrap-job.yaml"
	raw, err := os.ReadFile(filepath.Join(chartRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if strings.Contains(string(raw), chartComponentSource) {
		t.Errorf("%s carries the components/ Source header string; the golden compare matches it against whole documents and would enroll this chart-only file", rel)
	}
	if _, err := os.Stat(filepath.Join(chartRoot, "templates/components/bootstrap-job.yaml")); err == nil {
		t.Error("the bootstrap Job must not live under templates/components/, which is strictly the renderComponents mirror set")
	}

	// The Job document is not parseable YAML on its own (`image: {{ ... }}`
	// opens a flow mapping), so its two pins are string checks. The args value
	// shares one exported const with the operator's dispatch: a typo there would
	// fall through and start the controller manager under this Job's SA instead.
	// The serviceAccountName is the link that makes the create-only Role below
	// the Job's ACTUAL reach; pointed at orkano-operator instead, the Job would
	// silently inherit secret reads, registry deployment writes and job creates.
	for _, want := range []string{
		`args: ["` + platformsecrets.SeedSubcommand + `"]`,
		"serviceAccountName: " + bootstrapIdentity,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s should carry %s", rel, want)
		}
	}
	if strings.Contains(string(raw), "ORKANO_REPO_ALLOWLIST") {
		t.Errorf("%s must not carry repository seed state; changing it would make helm upgrade patch this fixed-name Job's immutable pod template", rel)
	}

	// The leading {{- /* ... */ -}} header would otherwise make the whole first
	// document (the ServiceAccount) look templated and skip it.
	docs := strings.Split(chartTemplateComment.ReplaceAllString(string(raw), ""), "\n---\n")
	var (
		sa      bool
		role    *rbacv1.Role
		binding *rbacv1.RoleBinding
	)
	for _, doc := range docs {
		if strings.Contains(doc, "{{") {
			continue // templated: not parseable without a render
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("parse a %s document: %v", rel, err)
		}
		switch meta.Kind {
		case "ServiceAccount":
			var got struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
				t.Fatalf("parse the bootstrap ServiceAccount: %v", err)
			}
			sa = got.Metadata.Name == bootstrapIdentity && got.Metadata.Namespace == platformsecrets.Namespace
		case "Role":
			role = &rbacv1.Role{}
			if err := yaml.Unmarshal([]byte(doc), role); err != nil {
				t.Fatalf("parse the bootstrap Role: %v", err)
			}
		case "RoleBinding":
			binding = &rbacv1.RoleBinding{}
			if err := yaml.Unmarshal([]byte(doc), binding); err != nil {
				t.Fatalf("parse the bootstrap RoleBinding: %v", err)
			}
		}
	}

	if !sa {
		t.Errorf("%s carries no ServiceAccount %s/%s", rel, platformsecrets.Namespace, bootstrapIdentity)
	}
	if role == nil {
		t.Fatalf("%s carries no Role", rel)
	}
	if role.Name != bootstrapIdentity || role.Namespace != platformsecrets.Namespace {
		t.Errorf("bootstrap Role is %s/%s, want %s/%s", role.Namespace, role.Name, platformsecrets.Namespace, bootstrapIdentity)
	}
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}},
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Errorf("bootstrap Role grants %+v, want exactly %+v — create-plus-AlreadyExists IS generate-once; any other verb lets a helm upgrade rotate a live credential or reset the repository allowlist", role.Rules, want)
	}

	// The third link: without it the Role above binds to nobody and the Job runs
	// on whatever the bound identity happens to hold.
	if binding == nil {
		t.Fatalf("%s carries no RoleBinding", rel)
	}
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: bootstrapIdentity}
	if !reflect.DeepEqual(binding.RoleRef, wantRef) {
		t.Errorf("bootstrap RoleBinding points at %+v, want %+v", binding.RoleRef, wantRef)
	}
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: bootstrapIdentity, Namespace: platformsecrets.Namespace}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Errorf("bootstrap RoleBinding binds %+v, want exactly %+v", binding.Subjects, wantSubjects)
	}
}

// TestChartRepoAllowlistBootstrapJobIsUpgradeSafe pins the repository seeder's
// independent subcommand and the inputs to its name fingerprint. A fixed name,
// or a fingerprint that omits one mutable pod-template input, makes a Helm
// upgrade fail while the previous Job still exists because Job templates are
// immutable.
func TestChartRepoAllowlistBootstrapJobIsUpgradeSafe(t *testing.T) {
	const rel = "templates/repo-allowlist-bootstrap-job.yaml"
	raw, err := os.ReadFile(filepath.Join(chartRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	text := string(raw)
	if strings.Contains(text, chartComponentSource) {
		t.Errorf("%s carries the components/ Source header string; the golden compare would enroll this chart-only file", rel)
	}
	if _, err := os.Stat(filepath.Join(chartRoot, "templates/components/repo-allowlist-bootstrap-job.yaml")); err == nil {
		t.Error("the repository allowlist bootstrap Job must stay outside templates/components/, which is strictly the renderComponents mirror set")
	}

	for _, want := range []string{
		`args: ["` + repoallowlist.SeedSubcommand + `"]`,
		"serviceAccountName: " + bootstrapIdentity,
		"name: ORKANO_REPO_ALLOWLIST",
		".Chart.Version",
		".Values.images.repository",
		`include "orkano.imageTag" .`,
		".Values.repoAllowlist",
		"sha256sum",
		"trunc 12",
		"name: orkano-repo-allowlist-bootstrap-{{ $fingerprint }}",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s should carry %q", rel, want)
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
