package k3s_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// scriptedNode is a minimal healthy node: nothing installed yet, install
// succeeds, the node is immediately Ready, encryption is enabled, the audit log
// exists. It records files written so the test can assert the rendered config.
type scriptedNode struct {
	mu        sync.Mutex
	files     map[string]string
	installed bool
	// existingReady seeds the Ready count with peers already up (a joiner's first
	// server); readyAfter delays this node's own Ready by that many polls so the
	// HA test exercises waitReady's count gate over real SSH.
	existingReady int
	readyAfter    int
	getNodesCalls int
}

func (n *scriptedNode) handle(raw string) (string, string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cmd := strings.ReplaceAll(raw, "sudo ", "")

	switch {
	case strings.HasSuffix(cmd, "/k3s --version"):
		if !n.installed {
			return "", "not found", 127
		}
		return "k3s version v1.35.5+k3s1 (abc)\n", "", 0
	case strings.Contains(cmd, "| base64 -d |"):
		path, content := decodeWrite(cmd)
		n.files[path] = content
		return "", "", 0
	case cmd == "cat /etc/rancher/k3s/k3s.yaml":
		return "apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\n", "", 0
	case cmd == "cat /var/lib/rancher/k3s/server/token":
		return "K10cafef00d::server:abc123def456\n", "", 0
	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimPrefix(cmd, "cat ")
		if c, ok := n.files[path]; ok {
			return c, "", 0
		}
		return "", "no such file", 1
	case strings.HasPrefix(cmd, "sysctl -p"), strings.HasPrefix(cmd, "mkdir -p -m 700"):
		return "", "", 0
	case strings.Contains(cmd, "get.k3s.io"):
		n.installed = true
		return "installing\n", "", 0
	case strings.Contains(cmd, "kubectl get nodes"):
		n.getNodesCalls++
		ready := n.existingReady
		if n.getNodesCalls > n.readyAfter {
			ready++ // this node has joined and become Ready
		}
		if ready == 0 {
			return "node1 NotReady control-plane,etcd,master 3s v1.35.5+k3s1\n", "", 0
		}
		var b strings.Builder
		for i := 0; i < ready; i++ {
			fmt.Fprintf(&b, "node%d Ready control-plane,etcd,master 30s v1.35.5+k3s1\n", i+1)
		}
		return b.String(), "", 0
	case strings.Contains(cmd, "secrets-encrypt status"):
		return "Encryption Status: Enabled\n", "", 0
	case strings.HasPrefix(cmd, "test -f "):
		return "", "", 0
	default:
		return "", "unexpected: " + cmd, 127
	}
}

func decodeWrite(cmd string) (path, content string) {
	const start = "printf %s '"
	i := strings.Index(cmd, start)
	j := strings.Index(cmd, "' | base64 -d")
	dec, _ := base64.StdEncoding.DecodeString(cmd[i+len(start) : j])
	const teeMark = "tee "
	rest := cmd[strings.Index(cmd, teeMark)+len(teeMark):]
	return strings.TrimSpace(rest[:strings.Index(rest, " >/dev/null")]), string(dec)
}

// TestBootstrapOverRealSSH proves the real SSH client drives the whole bootstrap
// against a server speaking the genuine wire protocol, on one shared connection.
func TestBootstrapOverRealSSH(t *testing.T) {
	node := &scriptedNode{files: map[string]string{}}
	srv := sshtest.New(node.handle)
	defer srv.Close()

	c, err := ssh.New(ssh.Config{
		Addr:       srv.Addr,
		User:       srv.User,
		PrivateKey: srv.ClientPrivateKey,
		HostKey:    srv.HostKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("ssh.New: %v", err)
	}
	defer func() { _ = c.Close() }()

	res, err := k3s.Bootstrap(context.Background(), c, k3s.Config{NodeAddress: "198.51.100.7", Sudo: true})
	if err != nil {
		t.Fatalf("Bootstrap over SSH: %v", err)
	}
	if res.AlreadyInstalled || !res.Changed {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.SecretsEncryption != "Enabled" || !res.AuditLogPresent {
		t.Errorf("hardening not verified: %+v", res)
	}
	if !strings.Contains(string(res.Kubeconfig), "https://198.51.100.7:6443") {
		t.Errorf("kubeconfig not rewritten:\n%s", res.Kubeconfig)
	}
	node.mu.Lock()
	cfg := node.files["/etc/rancher/k3s/config.yaml"]
	node.mu.Unlock()
	if !strings.Contains(cfg, `- "198.51.100.7"`) {
		t.Errorf("config.yaml SAN not rendered over SSH:\n%s", cfg)
	}
	if n := srv.Connections(); n != 1 {
		t.Errorf("server saw %d connections, want 1 (shared SSH connection)", n)
	}
}

func dialSSH(t *testing.T, srv *sshtest.Server) *ssh.Client {
	t.Helper()
	c, err := ssh.New(ssh.Config{
		Addr:       srv.Addr,
		User:       srv.User,
		PrivateKey: srv.ClientPrivateKey,
		HostKey:    srv.HostKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("ssh.New: %v", err)
	}
	return c
}

// TestBootstrapHAJoinOverRealSSH proves the two-server HA flow over the genuine
// SSH wire protocol: the first server initialises the cluster and yields a join
// token, which the second server consumes to join — its rendered config carries
// the server URL + shared token and no cluster-init.
func TestBootstrapHAJoinOverRealSSH(t *testing.T) {
	first := &scriptedNode{files: map[string]string{}}
	srv1 := sshtest.New(first.handle)
	defer srv1.Close()
	c1 := dialSSH(t, srv1)
	defer func() { _ = c1.Close() }()

	res1, err := k3s.Bootstrap(context.Background(), c1, k3s.Config{NodeAddress: "198.51.100.7", Sudo: true})
	if err != nil {
		t.Fatalf("bootstrap first server: %v", err)
	}
	if res1.Token == "" {
		t.Fatal("first server returned no join token")
	}

	// existingReady=1 (first server up), readyAfter=1 so this node is Ready only
	// on the second poll — exercising waitReady's count gate over the real wire.
	second := &scriptedNode{files: map[string]string{}, existingReady: 1, readyAfter: 1}
	srv2 := sshtest.New(second.handle)
	defer srv2.Close()
	c2 := dialSSH(t, srv2)
	defer func() { _ = c2.Close() }()

	res2, err := k3s.Bootstrap(context.Background(), c2, k3s.Config{
		NodeAddress:   "198.51.100.8",
		ServerURL:     "https://198.51.100.7:6443",
		Token:         res1.Token,
		MinReadyNodes: 2,
		Sudo:          true, // same operator/user as the first server
	})
	if err != nil {
		t.Fatalf("bootstrap joining server: %v", err)
	}
	if res2.Token != "" {
		t.Error("joining server should not surface a token")
	}
	if n := srv2.Connections(); n != 1 {
		t.Errorf("joiner saw %d connections, want 1 (shared SSH connection across polls)", n)
	}

	second.mu.Lock()
	cfg2 := second.files["/etc/rancher/k3s/config.yaml"]
	second.mu.Unlock()
	if !strings.Contains(cfg2, `server: "https://198.51.100.7:6443"`) {
		t.Errorf("joiner config missing the server URL:\n%s", cfg2)
	}
	if !strings.Contains(cfg2, res1.Token) {
		t.Errorf("joiner config missing the shared token:\n%s", cfg2)
	}
	if strings.Contains(cfg2, "cluster-init") {
		t.Errorf("joiner config must not set cluster-init:\n%s", cfg2)
	}
}
