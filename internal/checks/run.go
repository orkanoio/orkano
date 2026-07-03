package checks

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/orkanoio/orkano/api/check"
)

// Outcome is the per-check result of a Run. It is richer than check.Status: a
// probe that could not run (an error, a panic, a cancelled context) becomes
// OutcomeError rather than a fail, and a check whose requirements did not pass
// is reported without being probed at all.
type Outcome string

const (
	OutcomePass    Outcome = "pass"
	OutcomeFail    Outcome = "fail"
	OutcomeSkip    Outcome = "skip"    // not applicable to this install
	OutcomeError   Outcome = "error"   // probe could not run — unknown, never hardened
	OutcomeBlocked Outcome = "blocked" // a requirement failed, so the check was not probed
)

// CheckResult is the outcome of one check in a Run, carrying enough check
// metadata to render and gate without re-consulting the registry.
type CheckResult struct {
	ID          string
	Severity    check.Severity
	Summary     string
	Remediation string
	// Fixable reports that the check has a Fix func. RunAndFix attempts the
	// remediation only when Outcome is OutcomeFail, but the flag is set from the
	// Fix's presence regardless of outcome.
	Fixable  bool
	Outcome  Outcome
	Message  string
	Err      error    // set when Outcome == OutcomeError
	Blockers []string // requirement IDs that blocked it, when Outcome == OutcomeBlocked
}

// Run is the result of executing a registry, in dependency order.
type Run struct {
	Results []CheckResult
}

// Run executes every registered check in dependency order. The graph is
// validated first; an invalid graph (missing requirement or cycle) is returned
// as an error and nothing is probed.
//
// A check is probed only when all of its requirements passed. Otherwise it is
// not probed: OutcomeBlocked if any requirement failed, errored, or was itself
// blocked; OutcomeSkip if the only unmet requirements were themselves skipped
// (a not-applicable requirement makes the dependent not-applicable, never
// unhardened). A Probe that returns an error, panics, or runs under an already
// cancelled context yields OutcomeError — unknown is reported as unknown, never
// as a pass or a fail.
func (r *Registry) Run(ctx context.Context) (*Run, error) {
	order, err := r.Plan()
	if err != nil {
		return nil, err
	}

	results := make(map[string]CheckResult, len(order))
	run := &Run{Results: make([]CheckResult, 0, len(order))}

	for _, c := range order {
		res := CheckResult{
			ID:          c.ID,
			Severity:    c.Severity,
			Summary:     c.Summary,
			Remediation: c.Remediation,
			Fixable:     c.Fix != nil,
		}

		var hardBlockers, skipped []string
		for _, req := range c.Requires {
			switch results[req].Outcome {
			case OutcomePass:
				// requirement satisfied
			case OutcomeSkip:
				skipped = append(skipped, req)
			default: // fail, error, or blocked all hard-block the dependent
				hardBlockers = append(hardBlockers, req)
			}
		}

		switch {
		case len(hardBlockers) > 0:
			res.Outcome = OutcomeBlocked
			res.Blockers = hardBlockers
			res.Message = fmt.Sprintf("not run: requirement(s) %v did not pass", hardBlockers)
		case len(skipped) > 0:
			res.Outcome = OutcomeSkip
			res.Message = fmt.Sprintf("skipped: requirement(s) %v not applicable", skipped)
		default:
			probe(ctx, c, &res)
		}

		results[c.ID] = res
		run.Results = append(run.Results, res)
	}
	return run, nil
}

// probe runs a single check's Probe and records the outcome on res. It treats
// every way a probe can fail to produce a Result — a returned error, a panic,
// a cancelled context, or an unrecognised status — as OutcomeError. The
// ctx-guard + panic-recovery idiom is mirrored by runFix in fix.go; keep the
// two in sync.
func probe(ctx context.Context, c check.Check, res *CheckResult) {
	if err := ctx.Err(); err != nil {
		res.Outcome = OutcomeError
		res.Err = err
		res.Message = err.Error()
		return
	}

	defer func() {
		if p := recover(); p != nil {
			res.Outcome = OutcomeError
			// A contributed check that panics is a bug; capture the stack (it
			// still holds the panic site here, before the frames unwind) so the
			// maintainer can locate it, and keep the %w chain when it panicked
			// with an error value.
			if err, ok := p.(error); ok {
				res.Err = fmt.Errorf("probe panicked: %w\n%s", err, debug.Stack())
			} else {
				res.Err = fmt.Errorf("probe panicked: %v\n%s", p, debug.Stack())
			}
			res.Message = res.Err.Error()
		}
	}()

	result, err := c.Probe(ctx)
	if err != nil {
		res.Outcome = OutcomeError
		res.Err = err
		res.Message = err.Error()
		return
	}

	res.Message = result.Message
	switch result.Status {
	case check.StatusPass:
		res.Outcome = OutcomePass
	case check.StatusFail:
		res.Outcome = OutcomeFail
	case check.StatusSkip:
		res.Outcome = OutcomeSkip
	default:
		res.Outcome = OutcomeError
		// Keep the probe's own message — it is the diagnostic, not the bad status.
		if result.Message != "" {
			res.Err = fmt.Errorf("probe returned unknown status %q: %s", result.Status, result.Message)
		} else {
			res.Err = fmt.Errorf("probe returned unknown status %q", result.Status)
		}
		res.Message = res.Err.Error()
	}
}

// Summary aggregates a Run by outcome.
type Summary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errored int `json:"errored"`
	Blocked int `json:"blocked"`
	Skipped int `json:"skipped"`
}

// Summary counts the run's outcomes.
func (run *Run) Summary() Summary {
	var s Summary
	for _, res := range run.Results {
		s.Total++
		switch res.Outcome {
		case OutcomePass:
			s.Passed++
		case OutcomeFail:
			s.Failed++
		case OutcomeError:
			s.Errored++
		case OutcomeBlocked:
			s.Blocked++
		case OutcomeSkip:
			s.Skipped++
		}
	}
	return s
}

// Exit codes returned by ExitCode, suitable for preflight and doctor in CI.
const (
	ExitOK            = 0 // every critical check passed (or was not applicable)
	ExitCritical      = 1 // at least one critical check failed
	ExitIndeterminate = 2 // a critical check could not be determined (errored or blocked)
)

// ExitCode gates on critical severity, matching the preflight contract — refuse
// on critical failure. An outright critical failure is the most actionable
// signal (ExitCritical) and takes precedence over a critical check that merely
// could not run (ExitIndeterminate); warnings and info never change the code.
// Because unknown never counts as hardened, a critical Error or Blocked is
// still non-zero.
func (run *Run) ExitCode() int {
	code := ExitOK
	for _, res := range run.Results {
		if res.Severity != check.SeverityCritical {
			continue
		}
		switch res.Outcome {
		case OutcomeFail:
			return ExitCritical
		case OutcomeError, OutcomeBlocked:
			code = ExitIndeterminate
		}
	}
	return code
}

// OK reports whether the run cleared the gate: no critical failure and no
// critical unknown. It is equivalent to ExitCode() == ExitOK.
func (run *Run) OK() bool { return run.ExitCode() == ExitOK }
