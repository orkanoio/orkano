package checks

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteText renders a human-readable report — one line per check in execution
// order, the remediation indented under anything actionable, then a summary
// line. It is the default (non-JSON) output for preflight and doctor.
func (run *Run) WriteText(w io.Writer) error {
	for _, res := range run.Results {
		line := fmt.Sprintf("[%-7s] %s", outcomeLabel(res.Outcome), res.ID)
		if res.Message != "" {
			line += "  " + res.Message
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if res.Remediation != "" && isActionable(res.Outcome) {
			if _, err := fmt.Fprintf(w, "          fix: %s\n", res.Remediation); err != nil {
				return err
			}
		}
	}

	s := run.Summary()
	_, err := fmt.Fprintf(w,
		"\n%d checks: %d passed, %d failed, %d errored, %d blocked, %d skipped\n",
		s.Total, s.Passed, s.Failed, s.Errored, s.Blocked, s.Skipped)
	return err
}

// isActionable reports whether a check's own remediation is worth printing. A
// blocked check never ran, so its remediation is premature — the actionable
// signal is the blocker IDs already in its message — and is excluded here.
func isActionable(o Outcome) bool {
	switch o {
	case OutcomeFail, OutcomeError:
		return true
	default:
		return false
	}
}

func outcomeLabel(o Outcome) string {
	switch o {
	case OutcomePass:
		return "PASS"
	case OutcomeFail:
		return "FAIL"
	case OutcomeError:
		return "ERROR"
	case OutcomeBlocked:
		return "BLOCKED"
	case OutcomeSkip:
		return "SKIP"
	default:
		return "?"
	}
}

// jsonResult is the marshalable projection of a CheckResult — the Check func
// fields cannot marshal, and Err is flattened to a string.
type jsonResult struct {
	ID          string   `json:"id"`
	Severity    string   `json:"severity"`
	Summary     string   `json:"summary,omitempty"`
	Outcome     string   `json:"outcome"`
	Message     string   `json:"message,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
}

type jsonReport struct {
	Results  []jsonResult `json:"results"`
	Summary  Summary      `json:"summary"`
	ExitCode int          `json:"exitCode"`
}

// WriteJSON renders the run as stable, indented JSON for --json consumers (CI,
// the wizard). The shape is flat and free of the Check func fields; the summary
// and the gate's exit code are included so a CI consumer needs no second pass.
func (run *Run) WriteJSON(w io.Writer) error {
	rep := jsonReport{
		Results:  make([]jsonResult, 0, len(run.Results)),
		Summary:  run.Summary(),
		ExitCode: run.ExitCode(),
	}
	for _, res := range run.Results {
		rep.Results = append(rep.Results, jsonResult{
			ID:          res.ID,
			Severity:    string(res.Severity),
			Summary:     res.Summary,
			Outcome:     string(res.Outcome),
			Message:     res.Message,
			Blockers:    res.Blockers,
			Remediation: res.Remediation,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
