package localexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/install"
	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/nodeprep"
	"github.com/orkanoio/orkano/internal/preflight"
)

// The whole point of ADR-0017 is that this runner drops into the existing
// engine's Runner/Executor slots unchanged. If any of those interfaces or this
// Run method drift, these assignments stop compiling — the drift guard.
var (
	_ install.Runner     = (*Runner)(nil)
	_ k3s.Runner         = (*Runner)(nil)
	_ nodeprep.Runner    = (*Runner)(nil)
	_ preflight.Executor = (*Runner)(nil)
)

func TestRunSuccess(t *testing.T) {
	res, err := New().Run(context.Background(), "echo out; echo err 1>&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err\n")
	}
	if res.ExitStatus != 0 {
		t.Errorf("exit = %d, want 0", res.ExitStatus)
	}
}

func TestRunNonZeroExitIsNotAnError(t *testing.T) {
	// A clean non-zero exit is the command's answer, not a Go error — the
	// contract the checks' error-vs-fail mapping depends on.
	res, err := New().Run(context.Background(), "echo before; exit 7")
	if err != nil {
		t.Fatalf("non-zero exit surfaced as an error: %v", err)
	}
	if res.ExitStatus != 7 {
		t.Errorf("exit = %d, want 7", res.ExitStatus)
	}
	if res.Stdout != "before\n" {
		t.Errorf("stdout = %q, want output captured before the exit", res.Stdout)
	}
}

func TestRunFalseExitsOne(t *testing.T) {
	res, err := New().Run(context.Background(), "false")
	if err != nil {
		t.Fatalf("Run false: %v", err)
	}
	if res.ExitStatus != 1 {
		t.Errorf("exit = %d, want 1", res.ExitStatus)
	}
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := New().Run(ctx, "sleep 5")
	if err == nil {
		t.Fatal("want an error when ctx expires mid-command")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if res.ExitStatus != 0 || res.Stdout != "" {
		t.Errorf("want an empty Result on cancellation, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run did not honour ctx promptly (took %s)", elapsed)
	}
}

func TestRunShellMissingIsAnError(t *testing.T) {
	// A missing shell means the command could not be started at all — a transport
	// failure, surfaced as a Go error (not a Result), distinct from a non-zero exit.
	r := &Runner{shell: "/nonexistent/orkano-no-such-shell"}
	res, err := r.Run(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("want an error when the shell cannot be started")
	}
	if !strings.Contains(err.Error(), "localexec:") {
		t.Errorf("error not wrapped: %v", err)
	}
	if res.ExitStatus != 0 || res.Stdout != "" {
		t.Errorf("want an empty Result when the command cannot start, got %+v", res)
	}
}

func TestNewUsesDefaultShell(t *testing.T) {
	if got := New().shell; got != DefaultShell {
		t.Errorf("New().shell = %q, want %q", got, DefaultShell)
	}
}

// Sanity: the runner satisfies the interfaces at runtime too (not just at
// compile time), so a nil-typed assertion above can never mask a real gap.
func TestRunnerIsAUsableExecutor(t *testing.T) {
	var exec preflight.Executor = New()
	if _, err := exec.Run(context.Background(), "true"); err != nil {
		t.Fatalf("Executor.Run: %v", err)
	}
}
