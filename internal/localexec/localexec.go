// Package localexec is the on-box command transport for `orkano init --local`:
// it runs the same bootstrap engine (internal/k3s, internal/install,
// internal/nodeprep, internal/preflight) directly on the local machine over
// os/exec, without SSH. It satisfies those packages' Runner/Executor interface
// (Run(ctx, cmd) (ssh.Result, error)) unchanged, so the engine forks nowhere —
// the on-box install is a transport swap, not a second code path (ADR-0017).
package localexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/orkanoio/orkano/internal/ssh"
)

// DefaultShell is the shell each command runs under. The engine emits POSIX
// sh-compatible commands — the same strings sshd runs as `sh -c` on the remote
// node — so running them through sh locally matches the SSH transport exactly.
const DefaultShell = "sh"

// Runner runs commands on the local machine. It is stateless (there is no
// connection to cache) and safe for the install runner's sequential use.
type Runner struct {
	shell string
}

// New returns a Runner that executes commands via the default shell.
func New() *Runner {
	return &Runner{shell: DefaultShell}
}

// Run executes cmd locally and returns its output and exit status, mirroring
// ssh.Client.Run's contract exactly: a command that runs to completion with a
// non-zero exit is a Result with that ExitStatus and a nil error — the caller
// decides what an exit code means (load-bearing for the checks' error-vs-fail
// mapping). Only a failure to run the command at all (the shell missing, the
// process killed by a signal) returns an error. Run honours ctx: on
// cancellation the process is killed and ctx's error is returned.
func (r *Runner) Run(ctx context.Context, cmd string) (ssh.Result, error) {
	// The bootstrap engine's own composed commands run via sh -c — the entire
	// purpose of this on-box transport (ADR-0017). They are internally composed
	// (base64|tee writes, the version-pinned k3s installer), exactly what sshd
	// runs remotely for the SSH transport; there is no user-supplied command here.
	//nolint:gosec // G204: running the engine's composed commands is the point.
	c := exec.CommandContext(ctx, r.shell, "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	res := ssh.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}

	// A cancelled or expired ctx tears the process down; report it as ctx's error,
	// not as a command answer (mirrors ssh.Run's ctx.Done branch). Checked first
	// because CommandContext surfaces the kill as an ExitError with no clean exit.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ssh.Result{}, fmt.Errorf("localexec: run %q: %w", cmd, ctxErr)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.Exited() {
			// The process reported a clean non-zero exit — the command's own answer.
			res.ExitStatus = exitErr.ExitCode()
			return res, nil
		}
		// Killed by a signal without a clean exit: a transport-level failure, not
		// the command's answer (mirrors ssh.Run's non-ExitError branch).
		return ssh.Result{}, fmt.Errorf("localexec: run %q: killed: %w", cmd, err)
	}

	// The command could not be started at all (e.g. the shell is missing).
	return ssh.Result{}, fmt.Errorf("localexec: run %q: %w", cmd, err)
}
