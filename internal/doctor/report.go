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

// WriteText renders the human report: the shared per-check lines, any --fix
// attempt results, a hint when failing checks carry an automatic fix, then the
// hardening-score headline.
func WriteText(w io.Writer, run *checks.Run, attempts []checks.FixAttempt) error {
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
	_, err := fmt.Fprintf(w, "\nHardening score: %d%% (%d of %d applicable checks passed)\n", s.Value, s.Passed, s.Scored)
	return err
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
	Fixes    []jsonFix      `json:"fixes,omitempty"`
	ExitCode int            `json:"exitCode"`
}

// WriteJSON renders the doctor report as stable, indented JSON for --json
// consumers (CI). The exit code and score are included so a consumer needs no
// second pass.
func WriteJSON(w io.Writer, run *checks.Run, attempts []checks.FixAttempt) error {
	rep := jsonReport{
		Results:  make([]jsonCheck, 0, len(run.Results)),
		Summary:  run.Summary(),
		Score:    run.Score(),
		ExitCode: run.ExitCode(),
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
