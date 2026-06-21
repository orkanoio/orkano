package checks

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/api/check"
)

// fixed returns a check that always reports the given status. counter, when
// non-nil, is incremented every time the probe is invoked, so a test can prove
// a probe was (or was not) run.
func fixed(id string, sev check.Severity, status check.Status, counter *int, requires ...string) check.Check {
	return check.Check{
		ID:       id,
		Severity: sev,
		Requires: requires,
		Probe: func(context.Context) (check.Result, error) {
			if counter != nil {
				*counter++
			}
			return check.Result{Status: status, Message: id + " ran"}, nil
		},
	}
}

func runIDs(t *testing.T, r *Registry) *Run {
	t.Helper()
	run, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return run
}

func outcomeOf(run *Run, id string) Outcome {
	for _, res := range run.Results {
		if res.ID == id {
			return res.Outcome
		}
	}
	return Outcome("<absent>")
}

func blockersOf(run *Run, id string) []string {
	for _, res := range run.Results {
		if res.ID == id {
			return res.Blockers
		}
	}
	return nil
}

func TestRunMapsStatuses(t *testing.T) {
	r := New()
	r.MustRegister(fixed("p", check.SeverityInfo, check.StatusPass, nil))
	r.MustRegister(fixed("f", check.SeverityInfo, check.StatusFail, nil))
	r.MustRegister(fixed("s", check.SeverityInfo, check.StatusSkip, nil))

	run := runIDs(t, r)
	for id, want := range map[string]Outcome{"p": OutcomePass, "f": OutcomeFail, "s": OutcomeSkip} {
		if got := outcomeOf(run, id); got != want {
			t.Errorf("%s outcome = %s, want %s", id, got, want)
		}
	}
}

func TestRunProbeErrorIsErrorNotFail(t *testing.T) {
	r := New()
	sentinel := errors.New("network down")
	r.MustRegister(check.Check{
		ID:       "net",
		Severity: check.SeverityCritical,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{}, sentinel
		},
	})

	run := runIDs(t, r)
	res := run.Results[0]
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %s, want error (probe error must never be a fail)", res.Outcome)
	}
	if !errors.Is(res.Err, sentinel) {
		t.Fatalf("Err = %v, want the probe's own error (stored as-is)", res.Err)
	}
}

func TestRunProbePanicBecomesError(t *testing.T) {
	r := New()
	r.MustRegister(check.Check{
		ID:       "boom",
		Severity: check.SeverityWarning,
		Probe: func(context.Context) (check.Result, error) {
			panic("contributor bug")
		},
	})

	run := runIDs(t, r)
	res := run.Results[0]
	if res.Outcome != OutcomeError {
		t.Fatalf("panicking probe outcome = %s, want error", res.Outcome)
	}
	if !strings.Contains(res.Message, "goroutine") {
		t.Fatalf("panic message lacks a stack trace: %q", res.Message)
	}
}

func TestRunPanicWithErrorKeepsChain(t *testing.T) {
	r := New()
	sentinel := errors.New("nil map write")
	r.MustRegister(check.Check{
		ID:       "boom",
		Severity: check.SeverityWarning,
		Probe: func(context.Context) (check.Result, error) {
			panic(sentinel)
		},
	})

	res := runIDs(t, r).Results[0]
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %s, want error", res.Outcome)
	}
	if !errors.Is(res.Err, sentinel) {
		t.Fatalf("Err = %v, want the panicked error preserved via %%w", res.Err)
	}
}

func TestRunUnknownStatusBecomesError(t *testing.T) {
	r := New()
	r.MustRegister(check.Check{
		ID:       "weird",
		Severity: check.SeverityInfo,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: "maybe", Message: "connection timed out"}, nil
		},
	})
	res := runIDs(t, r).Results[0]
	if res.Outcome != OutcomeError {
		t.Fatalf("unknown status outcome = %s, want error", res.Outcome)
	}
	if !strings.Contains(res.Message, "connection timed out") {
		t.Fatalf("the probe's own message was dropped: %q", res.Message)
	}
}

func TestRunCancelledContextDoesNotProbe(t *testing.T) {
	r := New()
	var probed int
	// Three independent checks: a cancelled context must error every one, and
	// the run must not short-circuit — preflight and doctor need the full picture.
	for _, id := range []string{"a", "b", "c"} {
		r.MustRegister(fixed(id, check.SeverityCritical, check.StatusPass, &probed))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probed != 0 {
		t.Fatalf("a probe ran %d times under a cancelled context, want 0", probed)
	}
	if len(run.Results) != 3 {
		t.Fatalf("Results len = %d, want 3 (the run must not short-circuit)", len(run.Results))
	}
	for _, res := range run.Results {
		if res.Outcome != OutcomeError {
			t.Fatalf("%s outcome = %s, want error under cancellation", res.ID, res.Outcome)
		}
	}
}

func TestRunBlocksOnFailedRequirement(t *testing.T) {
	r := New()
	var depProbed int
	r.MustRegister(fixed("req", check.SeverityCritical, check.StatusFail, nil))
	r.MustRegister(fixed("dep", check.SeverityCritical, check.StatusPass, &depProbed, "req"))

	run := runIDs(t, r)
	if got := outcomeOf(run, "dep"); got != OutcomeBlocked {
		t.Fatalf("dep outcome = %s, want blocked", got)
	}
	if depProbed != 0 {
		t.Fatalf("blocked dependent was probed %d times, want 0", depProbed)
	}
	for _, res := range run.Results {
		if res.ID == "dep" && !slices.Equal(res.Blockers, []string{"req"}) {
			t.Fatalf("dep blockers = %v, want [req]", res.Blockers)
		}
	}
}

func TestRunBlockingIsTransitive(t *testing.T) {
	r := New()
	var bProbed, cProbed int
	r.MustRegister(fixed("a", check.SeverityCritical, check.StatusFail, nil))
	r.MustRegister(fixed("b", check.SeverityCritical, check.StatusPass, &bProbed, "a"))
	r.MustRegister(fixed("c", check.SeverityCritical, check.StatusPass, &cProbed, "b"))

	run := runIDs(t, r)
	for _, id := range []string{"b", "c"} {
		if got := outcomeOf(run, id); got != OutcomeBlocked {
			t.Fatalf("%s outcome = %s, want blocked (transitive)", id, got)
		}
	}
	if bProbed != 0 || cProbed != 0 {
		t.Fatalf("a blocked check was probed (b=%d c=%d), want 0", bProbed, cProbed)
	}
	// Each check names its DIRECT blocker, not the transitive root cause.
	if got := blockersOf(run, "b"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("b blockers = %v, want [a]", got)
	}
	if got := blockersOf(run, "c"); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("c blockers = %v, want [b]", got)
	}
}

func TestRunErroredRequirementBlocksDependent(t *testing.T) {
	r := New()
	var depProbed int
	r.MustRegister(check.Check{ID: "req", Severity: check.SeverityCritical,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{}, errors.New("probe could not run")
		}})
	r.MustRegister(fixed("dep", check.SeverityCritical, check.StatusPass, &depProbed, "req"))

	run := runIDs(t, r)
	if got := outcomeOf(run, "dep"); got != OutcomeBlocked {
		t.Fatalf("dep outcome = %s, want blocked (an errored requirement is unknown, never hardened)", got)
	}
	if depProbed != 0 {
		t.Fatalf("dependent of an errored requirement was probed %d times, want 0", depProbed)
	}
	if got := blockersOf(run, "dep"); !slices.Equal(got, []string{"req"}) {
		t.Fatalf("dep blockers = %v, want [req]", got)
	}
}

func TestRunDeduplicatesRequires(t *testing.T) {
	r := New()
	r.MustRegister(fixed("a", check.SeverityCritical, check.StatusFail, nil))
	r.MustRegister(fixed("b", check.SeverityCritical, check.StatusPass, nil, "a", "a"))

	run := runIDs(t, r)
	if got := outcomeOf(run, "b"); got != OutcomeBlocked {
		t.Fatalf("b outcome = %s, want blocked", got)
	}
	if got := blockersOf(run, "b"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("b blockers = %v, want [a] (a duplicate requirement must collapse)", got)
	}
}

func TestRunSkipPropagatesAsSkipNotBlocked(t *testing.T) {
	r := New()
	var depProbed int
	r.MustRegister(fixed("na", check.SeverityInfo, check.StatusSkip, nil))
	r.MustRegister(fixed("dep", check.SeverityInfo, check.StatusPass, &depProbed, "na"))

	run := runIDs(t, r)
	if got := outcomeOf(run, "dep"); got != OutcomeSkip {
		t.Fatalf("dep outcome = %s, want skip (an N/A requirement is not a hard block)", got)
	}
	if depProbed != 0 {
		t.Fatalf("skip-propagated dependent was probed %d times, want 0", depProbed)
	}
}

func TestRunHardBlockBeatsSkip(t *testing.T) {
	r := New()
	r.MustRegister(fixed("na", check.SeverityInfo, check.StatusSkip, nil))
	r.MustRegister(fixed("bad", check.SeverityCritical, check.StatusFail, nil))
	r.MustRegister(fixed("dep", check.SeverityCritical, check.StatusPass, nil, "na", "bad"))

	if got := outcomeOf(runIDs(t, r), "dep"); got != OutcomeBlocked {
		t.Fatalf("dep outcome = %s, want blocked (a hard failure wins over a skip)", got)
	}
}

func TestRunReturnsGraphError(t *testing.T) {
	r := New()
	r.MustRegister(passing("x", "missing"))
	if _, err := r.Run(context.Background()); !errors.Is(err, ErrMissingRequirement) {
		t.Fatalf("Run = %v, want ErrMissingRequirement", err)
	}
}

func TestSummaryCounts(t *testing.T) {
	r := New()
	r.MustRegister(fixed("p", check.SeverityInfo, check.StatusPass, nil))
	r.MustRegister(fixed("f", check.SeverityWarning, check.StatusFail, nil))
	r.MustRegister(fixed("s", check.SeverityInfo, check.StatusSkip, nil))
	r.MustRegister(check.Check{ID: "e", Severity: check.SeverityInfo, Probe: func(context.Context) (check.Result, error) {
		return check.Result{}, errors.New("x")
	}})
	r.MustRegister(fixed("b", check.SeverityInfo, check.StatusPass, nil, "f"))

	got := runIDs(t, r).Summary()
	want := Summary{Total: 5, Passed: 1, Failed: 1, Errored: 1, Blocked: 1, Skipped: 1}
	if got != want {
		t.Fatalf("Summary = %+v, want %+v", got, want)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name   string
		checks []check.Check
		want   int
	}{
		{
			name:   "all pass",
			checks: []check.Check{fixed("a", check.SeverityCritical, check.StatusPass, nil)},
			want:   ExitOK,
		},
		{
			name:   "critical skip is OK",
			checks: []check.Check{fixed("a", check.SeverityCritical, check.StatusSkip, nil)},
			want:   ExitOK,
		},
		{
			name:   "warning fail does not gate",
			checks: []check.Check{fixed("a", check.SeverityWarning, check.StatusFail, nil)},
			want:   ExitOK,
		},
		{
			name:   "critical fail",
			checks: []check.Check{fixed("a", check.SeverityCritical, check.StatusFail, nil)},
			want:   ExitCritical,
		},
		{
			name: "critical error is indeterminate",
			checks: []check.Check{{ID: "a", Severity: check.SeverityCritical, Probe: func(context.Context) (check.Result, error) {
				return check.Result{}, errors.New("x")
			}}},
			want: ExitIndeterminate,
		},
		{
			name: "critical fail beats a downstream critical blocked",
			checks: []check.Check{
				fixed("req", check.SeverityCritical, check.StatusFail, nil),
				fixed("dep", check.SeverityCritical, check.StatusPass, nil, "req"),
			},
			// req fails (1), so the run is Critical regardless of dep's block.
			want: ExitCritical,
		},
		{
			name: "critical blocked by a warning failure is indeterminate",
			checks: []check.Check{
				fixed("warnreq", check.SeverityWarning, check.StatusFail, nil),
				fixed("crit", check.SeverityCritical, check.StatusPass, nil, "warnreq"),
			},
			// No critical Fail anywhere: crit is merely blocked (unknown), which
			// must gate as indeterminate — the only test that reaches the
			// OutcomeBlocked arm of ExitCode in isolation.
			want: ExitIndeterminate,
		},
		{
			name: "fail beats indeterminate",
			checks: []check.Check{
				{ID: "e", Severity: check.SeverityCritical, Probe: func(context.Context) (check.Result, error) {
					return check.Result{}, errors.New("x")
				}},
				fixed("f", check.SeverityCritical, check.StatusFail, nil),
			},
			want: ExitCritical,
		},
		{
			name: "only critical indeterminate",
			checks: []check.Check{
				fixed("warnfail", check.SeverityWarning, check.StatusFail, nil),
				{ID: "e", Severity: check.SeverityCritical, Probe: func(context.Context) (check.Result, error) {
					return check.Result{}, errors.New("x")
				}},
			},
			want: ExitIndeterminate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			for _, c := range tt.checks {
				r.MustRegister(c)
			}
			run := runIDs(t, r)
			if got := run.ExitCode(); got != tt.want {
				t.Fatalf("ExitCode = %d, want %d", got, tt.want)
			}
			if (run.ExitCode() == ExitOK) != run.OK() {
				t.Fatalf("OK() disagrees with ExitCode() == ExitOK")
			}
		})
	}
}
