package checks

import "github.com/orkanoio/orkano/api/check"

// Score is the hardening score for a run: a severity-weighted percentage of the
// applicable checks that passed. It is the doctor's braggable, CI-gateable
// headline number — computed on demand and surfaced by the doctor command (in
// text and --json). The preflight and wizard formatters deliberately omit it: a
// hardening percentage belongs to doctor, not to a pass/fail install gate.
//
// Skipped (not-applicable) checks are excluded entirely — a check that does not
// apply to this install can neither raise nor lower the score. A check that
// could not be determined (errored or blocked) counts against the score exactly
// like a failure: unknown is never hardening credit, the same rule the runner
// applies everywhere else.
type Score struct {
	// Value is the headline percentage, 0–100. It reads 100 only when every
	// scored check passed — or, vacuously, when no check applies to this install
	// (Scored == 0) — and 0 only when none passed; a single unhardened check can
	// never round up to a perfect score, nor a single hardened check round down
	// to zero.
	Value int `json:"value"`
	// Earned is the summed weight of the passed checks; Possible the summed
	// weight of every scored (non-skipped) check. Value is Earned over Possible.
	Earned   int `json:"earned"`
	Possible int `json:"possible"`
	// Passed and Scored are the plain check counts behind the weighted figures.
	// They mirror Summary.Passed and Summary.Total−Summary.Skipped, carried here
	// so the score can be displayed or gated on without also holding a Summary.
	Passed int `json:"passed"`
	Scored int `json:"scored"`
}

// Score computes the hardening score for the run.
func (run *Run) Score() Score {
	var s Score
	for _, res := range run.Results {
		if res.Outcome == OutcomeSkip {
			continue // not applicable to this install — outside the score
		}
		w := severityWeight(res.Severity)
		s.Possible += w
		s.Scored++
		if res.Outcome == OutcomePass {
			s.Earned += w
			s.Passed++
		}
	}
	s.Value = scorePercent(s.Earned, s.Possible)
	return s
}

// severityWeight ranks a passed check's contribution by how much it matters: a
// critical control is worth far more than an informational one, so the score
// tracks real hardening rather than a raw check count. An unknown severity
// (never produced by the registry, which validates severity on Register) falls
// back to the lowest weight rather than crediting an unclassified check heavily.
func severityWeight(sev check.Severity) int {
	switch sev {
	case check.SeverityCritical:
		return 10
	case check.SeverityWarning:
		return 3
	default: // info, and any unexpected value defensively
		return 1
	}
}

// scorePercent converts earned/possible weight into a 0–100 figure, rounding to
// nearest but refusing a dishonest extreme: an install with any unhardened
// scored check never reads 100, and one with any hardened check never reads 0.
// An install with nothing applicable to score is vacuously 100.
func scorePercent(earned, possible int) int {
	switch {
	case possible == 0:
		return 100
	case earned == possible:
		// Must precede the rounding below: a genuine perfect score computes to
		// exactly 100, which the >=100 honesty clamp would otherwise turn into 99.
		return 100
	case earned == 0:
		return 0
	}
	p := (earned*100 + possible/2) / possible // round to nearest
	if p >= 100 {
		return 99 // imperfect: never claim a perfect score
	}
	if p <= 0 {
		return 1 // some credit earned: never report a bare zero
	}
	return p
}
