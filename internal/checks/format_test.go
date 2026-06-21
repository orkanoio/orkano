package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/api/check"
)

// sampleRun builds a run exercising every outcome for the formatters.
func sampleRun(t *testing.T) *Run {
	t.Helper()
	r := New()
	r.MustRegister(check.Check{
		ID: "net.dns", Severity: check.SeverityCritical, Summary: "DNS resolves",
		Remediation: "point resolv.conf at a reachable resolver",
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusPass, Message: "resolved in 3ms"}, nil
		},
	})
	r.MustRegister(check.Check{
		ID: "net.egress", Severity: check.SeverityCritical, Summary: "egress allowed",
		Remediation: "open 443 to the registry",
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusFail, Message: "443 blocked"}, nil
		},
	})
	r.MustRegister(check.Check{
		ID: "ports.free", Severity: check.SeverityCritical, Summary: "ports free",
		Remediation: "free 6443",
		Probe:       func(context.Context) (check.Result, error) { return check.Result{}, errors.New("ssh refused") },
		Requires:    nil,
	})
	r.MustRegister(check.Check{
		ID: "byo.storageclass", Severity: check.SeverityInfo, Summary: "storage class",
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusSkip, Message: "bootstrapped cluster"}, nil
		},
	})
	run, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return run
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleRun(t).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"[PASS   ] net.dns",
		"[FAIL   ] net.egress",
		"[ERROR  ] ports.free",
		"[SKIP   ] byo.storageclass",
		"fix: open 443 to the registry", // remediation under a FAIL
		"fix: free 6443",                // remediation under an ERROR
		"4 checks: 1 passed, 1 failed, 1 errored, 0 blocked, 1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteText missing %q\n---\n%s", want, out)
		}
	}

	// A passing check must not print its remediation (net.dns carries one).
	if strings.Contains(out, "point resolv.conf") {
		t.Errorf("remediation printed under a passing check:\n%s", out)
	}
}

func TestWriteTextBlocked(t *testing.T) {
	r := New()
	r.MustRegister(check.Check{ID: "dep", Severity: check.SeverityCritical, Summary: "dep",
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusFail, Message: "down"}, nil
		}})
	r.MustRegister(check.Check{ID: "app", Severity: check.SeverityCritical, Summary: "app",
		Remediation: "restart the app", Requires: []string{"dep"},
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusPass}, nil
		}})

	var buf bytes.Buffer
	run, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := run.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "[BLOCKED] app") {
		t.Errorf("missing BLOCKED label:\n%s", out)
	}
	// A blocked check never ran: its own remediation must not be offered — the
	// blocker IDs in its message are the actionable signal.
	if strings.Contains(out, "restart the app") {
		t.Errorf("remediation printed under a blocked check:\n%s", out)
	}
	if !strings.Contains(out, "requirement(s) [dep]") {
		t.Errorf("blocked message should name the blocker:\n%s", out)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleRun(t).WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var rep struct {
		Results []struct {
			ID          string `json:"id"`
			Severity    string `json:"severity"`
			Outcome     string `json:"outcome"`
			Message     string `json:"message"`
			Remediation string `json:"remediation"`
		} `json:"results"`
		Summary  Summary `json:"summary"`
		ExitCode int     `json:"exitCode"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if len(rep.Results) != 4 {
		t.Fatalf("results len = %d, want 4", len(rep.Results))
	}
	if rep.Results[0].ID != "net.dns" || rep.Results[0].Outcome != "pass" {
		t.Errorf("first result = %+v, want net.dns/pass", rep.Results[0])
	}
	if rep.Summary != (Summary{Total: 4, Passed: 1, Failed: 1, Errored: 1, Skipped: 1}) {
		t.Errorf("summary = %+v", rep.Summary)
	}
	if rep.ExitCode != ExitCritical {
		t.Errorf("exitCode = %d, want %d (a critical fail is present)", rep.ExitCode, ExitCritical)
	}

	// The error message must surface in JSON, classified as an error outcome.
	var sawError bool
	for _, res := range rep.Results {
		if res.ID == "ports.free" {
			sawError = res.Outcome == "error" && strings.Contains(res.Message, "ssh refused")
		}
	}
	if !sawError {
		t.Errorf("ports.free not reported as an error with its message:\n%s", buf.String())
	}
}

func TestWriteJSONBlockersAndOmitEmpty(t *testing.T) {
	r := New()
	r.MustRegister(check.Check{ID: "req", Severity: check.SeverityCritical,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusFail}, nil
		}})
	r.MustRegister(check.Check{ID: "dep", Severity: check.SeverityCritical, Requires: []string{"req"},
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusPass}, nil
		}})

	var buf bytes.Buffer
	run, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := run.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	results := rep["results"].([]any)
	dep := results[1].(map[string]any)
	if dep["outcome"] != "blocked" {
		t.Fatalf("dep outcome = %v, want blocked", dep["outcome"])
	}
	blockers, ok := dep["blockers"].([]any)
	if !ok || len(blockers) != 1 || blockers[0] != "req" {
		t.Fatalf("dep blockers = %v, want [req]", dep["blockers"])
	}
	// req carries no summary/remediation and an empty message and blockers list:
	// all must be omitted (the omitempty contract --json consumers rely on).
	reqJSON := results[0].(map[string]any)
	for _, key := range []string{"summary", "message", "remediation", "blockers"} {
		if _, present := reqJSON[key]; present {
			t.Errorf("req JSON should omit empty %q: %v", key, reqJSON)
		}
	}
}
