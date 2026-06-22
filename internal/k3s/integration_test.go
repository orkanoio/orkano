package k3s_test

import (
	"context"
	"encoding/base64"
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
		return "node1 Ready control-plane,etcd,master 30s v1.35.5+k3s1\n", "", 0
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
