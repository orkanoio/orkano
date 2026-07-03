package checks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/api/check"
)

// toggle backs a check that fails until its Fix runs, then passes — the shape
// the --fix re-probe path exercises. probes/fixes count invocations; fixErr,
// when set, makes the Fix fail without flipping the state.
type toggle struct {
	fixed  bool
	probes int
	fixes  int
	fixErr error
}

func (tg *toggle) check(id string, sev check.Severity, requires ...string) check.Check {
	return check.Check{
		ID:       id,
		Severity: sev,
		Requires: requires,
		Probe: func(context.Context) (check.Result, error) {
			tg.probes++
			if tg.fixed {
				return check.Result{Status: check.StatusPass, Message: "ok"}, nil
			}
			return check.Result{Status: check.StatusFail, Message: "needs fixing"}, nil
		},
		Fix: func(context.Context) error {
			tg.fixes++
			if tg.fixErr != nil {
				return tg.fixErr
			}
			tg.fixed = true
			return nil
		},
	}
}

func attemptByID(attempts []FixAttempt, id string) (FixAttempt, bool) {
	for _, a := range attempts {
		if a.ID == id {
			return a, true
		}
	}
	return FixAttempt{}, false
}

func TestRunAndFixResolvesFailure(t *testing.T) {
	tg := &toggle{}
	r := New()
	r.MustRegister(tg.check("thing", check.SeverityCritical))

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if tg.fixes != 1 {
		t.Fatalf("Fix ran %d times, want 1", tg.fixes)
	}
	if tg.probes != 2 {
		t.Fatalf("probed %d times, want 2 (once before the fix, once to confirm)", tg.probes)
	}
	if got := outcomeOf(run, "thing"); got != OutcomePass {
		t.Fatalf("final outcome = %s, want pass (the fix resolved it)", got)
	}
	a, ok := attemptByID(attempts, "thing")
	if !ok {
		t.Fatalf("no fix attempt recorded for thing")
	}
	if a.Err != nil || !a.Resolved {
		t.Fatalf("attempt = %+v, want no error + resolved", a)
	}
}

func TestRunAndFixLeavesNonFailingAndUnfixableAlone(t *testing.T) {
	// A passing check and a failing-but-unfixable check must not be touched, and
	// with nothing fixable the registry runs exactly once (no re-run).
	var passProbes int
	r := New()
	r.MustRegister(fixed("ok", check.SeverityInfo, check.StatusPass, &passProbes))
	r.MustRegister(fixed("broken", check.SeverityCritical, check.StatusFail, nil)) // no Fix

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %v, want none (nothing is fixable)", attempts)
	}
	if passProbes != 1 {
		t.Fatalf("passing check probed %d times, want 1 (no re-run when nothing was fixed)", passProbes)
	}
	if got := outcomeOf(run, "broken"); got != OutcomeFail {
		t.Fatalf("broken outcome = %s, want fail (unchanged)", got)
	}
}

func TestRunAndFixSkipsPassingFixable(t *testing.T) {
	// A fixable check that already passes must not have its Fix run, and with
	// nothing fixed the registry runs exactly once.
	tg := &toggle{fixed: true} // starts passing
	r := New()
	r.MustRegister(tg.check("thing", check.SeverityInfo))

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if tg.fixes != 0 {
		t.Fatalf("Fix ran %d times on a passing check, want 0", tg.fixes)
	}
	if tg.probes != 1 {
		t.Fatalf("probed %d times, want 1 (no re-run when nothing was fixed)", tg.probes)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %v, want none", attempts)
	}
	// The Fix exists even though the check passes: Fixable reflects availability.
	for _, res := range run.Results {
		if res.ID == "thing" && !res.Fixable {
			t.Fatalf("passing fixable check Fixable = false, want true (a Fix exists)")
		}
	}
}

func TestRunAndFixSkipsNonFailingOutcomes(t *testing.T) {
	// Fix runs only on OutcomeFail. A fixable check that is errored or blocked
	// keeps Fixable=true (the Fix exists) yet never has its Fix run.
	erroredFixes, blockedFixes := 0, 0
	r := New()
	r.MustRegister(check.Check{
		ID: "errored", Severity: check.SeverityInfo,
		Probe: func(context.Context) (check.Result, error) { return check.Result{}, errors.New("cannot probe") },
		Fix:   func(context.Context) error { erroredFixes++; return nil },
	})
	r.MustRegister(fixed("badreq", check.SeverityInfo, check.StatusFail, nil)) // unfixable failure
	r.MustRegister(check.Check{
		ID: "blocked", Severity: check.SeverityInfo, Requires: []string{"badreq"},
		Probe: func(context.Context) (check.Result, error) { return check.Result{Status: check.StatusPass}, nil },
		Fix:   func(context.Context) error { blockedFixes++; return nil },
	})

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	// Only badreq FAILED, and it has no Fix — so nothing is attempted at all.
	if len(attempts) != 0 {
		t.Fatalf("attempts = %v, want none (no fixable check FAILED)", attempts)
	}
	if erroredFixes != 0 || blockedFixes != 0 {
		t.Fatalf("a Fix ran on a non-failed check (errored=%d blocked=%d)", erroredFixes, blockedFixes)
	}
	if got := outcomeOf(run, "errored"); got != OutcomeError {
		t.Fatalf("errored outcome = %s, want error", got)
	}
	if got := outcomeOf(run, "blocked"); got != OutcomeBlocked {
		t.Fatalf("blocked outcome = %s, want blocked", got)
	}
	// Fixable reflects "a Fix exists", independent of outcome.
	for _, res := range run.Results {
		switch res.ID {
		case "errored", "blocked":
			if !res.Fixable {
				t.Fatalf("%s Fixable = false, want true (it has a Fix func)", res.ID)
			}
		case "badreq":
			if res.Fixable {
				t.Fatalf("badreq Fixable = true, want false (no Fix func)")
			}
		}
	}
}

func TestRunAndFixRecordsFixError(t *testing.T) {
	sentinel := errors.New("could not remediate")
	tg := &toggle{fixErr: sentinel}
	r := New()
	r.MustRegister(tg.check("thing", check.SeverityCritical))

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	a, ok := attemptByID(attempts, "thing")
	if !ok {
		t.Fatalf("no attempt recorded")
	}
	if !errors.Is(a.Err, sentinel) {
		t.Fatalf("attempt.Err = %v, want the fix's own error", a.Err)
	}
	if a.Resolved {
		t.Fatalf("attempt marked resolved, but the fix errored")
	}
	if got := outcomeOf(run, "thing"); got != OutcomeFail {
		t.Fatalf("final outcome = %s, want fail (fix did not resolve)", got)
	}
	// The re-run still happened (a fix may have partially applied): 2 probes.
	if tg.probes != 2 {
		t.Fatalf("probed %d times, want 2", tg.probes)
	}
}

func TestRunAndFixMultipleFixablesResolveIndependently(t *testing.T) {
	// Two independent fixable failures are both fixed in one pass; a coexisting
	// unfixable failure is left alone. Proves the loop handles every entry and
	// the second run reconfirms each independently.
	tgA, tgB := &toggle{}, &toggle{}
	r := New()
	r.MustRegister(tgA.check("a", check.SeverityCritical))
	r.MustRegister(tgB.check("b", check.SeverityWarning))
	r.MustRegister(fixed("broken", check.SeverityInfo, check.StatusFail, nil)) // no Fix

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (only the two fixable failures)", len(attempts))
	}
	if _, ok := attemptByID(attempts, "broken"); ok {
		t.Fatalf("the unfixable failure was attempted")
	}
	if tgA.fixes != 1 || tgB.fixes != 1 {
		t.Fatalf("fix counts a=%d b=%d, want 1/1", tgA.fixes, tgB.fixes)
	}
	for _, id := range []string{"a", "b"} {
		a, _ := attemptByID(attempts, id)
		if !a.Resolved {
			t.Fatalf("%s not marked resolved", id)
		}
		if got := outcomeOf(run, id); got != OutcomePass {
			t.Fatalf("%s outcome = %s, want pass", id, got)
		}
	}
	if got := outcomeOf(run, "broken"); got != OutcomeFail {
		t.Fatalf("broken outcome = %s, want fail (unchanged)", got)
	}
}

func TestRunAndFixPartialResolution(t *testing.T) {
	// Resolved is per-attempt: a working fix resolves while a failing fix does
	// not — a naive implementation copying one Resolved across all would pass
	// every other test but fail here.
	sentinel := errors.New("nope")
	tgOK, tgErr := &toggle{}, &toggle{fixErr: sentinel}
	r := New()
	r.MustRegister(tgOK.check("ok", check.SeverityCritical))
	r.MustRegister(tgErr.check("err", check.SeverityCritical))

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	aOK, _ := attemptByID(attempts, "ok")
	aErr, _ := attemptByID(attempts, "err")
	if aOK.Err != nil || !aOK.Resolved || outcomeOf(run, "ok") != OutcomePass {
		t.Fatalf("ok attempt = %+v (outcome %s), want resolved+no-error+pass", aOK, outcomeOf(run, "ok"))
	}
	if !errors.Is(aErr.Err, sentinel) || aErr.Resolved || outcomeOf(run, "err") != OutcomeFail {
		t.Fatalf("err attempt = %+v (outcome %s), want unresolved+error+fail", aErr, outcomeOf(run, "err"))
	}
}

func TestRunAndFixRecoversPanickingFix(t *testing.T) {
	r := New()
	r.MustRegister(check.Check{
		ID:       "boom",
		Severity: check.SeverityCritical,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusFail}, nil
		},
		Fix: func(context.Context) error { panic("contributor bug") },
	})

	_, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	a, ok := attemptByID(attempts, "boom")
	if !ok || a.Err == nil {
		t.Fatalf("panicking fix not captured as an error: %+v", a)
	}
	if !strings.Contains(a.Err.Error(), "fix panicked") {
		t.Fatalf("error = %v, want a 'fix panicked' message", a.Err)
	}
	if !strings.Contains(a.Err.Error(), "goroutine") {
		t.Fatalf("error = %v, want an embedded stack trace", a.Err)
	}
}

func TestRunAndFixCancelledContext(t *testing.T) {
	// Under a pre-cancelled context every probe errors (never fails), so nothing
	// is fixable: RunAndFix runs once, attempts nothing, and does not error.
	tg := &toggle{}
	r := New()
	r.MustRegister(tg.check("thing", check.SeverityCritical))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run, attempts, err := r.RunAndFix(ctx)
	if err != nil {
		t.Fatalf("RunAndFix under cancelled ctx: %v, want nil", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %v, want none (a cancelled probe errors, never fails)", attempts)
	}
	if tg.fixes != 0 {
		t.Fatalf("Fix ran %d times, want 0", tg.fixes)
	}
	if got := outcomeOf(run, "thing"); got != OutcomeError {
		t.Fatalf("outcome = %s, want error", got)
	}
}

func TestRunAndFixUnblocksDependentsOnReRun(t *testing.T) {
	// Fixing a failing requirement lets a previously-blocked dependent probe on
	// the re-run — the reason RunAndFix runs the whole registry again.
	var depProbes int
	tg := &toggle{}
	r := New()
	r.MustRegister(tg.check("req", check.SeverityCritical))
	r.MustRegister(fixed("dep", check.SeverityCritical, check.StatusPass, &depProbes, "req"))

	run, _, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if got := outcomeOf(run, "req"); got != OutcomePass {
		t.Fatalf("req outcome = %s, want pass", got)
	}
	if got := outcomeOf(run, "dep"); got != OutcomePass {
		t.Fatalf("dep outcome = %s, want pass (unblocked once req was fixed)", got)
	}
	// dep was blocked (unprobed) on the first run and probed once on the re-run.
	if depProbes != 1 {
		t.Fatalf("dep probed %d times, want 1 (blocked first, probed on the re-run)", depProbes)
	}
}

func TestRunAndFixDoesNotFixUnblockedDependentInSamePass(t *testing.T) {
	// A dependent blocked in the first run and then failing in the second is not
	// fixed in the same call, even when it is itself fixable — the fix pass only
	// covers first-run failures. The user re-runs --fix to converge.
	req := &toggle{}
	dep := &toggle{} // fixable, but stays failing this pass — its Fix is never called
	r := New()
	r.MustRegister(req.check("req", check.SeverityCritical))
	r.MustRegister(dep.check("dep", check.SeverityCritical, "req"))

	run, attempts, err := r.RunAndFix(context.Background())
	if err != nil {
		t.Fatalf("RunAndFix: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (only req failed in the first run)", len(attempts))
	}
	if _, ok := attemptByID(attempts, "req"); !ok {
		t.Fatalf("req not among the attempts")
	}
	if dep.fixes != 0 {
		t.Fatalf("dep's Fix ran %d times, want 0 (it was blocked, not failed, in the first run)", dep.fixes)
	}
	if got := outcomeOf(run, "dep"); got != OutcomeFail {
		t.Fatalf("dep outcome = %s, want fail (unblocked on the re-run but not fixed)", got)
	}
}

func TestRunAndFixPropagatesGraphError(t *testing.T) {
	r := New()
	r.MustRegister(passing("x", "missing")) // requires an unregistered check
	if _, _, err := r.RunAndFix(context.Background()); !errors.Is(err, ErrMissingRequirement) {
		t.Fatalf("RunAndFix = %v, want ErrMissingRequirement", err)
	}
}

func TestFixableFlag(t *testing.T) {
	tg := &toggle{}
	r := New()
	r.MustRegister(tg.check("has-fix", check.SeverityInfo))
	r.MustRegister(fixed("no-fix", check.SeverityInfo, check.StatusFail, nil))

	// A plain Run (what doctor uses to decide whether to suggest --fix) must set
	// Fixable from the presence of a Fix func.
	run := runIDs(t, r)
	for _, res := range run.Results {
		wantFixable := res.ID == "has-fix"
		if res.Fixable != wantFixable {
			t.Fatalf("%s Fixable = %v, want %v", res.ID, res.Fixable, wantFixable)
		}
	}
}
