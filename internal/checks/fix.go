package checks

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/orkanoio/orkano/api/check"
)

// FixAttempt records one --fix attempt made by RunAndFix.
type FixAttempt struct {
	ID string
	// Err is the Fix func's own error or a recovered panic — or, when the context
	// was already cancelled, the context error, in which case Fix was never
	// called. A fix that errors leaves the check to fail again on the re-run.
	Err error
	// Resolved reports that the check passed when re-probed after the fix.
	Resolved bool
}

// RunAndFix runs the registry, applies the Fix of every check that FAILED and
// has one, then runs the registry again and returns the second run together
// with the attempts. Only OutcomeFail is fixed — the api/check contract runs a
// Fix "only after Probe reports StatusFail", so an errored or blocked check
// (unknown, never a definite failure) is never auto-remediated.
//
// The second run gives a consistent post-fix picture: fixed checks re-probe and
// a dependent that was blocked by a now-passing requirement re-evaluates. Every
// Probe therefore runs twice, so probes must be cheap and safe to repeat. Fixing
// is single-pass: a check that was blocked in the first run and then FAILS in
// the second is not fixed in the same call — invoke --fix again to converge.
//
// A fix that errors or panics is recorded and its check simply fails again. When
// nothing is fixable the first run is returned unchanged and the registry runs
// once. If the second run itself fails (unreachable on an immutable registry —
// the first run validated the same graph) the attempts so far are returned with
// a nil run and the error.
func (r *Registry) RunAndFix(ctx context.Context) (*Run, []FixAttempt, error) {
	first, err := r.Run(ctx)
	if err != nil {
		return nil, nil, err
	}

	var attempts []FixAttempt
	for _, res := range first.Results {
		if res.Outcome != OutcomeFail {
			continue
		}
		// The ID came from this registry's own Run, so Get always finds it.
		c, _ := r.Get(res.ID)
		if c.Fix == nil {
			continue
		}
		attempts = append(attempts, FixAttempt{ID: res.ID, Err: runFix(ctx, c)})
	}
	if len(attempts) == 0 {
		return first, nil, nil
	}

	second, err := r.Run(ctx)
	if err != nil {
		// Unreachable on an immutable registry (the first Run already validated
		// the same graph); defensive against future mutability.
		return nil, attempts, err
	}
	outcome := make(map[string]Outcome, len(second.Results))
	for _, res := range second.Results {
		outcome[res.ID] = res.Outcome
	}
	for i := range attempts {
		attempts[i].Resolved = outcome[attempts[i].ID] == OutcomePass
	}
	return second, attempts, nil
}

// runFix invokes a check's Fix, mirroring probe's safety: an already-cancelled
// context is refused without calling Fix, and a panicking Fix (a contributor
// bug) is recovered into an error with a stack rather than crashing doctor. The
// Fix's own returned error is passed through unwrapped, like a probe's.
func runFix(ctx context.Context, c check.Check) (err error) {
	if e := ctx.Err(); e != nil {
		return e
	}
	defer func() {
		if p := recover(); p != nil {
			if pe, ok := p.(error); ok {
				err = fmt.Errorf("fix panicked: %w\n%s", pe, debug.Stack())
			} else {
				err = fmt.Errorf("fix panicked: %v\n%s", p, debug.Stack())
			}
		}
	}()
	return c.Fix(ctx)
}
