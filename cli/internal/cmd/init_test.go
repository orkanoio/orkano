package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// healthyNode answers both the preflight probes and the k3s bootstrap commands
// for a node that installs cleanly and is immediately Ready. ssOutput is the
// `ss -Hltn` body, so a test can make a required port look occupied.
func healthyNode(ssOutput string) sshtest.ExecHandler {
	installed := false
	files := map[string]string{}
	return func(raw string) (string, string, int) {
		cmd := strings.ReplaceAll(raw, "sudo ", "")
		switch {
		// preflight
		case cmd == "true":
			return "", "", 0
		case cmd == "uname -m":
			return "x86_64\n", "", 0
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
			files["written"] = cmd
			return "", "", 0
		case cmd == "cat /etc/rancher/k3s/k3s.yaml":
			return "clusters:\n- cluster:\n    server: https://127.0.0.1:6443\n", "", 0
		case cmd == "cat /var/lib/rancher/k3s/server/token":
			return "K10cafef00d::server:abc123def456\n", "", 0
		case strings.HasPrefix(cmd, "cat "):
			return "", "no such file", 1
		case strings.HasPrefix(cmd, "sysctl -p"), strings.HasPrefix(cmd, "mkdir -p -m 700"):
			return "", "", 0
		case strings.Contains(cmd, "get.k3s.io"):
			installed = true
			return "installing\n", "", 0
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

func TestInitHappyPath(t *testing.T) {
	srv := sshtest.New(healthyNode(""))
	defer srv.Close()

	opt := baseOptions(t, srv)
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit: %v\nstderr:\n%s", err, errw.String())
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

	opt := baseOptions(t, srv)
	opt.skipPreflight = true
	var out, errw bytes.Buffer
	if err := runInit(context.Background(), &out, &errw, opt); err != nil {
		t.Fatalf("runInit with --skip-preflight: %v", err)
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
	type call struct {
		node string
		cfg  k3s.Config
	}
	var calls []call

	orig := bootstrapOne
	defer func() { bootstrapOne = orig }()
	bootstrapOne = func(_ context.Context, _, _ io.Writer, _ *initOptions, _ []byte, node, _ string, cfg k3s.Config) (*k3s.Result, error) {
		calls = append(calls, call{node, cfg})
		res := &k3s.Result{Version: "v1.35.5+k3s1", SecretsEncryption: "Enabled", AuditLogPresent: true, Changed: true}
		if cfg.ServerURL == "" { // the first (cluster-init) server
			res.Token = token
			res.Kubeconfig = []byte("kubeconfig-from-first\n")
		}
		return res, nil
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
	if !strings.Contains(out.String(), "load balancer") {
		t.Errorf("summary missing the HA kubeconfig caveat:\n%s", out.String())
	}
	if strings.Contains(out.String(), token) || strings.Contains(errw.String(), token) {
		t.Error("join token leaked into CLI output")
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
