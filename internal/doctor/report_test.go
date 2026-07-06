package doctor_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/doctor"
)

func sampleRun() *checks.Run {
	return &checks.Run{Results: []checks.CheckResult{
		{ID: "a.pass", Severity: check.SeverityCritical, Outcome: checks.OutcomePass, Message: "ok"},
		{ID: "b.fail", Severity: check.SeverityCritical, Outcome: checks.OutcomeFail, Message: "bad", Fixable: true},
	}}
}

func TestWriteText(t *testing.T) {
	attempts := []checks.FixAttempt{
		{ID: "b.fail", Resolved: false},
		{ID: "c.other", Err: errors.New("fix exploded")},
	}
	var buf bytes.Buffer
	if err := doctor.WriteText(&buf, sampleRun(), attempts, 0); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"a.pass",
		"b.fail",
		"fix b.fail: applied, but the check still does not pass",
		"fix c.other: fix exploded",
		"1 failing check(s) have an automatic fix",
		"Hardening score: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Score gate:") {
		t.Errorf("no gate verdict should render when --min-score is unset:\n%s", out)
	}
}

func TestWriteTextScoreGate(t *testing.T) {
	// sampleRun scores 50% (one of two equal-weight criticals passed).
	for _, tc := range []struct {
		name     string
		minScore int
		want     string
	}{
		{"below threshold", 80, "Score gate: 50% is below the required 80% (--min-score)"},
		{"meets threshold", 50, "Score gate: 50% meets the required 50% (--min-score)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := doctor.WriteText(&buf, sampleRun(), nil, tc.minScore); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, buf.String())
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	attempts := []checks.FixAttempt{{ID: "b.fail", Resolved: true}}
	var buf bytes.Buffer
	if err := doctor.WriteJSON(&buf, sampleRun(), attempts, 0); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var rep struct {
		Results []struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
			Fixable bool   `json:"fixable"`
		} `json:"results"`
		Score struct {
			Value  int `json:"value"`
			Scored int `json:"scored"`
		} `json:"score"`
		Fixes []struct {
			ID       string `json:"id"`
			Resolved bool   `json:"resolved"`
		} `json:"fixes"`
		ExitCode int `json:"exitCode"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if len(rep.Results) != 2 || rep.Results[1].ID != "b.fail" || !rep.Results[1].Fixable {
		t.Errorf("results = %+v", rep.Results)
	}
	// One of two equal-weight critical checks passed: 50%, both scored.
	if rep.Score.Value != 50 || rep.Score.Scored != 2 {
		t.Errorf("score = %+v", rep.Score)
	}
	if rep.ExitCode != checks.ExitCritical {
		t.Errorf("exitCode = %d, want %d", rep.ExitCode, checks.ExitCritical)
	}
	if len(rep.Fixes) != 1 || !rep.Fixes[0].Resolved {
		t.Errorf("fixes = %+v", rep.Fixes)
	}
	if strings.Contains(buf.String(), "minScore") {
		t.Errorf("minScore should be omitted when the gate is unset:\n%s", buf.String())
	}
}

// TestWriteJSONMinScoreGate pins the --json contract for CI consumers: the
// envelope carries the threshold, and its exitCode reflects the gate so no
// second pass is needed.
func TestWriteJSONMinScoreGate(t *testing.T) {
	// A warnings-only degradation: critical passed, warning failed → the run's
	// own exit code is 0, and only the gate can turn it into a failure.
	run := &checks.Run{Results: []checks.CheckResult{
		{ID: "a.pass", Severity: check.SeverityCritical, Outcome: checks.OutcomePass},
		{ID: "w.fail", Severity: check.SeverityWarning, Outcome: checks.OutcomeFail},
	}}

	var buf bytes.Buffer
	if err := doctor.WriteJSON(&buf, run, nil, 90); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var rep struct {
		MinScore int `json:"minScore"`
		ExitCode int `json:"exitCode"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if rep.MinScore != 90 {
		t.Errorf("minScore = %d, want 90", rep.MinScore)
	}
	if rep.ExitCode != checks.ExitCritical {
		t.Errorf("exitCode = %d, want the gated %d", rep.ExitCode, checks.ExitCritical)
	}
}

func TestGateExitCode(t *testing.T) {
	pass := checks.CheckResult{ID: "c.pass", Severity: check.SeverityCritical, Outcome: checks.OutcomePass}
	warnFail := checks.CheckResult{ID: "w.fail", Severity: check.SeverityWarning, Outcome: checks.OutcomeFail}
	critFail := checks.CheckResult{ID: "c.fail", Severity: check.SeverityCritical, Outcome: checks.OutcomeFail}
	critErr := checks.CheckResult{ID: "c.err", Severity: check.SeverityCritical, Outcome: checks.OutcomeError}

	for _, tc := range []struct {
		name     string
		results  []checks.CheckResult
		minScore int
		want     int
	}{
		{"all pass, gate off", []checks.CheckResult{pass}, 0, checks.ExitOK},
		{"all pass meets 100", []checks.CheckResult{pass}, 100, checks.ExitOK},
		// The gate's reason to exist: a warnings-only regression (run exit 0).
		{"warning fail below gate", []checks.CheckResult{pass, warnFail}, 90, checks.ExitCritical},
		{"warning fail, gate off", []checks.CheckResult{pass, warnFail}, 0, checks.ExitOK},
		{"critical fail stays critical", []checks.CheckResult{pass, critFail}, 1, checks.ExitCritical},
		// A definitive gate failure beats an indeterminate run, mirroring the
		// runner's critical-fail-beats-indeterminate precedence.
		{"indeterminate below gate", []checks.CheckResult{critErr}, 50, checks.ExitCritical},
		{"indeterminate meets gate", []checks.CheckResult{pass, pass, pass, critErr}, 50, checks.ExitIndeterminate},
		{"indeterminate, gate off", []checks.CheckResult{critErr}, 0, checks.ExitIndeterminate},
		// A vacuous run scores 100, so any gate passes.
		{"all skipped meets 100", []checks.CheckResult{{ID: "s", Severity: check.SeverityCritical, Outcome: checks.OutcomeSkip}}, 100, checks.ExitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &checks.Run{Results: tc.results}
			if got := doctor.GateExitCode(run, tc.minScore); got != tc.want {
				t.Errorf("GateExitCode(%s, %d) = %d, want %d", tc.name, tc.minScore, got, tc.want)
			}
		})
	}
}
