package cmd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
		node:         host,
		sshUser:      srv.User,
		sshPort:      port,
		sshKeyPath:   writeTemp(t, "id", srv.ClientPrivateKey),
		hostKeyPath:  writeTemp(t, "hostkey", srv.HostKeyAuthorized),
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
	if !strings.Contains(string(kc), "https://"+opt.node+":6443") {
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
	if err := runInit(context.Background(), &out, &errw, &initOptions{node: "n"}); err == nil {
		t.Error("want error when --ssh-key is missing")
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
