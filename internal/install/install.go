// Package install deploys Orkano's platform components onto a freshly
// bootstrapped k3s cluster from `orkano init`. It writes the embedded manifest
// set into k3s's auto-deploy directory over the same SSH connection the
// bootstrap used — k3s then applies and continuously reconciles them — and
// waits for the critical path to come up before returning.
//
// Deploying through the auto-deploy directory (rather than client-go from the
// control host) is deliberate: it is how k3s ships its own add-ons, it needs no
// API-server reachability from the control host (only SSH), it self-heals if a
// manifest is removed, and it survives a reboot. The same base64|tee write the
// k3s bootstrap uses keeps the payload injection-safe.
package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/orkanoio/orkano/config"
	"github.com/orkanoio/orkano/internal/ssh"
)

const (
	// DefaultAutoDeployDir is k3s's server manifests directory: anything written
	// here is applied and reconciled by k3s's deploy controller.
	DefaultAutoDeployDir = "/var/lib/rancher/k3s/server/manifests"

	// manifestSubdir groups Orkano's manifests under the auto-deploy directory so
	// they never collide with k3s's own add-ons (traefik, coredns, …).
	manifestSubdir = "orkano"

	// DefaultWaitTimeout bounds how long Apply waits for the critical path.
	DefaultWaitTimeout = 5 * time.Minute

	// k3sBin is referenced by absolute path: RHEL sudo's secure_path excludes
	// /usr/local/bin, so a bare `k3s` would be command-not-found under sudo.
	k3sBin = "/usr/local/bin/k3s"

	// manifestMode keeps the written manifests root-only on the server node.
	manifestMode = "0600"

	// crdManifestPrefix identifies the flattened config/crd/*.yaml files. CRDs
	// must be established before any controller-runtime manager starts watching
	// Orkano types; otherwise the operator can crash on REST mapping discovery.
	crdManifestPrefix = "crd-"
)

// Runner runs a command on the server node. *ssh.Client satisfies it; tests
// supply a fake. The reuse of ssh.Result mirrors internal/k3s and preflight.
type Runner interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
}

// Config parameterises an Apply.
type Config struct {
	// AutoDeployDir overrides DefaultAutoDeployDir (tests).
	AutoDeployDir string
	// Version is the CLI's own version; it tags the first-party operator and
	// receiver images so the deployed components match the installer. Empty
	// renders no component manifests (the static-only path tests exercise).
	Version string
	// ACMEEmail registers the orkano-platform ACME account (optional).
	ACMEEmail string
	// ACMEProd selects Let's Encrypt production; false uses staging (default).
	ACMEProd bool
	// RepoAllowlist seeds the receiver's ORKANO_REPO_ALLOWLIST (owner/name).
	RepoAllowlist []string
	// UnsafeFeatures is the explicit allowlist of security-sensitive source and
	// build capabilities enabled for this installation. Empty is fail-closed.
	// Unknown IDs are rejected before any manifests are written.
	UnsafeFeatures []string
	// ReceiverHost is the public hostname for the webhook receiver's Ingress.
	// Empty renders no Ingress — the receiver stays ClusterIP-only (INV-05).
	ReceiverHost string
	// SecretsVault adds the vendored External Secrets Operator to the write
	// set (`orkano init --secrets-vault`, ADR-0018). Opt-in and one-way from
	// the installer's side: a later re-run without the flag leaves the file in
	// place — k3s re-applies auto-deploy manifests on restart but never
	// deletes resources when a file disappears, so disabling is a deliberate
	// two-step manual operation (remove the file, then delete the resources —
	// which cascades to every synced Secret), never a forgotten flag.
	// docs/vault.md documents the procedure.
	SecretsVault bool
	// ReadinessTargets are the workloads Apply waits to become Ready before
	// returning. Empty skips the wait.
	ReadinessTargets []Workload
	// WaitTimeout overrides DefaultWaitTimeout when positive.
	WaitTimeout time.Duration
	// RestartReadyTimeout bounds the wait for a node's apiserver to come back
	// after a registries.yaml-triggered k3s restart; 0 uses the package default.
	// The CLI feeds it the same budget as the bootstrap ready wait (--ready-timeout).
	RestartReadyTimeout time.Duration
	// Sudo prefixes privileged commands with sudo (non-root SSH user).
	Sudo bool
	// Logf receives human-readable progress lines; nil discards them.
	Logf func(format string, args ...any)
}

// Workload identifies a Deployment or StatefulSet whose readiness Apply waits
// for, in the namespace the component-deploy created. Its fields land in a
// kubectl shell command, so they are validated before use (validateTargets).
type Workload struct {
	Namespace string
	Kind      string // "deployment" or "statefulset"
	Name      string
}

// workloadNameRe bounds namespace and name to DNS-name characters, and
// workloadKinds to the two kinds workloadReady can query — so a Workload can
// never inject into the kubectl command built in wait.go (defense in depth: the
// callers pass fixed Orkano constants).
var (
	workloadNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	workloadKinds  = map[string]bool{"deployment": true, "statefulset": true}
)

func validateTargets(targets []Workload) error {
	for _, w := range targets {
		if !workloadNameRe.MatchString(w.Namespace) || !workloadNameRe.MatchString(w.Name) {
			return fmt.Errorf("install: invalid readiness target namespace/name %q/%q", w.Namespace, w.Name)
		}
		if !workloadKinds[w.Kind] {
			return fmt.Errorf("install: unsupported readiness target kind %q (want deployment or statefulset)", w.Kind)
		}
	}
	return nil
}

// Result reports what Apply did.
type Result struct {
	// Changed is true when Apply wrote a manifest or created a Secret.
	Changed bool
	// BootstrapToken is the plaintext one-time install token, set only when this
	// run generated it (a first install). Empty on a re-run where it already
	// existed — only its hash is stored, so it can never be reprinted. The caller
	// prints it exactly once (ADR-0003).
	BootstrapToken string
}

// DefaultReadinessTargets is the critical path Apply waits for after a full
// component deploy: cert-manager (so the operator's Certificate watches and the
// registry TLS can issue), the registry, the platform Postgres, and the
// operator, receiver, and dashboard themselves.
func DefaultReadinessTargets() []Workload {
	return []Workload{
		{Namespace: "cert-manager", Kind: "deployment", Name: "cert-manager-webhook"},
		{Namespace: "cert-manager", Kind: "deployment", Name: "cert-manager"},
		{Namespace: "cert-manager", Kind: "deployment", Name: "cert-manager-cainjector"},
		{Namespace: "orkano-system", Kind: "statefulset", Name: "orkano-postgres"},
		{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-registry"},
		{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-operator"},
		{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-receiver"},
		{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-dashboard"},
	}
}

// SecretsVaultReadinessTargets is the additional critical path when the
// External Secrets Operator is opted in (ADR-0018): its controller, the
// validating webhook (without which every SecretStore/ExternalSecret write is
// rejected), and the cert-controller that issues the webhook's serving cert.
func SecretsVaultReadinessTargets() []Workload {
	return []Workload{
		{Namespace: "external-secrets", Kind: "deployment", Name: "external-secrets"},
		{Namespace: "external-secrets", Kind: "deployment", Name: "external-secrets-webhook"},
		{Namespace: "external-secrets", Kind: "deployment", Name: "external-secrets-cert-controller"},
	}
}

func (c Config) autoDeployDir() string {
	if c.AutoDeployDir == "" {
		return DefaultAutoDeployDir
	}
	return c.AutoDeployDir
}

func (c Config) waitTimeout() time.Duration {
	if c.WaitTimeout <= 0 {
		return DefaultWaitTimeout
	}
	return c.WaitTimeout
}

func (c Config) restartReadyTimeout() time.Duration {
	if c.RestartReadyTimeout <= 0 {
		return defaultRestartReadyTimeout
	}
	return c.RestartReadyTimeout
}

// Apply writes the static manifest set into the server node's auto-deploy
// directory (idempotently — only files whose contents differ are rewritten),
// then waits for the configured workloads to become Ready. It is safe to re-run.
func Apply(ctx context.Context, r Runner, cfg Config) (*Result, error) {
	if r == nil {
		return nil, errors.New("install: runner is required")
	}
	if err := validateTargets(cfg.ReadinessTargets); err != nil {
		return nil, err
	}
	n := newNode(r, cfg.Sudo, cfg.Logf)
	res := &Result{}

	files, err := staticManifests()
	if err != nil {
		return nil, err
	}
	// The vendored External Secrets Operator joins the write set only on
	// opt-in (ADR-0018). Its own CRDs are deliberately not routed through the
	// Established gate below: nothing Orkano runs watches ESO types at startup
	// (the dashboard's lazy RESTMapper self-heals), matching the cert-manager
	// precedent.
	if cfg.SecretsVault {
		eso, err := externalSecretsManifests()
		if err != nil {
			return nil, err
		}
		files = append(files, eso...)
	}
	// Per-install component manifests (version-tagged images, ACME/allowlist
	// values) join the static set; both are written and reconciled identically.
	comps, err := renderComponents(cfg)
	if err != nil {
		return nil, err
	}
	files = append(files, comps...)

	base := path.Join(cfg.autoDeployDir(), manifestSubdir)
	crds, rest, err := splitCRDManifests(base, files)
	if err != nil {
		return nil, err
	}

	for _, c := range crds {
		changed, err := n.ensureFile(ctx, c.path, c.content, manifestMode)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Changed = true
			n.logf("wrote %s", c.name)
		}
	}
	// Apply + wait runs on EVERY run, not only when a file changed: the gate is
	// about cluster state (Established), which a crashed earlier run or a still-
	// converging server can leave unmet behind unchanged files. Both commands are
	// idempotent no-ops against an already-Established CRD. The files also stay
	// in the auto-deploy dir, so k3s's AddOn controller independently applies the
	// same byte-identical content — a deliberate, harmless second path (wrangler
	// apply converges onto pre-existing objects rather than conflicting).
	if len(crds) > 0 {
		if err := n.applyAndWaitCRDs(ctx, crds, cfg.waitTimeout()); err != nil {
			return nil, err
		}
	}

	for _, f := range rest {
		changed, err := n.ensureFile(ctx, path.Join(base, f.name), f.content, manifestMode)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Changed = true
			n.logf("wrote %s", f.name)
		}
	}

	// The generated Secrets the component workloads reference are created
	// imperatively (into etcd, encrypted at rest), generate-once, after k3s has
	// created their namespace — never written to disk in the auto-deploy dir.
	// This is the component path; the static-only path (no Version) skips it.
	if cfg.Version != "" {
		if err := n.waitNamespace(ctx, systemNS, cfg.waitTimeout()); err != nil {
			return nil, err
		}
		// Fresh values are generated every run but used only for Secrets that do
		// not yet exist; ensureSecrets preserves any already present (generate-
		// once), so a re-run discards these in memory and never rotates a live
		// credential. The bootstrap token is returned only if it was just created.
		vals, err := generateSecretValues()
		if err != nil {
			return nil, err
		}
		token, secChanged, err := ensureSecrets(ctx, n, vals)
		res.BootstrapToken = token
		res.Changed = res.Changed || secChanged
		if err != nil {
			return res, err
		}
	}

	if len(cfg.ReadinessTargets) > 0 {
		if err := n.waitReady(ctx, cfg.ReadinessTargets, cfg.waitTimeout()); err != nil {
			return res, err
		}
	}
	return res, nil
}

// manifestFile is one manifest to write, named by its flattened path under
// config/ (slashes become dashes, e.g. "crd-orkano.io_apps.yaml"). Flattening
// keeps every file a unique name in one auto-deploy directory: k3s derives an
// AddOn's identity from the filename, so two same-basename files in different
// config/ subdirs would otherwise collide.
type manifestFile struct {
	name    string
	content []byte
}

// crdManifest is a manifestFile plus the applied path and API name. CRDs are
// treated specially because the operator cannot even create its manager until
// discovery knows these types.
type crdManifest struct {
	manifestFile
	path    string
	crdName string
}

func splitCRDManifests(base string, files []manifestFile) ([]crdManifest, []manifestFile, error) {
	var crds []crdManifest
	rest := make([]manifestFile, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f.name, crdManifestPrefix) {
			rest = append(rest, f)
			continue
		}
		name, err := crdName(f.content)
		if err != nil {
			return nil, nil, fmt.Errorf("install: parse %s: %w", f.name, err)
		}
		crds = append(crds, crdManifest{
			manifestFile: f,
			path:         path.Join(base, f.name),
			crdName:      name,
		})
	}
	return crds, rest, nil
}

func crdName(content []byte) (string, error) {
	// yaml.Unmarshal silently reads only the FIRST document of a multi-document
	// stream, but kubectl apply would apply them all — the Established wait would
	// then cover only the first CRD, silently reintroducing the operator-starts-
	// before-CRD race for the rest. Refuse the shape outright; config/crd/ files
	// are one CRD per file by construction (controller-gen output).
	if err := ensureSingleDocument(content); err != nil {
		return "", err
	}
	var doc struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return "", err
	}
	if doc.Kind != "CustomResourceDefinition" {
		return "", fmt.Errorf("kind %q is not CustomResourceDefinition", doc.Kind)
	}
	if doc.Metadata.Name == "" {
		return "", errors.New("metadata.name is empty")
	}
	return doc.Metadata.Name, nil
}

// ensureSingleDocument rejects a YAML stream holding more than one document. A
// leading `---` marker (controller-gen emits one) is fine; a separator after
// any content means a second document follows.
func ensureSingleDocument(content []byte) error {
	seenContent := false
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if seenContent {
				return errors.New("multi-document manifest; CRD files must hold exactly one CRD")
			}
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			seenContent = true
		}
	}
	return nil
}

// staticManifests walks the embedded config/ tree into a deterministic,
// sorted list so a re-run writes the same files in the same order.
func staticManifests() ([]manifestFile, error) {
	return manifestsFromFS(config.StaticManifests)
}

// externalSecretsManifests returns the vendored External Secrets Operator set
// (ADR-0018), embedded separately from StaticManifests so it can only ever be
// written on opt-in.
func externalSecretsManifests() ([]manifestFile, error) {
	return manifestsFromFS(config.ExternalSecretsManifest)
}

func manifestsFromFS(fsys fs.FS) ([]manifestFile, error) {
	var files []manifestFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".yaml" {
			return nil
		}
		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		files = append(files, manifestFile{name: strings.ReplaceAll(p, "/", "-"), content: content})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk embedded manifests: %w", err)
	}
	if len(files) == 0 {
		return nil, errors.New("install: no embedded manifests found")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}
