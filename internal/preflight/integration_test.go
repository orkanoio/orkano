package preflight_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/preflight"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

// *ssh.Client must satisfy the Executor the checks close over — the whole point
// of the package is that the real SSH client drives them.
var _ preflight.Executor = (*ssh.Client)(nil)

// nodeScript answers the four probe commands the way a healthy node would.
func nodeScript(ssOutput string) sshtest.ExecHandler {
	return func(cmd string) (string, string, int) {
		switch cmd {
		case "true":
			return "", "", 0
		case "uname -m":
			return "x86_64\n", "", 0
		case "ss -Hltn":
			return ssOutput, "", 0
		case "date -u +%s":
			return strconv.FormatInt(time.Now().Unix(), 10) + "\n", "", 0
		default:
			return "", "unexpected command: " + cmd, 127
		}
	}
}

func clientFor(t *testing.T, srv *sshtest.Server) *ssh.Client {
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
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestPreflightOverRealSSH is the behavioral proof: the real client speaks the
// SSH wire protocol to a real server, the probes parse genuine command output,
// and a healthy node clears the gate — reusing a single connection.
func TestPreflightOverRealSSH(t *testing.T) {
	srv := sshtest.New(nodeScript("LISTEN 0 128 127.0.0.1:5432 0.0.0.0:*\n"))
	defer srv.Close()

	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: clientFor(t, srv), Target: srv.User + "@" + srv.Addr}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	run, err := reg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !run.OK() {
		t.Fatalf("healthy node did not clear the gate: %+v", run.Results)
	}
	// run.OK() only gates on critical checks; assert the warning-severity
	// time.synced explicitly so its date-parsing path is covered end to end.
	for _, id := range []string{preflight.IDSSHReachable, preflight.IDArchSupported, preflight.IDPortsFree, preflight.IDTimeSynced} {
		if got := outcome(run, id); got != checks.OutcomePass {
			t.Errorf("%s outcome = %s, want pass", id, got)
		}
	}
	if n := srv.Connections(); n != 1 {
		t.Errorf("server saw %d connections, want 1 (one shared SSH connection)", n)
	}
}

// TestPreflightOccupiedPortFailsOverSSH proves a real failure: a listener on a
// required port makes ports.free fail and the gate refuse.
func TestPreflightOccupiedPortFailsOverSSH(t *testing.T) {
	srv := sshtest.New(nodeScript("LISTEN 0 128 0.0.0.0:6443 0.0.0.0:*\n"))
	defer srv.Close()

	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: clientFor(t, srv)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	run, err := reg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := outcome(run, preflight.IDPortsFree); got != checks.OutcomeFail {
		t.Errorf("ports.free outcome = %s, want fail", got)
	}
	if run.ExitCode() != checks.ExitCritical {
		t.Errorf("ExitCode = %d, want ExitCritical", run.ExitCode())
	}
}

// TestPreflightUnreachableOverSSH points the client at a closed address: every
// node check must block behind ssh.reachable, exactly as the runner intends.
func TestPreflightUnreachableOverSSH(t *testing.T) {
	srv := sshtest.New(nodeScript(""))
	addr, hostKey, clientKey, user := srv.Addr, srv.HostKeyAuthorized, srv.ClientPrivateKey, srv.User
	srv.Close() // the address is now refused

	c, err := ssh.New(ssh.Config{Addr: addr, User: user, PrivateKey: clientKey, HostKey: hostKey, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("ssh.New: %v", err)
	}
	defer func() { _ = c.Close() }()

	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: c}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	run, err := reg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := outcome(run, preflight.IDSSHReachable); got != checks.OutcomeFail {
		t.Errorf("ssh.reachable outcome = %s, want fail", got)
	}
	for _, id := range []string{preflight.IDArchSupported, preflight.IDPortsFree, preflight.IDTimeSynced} {
		if got := outcome(run, id); got != checks.OutcomeBlocked {
			t.Errorf("%s outcome = %s, want blocked", id, got)
		}
	}
}
