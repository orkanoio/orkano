package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/features"
	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// healthyNode answers both the preflight probes and the k3s bootstrap commands
// for a node that installs cleanly and is immediately Ready. ssOutput is the
// `ss -Hltn` body, so a test can make a required port look occupied.
func healthyNode(ssOutput string) sshtest.ExecHandler {
	installed := false
	apparmorLoaded := false
	files := map[string]string{}
	return func(raw string) (string, string, int) {
		cmd := strings.ReplaceAll(raw, "sudo ", "")
		switch {
		// preflight
		case cmd == "true":
			return "", "", 0
		case cmd == "uname -m":
			return "x86_64\n", "", 0
		case cmd == "command -v curl":
			return "/usr/bin/curl\n", "", 0
		case cmd == "ss -Hltn":
			return ssOutput, "", 0
		case cmd == "date -u +%s":
			return strconv.FormatInt(time.Now().Unix(), 10) + "\n", "", 0
		// k3s bootstrap
		case strings.HasSuffix(cmd, "/k3s --version"):
			if !installed {
				return "", "not found", 127
			}
			return "k3s version v1.35.5+k3s1 (abc)\n", "", 0
		case strings.Contains(cmd, "| base64 -d |"):
			path, content := decodeWrite(cmd)
			files[path] = content
			return "", "", 0
		case cmd == "cat /etc/rancher/k3s/k3s.yaml":
			return "clusters:\n- cluster:\n    server: https://127.0.0.1:6443\n", "", 0
		case cmd == "cat /var/lib/rancher/k3s/server/token":
			return "K10cafef00d::server:abc123def456\n", "", 0
		// nodeprep: AppArmor profile load + verification
		case strings.Contains(cmd, "apparmor_parser -r"):
			apparmorLoaded = true
			return "", "", 0
		case cmd == "cat /sys/kernel/security/apparmor/profiles":
			if apparmorLoaded {
				return "cri-containerd.apparmor.d (enforce)\norkano-buildkit (enforce)\n", "", 0
			}
			return "cri-containerd.apparmor.d (enforce)\n", "", 0
		case strings.HasPrefix(cmd, "cat "):
			// Serve a previously written file by path so a re-run converges to a
			// no-op (matches a real node); anything unwritten is absent.
			if c, ok := files[strings.TrimPrefix(cmd, "cat ")]; ok {
				return c, "", 0
			}
			return "", "no such file", 1
		case strings.HasPrefix(cmd, "sysctl -p"), strings.HasPrefix(cmd, "mkdir -p -m 700"):
			return "", "", 0
		case strings.Contains(cmd, "get.k3s.io"):
			installed = true
			return "installing\n", "", 0
		case strings.Contains(cmd, "kubectl get --raw=/readyz"):
			// A fresh node runs no k3s: the rerun-allowance probe is refused. The
			// rerun test overlays this with an "ok" answer.
			return "", "connection refused", 1
		case strings.Contains(cmd, "kubectl get nodes"):
			return "node1 Ready control-plane,etcd,master 30s v1.35.5+k3s1\n", "", 0
		case strings.Contains(cmd, "secrets-encrypt status"):
			return "Encryption Status: Enabled\n", "", 0
		case strings.HasPrefix(cmd, "test -f "):
			return "", "", 0
		default:
			return "", "unexpected: " + cmd, 127
		}
	}
}

// failingAppArmorNode is a healthy node whose AppArmor profile load fails, to
// prove orkano init refuses the node rather than leaving builds silently broken.
func failingAppArmorNode() sshtest.ExecHandler {
	base := healthyNode("")
	return func(raw string) (string, string, int) {
		if strings.Contains(strings.ReplaceAll(raw, "sudo ", ""), "apparmor_parser -r") {
			return "", "apparmor_parser: profile failed to load", 1
		}
		return base(raw)
	}
}

// decodeWrite extracts the path and decoded content from an ensureFile write
// command (`printf %s 'BASE64' | base64 -d | tee PATH >/dev/null && ...`).
func decodeWrite(cmd string) (path, content string) {
	const start = "printf %s '"
	i := strings.Index(cmd, start)
	j := strings.Index(cmd, "' | base64 -d")
	if i < 0 || j < 0 {
		return "", ""
	}
	dec, _ := base64.StdEncoding.DecodeString(cmd[i+len(start) : j])
	const teeMark = "tee "
	rest := cmd[strings.Index(cmd, teeMark)+len(teeMark):]
	return strings.TrimSpace(rest[:strings.Index(rest, " >/dev/null")]), string(dec)
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func baseOptions(t *testing.T, srv *sshtest.Server) *initOptions {
	t.Helper()
	host, port := hostPort(t, srv.Addr)
	return &initOptions{
		nodes:        []string{host},
		sshUser:      srv.User,
		sshPort:      port,
		sshKeyPath:   writeTemp(t, "id", srv.ClientPrivateKey),
		hostKeyPaths: []string{writeTemp(t, "hostkey", srv.HostKeyAuthorized)},
		k3sVersion:   "v1.35.5+k3s1",
		kubeconfig:   filepath.Join(t.TempDir(), "kubeconfig"),
		readyTimeout: 30 * time.Second,
	}
}

// deployCall records what runInit passed to the (stubbed) component-deploy step.
type deployCall struct {
	called bool
	opt    *initOptions
}

// stubDeploy replaces the component-deploy step with a stub returning token,
// restoring the real step on cleanup. The deploy itself is engine-tested in
// internal/install; here we only assert the CLI orchestration around it (like
// bootstrapOne for the bootstrap loop).
func stubDeploy(t *testing.T, token string) *deployCall {
	t.Helper()
	orig := deployComponents
	t.Cleanup(func() { deployComponents = orig })
	c := &deployCall{}
	deployComponents = func(_ context.Context, _, _ io.Writer, opt *initOptions, _, _ []byte) (string, error) {
		c.called, c.opt = true, opt
		return token, nil
	}
	return c
}

// wireCall records what runInit passed to the (stubbed) registry-wiring step.
type wireCall struct {
	called   bool
	hostKeys [][]byte
}

// stubWireRegistry replaces the node-registry-wiring step with a stub, restoring
// the real one on cleanup. The wiring is engine-tested in internal/install; here
// we only assert the CLI orchestration around it (it is called after deploy with
// one host key per node).
func stubWireRegistry(t *testing.T) *wireCall {
	t.Helper()
	orig := wireRegistry
	t.Cleanup(func() { wireRegistry = orig })
	c := &wireCall{}
	wireRegistry = func(_ context.Context, _, _ io.Writer, _ *initOptions, _ []byte, hostKeys [][]byte) error {
		c.called, c.hostKeys = true, hostKeys
		return nil
	}
	return c
}

func TestInitHappyPath(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	stubDeploy(t, "boot-token-xyz")
	wire := stubWireRegistry(t)

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit: %v\nstderr:\n%s", err, errw.String())
	}
	if !strings.Contains(out.String(), "components:         deployed") {
		t.Errorf("summary missing the components line:\n%s", out.String())
	}
	if !wire.called {
		t.Error("registry wiring step was not invoked")
	}
	if len(wire.hostKeys) != 1 {
		t.Errorf("registry wiring got %d host keys, want 1", len(wire.hostKeys))
	}
	if !strings.Contains(out.String(), "registry:           wired on every node") {
		t.Errorf("summary missing the registry line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "receiver:           cluster-internal") {
		t.Errorf("summary should note the receiver stays cluster-internal without --receiver-host:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Bootstrap token (shown once") || !strings.Contains(out.String(), "boot-token-xyz") {
		t.Errorf("summary missing the one-time bootstrap token:\n%s", out.String())
	}

	kc, err := os.ReadFile(opt.kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfig not written: %v", err)
	}
	if !strings.Contains(string(kc), "https://"+opt.nodes[0]+":6443") {
		t.Errorf("kubeconfig not rewritten to node:\n%s", kc)
	}
	if !strings.Contains(out.String(), "Installed k3s") {
		t.Errorf("summary missing install line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "KUBECONFIG="+opt.kubeconfig) {
		t.Errorf("summary missing next-step hint:\n%s", out.String())
	}
	// The AppArmor profile load actually ran over sshtest (healthyNode answers
	// apparmor_parser + the profiles read); the summary reports the confinement.
	if !strings.Contains(out.String(), "build confinement:  AppArmor orkano-buildkit (enforce)") {
		t.Errorf("summary missing the build-confinement line:\n%s", out.String())
	}
}

func TestInitSecretsVaultThreading(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	deploy := stubDeploy(t, "")
	stubWireRegistry(t)

	opt := baseOptions(t, srv)
	opt.secretsVault = true
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit: %v\nstderr:\n%s", err, errw.String())
	}
	if !deploy.called || !deploy.opt.secretsVault {
		t.Error("component deploy did not receive --secrets-vault")
	}
	if !strings.Contains(out.String(), "secrets vault:      External Secrets Operator deployed") {
		t.Errorf("summary missing the secrets-vault line:\n%s", out.String())
	}

	// Without the flag the deploy gets false and the summary stays silent —
	// ESO is strictly opt-in (ADR-0018). Fresh server: the fake node is
	// stateful across a run.
	srv2 := sshtest.New(healthyNode(""))
	defer srv2.Close()
	opt = baseOptions(t, srv2)
	out.Reset()
	errw.Reset()
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit without flag: %v\nstderr:\n%s", err, errw.String())
	}
	if deploy.opt.secretsVault {
		t.Error("component deploy received secretsVault without the flag")
	}
	if strings.Contains(out.String(), "secrets vault:") {
		t.Errorf("summary mentions the secrets vault without the flag:\n%s", out.String())
	}
}

func TestInitLocalSecretsVaultThreading(t *testing.T) {
	stubLocalNode(t, healthyNode(""))
	deploy := stubLocalDeploy(t, "")
	stubLocalWire(t)

	opt := &initOptions{
		local:        true,
		nodes:        []string{"10.0.0.9"},
		k3sVersion:   "v1.35.5+k3s1",
		kubeconfig:   filepath.Join(t.TempDir(), "kubeconfig"),
		readyTimeout: 30 * time.Second,
		secretsVault: true,
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit --local: %v\nstderr:\n%s", err, errw.String())
	}
	if !deploy.called || !deploy.opt.secretsVault {
		t.Error("local component deploy did not receive --secrets-vault")
	}
	if !strings.Contains(out.String(), "secrets vault:      External Secrets Operator deployed") {
		t.Errorf("local summary missing the secrets-vault line:\n%s", out.String())
	}
}

func TestInitSecretsVaultFlagDefaultOff(t *testing.T) {
	cmd := newInitCommand("test")
	f := cmd.Flags().Lookup("secrets-vault")
	if f == nil {
		t.Fatal("--secrets-vault flag not registered")
	}
	if f.DefValue != "false" {
		t.Fatalf("--secrets-vault must default off (ADR-0018 opt-in), got %q", f.DefValue)
	}
}

func TestInitUnsafeFeatureFlagDefaultOff(t *testing.T) {
	cmd := newInitCommand("test")
	f := cmd.Flags().Lookup("enable-unsafe-feature")
	if f == nil {
		t.Fatal("--enable-unsafe-feature flag not registered")
	}
	if f.DefValue != "[]" {
		t.Fatalf("--enable-unsafe-feature must default off, got %q", f.DefValue)
	}
	if !strings.Contains(f.Usage, "UNSAFE") {
		t.Fatalf("--enable-unsafe-feature help must mark the opt-in unsafe, got %q", f.Usage)
	}
}

func TestInitRejectsUnknownUnsafeFeatureBeforeMutation(t *testing.T) {
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, &initOptions{
		unsafeFeatures: []string{"source.unknown"},
	})
	if err == nil || !strings.Contains(err.Error(), "source.unknown") {
		t.Fatalf("want unknown unsafe-feature error, got %v", err)
	}
	if strings.Contains(err.Error(), "--node is required") {
		t.Fatalf("unsafe features were not validated before install setup: %v", err)
	}
}

func TestInitUnsafeFeaturesThreading(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	deploy := stubDeploy(t, "")
	stubWireRegistry(t)

	opt := baseOptions(t, srv)
	opt.unsafeFeatures = []string{string(features.SourceZip), string(features.SourceGit), string(features.SourceZip), string(features.BuildNixpacks)}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit: %v\nstderr:\n%s", err, errw.String())
	}
	want := []string{string(features.BuildNixpacks), string(features.SourceGit), string(features.SourceZip)}
	if !deploy.called || !slices.Equal(deploy.opt.unsafeFeatures, want) {
		t.Fatalf("component deploy unsafe features = %v, want canonical %v", deploy.opt.unsafeFeatures, want)
	}
	if !strings.Contains(out.String(), "UNSAFE features:    build.nixpacks, source.git, source.zip") {
		t.Errorf("summary missing unsafe-feature warning:\n%s", out.String())
	}
}

func TestReadinessTargetsSecretsVault(t *testing.T) {
	base := readinessTargets(&initOptions{})
	for _, w := range base {
		if w.Namespace == "external-secrets" {
			t.Errorf("ESO readiness target %+v present without --secrets-vault", w)
		}
	}
	with := readinessTargets(&initOptions{secretsVault: true})
	if len(with) != len(base)+3 {
		t.Errorf("expected the 3 ESO targets appended, got %d -> %d", len(base), len(with))
	}
}

func TestInitRefusesOnAppArmorFailure(t *testing.T) {
	// The node bootstraps fine but the AppArmor profile fails to load — init must
	// refuse it (a node without the profile silently breaks every build) and not
	// write the kubeconfig.
	srv := sshtest.New(failingAppArmorNode())
	defer srv.Close()

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "AppArmor profile") {
		t.Fatalf("want AppArmor load refusal, got %v", err)
	}
	if _, statErr := os.Stat(opt.kubeconfig); statErr == nil {
		t.Error("kubeconfig was written despite the AppArmor load failure")
	}
}

func TestInitRefusesOnPreflightFailure(t *testing.T) {
	// A listener on the API server port makes ports.free fail (critical).
	srv := sshtest.New(healthyNode("LISTEN 0 128 0.0.0.0:6443 0.0.0.0:*\n"))
	defer srv.Close()

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("want preflight refusal, got %v", err)
	}
	if _, statErr := os.Stat(opt.kubeconfig); statErr == nil {
		t.Error("kubeconfig was written despite preflight refusal")
	}
}

func TestInitSkipPreflightProceeds(t *testing.T) {
	// Same occupied port, but --skip-preflight bypasses the gate.
	srv := sshtest.New(healthyNode("LISTEN 0 128 0.0.0.0:6443 0.0.0.0:*\n"))
	defer srv.Close()
	stubDeploy(t, "")
	stubWireRegistry(t)

	opt := baseOptions(t, srv)
	opt.skipPreflight = true
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit with --skip-preflight: %v", err)
	}
	// The empty deploy token marks a re-run: the summary must print the recovery
	// recipe built on the WORKSTATION-side kubeconfig — the SSH-path reader is
	// not on the server, so an on-box k3s kubectl recipe would strand them.
	s := out.String()
	if !strings.Contains(s, "plaintext token cannot be recovered") ||
		!strings.Contains(s, "rollout restart deploy/orkano-dashboard") {
		t.Errorf("SSH-path summary missing the bootstrap token recovery recipe:\n%s", s)
	}
	if !strings.Contains(s, "KUBECONFIG=\""+opt.kubeconfig+"\" kubectl") {
		t.Errorf("SSH-path recovery recipe must use the local kubeconfig, not an on-box k3s kubectl:\n%s", s)
	}
}

func TestInitRerunWithExistingK3sPassesPreflight(t *testing.T) {
	// The literal live-v0.0.2 rerun scenario: k3s from the first run still holds
	// its control-plane ports, but its API answers /readyz — preflight must treat
	// that as an idempotent converge, not an occupied-ports refusal.
	base := healthyNode("LISTEN 0 128 *:6443 *:*\nLISTEN 0 128 *:2379 *:*\n" +
		"LISTEN 0 128 *:2380 *:*\nLISTEN 0 128 *:10250 *:*\n")
	srv := sshtest.New(func(raw string) (string, string, int) {
		if strings.Contains(strings.ReplaceAll(raw, "sudo ", ""), "kubectl get --raw=/readyz") {
			return "ok\n", "", 0
		}
		return base(raw)
	})
	defer srv.Close()
	stubDeploy(t, "")
	stubWireRegistry(t)

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("rerun against a running k3s should pass preflight, got: %v\nstderr:\n%s", err, errw.String())
	}
	if !strings.Contains(out.String(), "idempotent converge") {
		t.Errorf("preflight output should explain the rerun allowance:\n%s", out.String())
	}
}

func TestInitPrintsFreshBootstrapTokenWhenDeployFails(t *testing.T) {
	// SSH-path analog of the --local test: a deploy failure after the token
	// secret was created must still surface the plaintext.
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	orig := deployComponents
	t.Cleanup(func() { deployComponents = orig })
	deployComponents = func(_ context.Context, _, _ io.Writer, _ *initOptions, _, _ []byte) (string, error) {
		return "ssh-fresh-token", errors.New("operator never became ready")
	}

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "deploy components") {
		t.Fatalf("expected deploy failure, got %v", err)
	}
	if !strings.Contains(out.String(), "ssh-fresh-token") ||
		!strings.Contains(out.String(), "will not be shown again") {
		t.Errorf("fresh token from failed deploy should still be printed:\n%s", out.String())
	}
}

func TestInitPrintsFreshBootstrapTokenWhenWireRegistryFails(t *testing.T) {
	// A registry-wiring failure comes AFTER the deploy created a fresh token and
	// BEFORE printSummary, its only other outlet — losing it there is the same
	// lockout as losing it on a deploy failure.
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	stubDeploy(t, "wire-fail-token")
	orig := wireRegistry
	t.Cleanup(func() { wireRegistry = orig })
	wireRegistry = func(_ context.Context, _, _ io.Writer, _ *initOptions, _ []byte, _ [][]byte) error {
		return errors.New("k3s restart never came back")
	}

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "wire registry") {
		t.Fatalf("expected wire-registry failure, got %v", err)
	}
	if !strings.Contains(out.String(), "wire-fail-token") ||
		!strings.Contains(out.String(), "will not be shown again") {
		t.Errorf("fresh token should be printed before the wire-registry error returns:\n%s", out.String())
	}
}

func TestInitRequiresFlags(t *testing.T) {
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, &initOptions{sshKeyPath: "x"}); err == nil {
		t.Error("want error when --node is missing")
	}
	if err := runInit(context.Background(), &out, &errw, &initOptions{nodes: []string{"n"}}); err == nil {
		t.Error("want error when --ssh-key is missing")
	}
	if err := runInit(context.Background(), &out, &errw, &initOptions{nodes: []string{"a", "b"}, sshKeyPath: "x"}); err == nil {
		t.Error("want error for an even number of servers")
	}
}

func TestInitRejectsBadNodeSets(t *testing.T) {
	var out, errw bytes.Buffer

	dup := &initOptions{sshKeyPath: "x", nodes: []string{"a", "a", "a"}}
	if err := runInit(context.Background(), &out, &errw, dup); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("want duplicate-node error, got %v", err)
	}

	mismatch := &initOptions{sshKeyPath: "x", nodes: []string{"a", "b", "c"}, hostKeyPaths: []string{"one"}}
	if err := runInit(context.Background(), &out, &errw, mismatch); err == nil || !strings.Contains(err.Error(), "once per --node") {
		t.Errorf("want host-key count mismatch error, got %v", err)
	}
}

func TestOtherNodes(t *testing.T) {
	got := otherNodes([]string{"a", "b", "c"}, 1)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("otherNodes(_, 1) = %v, want [a c]", got)
	}
	if got := otherNodes([]string{"solo"}, 0); len(got) != 0 {
		t.Errorf("otherNodes of a single node = %v, want empty", got)
	}
}

func TestFirstDuplicate(t *testing.T) {
	if d := firstDuplicate([]string{"a", "b", "c"}); d != "" {
		t.Errorf("unique set reported duplicate %q", d)
	}
	if d := firstDuplicate([]string{"a", "b", "a"}); d != "a" {
		t.Errorf("firstDuplicate = %q, want a", d)
	}
}

// TestInitHAOrchestration stubs the per-node bootstrap to assert the HA loop's
// orchestration: the first node initialises (no ServerURL/token), each later
// node joins the first with its token at the right MinReadyNodes, the kubeconfig
// comes from the first node, the HA summary is printed, and the join token never
// leaks into operator-facing output. The actual join-over-SSH is proven at the
// k3s layer (TestBootstrapHAJoinOverRealSSH); the CLI multi-node SSH path can't
// be exercised through sshtest (one --ssh-port can't reach three servers).
func TestInitHAOrchestration(t *testing.T) {
	const token = "K10secret::server:deadbeef"
	stubDeploy(t, "") // HA test focuses on the bootstrap loop; deploy is stubbed
	wire := stubWireRegistry(t)
	type call struct {
		node string
		cfg  k3s.Config
	}
	var calls []call

	orig := bootstrapOne
	defer func() { bootstrapOne = orig }()
	bootstrapOne = func(_ context.Context, _, _ io.Writer, _ *initOptions, _ []byte, node, _ string, cfg k3s.Config) (*k3s.Result, []byte, bool, error) {
		calls = append(calls, call{node, cfg})
		res := &k3s.Result{Version: "v1.35.5+k3s1", SecretsEncryption: "Enabled", AuditLogPresent: true, Changed: true}
		if cfg.ServerURL == "" { // the first (cluster-init) server
			res.Token = token
			res.Kubeconfig = []byte("kubeconfig-from-first\n")
		}
		// Distinct per-node host keys so the registry-wiring threading is provable.
		return res, []byte("hostkey-" + node), false, nil
	}

	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{
		nodes:      []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		sshUser:    "root",
		sshPort:    22,
		sshKeyPath: writeTemp(t, "id", []byte("dummy-key")),
		kubeconfig: kcPath,
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit HA: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("want 3 bootstrap calls, got %d", len(calls))
	}
	if c := calls[0].cfg; c.ServerURL != "" || c.Token != "" || c.MinReadyNodes != 1 {
		t.Errorf("node 0 should cluster-init (min 1), got ServerURL=%q Token set=%v Min=%d", c.ServerURL, c.Token != "", c.MinReadyNodes)
	}
	if c := calls[1].cfg; c.ServerURL != "https://10.0.0.1:6443" || c.Token != token || c.MinReadyNodes != 2 {
		t.Errorf("node 1 should join node 0 with the token (min 2), got ServerURL=%q Token match=%v Min=%d", c.ServerURL, c.Token == token, c.MinReadyNodes)
	}
	if c := calls[2].cfg; c.ServerURL != "https://10.0.0.1:6443" || c.Token != token || c.MinReadyNodes != 3 {
		t.Errorf("node 2 should join node 0 with the token (min 3), got Min=%d", c.MinReadyNodes)
	}
	if len(calls[0].cfg.ExtraTLSSANs) != 2 {
		t.Errorf("node 0 SANs = %v, want the two peer addresses", calls[0].cfg.ExtraTLSSANs)
	}

	if kc, _ := os.ReadFile(kcPath); string(kc) != "kubeconfig-from-first\n" {
		t.Errorf("kubeconfig = %q, want the first server's", kc)
	}
	if !strings.Contains(out.String(), "3 (HA, embedded etcd)") {
		t.Errorf("summary missing the HA server count:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "build confinement:") {
		t.Errorf("summary missing the build-confinement line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "load balancer") {
		t.Errorf("summary missing the HA kubeconfig caveat:\n%s", out.String())
	}
	if strings.Contains(out.String(), token) || strings.Contains(errw.String(), token) {
		t.Error("join token leaked into CLI output")
	}
	// The registry wiring must receive one host key per node, in node order, so it
	// can reach every node (registries.yaml is written cluster-wide, not just on
	// node 0) reusing each node's pinned key.
	if !wire.called || len(wire.hostKeys) != 3 {
		t.Fatalf("registry wiring got %d host keys, want 3 (one per node)", len(wire.hostKeys))
	}
	for i, node := range opt.nodes {
		if string(wire.hostKeys[i]) != "hostkey-"+node {
			t.Errorf("registry wiring host key[%d] = %q, want the key pinned for %s", i, wire.hostKeys[i], node)
		}
	}
}

// TestInitRunsComponentDeploy stubs the bootstrap and deploy steps to assert
// runInit invokes the component deploy after bootstrap and threads the ACME and
// allowlist flags + the CLI version into it, printing the returned token once.
func TestInitRunsComponentDeploy(t *testing.T) {
	origBootstrap := bootstrapOne
	defer func() { bootstrapOne = origBootstrap }()
	bootstrapOne = func(_ context.Context, _, _ io.Writer, _ *initOptions, _ []byte, node, _ string, cfg k3s.Config) (*k3s.Result, []byte, bool, error) {
		return &k3s.Result{Version: "v1.35.5+k3s1", SecretsEncryption: "Enabled", AuditLogPresent: true, Kubeconfig: []byte("kc\n")}, nil, false, nil
	}
	deploy := stubDeploy(t, "the-install-token")
	stubWireRegistry(t)

	opt := &initOptions{
		version:      "9.9.9",
		nodes:        []string{"10.0.0.1"},
		sshUser:      "root",
		sshPort:      22,
		sshKeyPath:   writeTemp(t, "id", []byte("dummy-key")),
		kubeconfig:   filepath.Join(t.TempDir(), "kubeconfig"),
		acmeEmail:    "ops@example.com",
		acmeProd:     true,
		allowRepos:   []string{"orkanoio/orkano"},
		receiverHost: "hooks.example.com",
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	if !deploy.called {
		t.Fatal("component deploy was not invoked")
	}
	if deploy.opt.version != "9.9.9" || deploy.opt.acmeEmail != "ops@example.com" || !deploy.opt.acmeProd {
		t.Errorf("deploy did not receive the version/ACME flags: %+v", deploy.opt)
	}
	if deploy.opt.receiverHost != "hooks.example.com" {
		t.Errorf("deploy did not receive --receiver-host: %q", deploy.opt.receiverHost)
	}
	if len(deploy.opt.allowRepos) != 1 || deploy.opt.allowRepos[0] != "orkanoio/orkano" {
		t.Errorf("deploy did not receive the allowlist: %v", deploy.opt.allowRepos)
	}
	if !strings.Contains(out.String(), "receiver:           https://hooks.example.com") {
		t.Errorf("summary missing the exposed-receiver line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the-install-token") {
		t.Errorf("bootstrap token not printed:\n%s", out.String())
	}
}

func TestResolveHostKeyExplicitFile(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()
	path := writeTemp(t, "hk", srv.HostKeyAuthorized)

	got, err := resolveHostKey(context.Background(), &bytes.Buffer{}, srv.Addr, path, false)
	if err != nil {
		t.Fatalf("resolveHostKey: %v", err)
	}
	if !bytes.Equal(got, srv.HostKeyAuthorized) {
		t.Error("explicit host key not returned verbatim")
	}
}

func TestResolveHostKeyAcceptNew(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()

	var errw bytes.Buffer
	got, err := resolveHostKey(context.Background(), &errw, srv.Addr, "", true)
	if err != nil {
		t.Fatalf("resolveHostKey accept-new: %v", err)
	}
	if !bytes.Equal(got, srv.HostKeyAuthorized) {
		t.Error("scanned host key does not match the server's")
	}
	if !strings.Contains(errw.String(), "fingerprint SHA256:") {
		t.Errorf("fingerprint not shown:\n%s", errw.String())
	}
}

func TestResolveHostKeyRefusesUntrusted(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()

	_, err := resolveHostKey(context.Background(), &bytes.Buffer{}, srv.Addr, "", false)
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("want untrusted-host refusal with fingerprint, got %v", err)
	}
	if !strings.Contains(err.Error(), "accept-new-host-key") {
		t.Errorf("refusal should name the opt-in flag: %v", err)
	}
}

// --- orkano init --local (ADR-0017) ------------------------------------------

// fakeLocalRunner adapts a command-dispatching ExecHandler (healthyNode) to the
// localRunner interface, so the --local happy path exercises the real k3s +
// AppArmor engine against a fake node — the local analog of sshtest for the SSH
// path (the real localexec.Runner is unit-tested in internal/localexec, and the
// on-box acceptance rides CI, mirroring the SSH sshtest-vs-E2E split).
type fakeLocalRunner struct{ h sshtest.ExecHandler }

func (f fakeLocalRunner) Run(_ context.Context, cmd string) (ssh.Result, error) {
	so, se, code := f.h(cmd)
	return ssh.Result{Stdout: so, Stderr: se, ExitStatus: code}, nil
}

// stubLocalNode makes geteuid report root and newLocalRunner return a fake node.
func stubLocalNode(t *testing.T, node sshtest.ExecHandler) {
	t.Helper()
	origEuid, origRunner := geteuid, newLocalRunner
	t.Cleanup(func() { geteuid, newLocalRunner = origEuid, origRunner })
	geteuid = func() int { return 0 }
	newLocalRunner = func() localRunner { return fakeLocalRunner{node} }
}

// stubLocalDeploy replaces the local component-deploy step (engine-tested in
// internal/install) with a stub returning token, so the --local orchestration
// test asserts around it — the local analog of stubDeploy.
func stubLocalDeploy(t *testing.T, token string) *deployCall {
	t.Helper()
	orig := deployLocal
	t.Cleanup(func() { deployLocal = orig })
	c := &deployCall{}
	deployLocal = func(_ context.Context, _ io.Writer, opt *initOptions, _ localRunner) (string, error) {
		c.called, c.opt = true, opt
		return token, nil
	}
	return c
}

// stubLocalWire replaces the local registry-wiring step (engine-tested in
// internal/install) with a stub, returning a pointer to a called flag.
func stubLocalWire(t *testing.T) *bool {
	t.Helper()
	orig := wireRegistryLocal
	t.Cleanup(func() { wireRegistryLocal = orig })
	called := false
	wireRegistryLocal = func(_ context.Context, _ io.Writer, _ *initOptions, _ localRunner, _ string) error {
		called = true
		return nil
	}
	return &called
}

func TestInitLocalHappyPath(t *testing.T) {
	stubLocalNode(t, healthyNode(""))
	deploy := stubLocalDeploy(t, "local-token-abc")
	wireCalled := stubLocalWire(t)

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{
		local:        true,
		nodes:        []string{"10.0.0.9"},
		k3sVersion:   "v1.35.5+k3s1",
		kubeconfig:   kc,
		readyTimeout: 30 * time.Second,
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit --local: %v\nstderr:\n%s", err, errw.String())
	}

	s := out.String()
	for _, want := range []string{
		"Installed k3s",
		"single node — not highly available",
		"build confinement:  AppArmor orkano-buildkit (enforce)",
		"components:         deployed",
		"registry:           wired",
		"local-token-abc",
		"Reach the dashboard",
		"ssh -L 9090:127.0.0.1:9090 root@10.0.0.9",
		"port-forward --address 127.0.0.1 svc/orkano-dashboard 9090:80",
		"http://localhost:9090",
		"orkano init --node A --node B --node C",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	// The dashboard link must be localhost, never a public URL (INV-05).
	if strings.Contains(s, "https://10.0.0.9") {
		t.Errorf("summary exposed a public dashboard URL (must stay private):\n%s", s)
	}
	if !deploy.called {
		t.Error("component deploy not invoked")
	}
	if !*wireCalled {
		t.Error("registry wiring not invoked")
	}

	kcData, err := os.ReadFile(kc)
	if err != nil {
		t.Fatalf("kubeconfig not written: %v", err)
	}
	// Proves NodeAddress threaded through k3s.Bootstrap over the local transport.
	if !strings.Contains(string(kcData), "https://10.0.0.9:6443") {
		t.Errorf("kubeconfig not rewritten to the node address:\n%s", kcData)
	}
}

func TestInitLocalRequiresRoot(t *testing.T) {
	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return 1000 }

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, nodes: []string{"10.0.0.9"}, kubeconfig: kc}
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("want root refusal, got %v", err)
	}
	if _, statErr := os.Stat(kc); statErr == nil {
		t.Error("kubeconfig written despite the root refusal")
	}
}

func TestInitLocalRejectsSSHFlags(t *testing.T) {
	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return 0 }

	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw,
		&initOptions{local: true, nodes: []string{"n"}, sshKeyPath: "x"})
	if err == nil || !strings.Contains(err.Error(), "no SSH flags") {
		t.Errorf("want SSH-flag rejection, got %v", err)
	}

	// --ssh-user/--ssh-port are rejected only on NON-default values (the flags
	// always carry "root"/22, so defaults can't be told apart from unset).
	err = runInit(context.Background(), &out, &errw,
		&initOptions{local: true, nodes: []string{"n"}, sshUser: "ubuntu"})
	if err == nil || !strings.Contains(err.Error(), "no SSH flags") {
		t.Errorf("want --ssh-user rejection, got %v", err)
	}

	err = runInit(context.Background(), &out, &errw,
		&initOptions{local: true, nodes: []string{"n"}, sshPort: 2222})
	if err == nil || !strings.Contains(err.Error(), "no SSH flags") {
		t.Errorf("want --ssh-port rejection, got %v", err)
	}

	err = runInit(context.Background(), &out, &errw,
		&initOptions{local: true, nodes: []string{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "single node") {
		t.Errorf("want single-node rejection, got %v", err)
	}
}

func TestInitLocalPreflightRefuses(t *testing.T) {
	// A listener on the API server port makes ports.free fail (critical), so the
	// on-box preflight refuses before touching the machine.
	stubLocalNode(t, healthyNode("LISTEN 0 128 0.0.0.0:6443 0.0.0.0:*\n"))

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, nodes: []string{"10.0.0.9"}, k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second}
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("want preflight refusal, got %v", err)
	}
	if _, statErr := os.Stat(kc); statErr == nil {
		t.Error("kubeconfig written despite preflight refusal")
	}
}

func TestInitLocalSkipPreflight(t *testing.T) {
	// Same occupied port as the refusal test, but --skip-preflight bypasses the
	// gate. The flag-default --ssh-user/--ssh-port values must not trip the
	// SSH-flag rejection.
	stubLocalNode(t, healthyNode("LISTEN 0 128 0.0.0.0:6443 0.0.0.0:*\n"))
	stubLocalDeploy(t, "")
	stubLocalWire(t)

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{
		local: true, nodes: []string{"10.0.0.9"}, sshUser: "root", sshPort: 22,
		k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second,
		skipPreflight: true,
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit --local --skip-preflight: %v\n%s", err, errw.String())
	}
	// deployLocal returned no token (a re-run) — ADR-0003: shown exactly once,
	// ever, so the summary must say so and not print redemption instructions.
	if !strings.Contains(out.String(), "already generated on a previous run") {
		t.Errorf("summary missing the re-run token notice:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "plaintext token cannot be recovered") ||
		!strings.Contains(out.String(), "rollout restart deploy/orkano-dashboard") {
		t.Errorf("summary missing the bootstrap token recovery recipe:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Redeem it at first dashboard login") {
		t.Errorf("re-run summary must not print token redemption instructions:\n%s", out.String())
	}
}

func TestInitLocalPrintsFreshBootstrapTokenWhenDeployLaterFails(t *testing.T) {
	stubLocalNode(t, healthyNode(""))
	origDeploy := deployLocal
	t.Cleanup(func() { deployLocal = origDeploy })
	deployLocal = func(_ context.Context, _ io.Writer, _ *initOptions, _ localRunner) (string, error) {
		return "created-before-timeout", errors.New("operator never became ready")
	}

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, nodes: []string{"10.0.0.9"}, k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second}
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "operator never became ready") {
		t.Fatalf("expected deploy failure, got %v", err)
	}
	if !strings.Contains(out.String(), "created-before-timeout") ||
		!strings.Contains(out.String(), "will not be shown again") {
		t.Errorf("fresh token from failed deploy should still be printed:\n%s", out.String())
	}
}

func TestInitLocalPrintsFreshBootstrapTokenWhenWireRegistryFails(t *testing.T) {
	// Local analog of the SSH wire-registry token test.
	stubLocalNode(t, healthyNode(""))
	stubLocalDeploy(t, "local-wire-fail-token")
	orig := wireRegistryLocal
	t.Cleanup(func() { wireRegistryLocal = orig })
	wireRegistryLocal = func(_ context.Context, _ io.Writer, _ *initOptions, _ localRunner, _ string) error {
		return errors.New("registries.yaml write failed")
	}

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, nodes: []string{"10.0.0.9"}, k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second}
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "wire registry") {
		t.Fatalf("expected wire-registry failure, got %v", err)
	}
	if !strings.Contains(out.String(), "local-wire-fail-token") ||
		!strings.Contains(out.String(), "will not be shown again") {
		t.Errorf("fresh token should be printed before the wire-registry error returns:\n%s", out.String())
	}
}

func TestInitLocalRefusesOnAppArmorFailure(t *testing.T) {
	// The machine bootstraps fine but the AppArmor profile fails to load — the
	// on-box init must refuse (a node without the profile silently breaks every
	// build) and must not write the kubeconfig.
	stubLocalNode(t, failingAppArmorNode())

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, nodes: []string{"10.0.0.9"}, k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second}
	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, opt)
	if err == nil || !strings.Contains(err.Error(), "AppArmor profile") {
		t.Fatalf("want AppArmor load refusal, got %v", err)
	}
	if _, statErr := os.Stat(kc); statErr == nil {
		t.Error("kubeconfig written despite the AppArmor load failure")
	}
}

// TestInitLocalRunsComponentDeploy mirrors TestInitRunsComponentDeploy for the
// --local transport: the version/ACME/allowlist/receiver-host flags must thread
// into the component deploy, and the fresh token prints once.
func TestInitLocalRunsComponentDeploy(t *testing.T) {
	stubLocalNode(t, healthyNode(""))
	deploy := stubLocalDeploy(t, "the-local-token")
	stubLocalWire(t)

	opt := &initOptions{
		version:      "9.9.9",
		local:        true,
		nodes:        []string{"10.0.0.9"},
		k3sVersion:   "v1.35.5+k3s1",
		kubeconfig:   filepath.Join(t.TempDir(), "kubeconfig"),
		readyTimeout: 30 * time.Second,
		acmeEmail:    "ops@example.com",
		acmeProd:     true,
		allowRepos:   []string{"orkanoio/orkano"},
		receiverHost: "hooks.example.com",
	}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit --local: %v\n%s", err, errw.String())
	}
	if !deploy.called {
		t.Fatal("component deploy was not invoked")
	}
	if deploy.opt.version != "9.9.9" || deploy.opt.acmeEmail != "ops@example.com" || !deploy.opt.acmeProd {
		t.Errorf("deploy did not receive the version/ACME flags: %+v", deploy.opt)
	}
	if deploy.opt.receiverHost != "hooks.example.com" {
		t.Errorf("deploy did not receive --receiver-host: %q", deploy.opt.receiverHost)
	}
	if len(deploy.opt.allowRepos) != 1 || deploy.opt.allowRepos[0] != "orkanoio/orkano" {
		t.Errorf("deploy did not receive the allowlist: %v", deploy.opt.allowRepos)
	}
	if !strings.Contains(out.String(), "receiver:           https://hooks.example.com") {
		t.Errorf("summary missing the exposed-receiver line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the-local-token") {
		t.Errorf("bootstrap token not printed:\n%s", out.String())
	}
}

func TestInitLocalAddressDetectFailure(t *testing.T) {
	// No --node and no detectable primary IP: refuse with the pass-me-an-address
	// message before the runner is even created.
	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return 0 }
	origAddr := localAddress
	t.Cleanup(func() { localAddress = origAddr })
	localAddress = func() (string, error) { return "", errors.New("no route") }

	var out, errw bytes.Buffer
	err := runInit(context.Background(), &out, &errw, &initOptions{local: true})
	if err == nil || !strings.Contains(err.Error(), "could not determine") {
		t.Fatalf("want address-detection refusal pointing at --node, got %v", err)
	}
}

func TestInitLocalAutoDetectsAddress(t *testing.T) {
	stubLocalNode(t, healthyNode(""))
	stubLocalDeploy(t, "")
	stubLocalWire(t)
	origAddr := localAddress
	t.Cleanup(func() { localAddress = origAddr })
	localAddress = func() (string, error) { return "10.9.9.9", nil }

	kc := filepath.Join(t.TempDir(), "kubeconfig")
	opt := &initOptions{local: true, k3sVersion: "v1.35.5+k3s1", kubeconfig: kc, readyTimeout: 30 * time.Second}
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit --local auto-detect: %v\n%s", err, errw.String())
	}
	if !strings.Contains(out.String(), "10.9.9.9") {
		t.Errorf("summary missing the auto-detected address:\n%s", out.String())
	}
	if kcData, _ := os.ReadFile(kc); !strings.Contains(string(kcData), "https://10.9.9.9:6443") {
		t.Errorf("kubeconfig not rewritten to the auto-detected address:\n%s", kcData)
	}
}

func TestDetectPrimaryIP(t *testing.T) {
	ip, err := detectPrimaryIP()
	if err != nil {
		t.Skipf("no route on this host: %v", err)
	}
	if net.ParseIP(ip) == nil {
		t.Errorf("detectPrimaryIP returned %q, not an IP", ip)
	}
}
