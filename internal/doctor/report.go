package doctor

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/orkanoio/orkano/internal/checks"
)

// The doctor owns its report shapes rather than extending the shared
// checks.WriteText/WriteJSON: the hardening score and the fixable flag belong
// to doctor, not to the preflight's pass/fail install gate (the commit-1/2
// decision), and the shared JSON deliberately stays free of them.

// GateExitCode maps a run to doctor's process exit code, folding in the
// --min-score gate: a hardening score below the threshold is a definitive gate
// failure and exits ExitCritical, following the runner's precedence where a
// definitive critical failure beats an indeterminate one — including over a
// run that would otherwise exit ExitIndeterminate. NB the score counts
// errored/blocked (unknown) checks against the install, so a gate miss does
// not always mean a check definitively failed; consumers needing that
// distinction should read the per-check outcomes in the JSON body, not the
// bare exit code. minScore 0 (or any non-positive value) disables the gate and
// the run's own exit code stands; range validation is the caller's job (the
// CLI enforces 0–100).
func GateExitCode(run *checks.Run, minScore int) int {
	if minScore > 0 && run.Score().Value < minScore {
		return checks.ExitCritical
	}
	return run.ExitCode()
}

// WriteText renders the human report: the shared per-check lines, any --fix
// attempt results, a hint when failing checks carry an automatic fix, then the
// hardening-score headline and, when a --min-score gate is set, its verdict.
func WriteText(w io.Writer, run *checks.Run, attempts []checks.FixAttempt, minScore int) error {
	if err := run.WriteText(w); err != nil {
		return err
	}

	for _, a := range attempts {
		var line string
		switch {
		case a.Err != nil:
			line = fmt.Sprintf("fix %s: %v", a.ID, a.Err)
		case a.Resolved:
			line = fmt.Sprintf("fix %s: applied — the check now passes", a.ID)
		default:
			line = fmt.Sprintf("fix %s: applied, but the check still does not pass", a.ID)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	if n := fixableFailing(run); n > 0 {
		if _, err := fmt.Fprintf(w, "%d failing check(s) have an automatic fix — run `orkano doctor --fix`\n", n); err != nil {
			return err
		}
	}

	s := run.Score()
	if _, err := fmt.Fprintf(w, "\nHardening score: %d%% (%d of %d applicable checks passed)\n", s.Value, s.Passed, s.Scored); err != nil {
		return err
	}
	if minScore > 0 {
		verdict := "meets"
		if s.Value < minScore {
			verdict = "is below"
		}
		if _, err := fmt.Fprintf(w, "Score gate: %d%% %s the required %d%% (--min-score)\n", s.Value, verdict, minScore); err != nil {
			return err
		}
	}
	return nil
}

// fixableFailing counts checks that failed and have a Fix — the ones a --fix
// (or another --fix, since fixing is single-pass) would attempt.
func fixableFailing(run *checks.Run) int {
	n := 0
	for _, res := range run.Results {
		if res.Fixable && res.Outcome == checks.OutcomeFail {
			n++
		}
	}
	return n
}

// jsonCheck mirrors the shared checks JSON result plus the doctor-only
// fixable flag.
type jsonCheck struct {
	ID          string   `json:"id"`
	Severity    string   `json:"severity"`
	Summary     string   `json:"summary,omitempty"`
	Outcome     string   `json:"outcome"`
	Message     string   `json:"message,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Fixable     bool     `json:"fixable,omitempty"`
}

type jsonFix struct {
	ID       string `json:"id"`
	Error    string `json:"error,omitempty"`
	Resolved bool   `json:"resolved"`
}

type jsonReport struct {
	Results  []jsonCheck    `json:"results"`
	Summary  checks.Summary `json:"summary"`
	Score    checks.Score   `json:"score"`
	MinScore int            `json:"minScore,omitempty"`
	Fixes    []jsonFix      `json:"fixes,omitempty"`
	ExitCode int            `json:"exitCode"`
}

// WriteJSON renders the doctor report as stable, indented JSON for --json
// consumers (CI). The exit code — gated by minScore, so it always matches the
// process exit code — and the score are included so a consumer needs no second
// pass.
func WriteJSON(w io.Writer, run *checks.Run, attempts []checks.FixAttempt, minScore int) error {
	rep := jsonReport{
		Results:  make([]jsonCheck, 0, len(run.Results)),
		Summary:  run.Summary(),
		Score:    run.Score(),
		MinScore: minScore,
		ExitCode: GateExitCode(run, minScore),
	}
	for _, res := range run.Results {
		rep.Results = append(rep.Results, jsonCheck{
			ID:          res.ID,
			Severity:    string(res.Severity),
			Summary:     res.Summary,
			Outcome:     string(res.Outcome),
			Message:     res.Message,
			Blockers:    res.Blockers,
			Remediation: res.Remediation,
			Fixable:     res.Fixable,
		})
	}
	for _, a := range attempts {
		f := jsonFix{ID: a.ID, Resolved: a.Resolved}
		if a.Err != nil {
			f.Error = a.Err.Error()
		}
		rep.Fixes = append(rep.Fixes, f)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
