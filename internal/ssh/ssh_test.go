package ssh_test

import (
	"context"
	"strings"
	"testing"
	"time"

	orkssh "github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// dial builds a Client against srv with the credentials srv hands out.
func dial(t *testing.T, srv *sshtest.Server) *orkssh.Client {
	t.Helper()
	c, err := orkssh.New(orkssh.Config{
		Addr:       srv.Addr,
		User:       srv.User,
		PrivateKey: srv.ClientPrivateKey,
		HostKey:    srv.HostKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRunSuccess(t *testing.T) {
	srv := sshtest.New(func(cmd string) (string, string, int) {
		if cmd != "echo hi" {
			return "", "unexpected", 1
		}
		return "hi\n", "", 0
	})
	defer srv.Close()

	res, err := dial(t, srv).Run(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hi\n")
	}
	if res.ExitStatus != 0 {
		t.Errorf("ExitStatus = %d, want 0", res.ExitStatus)
	}
}

func TestRunNonZeroExitIsNotAnError(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) {
		return "out", "boom\n", 3
	})
	defer srv.Close()

	res, err := dial(t, srv).Run(context.Background(), "false")
	if err != nil {
		t.Fatalf("Run returned an error for a clean non-zero exit: %v", err)
	}
	if res.ExitStatus != 3 {
		t.Errorf("ExitStatus = %d, want 3", res.ExitStatus)
	}
	if res.Stdout != "out" || res.Stderr != "boom\n" {
		t.Errorf("output = (%q,%q), want (out,boom\\n)", res.Stdout, res.Stderr)
	}
}

func TestRunReceivesCommand(t *testing.T) {
	var got string
	srv := sshtest.New(func(cmd string) (string, string, int) {
		got = cmd
		return "", "", 0
	})
	defer srv.Close()

	if _, err := dial(t, srv).Run(context.Background(), "uname -m"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "uname -m" {
		t.Errorf("server saw %q, want %q", got, "uname -m")
	}
}

func TestConnectionIsCachedAcrossRuns(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	c := dial(t, srv)
	for i := 0; i < 3; i++ {
		if _, err := c.Run(context.Background(), "true"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if n := srv.Connections(); n != 1 {
		t.Fatalf("server accepted %d connections, want 1 (the client must reuse one)", n)
	}
}

func TestUnreachableNodeErrors(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	addr := srv.Addr
	hostKey := srv.HostKeyAuthorized
	clientKey := srv.ClientPrivateKey
	srv.Close() // the address is now refused

	c, err := orkssh.New(orkssh.Config{Addr: addr, User: "orkano", PrivateKey: clientKey, HostKey: hostKey, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Run(context.Background(), "true"); err == nil {
		t.Fatal("Run against a closed address succeeded, want a dial error")
	}
}

func TestWrongHostKeyFailsHandshake(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	// Pin a host key the server does not present.
	_, otherHostKey := sshtest.GenerateKey()
	c, err := orkssh.New(orkssh.Config{
		Addr:       srv.Addr,
		User:       srv.User,
		PrivateKey: srv.ClientPrivateKey,
		HostKey:    otherHostKey,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Run(context.Background(), "true"); err == nil {
		t.Fatal("handshake with a mismatched host key succeeded, want a verification failure")
	}
}

func TestUnauthorizedKeyFailsAuth(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	// A valid key the server does not authorise.
	otherKey, _ := sshtest.GenerateKey()
	c, err := orkssh.New(orkssh.Config{
		Addr:       srv.Addr,
		User:       srv.User,
		PrivateKey: otherKey,
		HostKey:    srv.HostKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Run(context.Background(), "true"); err == nil {
		t.Fatal("auth with an unauthorized key succeeded, want a failure")
	}
}

func TestWrongUsernameFailsAuth(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	// The authorised key, but a username the server does not authorise.
	c, err := orkssh.New(orkssh.Config{
		Addr:       srv.Addr,
		User:       "intruder",
		PrivateKey: srv.ClientPrivateKey,
		HostKey:    srv.HostKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Run(context.Background(), "true"); err == nil {
		t.Fatal("auth with the wrong username succeeded, want a failure")
	}
}

// TestReconnectsAfterDrop proves the client does not wedge: when the cached
// connection breaks, a later Run drops it and reconnects rather than failing
// forever on the dead client.
func TestReconnectsAfterDrop(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "ok", "", 0 })
	defer srv.Close()

	c := dial(t, srv)
	if _, err := c.Run(context.Background(), "true"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	srv.DropConns() // the cached connection is now dead, the listener still open

	var recovered bool
	for i := 0; i < 5; i++ {
		if _, err := c.Run(context.Background(), "true"); err == nil {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatal("client never recovered after the connection dropped (it wedged)")
	}
	if n := srv.Connections(); n < 2 {
		t.Fatalf("server saw %d connections, want >= 2 (a reconnect)", n)
	}
}

func TestCancelledContextDoesNotConnect(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(t, srv).Run(ctx, "true"); err == nil {
		t.Fatal("Run under a cancelled context succeeded, want ctx error")
	}
}

func TestNewValidation(t *testing.T) {
	goodKey, goodHost := sshtest.GenerateKey()
	base := orkssh.Config{Addr: "node:22", User: "orkano", PrivateKey: goodKey, HostKey: goodHost}

	tests := []struct {
		name   string
		mutate func(*orkssh.Config)
	}{
		{"missing user", func(c *orkssh.Config) { c.User = "" }},
		{"missing addr", func(c *orkssh.Config) { c.Addr = "" }},
		{"missing private key", func(c *orkssh.Config) { c.PrivateKey = nil }},
		{"missing host key", func(c *orkssh.Config) { c.HostKey = nil }},
		{"garbage private key", func(c *orkssh.Config) { c.PrivateKey = []byte("not a key") }},
		{"garbage host key", func(c *orkssh.Config) { c.HostKey = []byte("not a key") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			if _, err := orkssh.New(cfg); err == nil {
				t.Fatalf("New(%s) = nil error, want a validation error", tt.name)
			}
		})
	}

	// The happy path parses and defaults the port.
	if _, err := orkssh.New(orkssh.Config{Addr: "node", User: "orkano", PrivateKey: goodKey, HostKey: goodHost}); err != nil {
		t.Fatalf("New(valid, bare host) = %v, want nil", err)
	}
}

func TestRunStderrCaptured(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "warning: x\n", 0 })
	defer srv.Close()

	res, err := dial(t, srv).Run(context.Background(), "noisy")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stderr, "warning: x") {
		t.Errorf("Stderr = %q, want it to contain the warning", res.Stderr)
	}
}
