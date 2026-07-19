package install

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/orkanoio/orkano/internal/features"
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
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	hostRe      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	templateExt = ".yaml.tmpl"
)

// receiverIngressTemplate is rendered only when a receiver host is configured;
// without one the receiver stays ClusterIP-only and the file is skipped.
const receiverIngressTemplate = "receiver-ingress.yaml.tmpl"

// templateData feeds the component templates.
type templateData struct {
	OperatorImage    string
	ReceiverImage    string
	DashboardImage   string
	ACMEServer       string
	ACMEEmail        string
	RepoAllowlist    string // comma-joined owner/repo list
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
	allowlist, err := joinAllowlist(cfg.RepoAllowlist)
	if err != nil {
		return nil, err
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
		RepoAllowlist:    allowlist,
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
		files = append(files, manifestFile{
			name:    strings.TrimSuffix(e.Name(), ".tmpl"),
			content: buf.Bytes(),
		})
	}
	return files, nil
}

func acmeServer(prod bool) string {
	if prod {
		return acmeProdServer
	}
	return acmeStagingServer
}

// joinAllowlist validates each owner/repo entry (it lands in a YAML scalar) and
// joins them with commas for ORKANO_REPO_ALLOWLIST.
func joinAllowlist(repos []string) (string, error) {
	for _, r := range repos {
		if !repoNameRe.MatchString(r) {
			return "", fmt.Errorf("install: invalid repo %q (want owner/name)", r)
		}
	}
	return strings.Join(repos, ","), nil
}
