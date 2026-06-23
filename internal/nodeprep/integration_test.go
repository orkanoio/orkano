package nodeprep_test

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/orkanoio/orkano/internal/nodeprep"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// apparmorNode is a minimal AppArmor-capable node spoken to over the genuine SSH
// wire protocol: it persists the installed profile file and the kernel load
// state across commands (and across two Ensure calls) so the integration test
// exercises both the load and the idempotent re-run.
type apparmorNode struct {
	mu     sync.Mutex
	files  map[string]string
	loaded bool
	mode   string
}

func (n *apparmorNode) handle(raw string) (string, string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cmd := strings.ReplaceAll(raw, "sudo ", "")

	switch {
	case strings.Contains(cmd, "apparmor_parser -r"):
		n.loaded = true
		n.mode = "enforce"
		return "", "", 0
	case cmd == "cat /sys/kernel/security/apparmor/profiles":
		out := "cri-containerd.apparmor.d (enforce)\n"
		if n.loaded {
			out += nodeprep.ProfileName + " (" + n.mode + ")\n"
		}
		return out, "", 0
	case strings.Contains(cmd, "| base64 -d |"):
		path, content := decodeWrite(cmd)
		n.files[path] = content
		return "", "", 0
	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimPrefix(cmd, "cat ")
		if c, ok := n.files[path]; ok {
			return c, "", 0
		}
		return "", "no such file", 1
	default:
		return "", "unexpected: " + cmd, 127
	}
}

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

// TestEnsureOverRealSSH proves the real SSH client drives the profile install +
// load against a server speaking the genuine wire protocol on one shared
// connection, and that a second Ensure on the same node is a clean no-op.
func TestEnsureOverRealSSH(t *testing.T) {
	node := &apparmorNode{files: map[string]string{}}
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

	res, err := nodeprep.EnsureAppArmorProfile(context.Background(), nodeprep.Options{Runner: c, Sudo: true})
	if err != nil {
		t.Fatalf("first Ensure over SSH: %v", err)
	}
	if !res.Changed || res.Mode != "enforce" {
		t.Errorf("first Ensure: got %+v, want Changed=true Mode=enforce", res)
	}

	node.mu.Lock()
	written := node.files["/etc/apparmor.d/"+nodeprep.ProfileName]
	node.mu.Unlock()
	if !strings.Contains(written, "profile "+nodeprep.ProfileName) {
		t.Errorf("profile not written over SSH:\n%s", written)
	}

	res2, err := nodeprep.EnsureAppArmorProfile(context.Background(), nodeprep.Options{Runner: c, Sudo: true})
	if err != nil {
		t.Fatalf("second Ensure over SSH: %v", err)
	}
	if res2.Changed {
		t.Error("second Ensure should be a no-op")
	}

	if n := srv.Connections(); n != 1 {
		t.Errorf("server saw %d connections, want 1 (shared SSH connection across both Ensure calls)", n)
	}
}
