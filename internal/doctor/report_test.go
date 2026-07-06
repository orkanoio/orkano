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
	if err := doctor.WriteText(&buf, sampleRun(), attempts); err != nil {
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
}

func TestWriteJSON(t *testing.T) {
	attempts := []checks.FixAttempt{{ID: "b.fail", Resolved: true}}
	var buf bytes.Buffer
	if err := doctor.WriteJSON(&buf, sampleRun(), attempts); err != nil {
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
}
