package preflight_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/preflight"
	"github.com/orkanoio/orkano/internal/ssh"
)

// fakeExecutor scripts a node's responses by exact command string.
type fakeExecutor struct {
	responses map[string]fakeResp
	calls     []string
}

type fakeResp struct {
	res ssh.Result
	err error
}

func (f *fakeExecutor) Run(_ context.Context, cmd string) (ssh.Result, error) {
	f.calls = append(f.calls, cmd)
	r, ok := f.responses[cmd]
	if !ok {
		return ssh.Result{}, errors.New("fakeExecutor: no scripted response for " + strconv.Quote(cmd))
	}
	return r.res, r.err
}

func out(stdout string) fakeResp { return fakeResp{res: ssh.Result{Stdout: stdout}} }
func exit(code int, stderr string) fakeResp {
	return fakeResp{res: ssh.Result{ExitStatus: code, Stderr: stderr}}
}
func fail(err string) fakeResp { return fakeResp{err: errors.New(err)} }

// probeByID runs a single named check's probe directly, for fine-grained
// branch assertions independent of the dependency graph.
func probeByID(t *testing.T, opt preflight.Options, id string) (check.Result, error) {
	t.Helper()
	for _, c := range preflight.Checks(opt) {
		if c.ID == id {
			return c.Probe(context.Background())
		}
	}
	t.Fatalf("no check with ID %q", id)
	return check.Result{}, nil
}

func assertStatus(t *testing.T, res check.Result, err error, want check.Status) {
	t.Helper()
	if err != nil {
		t.Fatalf("probe returned an error %v, want a %s result", err, want)
	}
	if res.Status != want {
		t.Fatalf("status = %q (%q), want %q", res.Status, res.Message, want)
	}
}

func assertProbeError(t *testing.T, res check.Result, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("probe returned status %q, want an error (unknown must never count as a result)", res.Status)
	}
}

func TestSSHReachable(t *testing.T) {
	t.Run("connects", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"true": out("")}}, Target: "root@node"}
		res, err := probeByID(t, opt, preflight.IDSSHReachable)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("transport failure is a fail not an error", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"true": fail("connection refused")}}, Target: "root@node"}
		res, err := probeByID(t, opt, preflight.IDSSHReachable)
		assertStatus(t, res, err, check.StatusFail)
		if !contains(res.Message, "root@node") || !contains(res.Message, "connection refused") {
			t.Errorf("message %q should name the target and the cause", res.Message)
		}
	})
	t.Run("non-zero exit of true is a fail", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"true": exit(1, "broken shell")}}}
		res, err := probeByID(t, opt, preflight.IDSSHReachable)
		assertStatus(t, res, err, check.StatusFail)
	})
}

func TestArchSupported(t *testing.T) {
	pass := []string{"x86_64", "amd64", "aarch64", "arm64", "x86_64\n", "  arm64  \n"}
	for _, machine := range pass {
		t.Run("pass/"+strconv.Quote(machine), func(t *testing.T) {
			opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"uname -m": out(machine)}}}
			res, err := probeByID(t, opt, preflight.IDArchSupported)
			assertStatus(t, res, err, check.StatusPass)
		})
	}
	failArch := []string{"armv7l", "i686", "ppc64le", "s390x", "riscv64"}
	for _, machine := range failArch {
		t.Run("fail/"+machine, func(t *testing.T) {
			opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"uname -m": out(machine)}}}
			res, err := probeByID(t, opt, preflight.IDArchSupported)
			assertStatus(t, res, err, check.StatusFail)
			if !contains(res.Message, machine) {
				t.Errorf("message %q should name the unsupported arch", res.Message)
			}
		})
	}
	t.Run("empty output errors", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"uname -m": out("\n")}}}
		_, err := probeByID(t, opt, preflight.IDArchSupported)
		assertProbeError(t, check.Result{}, err)
	})
	t.Run("non-zero exit errors", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"uname -m": exit(127, "uname: not found")}}}
		_, err := probeByID(t, opt, preflight.IDArchSupported)
		assertProbeError(t, check.Result{}, err)
	})
	t.Run("transport error errors", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"uname -m": fail("eof")}}}
		_, err := probeByID(t, opt, preflight.IDArchSupported)
		assertProbeError(t, check.Result{}, err)
	})
}

const ssListening = "LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"

func TestPortsFree(t *testing.T) {
	t.Run("all free", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": out(ssListening)}}}
		res, err := probeByID(t, opt, preflight.IDPortsFree)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("empty output passes", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": out("")}}}
		res, err := probeByID(t, opt, preflight.IDPortsFree)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("blank and malformed lines are skipped not false-positives", func(t *testing.T) {
		// A trailing newline, a whitespace-only line, and a too-short line must
		// not parse into a phantom occupied port.
		messy := "\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n   \nLISTEN garbage\n"
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": out(messy)}}}
		res, err := probeByID(t, opt, preflight.IDPortsFree)
		assertStatus(t, res, err, check.StatusPass)
	})
	occupied := []struct {
		name string
		line string
		port string
	}{
		{"ipv4", "LISTEN 0 128 0.0.0.0:80 0.0.0.0:*", "80"},
		{"ipv6 brackets", "LISTEN 0 128 [::]:443 [::]:*", "443"},
		{"wildcard", "LISTEN 0 128 *:6443 *:*", "6443"},
		{"specific addr", "LISTEN 0 128 192.168.1.5:10250 0.0.0.0:*", "10250"},
	}
	for _, tc := range occupied {
		t.Run("occupied/"+tc.name, func(t *testing.T) {
			opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": out(ssListening + tc.line + "\n")}}}
			res, err := probeByID(t, opt, preflight.IDPortsFree)
			assertStatus(t, res, err, check.StatusFail)
			if !contains(res.Message, tc.port) {
				t.Errorf("message %q should name occupied port %s", res.Message, tc.port)
			}
		})
	}
	t.Run("custom port list", func(t *testing.T) {
		opt := preflight.Options{
			Executor:      &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": out("LISTEN 0 128 0.0.0.0:9999 0.0.0.0:*\n")}},
			RequiredPorts: []int{9999},
		}
		res, err := probeByID(t, opt, preflight.IDPortsFree)
		assertStatus(t, res, err, check.StatusFail)
	})
	t.Run("ss failure errors", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": exit(127, "ss: command not found")}}}
		_, err := probeByID(t, opt, preflight.IDPortsFree)
		assertProbeError(t, check.Result{}, err)
	})
	t.Run("transport error errors", func(t *testing.T) {
		opt := preflight.Options{Executor: &fakeExecutor{responses: map[string]fakeResp{"ss -Hltn": fail("eof")}}}
		_, err := probeByID(t, opt, preflight.IDPortsFree)
		assertProbeError(t, check.Result{}, err)
	})
}

func TestTimeSynced(t *testing.T) {
	fixed := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixed }
	dateOut := func(offset time.Duration) fakeResp {
		return out(strconv.FormatInt(fixed.Add(offset).Unix(), 10) + "\n")
	}

	t.Run("in sync passes", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(0)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("within tolerance passes", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(3 * time.Second)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("node ahead fails", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(10 * time.Minute)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusFail)
		if !contains(res.Message, "ahead") {
			t.Errorf("message %q should say the node is ahead", res.Message)
		}
	})
	t.Run("node behind fails", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(-2 * time.Hour)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusFail)
		if !contains(res.Message, "behind") {
			t.Errorf("message %q should say the node is behind", res.Message)
		}
	})
	t.Run("custom tolerance", func(t *testing.T) {
		opt := preflight.Options{Now: now, MaxClockSkew: time.Hour, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(30 * time.Minute)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("exactly at tolerance passes", func(t *testing.T) {
		// The comparison is strictly greater-than, so the boundary passes.
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(preflight.DefaultMaxClockSkew)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusPass)
	})
	t.Run("one second over tolerance fails", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": dateOut(preflight.DefaultMaxClockSkew + time.Second)}}}
		res, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertStatus(t, res, err, check.StatusFail)
	})
	t.Run("garbage time errors", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": out("not-a-number\n")}}}
		_, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertProbeError(t, check.Result{}, err)
	})
	t.Run("non-zero exit errors", func(t *testing.T) {
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": exit(1, "date: bad")}}}
		_, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertProbeError(t, check.Result{}, err)
	})
	t.Run("transport error errors", func(t *testing.T) {
		// The load-bearing case: a transport failure must be an error (unknown),
		// never a StatusFail. Mirrors arch/ports.
		opt := preflight.Options{Now: now, Executor: &fakeExecutor{responses: map[string]fakeResp{"date -u +%s": fail("eof")}}}
		_, err := probeByID(t, opt, preflight.IDTimeSynced)
		assertProbeError(t, check.Result{}, err)
	})
}

// TestContract pins the permanent IDs, severities, and the dependency edges:
// the three node checks must require ssh.reachable so the runner blocks them on
// an unreachable node.
func TestContract(t *testing.T) {
	want := map[string]check.Severity{
		preflight.IDSSHReachable:  check.SeverityCritical,
		preflight.IDArchSupported: check.SeverityCritical,
		preflight.IDPortsFree:     check.SeverityCritical,
		preflight.IDTimeSynced:    check.SeverityWarning,
	}
	got := preflight.Checks(preflight.Options{})
	if len(got) != len(want) {
		t.Fatalf("Checks returned %d checks, want %d", len(got), len(want))
	}
	for _, c := range got {
		sev, ok := want[c.ID]
		if !ok {
			t.Fatalf("unexpected check ID %q", c.ID)
		}
		if c.Severity != sev {
			t.Errorf("%s severity = %q, want %q", c.ID, c.Severity, sev)
		}
		if c.Probe == nil {
			t.Errorf("%s has no Probe", c.ID)
		}
		if c.ID != preflight.IDSSHReachable {
			if !slices.Contains(c.Requires, preflight.IDSSHReachable) {
				t.Errorf("%s does not require %s", c.ID, preflight.IDSSHReachable)
			}
		}
	}
}

// TestRegisterAndPlanOrder proves the checks register cleanly and the runner
// plans ssh.reachable first.
func TestRegisterAndPlanOrder(t *testing.T) {
	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: &fakeExecutor{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	plan, err := reg.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan[0].ID != preflight.IDSSHReachable {
		t.Fatalf("plan[0] = %s, want %s first", plan[0].ID, preflight.IDSSHReachable)
	}
}

// TestUnreachableBlocksTheRest is the headline of the dependency wiring: when
// ssh.reachable fails, init must refuse (ExitCritical) and the node checks must
// be reported blocked, not run.
func TestUnreachableBlocksTheRest(t *testing.T) {
	reg := checks.New()
	exec := &fakeExecutor{responses: map[string]fakeResp{"true": fail("connection refused")}}
	if err := preflight.Register(reg, preflight.Options{Executor: exec}); err != nil {
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
	if !slices.Contains(exec.calls, "true") {
		t.Error("ssh.reachable did not probe")
	}
	if slices.Contains(exec.calls, "uname -m") {
		t.Error("a blocked check was probed (uname ran)")
	}
	if run.ExitCode() != checks.ExitCritical {
		t.Errorf("ExitCode = %d, want ExitCritical (init must refuse)", run.ExitCode())
	}
}

// TestHappyPathPasses runs the whole registry against a node that answers every
// probe well: every check passes and the gate clears.
func TestHappyPathPasses(t *testing.T) {
	fixed := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	exec := &fakeExecutor{responses: map[string]fakeResp{
		"true":        out(""),
		"uname -m":    out("x86_64\n"),
		"ss -Hltn":    out(ssListening),
		"date -u +%s": out(strconv.FormatInt(fixed.Unix(), 10) + "\n"),
	}}
	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: exec, Now: func() time.Time { return fixed }}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	run, err := reg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !run.OK() {
		t.Fatalf("run did not clear the gate: %+v", run.Summary())
	}
	if s := run.Summary(); s.Passed != 4 {
		t.Errorf("passed = %d, want 4", s.Passed)
	}
}

func outcome(run *checks.Run, id string) checks.Outcome {
	for _, res := range run.Results {
		if res.ID == id {
			return res.Outcome
		}
	}
	return checks.Outcome("<absent>")
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
