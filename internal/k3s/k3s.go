// Package k3s bootstraps a hardened, CIS-aligned k3s server onto a single node
// over an SSH transport. It is the engine behind `orkano init`: it writes the
// committed hardening templates (kernel parameters, audit policy, admission
// config, server config with embedded etcd + secrets encryption), runs the
// pinned k3s installer, waits for the node to become Ready, verifies that
// encryption and auditing are actually active, and returns a kubeconfig
// rewritten for remote access.
//
// Bootstrap is idempotent: a re-run converges the node to the desired state and
// does nothing when it is already there. It assumes the node has cleared the
// install preflight (see internal/preflight) — the CLI runs that gate first.
package k3s

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/orkanoio/orkano/internal/ssh"
)

//go:embed templates/90-kubelet.conf templates/audit.yaml templates/psa.yaml templates/config.yaml.tmpl
var templatesFS embed.FS

// DefaultK3sVersion is the pinned k3s release `orkano init` installs. It is a Go
// constant on purpose (Renovate does not bump it): bump it deliberately after
// confirming the release ships both amd64 and arm64. Current stable at the time
// of writing; the `latest` channel was v1.36.1+k3s1.
const DefaultK3sVersion = "v1.35.5+k3s1"

// DefaultReadyTimeout bounds how long Bootstrap waits for the node to report
// Ready after the installer returns.
const DefaultReadyTimeout = 5 * time.Minute

// Default embedded-etcd snapshot policy, mirroring k3s's own defaults: a
// snapshot every 12 hours, keeping the last five. These are written into the
// server config so the policy is explicit and version-controlled rather than
// implicit in the k3s release.
const (
	DefaultSnapshotCron      = "0 */12 * * *"
	DefaultSnapshotRetention = 5
)

const (
	installURL = "https://get.k3s.io"

	// k3sBin is referenced by absolute path on purpose: the installer drops k3s
	// in /usr/local/bin, which RHEL-family sudo secure_path excludes — a bare
	// `sudo k3s …` would fail there with command-not-found.
	k3sBin = "/usr/local/bin/k3s"

	pathConfig      = "/etc/rancher/k3s/config.yaml"
	pathServerDir   = "/var/lib/rancher/k3s/server"
	pathAudit       = pathServerDir + "/audit.yaml"
	pathPSA         = pathServerDir + "/psa.yaml"
	pathAuditLogDir = pathServerDir + "/logs"
	pathAuditLog    = pathAuditLogDir + "/audit.log"
	pathSysctl      = "/etc/sysctl.d/90-kubelet.conf"
	pathKubeconfig  = "/etc/rancher/k3s/k3s.yaml"

	// localServer is the placeholder address k3s writes into the generated
	// kubeconfig; Bootstrap rewrites it to the node's reachable address.
	localServer = "https://127.0.0.1:6443"
)

var (
	// versionRe validates a user-supplied k3s version before it is interpolated
	// into the installer shell command — an injection guard.
	versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+\+k3s\d+$`)
	// hostRe accepts a hostname or IPv4 literal for the TLS SAN and kubeconfig
	// server URL. IPv6 is deferred (it needs bracket handling throughout).
	hostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// cronRe guards the snapshot schedule before it is interpolated into a
	// double-quoted YAML scalar in config.yaml: it admits 5-field cron and the
	// @-shorthands but excludes the quote and newline that could break the line.
	cronRe = regexp.MustCompile(`^[A-Za-z0-9 @*/,-]+$`)
	// installedVersionRe pulls the version out of `k3s --version` output.
	installedVersionRe = regexp.MustCompile(`v\d+\.\d+\.\d+\+k3s\d+`)

	// Poll cadences, overridable in tests to keep them fast.
	readyPollInterval  = 5 * time.Second
	auditRetryInterval = 2 * time.Second
)

// Runner runs a command on the target node. *ssh.Client satisfies it; tests
// supply a fake. The deliberate reuse of ssh.Result mirrors internal/preflight.
type Runner interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
}

// Config parameterises a Bootstrap.
type Config struct {
	// NodeAddress is the node's reachable hostname or IPv4 address (no port). It
	// becomes a TLS SAN and the server URL in the returned kubeconfig. Required.
	NodeAddress string
	// ExtraTLSSANs are additional SANs to place on the server certificate (for
	// example a DNS name the cluster will also be reached by). Optional.
	ExtraTLSSANs []string
	// K3sVersion is the k3s release to install; DefaultK3sVersion when empty.
	K3sVersion string
	// SnapshotCron is the embedded-etcd snapshot schedule (cron syntax);
	// DefaultSnapshotCron when empty.
	SnapshotCron string
	// SnapshotRetention is how many scheduled snapshots to keep;
	// DefaultSnapshotRetention when not positive.
	SnapshotRetention int
	// Sudo prefixes privileged commands with sudo. Set it when the SSH user is
	// not root (the user must then have passwordless sudo).
	Sudo bool
	// ReadyTimeout overrides DefaultReadyTimeout when positive.
	ReadyTimeout time.Duration
	// Logf receives human-readable progress lines; nil discards them.
	Logf func(format string, args ...any)
}

func (c Config) version() string {
	if c.K3sVersion == "" {
		return DefaultK3sVersion
	}
	return c.K3sVersion
}

func (c Config) readyTimeout() time.Duration {
	if c.ReadyTimeout <= 0 {
		return DefaultReadyTimeout
	}
	return c.ReadyTimeout
}

func (c Config) snapshotCron() string {
	if c.SnapshotCron == "" {
		return DefaultSnapshotCron
	}
	return c.SnapshotCron
}

func (c Config) snapshotRetention() int {
	if c.SnapshotRetention <= 0 {
		return DefaultSnapshotRetention
	}
	return c.SnapshotRetention
}

func (c Config) tlsSANs() []string {
	sans := append([]string{c.NodeAddress}, c.ExtraTLSSANs...)
	return sans
}

// Result reports what Bootstrap found and did.
type Result struct {
	// AlreadyInstalled is true when k3s was present before this run.
	AlreadyInstalled bool
	// Changed is true when this run modified node state (wrote a file, installed
	// or upgraded k3s, or restarted the service).
	Changed bool
	// Version is the k3s version running after the run.
	Version string
	// SecretsEncryption is the verified at-rest secrets encryption status, as
	// reported by `k3s secrets-encrypt status` (expected "Enabled").
	SecretsEncryption string
	// AuditLogPresent is true when the API server audit log exists on the node.
	AuditLogPresent bool
	// Kubeconfig is the cluster kubeconfig with its server URL rewritten to
	// NodeAddress, ready to use from the control host.
	Kubeconfig []byte
}

// Bootstrap installs (or converges) a hardened k3s server on the node reachable
// through r and returns the resulting state. It is idempotent.
func Bootstrap(ctx context.Context, r Runner, cfg Config) (*Result, error) {
	if r == nil {
		return nil, errors.New("k3s: runner is required")
	}
	if !hostRe.MatchString(cfg.NodeAddress) {
		return nil, fmt.Errorf("k3s: invalid node address %q (hostname or IPv4 only)", cfg.NodeAddress)
	}
	for _, s := range cfg.ExtraTLSSANs {
		if !hostRe.MatchString(s) {
			return nil, fmt.Errorf("k3s: invalid TLS SAN %q", s)
		}
	}
	if !versionRe.MatchString(cfg.version()) {
		return nil, fmt.Errorf("k3s: invalid k3s version %q (want vX.Y.Z+k3sN)", cfg.version())
	}
	if !cronRe.MatchString(cfg.snapshotCron()) {
		return nil, fmt.Errorf("k3s: invalid snapshot schedule %q", cfg.snapshotCron())
	}

	b := &bootstrapper{r: r, cfg: cfg, res: &Result{}}
	if cfg.Sudo {
		b.sudo = "sudo "
	}

	if err := b.renderFiles(); err != nil {
		return nil, err
	}
	if err := b.detectInstalled(ctx); err != nil {
		return nil, err
	}
	if err := b.applyHostPrereqs(ctx); err != nil {
		return nil, err
	}
	if err := b.installOrConverge(ctx); err != nil {
		return nil, err
	}
	if err := b.waitReady(ctx); err != nil {
		return nil, err
	}
	if err := b.verify(ctx); err != nil {
		return nil, err
	}
	if err := b.fetchKubeconfig(ctx); err != nil {
		return nil, err
	}
	return b.res, nil
}

type bootstrapper struct {
	r    Runner
	cfg  Config
	sudo string
	res  *Result

	configYAML []byte
	auditYAML  []byte
	psaYAML    []byte
	sysctlConf []byte
}

func (b *bootstrapper) logf(format string, args ...any) {
	if b.cfg.Logf != nil {
		b.cfg.Logf(format, args...)
	}
}

// configData is the data model for config.yaml.tmpl.
type configData struct {
	TLSSANs           []string
	SnapshotCron      string
	SnapshotRetention int
}

// renderFiles materialises the four node files from the embedded templates.
func (b *bootstrapper) renderFiles() error {
	var err error
	if b.sysctlConf, err = templatesFS.ReadFile("templates/90-kubelet.conf"); err != nil {
		return fmt.Errorf("k3s: read sysctl template: %w", err)
	}
	if b.auditYAML, err = templatesFS.ReadFile("templates/audit.yaml"); err != nil {
		return fmt.Errorf("k3s: read audit template: %w", err)
	}
	if b.psaYAML, err = templatesFS.ReadFile("templates/psa.yaml"); err != nil {
		return fmt.Errorf("k3s: read psa template: %w", err)
	}

	raw, err := templatesFS.ReadFile("templates/config.yaml.tmpl")
	if err != nil {
		return fmt.Errorf("k3s: read config template: %w", err)
	}
	tmpl, err := template.New("config").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("k3s: parse config template: %w", err)
	}
	var buf bytes.Buffer
	data := configData{
		TLSSANs:           b.cfg.tlsSANs(),
		SnapshotCron:      b.cfg.snapshotCron(),
		SnapshotRetention: b.cfg.snapshotRetention(),
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("k3s: render config: %w", err)
	}
	b.configYAML = buf.Bytes()
	return nil
}

// detectInstalled records whether k3s is already present and at which version.
func (b *bootstrapper) detectInstalled(ctx context.Context) error {
	res, err := b.r.Run(ctx, k3sBin+" --version")
	if err != nil {
		return fmt.Errorf("k3s: probe installed version: %w", err)
	}
	if res.ExitStatus != 0 {
		return nil // not installed
	}
	b.res.AlreadyInstalled = true
	b.res.Version = installedVersionRe.FindString(res.Stdout)
	return nil
}

// applyHostPrereqs writes the kernel parameters, audit policy, and admission
// config, and creates the audit log directory — everything that must exist
// before the server starts. The sysctls are applied immediately because
// protect-kernel-defaults requires them active or the kubelet refuses to start.
func (b *bootstrapper) applyHostPrereqs(ctx context.Context) error {
	changed, err := b.ensureFile(ctx, pathSysctl, b.sysctlConf, "0644")
	if err != nil {
		return err
	}
	// Apply the sysctls now when they were (re)written or when this is a fresh
	// install; a no-op re-run leaves them alone.
	if changed || !b.res.AlreadyInstalled {
		if err := b.runOK(ctx, b.sudo+"sysctl -p "+pathSysctl, "apply kernel parameters"); err != nil {
			return err
		}
	}

	if err := b.runOK(ctx, b.sudo+"mkdir -p -m 700 "+pathAuditLogDir, "create audit log directory"); err != nil {
		return err
	}
	if _, err := b.ensureFile(ctx, pathAudit, b.auditYAML, "0600"); err != nil {
		return err
	}
	if _, err := b.ensureFile(ctx, pathPSA, b.psaYAML, "0600"); err != nil {
		return err
	}
	return nil
}

// installOrConverge installs k3s when absent, upgrades it when the version
// differs, and otherwise restarts the service only when a config file changed or
// the service is not running. config.yaml is written here (and counts toward the
// restart decision) because it must be in place before the installer runs.
func (b *bootstrapper) installOrConverge(ctx context.Context) error {
	configChanged, err := b.ensureFile(ctx, pathConfig, b.configYAML, "0600")
	if err != nil {
		return err
	}

	want := b.cfg.version()
	if !b.res.AlreadyInstalled || b.res.Version != want {
		if b.res.AlreadyInstalled {
			b.logf("upgrading k3s %s -> %s", b.res.Version, want)
		} else {
			b.logf("installing k3s %s", want)
		}
		// The installer self-elevates when the SSH user is not root, so it is not
		// prefixed with sudo. INSTALL_K3S_VERSION is validated against versionRe.
		cmd := fmt.Sprintf("curl -sfL %s | INSTALL_K3S_VERSION='%s' sh -s - server", installURL, want)
		if err := b.runOK(ctx, cmd, "install k3s"); err != nil {
			return err
		}
		b.res.Changed = true
		b.res.Version = want
		return nil
	}

	// Already at the desired version: apply any config change, else just ensure
	// the service is running.
	if configChanged {
		b.logf("configuration changed; restarting k3s")
		if err := b.runOK(ctx, b.sudo+"systemctl restart k3s", "restart k3s"); err != nil {
			return err
		}
		b.res.Changed = true
		return nil
	}

	res, err := b.r.Run(ctx, b.sudo+"systemctl is-active k3s")
	if err != nil {
		return fmt.Errorf("k3s: check service state: %w", err)
	}
	if strings.TrimSpace(res.Stdout) != "active" {
		b.logf("k3s service not active; starting")
		if err := b.runOK(ctx, b.sudo+"systemctl start k3s", "start k3s"); err != nil {
			return err
		}
		b.res.Changed = true
	}
	return nil
}

// waitReady polls until a node reports Ready or the ready timeout elapses.
func (b *bootstrapper) waitReady(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, b.cfg.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		res, err := b.r.Run(wait, b.sudo+k3sBin+" kubectl get nodes --no-headers")
		switch {
		case err != nil:
			// A transport error mid-install is expected (the API server is still
			// coming up); keep polling until the deadline.
			b.logf("waiting for node to be Ready: %v", err)
		case res.ExitStatus == 0 && nodeReady(res.Stdout):
			b.logf("node is Ready")
			return nil
		default:
			b.logf("waiting for node to be Ready")
		}

		select {
		case <-wait.Done():
			return fmt.Errorf("k3s: node did not become Ready within %s: %w", b.cfg.readyTimeout(), wait.Err())
		case <-ticker.C:
		}
	}
}

// verify confirms the two hardening guarantees `orkano init` promises: secrets
// encryption at rest and API server auditing are both active on the node.
func (b *bootstrapper) verify(ctx context.Context) error {
	res, err := b.r.Run(ctx, b.sudo+k3sBin+" secrets-encrypt status")
	if err != nil {
		return fmt.Errorf("k3s: check secrets encryption: %w", err)
	}
	if res.ExitStatus != 0 {
		return fmt.Errorf("k3s: secrets-encrypt status exited %d: %s", res.ExitStatus, firstLine(res.Stderr))
	}
	b.res.SecretsEncryption = encryptionStatus(res.Stdout)
	if !strings.EqualFold(b.res.SecretsEncryption, "Enabled") {
		return fmt.Errorf("k3s: secrets encryption is not enabled (status: %q)", b.res.SecretsEncryption)
	}

	// The audit log is created on the first audited request; by the time the node
	// is Ready the API server has logged plenty, but allow a brief grace window.
	for attempt := 0; attempt < 3; attempt++ {
		res, err := b.r.Run(ctx, b.sudo+"test -f "+pathAuditLog)
		if err != nil {
			return fmt.Errorf("k3s: check audit log: %w", err)
		}
		if res.ExitStatus == 0 {
			b.res.AuditLogPresent = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(auditRetryInterval):
		}
	}
	if !b.res.AuditLogPresent {
		return fmt.Errorf("k3s: audit log %s not found; auditing may not be active", pathAuditLog)
	}
	return nil
}

// fetchKubeconfig reads the generated kubeconfig and rewrites its loopback
// server URL to the node's reachable address.
func (b *bootstrapper) fetchKubeconfig(ctx context.Context) error {
	res, err := b.r.Run(ctx, b.sudo+"cat "+pathKubeconfig)
	if err != nil {
		return fmt.Errorf("k3s: read kubeconfig: %w", err)
	}
	if res.ExitStatus != 0 {
		return fmt.Errorf("k3s: read kubeconfig exited %d: %s", res.ExitStatus, firstLine(res.Stderr))
	}
	remote := fmt.Sprintf("https://%s:6443", b.cfg.NodeAddress)
	kc := strings.ReplaceAll(res.Stdout, localServer, remote)
	if !strings.Contains(kc, remote) {
		return fmt.Errorf("k3s: kubeconfig did not contain the expected server URL %q to rewrite", localServer)
	}
	b.res.Kubeconfig = []byte(kc)
	return nil
}

// ensureFile writes content to path with mode only when the node's current
// contents differ, reporting whether it wrote. Parent directories are created.
func (b *bootstrapper) ensureFile(ctx context.Context, path string, content []byte, mode string) (bool, error) {
	cur, err := b.r.Run(ctx, b.sudo+"cat "+path)
	if err != nil {
		return false, fmt.Errorf("k3s: read %s: %w", path, err)
	}
	if cur.ExitStatus == 0 && cur.Stdout == string(content) {
		return false, nil
	}

	dir := path[:strings.LastIndex(path, "/")]
	enc := base64.StdEncoding.EncodeToString(content)
	// base64's alphabet contains no shell metacharacters, so the single-quoted
	// payload cannot break out of the command.
	cmd := fmt.Sprintf("%smkdir -p %s && printf %%s '%s' | base64 -d | %stee %s >/dev/null && %schmod %s %s",
		b.sudo, dir, enc, b.sudo, path, b.sudo, mode, path)
	if err := b.runOK(ctx, cmd, "write "+path); err != nil {
		return false, err
	}
	b.logf("wrote %s", path)
	b.res.Changed = true
	return true, nil
}

// runOK runs cmd and turns a transport error or a non-zero exit into a Go error;
// it is for commands that must succeed (writes, installs, restarts).
func (b *bootstrapper) runOK(ctx context.Context, cmd, desc string) error {
	res, err := b.r.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("k3s: %s: %w", desc, err)
	}
	if res.ExitStatus != 0 {
		return fmt.Errorf("k3s: %s exited %d: %s", desc, res.ExitStatus, firstLine(res.Stderr))
	}
	return nil
}

// nodeReady reports whether any line of `kubectl get nodes --no-headers` output
// shows a node whose STATUS column is exactly "Ready" (not "NotReady").
func nodeReady(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "Ready" {
			return true
		}
	}
	return false
}

// encryptionStatus extracts the value after "Encryption Status:" from
// `k3s secrets-encrypt status` output.
func encryptionStatus(out string) string {
	const key = "Encryption Status:"
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			return strings.TrimSpace(line[i+len(key):])
		}
	}
	return ""
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
