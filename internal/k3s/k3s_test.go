package k3s

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/ssh"
)

func init() {
	// Keep the poll/retry waits negligible in unit tests.
	readyPollInterval = time.Millisecond
	auditRetryInterval = time.Millisecond
}

// fakeNode models a Linux node responding to the shell commands Bootstrap runs.
// It records every command, stores files written via the base64|tee idiom, and
// returns scripted results for the rest.
type fakeNode struct {
	files     map[string]string
	installed bool
	version   string
	cmds      []string

	// scriptable behaviour
	encryptionStatus string // value reported by `secrets-encrypt status`
	encryptExit      int    // non-zero makes `secrets-encrypt status` fail
	auditLogPresent  bool
	serviceActive    bool // systemctl is-active response
	readyAfter       int  // number of get-nodes polls before the node is Ready
	getNodesErrors   int  // transport errors returned before answering get-nodes
	kubeconfig       string

	getNodesCalls int
}

func newFakeNode() *fakeNode {
	return &fakeNode{
		files:            map[string]string{},
		encryptionStatus: "Enabled",
		auditLogPresent:  true,
		serviceActive:    true,
		kubeconfig:       sampleKubeconfig,
	}
}

func (n *fakeNode) Run(_ context.Context, raw string) (ssh.Result, error) {
	n.cmds = append(n.cmds, raw)
	cmd := strings.ReplaceAll(raw, "sudo ", "") // sudo never appears in a base64 payload

	switch {
	case cmd == k3sBin+" --version":
		if !n.installed {
			return ssh.Result{Stderr: "no such file", ExitStatus: 127}, nil
		}
		return ssh.Result{Stdout: "k3s version " + n.version + " (abcdef)\n"}, nil

	case strings.Contains(cmd, "| base64 -d |"):
		path, content := parseWrite(cmd)
		n.files[path] = content
		return ssh.Result{}, nil

	case cmd == "cat "+pathKubeconfig:
		return ssh.Result{Stdout: n.kubeconfig}, nil

	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimPrefix(cmd, "cat ")
		if c, ok := n.files[path]; ok {
			return ssh.Result{Stdout: c}, nil
		}
		return ssh.Result{Stderr: "No such file or directory", ExitStatus: 1}, nil

	case strings.HasPrefix(cmd, "sysctl -p"), strings.HasPrefix(cmd, "mkdir -p -m 700"):
		return ssh.Result{}, nil

	case strings.Contains(cmd, installURL):
		n.installed = true
		n.version = installVersion(cmd)
		return ssh.Result{Stdout: "installing\n"}, nil

	case cmd == "systemctl is-active k3s":
		if n.serviceActive {
			return ssh.Result{Stdout: "active\n"}, nil
		}
		return ssh.Result{Stdout: "inactive\n", ExitStatus: 3}, nil

	case strings.HasPrefix(cmd, "systemctl restart k3s"), strings.HasPrefix(cmd, "systemctl start k3s"):
		return ssh.Result{}, nil

	case strings.Contains(cmd, "kubectl get nodes"):
		if n.getNodesErrors > 0 {
			n.getNodesErrors--
			return ssh.Result{}, errors.New("dial tcp: connection refused")
		}
		n.getNodesCalls++
		if n.getNodesCalls > n.readyAfter {
			return ssh.Result{Stdout: "node1   Ready    control-plane,etcd,master   30s   " + n.version + "\n"}, nil
		}
		return ssh.Result{Stdout: "node1   NotReady   control-plane,etcd,master   3s   " + n.version + "\n"}, nil

	case strings.Contains(cmd, "secrets-encrypt status"):
		if n.encryptExit != 0 {
			return ssh.Result{Stderr: "apiserver not ready\n", ExitStatus: n.encryptExit}, nil
		}
		return ssh.Result{Stdout: "Encryption Status: " + n.encryptionStatus + "\nCurrent Rotation Stage: start\n"}, nil

	case strings.HasPrefix(cmd, "test -f "+pathAuditLog):
		if n.auditLogPresent {
			return ssh.Result{}, nil
		}
		return ssh.Result{ExitStatus: 1}, nil

	default:
		return ssh.Result{Stderr: "unexpected command: " + cmd, ExitStatus: 127}, nil
	}
}

// parseWrite extracts the destination path and decoded content from a write
// command of the form `…printf %s 'BASE64' | base64 -d | …tee PATH >/dev/null…`.
func parseWrite(cmd string) (path, content string) {
	const start = "printf %s '"
	i := strings.Index(cmd, start)
	j := strings.Index(cmd, "' | base64 -d")
	enc := cmd[i+len(start) : j]
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		panic("fakeNode: bad base64 in write: " + err.Error())
	}
	const teeMark = "tee "
	k := strings.Index(cmd, teeMark)
	rest := cmd[k+len(teeMark):]
	path = strings.TrimSpace(rest[:strings.Index(rest, " >/dev/null")])
	return path, string(dec)
}

func installVersion(cmd string) string {
	const m = "INSTALL_K3S_VERSION='"
	i := strings.Index(cmd, m)
	rest := cmd[i+len(m):]
	return rest[:strings.Index(rest, "'")]
}

const sampleKubeconfig = `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: default
contexts:
- context: {cluster: default, user: default}
  name: default
current-context: default
`

func mustBootstrap(t *testing.T, n *fakeNode, cfg Config) *Result {
	t.Helper()
	res, err := Bootstrap(context.Background(), n, cfg)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return res
}

func TestBootstrapFreshInstall(t *testing.T) {
	n := newFakeNode()
	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})

	if res.AlreadyInstalled {
		t.Error("AlreadyInstalled = true on a fresh node")
	}
	if !res.Changed {
		t.Error("Changed = false on a fresh install")
	}
	if res.Version != DefaultK3sVersion {
		t.Errorf("Version = %q, want %q", res.Version, DefaultK3sVersion)
	}
	if res.SecretsEncryption != "Enabled" {
		t.Errorf("SecretsEncryption = %q, want Enabled", res.SecretsEncryption)
	}
	if !res.AuditLogPresent {
		t.Error("AuditLogPresent = false")
	}

	// All four hardening files landed with the expected content.
	if got := n.files[pathSysctl]; !strings.Contains(got, "kernel.panic_on_oops=1") {
		t.Errorf("sysctl file missing expected content: %q", got)
	}
	if got := n.files[pathAudit]; !strings.Contains(got, "kind: Policy") {
		t.Errorf("audit file missing expected content: %q", got)
	}
	if got := n.files[pathPSA]; !strings.Contains(got, "EventRateLimit") || !strings.Contains(got, "PodSecurity") {
		t.Errorf("psa file must configure both PodSecurity and EventRateLimit: %q", got)
	}
	cfgFile := n.files[pathConfig]
	for _, want := range []string{
		"cluster-init: true", "secrets-encryption: true", "protect-kernel-defaults: true",
		`- "203.0.113.5"`, "audit-policy-file=/var/lib/rancher/k3s/server/audit.yaml",
		`etcd-snapshot-schedule-cron: "0 */12 * * *"`, "etcd-snapshot-retention: 5",
	} {
		if !strings.Contains(cfgFile, want) {
			t.Errorf("config.yaml missing %q:\n%s", want, cfgFile)
		}
	}

	// The installer ran with the pinned version.
	if !hasCmd(n.cmds, func(c string) bool {
		return strings.Contains(c, installURL) && strings.Contains(c, "INSTALL_K3S_VERSION='"+DefaultK3sVersion+"'")
	}) {
		t.Error("installer was not invoked with the pinned version")
	}

	// The kubeconfig was rewritten to the node address.
	if strings.Contains(string(res.Kubeconfig), "127.0.0.1") {
		t.Errorf("kubeconfig still references loopback:\n%s", res.Kubeconfig)
	}
	if !strings.Contains(string(res.Kubeconfig), "https://203.0.113.5:6443") {
		t.Errorf("kubeconfig not rewritten to the node address:\n%s", res.Kubeconfig)
	}
}

func TestBootstrapSnapshotConfig(t *testing.T) {
	// A custom retention and a range of valid schedules — 5-field cron and the
	// @-shorthands cronRe deliberately admits — all render verbatim.
	for _, cron := range []string{"0 3 * * *", "@daily", "@every 6h"} {
		t.Run(cron, func(t *testing.T) {
			n := newFakeNode()
			mustBootstrap(t, n, Config{
				NodeAddress:       "203.0.113.5",
				SnapshotCron:      cron,
				SnapshotRetention: 12,
			})

			cfg := n.files[pathConfig]
			if !strings.Contains(cfg, `etcd-snapshot-schedule-cron: "`+cron+`"`) {
				t.Errorf("snapshot cron %q not rendered:\n%s", cron, cfg)
			}
			if !strings.Contains(cfg, "etcd-snapshot-retention: 12") {
				t.Errorf("custom snapshot retention not rendered:\n%s", cfg)
			}
		})
	}
}

func TestBootstrapIdempotentRerun(t *testing.T) {
	n := newFakeNode()
	mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})

	n.cmds = nil // observe only the second run
	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})

	if !res.AlreadyInstalled {
		t.Error("AlreadyInstalled = false on a re-run")
	}
	if res.Changed {
		t.Error("Changed = true on a no-op re-run")
	}
	if hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, installURL) }) {
		t.Error("re-run reinstalled k3s")
	}
	if hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "systemctl restart") }) {
		t.Error("re-run restarted k3s with no config change")
	}
	if hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "| base64 -d |") }) {
		t.Error("re-run rewrote a file whose content was unchanged")
	}
}

func TestBootstrapConfigChangeRestarts(t *testing.T) {
	n := newFakeNode()
	mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})

	n.cmds = nil
	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5", ExtraTLSSANs: []string{"cluster.example.com"}})

	if !res.Changed {
		t.Error("Changed = false after a config change")
	}
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "tee "+pathConfig) }) {
		t.Error("config.yaml was not rewritten after a SAN change")
	}
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "systemctl restart k3s") }) {
		t.Error("k3s was not restarted after a config change")
	}
	if hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, installURL) }) {
		t.Error("a config change must not reinstall k3s")
	}
	if !strings.Contains(n.files[pathConfig], "cluster.example.com") {
		t.Error("extra SAN not present in config.yaml")
	}
}

func TestBootstrapUpgrade(t *testing.T) {
	n := newFakeNode()
	n.installed = true
	n.version = "v1.34.1+k3s1"

	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5", K3sVersion: "v1.35.5+k3s1"})

	if !res.Changed {
		t.Error("Changed = false on an upgrade")
	}
	if res.Version != "v1.35.5+k3s1" {
		t.Errorf("Version = %q, want the upgraded version", res.Version)
	}
	if !hasCmd(n.cmds, func(c string) bool {
		return strings.Contains(c, installURL) && strings.Contains(c, "v1.35.5+k3s1")
	}) {
		t.Error("upgrade did not re-run the installer with the new version")
	}
}

func TestBootstrapSudoPrefixes(t *testing.T) {
	n := newFakeNode()
	mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5", Sudo: true})

	if !hasCmd(n.cmds, func(c string) bool { return strings.HasPrefix(c, "sudo cat "+pathKubeconfig) }) {
		t.Error("kubeconfig was not read with sudo")
	}
	if !hasCmd(n.cmds, func(c string) bool {
		return strings.Contains(c, "| sudo tee "+pathConfig)
	}) {
		t.Error("config.yaml was not written through sudo tee")
	}
}

func TestBootstrapWaitsForReady(t *testing.T) {
	n := newFakeNode()
	n.readyAfter = 2 // NotReady for the first two polls

	mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})
	if n.getNodesCalls < 3 {
		t.Errorf("expected to poll until Ready (>=3 calls), got %d", n.getNodesCalls)
	}
}

func TestBootstrapEncryptionNotEnabledFails(t *testing.T) {
	n := newFakeNode()
	n.encryptionStatus = "Disabled"

	_, err := Bootstrap(context.Background(), n, Config{NodeAddress: "203.0.113.5"})
	if err == nil || !strings.Contains(err.Error(), "secrets encryption is not enabled") {
		t.Fatalf("want secrets-encryption error, got %v", err)
	}
}

func TestBootstrapAuditLogMissingFails(t *testing.T) {
	n := newFakeNode()
	n.auditLogPresent = false

	_, err := Bootstrap(context.Background(), n, Config{NodeAddress: "203.0.113.5"})
	if err == nil || !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("want audit-log error, got %v", err)
	}
}

func TestBootstrapStartsInactiveService(t *testing.T) {
	n := newFakeNode()
	mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"}) // converge once

	n.serviceActive = false // the service is installed but stopped (e.g. after a reboot)
	n.cmds = nil
	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})

	if !res.Changed {
		t.Error("Changed = false after starting a stopped service")
	}
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "systemctl start k3s") }) {
		t.Error("a stopped service was not started")
	}
	if hasCmd(n.cmds, func(c string) bool {
		return strings.Contains(c, "systemctl restart") || strings.Contains(c, installURL)
	}) {
		t.Error("starting a stopped service must not restart or reinstall")
	}
}

func TestBootstrapReadyTimeout(t *testing.T) {
	n := newFakeNode()
	n.readyAfter = 1 << 30 // never reports Ready

	_, err := Bootstrap(context.Background(), n, Config{NodeAddress: "203.0.113.5", ReadyTimeout: 30 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not become Ready") {
		t.Fatalf("want a Ready timeout error, got %v", err)
	}
}

func TestBootstrapWaitReadyToleratesTransientErrors(t *testing.T) {
	n := newFakeNode()
	n.getNodesErrors = 3 // the API server is still coming up

	res := mustBootstrap(t, n, Config{NodeAddress: "203.0.113.5"})
	if !res.Changed {
		t.Error("expected a successful install despite transient poll errors")
	}
	if n.getNodesErrors != 0 {
		t.Errorf("transient errors not all consumed: %d left", n.getNodesErrors)
	}
}

func TestBootstrapKubeconfigRewriteFailure(t *testing.T) {
	n := newFakeNode()
	// k3s produced a kubeconfig that lacks the loopback placeholder to rewrite.
	n.kubeconfig = "clusters:\n- cluster:\n    server: https://169.254.1.1:6443\n"

	_, err := Bootstrap(context.Background(), n, Config{NodeAddress: "203.0.113.5"})
	if err == nil || !strings.Contains(err.Error(), "did not contain the expected server URL") {
		t.Fatalf("want a kubeconfig-rewrite error, got %v", err)
	}
}

func TestBootstrapSecretsEncryptCommandFails(t *testing.T) {
	n := newFakeNode()
	n.encryptExit = 1 // `secrets-encrypt status` itself fails

	_, err := Bootstrap(context.Background(), n, Config{NodeAddress: "203.0.113.5"})
	if err == nil || !strings.Contains(err.Error(), "secrets-encrypt status exited") {
		t.Fatalf("want a secrets-encrypt command error, got %v", err)
	}
}

func TestBootstrapRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty address", Config{NodeAddress: ""}},
		{"address with port", Config{NodeAddress: "203.0.113.5:22"}},
		{"yaml-injecting address", Config{NodeAddress: "x\"]\nfoo: bar"}},
		{"bad version", Config{NodeAddress: "node", K3sVersion: "latest; rm -rf /"}},
		{"bad extra san", Config{NodeAddress: "node", ExtraTLSSANs: []string{"a b"}}},
		{"yaml-injecting snapshot cron", Config{NodeAddress: "node", SnapshotCron: "0 0 * * *\"\nfoo: bar"}},
		{"snapshot cron with embedded quote", Config{NodeAddress: "node", SnapshotCron: `0 0 * * *"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Bootstrap(context.Background(), newFakeNode(), tc.cfg); err == nil {
				t.Fatal("want a validation error, got nil")
			}
		})
	}
}

func TestBootstrapNilRunner(t *testing.T) {
	if _, err := Bootstrap(context.Background(), nil, Config{NodeAddress: "node"}); err == nil {
		t.Fatal("want an error for a nil runner")
	}
}

func hasCmd(cmds []string, pred func(string) bool) bool {
	for _, c := range cmds {
		if pred(c) {
			return true
		}
	}
	return false
}
