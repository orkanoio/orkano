package install

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/orkanoio/orkano/internal/features"
	"github.com/orkanoio/orkano/internal/repoallowlist"
)

//go:embed templates/*.yaml.tmpl
var componentTemplates embed.FS

// imageRepo is the registry namespace the first-party component images live in.
// Third-party images stay digest-pinned in the static manifests; the first-party
// operator and receiver images are tagged with the CLI's own version, so a
// given orkano CLI deploys the matching component version (and a release builds
// the binary and these images together — there is no digest to pin yet).
const imageRepo = "ghcr.io/orkanoio"

const (
	acmeStagingServer = "https://acme-staging-v02.api.letsencrypt.org/directory"
	acmeProdServer    = "https://acme-v02.api.letsencrypt.org/directory"
)

// These bound the values that land in a rendered manifest (an image tag, an
// email address in a YAML scalar, repo names in a comma-joined scalar) so a
// template value can never break the YAML or inject into it.
var (
	versionRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	emailRe     = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)
	hostRe      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	templateExt = ".yaml.tmpl"
)

// receiverIngressTemplate is rendered only when a receiver host is configured;
// without one the receiver stays ClusterIP-only and the file is skipped.
const receiverIngressTemplate = "receiver-ingress.yaml.tmpl"

// componentPrefix namespaces the rendered per-install manifests into the same
// flat AddOn-name space the embedded config/ tree uses, so a rendered file and
// an embedded one can never claim the same k3s AddOn identity. It matches
// config/components/, whose only member (platform-postgres) shares the space.
const componentPrefix = "components"

// templateData feeds the component templates.
type templateData struct {
	OperatorImage    string
	ReceiverImage    string
	DashboardImage   string
	ACMEServer       string
	ACMEEmail        string
	UnsafeFeatures   string // canonical, comma-joined explicit unsafe-feature IDs
	SourceZipEnabled bool   // pod label that opens dashboard-to-registry ingress
	ReceiverHost     string // public hostname for the receiver Ingress (may be empty)
}

// renderComponents renders the per-install component manifests (operator,
// receiver, and dashboard Deployments, the orkano-platform ACME ClusterIssuer,
// and the migration Job). It returns nil when cfg.Version is empty — the
// component images are version-tagged, so there is nothing to render without a
// version (the static-manifest-only path the engine-core tests exercise).
func renderComponents(cfg Config) ([]manifestFile, error) {
	enabledUnsafe, err := features.Parse(cfg.UnsafeFeatures)
	if err != nil {
		return nil, fmt.Errorf("install: unsafe features: %w", err)
	}
	if cfg.Version == "" {
		return nil, nil
	}
	if !versionRe.MatchString(cfg.Version) {
		return nil, fmt.Errorf("install: invalid version %q for component image tags", cfg.Version)
	}
	if _, err := repoallowlist.Normalize(cfg.RepoAllowlist); err != nil {
		return nil, fmt.Errorf("install: repository allowlist: %w", err)
	}
	if cfg.ACMEEmail != "" && !emailRe.MatchString(cfg.ACMEEmail) {
		return nil, fmt.Errorf("install: invalid ACME email %q", cfg.ACMEEmail)
	}
	if cfg.ReceiverHost != "" && !hostRe.MatchString(cfg.ReceiverHost) {
		return nil, fmt.Errorf("install: invalid receiver host %q", cfg.ReceiverHost)
	}

	data := templateData{
		OperatorImage:    imageRepo + "/orkano-operator:" + cfg.Version,
		ReceiverImage:    imageRepo + "/orkano-receiver:" + cfg.Version,
		DashboardImage:   imageRepo + "/orkano-dashboard:" + cfg.Version,
		ACMEServer:       acmeServer(cfg.ACMEProd),
		ACMEEmail:        cfg.ACMEEmail,
		UnsafeFeatures:   enabledUnsafe.CSV(),
		SourceZipEnabled: enabledUnsafe.Enabled(features.SourceZip),
		ReceiverHost:     cfg.ReceiverHost,
	}

	entries, err := componentTemplates.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("read component templates: %w", err)
	}
	var files []manifestFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), templateExt) {
			continue
		}
		// The receiver Ingress is optional: rendering it with an empty host would
		// emit an invalid Ingress, so it is skipped entirely without --receiver-host.
		if e.Name() == receiverIngressTemplate && cfg.ReceiverHost == "" {
			continue
		}
		raw, err := componentTemplates.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read template %s: %w", e.Name(), err)
		}
		// missingkey=error turns a typo'd field into a render error rather than a
		// silent "<no value>" in a manifest.
		tmpl, err := template.New(e.Name()).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", e.Name(), err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render template %s: %w", e.Name(), err)
		}
		// The rendered name carries the same directory prefix the embedded set
		// gets. k3s derives an AddOn's identity from the basename alone, so a
		// bare "dashboard.yaml" would claim the global AddOn name "dashboard"
		// and fight any same-named file elsewhere under the manifests tree.
		bare := strings.TrimSuffix(e.Name(), ".tmpl")
		name, err := manifestFilename(componentPrefix + "/" + bare)
		if err != nil {
			return nil, err
		}
		files = append(files, manifestFile{name: name, legacyName: bare, content: buf.Bytes()})
	}
	return files, nil
}

func acmeServer(prod bool) string {
	if prod {
		return acmeProdServer
	}
	return acmeStagingServer
}
