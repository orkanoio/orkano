package checks

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/orkanoio/orkano/api/check"
)

// erroring returns a check whose probe always fails to run (an error, not a
// StatusFail) — the "unknown" outcome the score must treat as unhardened.
func erroring(id string, sev check.Severity) check.Check {
	return check.Check{
		ID:       id,
		Severity: sev,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{}, errors.New(id + " could not run")
		},
	}
}

func scoreOf(t *testing.T, checks ...check.Check) Score {
	t.Helper()
	r := New()
	for _, c := range checks {
		r.MustRegister(c)
	}
	return runIDs(t, r).Score()
}

func TestScorePerfectAndZero(t *testing.T) {
	// Every scored check passing is the only way to read 100; every one failing
	// the only way to read 0.
	perfect := scoreOf(t,
		fixed("a", check.SeverityCritical, check.StatusPass, nil),
		fixed("b", check.SeverityWarning, check.StatusPass, nil),
		fixed("c", check.SeverityInfo, check.StatusPass, nil),
	)
	if perfect.Value != 100 {
		t.Fatalf("all-pass Value = %d, want 100", perfect.Value)
	}
	if perfect.Earned != perfect.Possible {
		t.Fatalf("all-pass Earned=%d Possible=%d, want equal", perfect.Earned, perfect.Possible)
	}
	if perfect.Passed != 3 || perfect.Scored != 3 {
		t.Fatalf("all-pass Passed=%d Scored=%d, want 3/3", perfect.Passed, perfect.Scored)
	}

	zero := scoreOf(t,
		fixed("a", check.SeverityCritical, check.StatusFail, nil),
		fixed("b", check.SeverityInfo, check.StatusFail, nil),
	)
	if zero.Value != 0 {
		t.Fatalf("all-fail Value = %d, want 0", zero.Value)
	}
	if zero.Earned != 0 || zero.Passed != 0 {
		t.Fatalf("all-fail Earned=%d Passed=%d, want 0/0", zero.Earned, zero.Passed)
	}
	if zero.Scored != 2 {
		t.Fatalf("all-fail Scored=%d, want 2 (failures are still scored)", zero.Scored)
	}
}

func TestScoreExcludesSkipped(t *testing.T) {
	// A not-applicable check is outside the score entirely: it cannot lift a
	// failing install to a pass, nor count against a passing one.
	s := scoreOf(t,
		fixed("pass", check.SeverityCritical, check.StatusPass, nil),
		fixed("na", check.SeverityCritical, check.StatusSkip, nil),
	)
	if s.Value != 100 {
		t.Fatalf("Value = %d, want 100 (the skip must not drag it down)", s.Value)
	}
	if s.Scored != 1 || s.Passed != 1 {
		t.Fatalf("Scored=%d Passed=%d, want 1/1 (the skip is excluded)", s.Scored, s.Passed)
	}
	if s.Possible != severityWeight(check.SeverityCritical) {
		t.Fatalf("Possible=%d, want only the one scored critical's weight", s.Possible)
	}
}

func TestScoreErroredAndBlockedCountAgainst(t *testing.T) {
	// error and blocked are "unknown", which is never hardening credit — each
	// must weigh against the score exactly like a fail.
	errored := scoreOf(t,
		fixed("ok", check.SeverityInfo, check.StatusPass, nil),
		erroring("boom", check.SeverityInfo),
	)
	if errored.Value != 50 {
		t.Fatalf("one pass + one error (equal weight) Value=%d, want 50", errored.Value)
	}
	if errored.Passed != 1 || errored.Scored != 2 {
		t.Fatalf("errored: Passed=%d Scored=%d, want 1/2", errored.Passed, errored.Scored)
	}

	// An error must contribute to the score identically to a fail — the whole
	// point of "unknown is never hardened". Same shape, fail instead of error.
	failed := scoreOf(t,
		fixed("ok", check.SeverityInfo, check.StatusPass, nil),
		fixed("bad", check.SeverityInfo, check.StatusFail, nil),
	)
	if failed.Value != errored.Value || failed.Scored != errored.Scored || failed.Passed != errored.Passed {
		t.Fatalf("fail contribution %+v != error contribution %+v (must be identical)", failed, errored)
	}

	// A dependent blocked by a FAILED requirement is scored-but-not-earned.
	blocked := scoreOf(t,
		fixed("req", check.SeverityInfo, check.StatusFail, nil),
		fixed("dep", check.SeverityInfo, check.StatusPass, nil, "req"),
	)
	if blocked.Value != 0 {
		t.Fatalf("fail + blocked Value=%d, want 0", blocked.Value)
	}
	if blocked.Scored != 2 || blocked.Passed != 0 {
		t.Fatalf("blocked: Scored=%d Passed=%d, want 2/0", blocked.Scored, blocked.Passed)
	}

	// A dependent blocked by an ERRORED requirement scores the same way: the
	// block still counts against the score, never as a skip.
	errBlocked := scoreOf(t,
		erroring("ereq", check.SeverityCritical),
		fixed("edep", check.SeverityCritical, check.StatusPass, nil, "ereq"),
	)
	if errBlocked.Value != 0 {
		t.Fatalf("error + blocked Value=%d, want 0", errBlocked.Value)
	}
	if errBlocked.Scored != 2 || errBlocked.Passed != 0 {
		t.Fatalf("error-blocked: Scored=%d Passed=%d, want 2/0", errBlocked.Scored, errBlocked.Passed)
	}
}

func TestScoreWeightsSeverity(t *testing.T) {
	// Passing the critical and failing the info must score far higher than
	// passing the info and failing the critical — the weighting is the point.
	critPassInfoFail := scoreOf(t,
		fixed("crit", check.SeverityCritical, check.StatusPass, nil),
		fixed("info", check.SeverityInfo, check.StatusFail, nil),
	)
	infoPassCritFail := scoreOf(t,
		fixed("crit", check.SeverityCritical, check.StatusFail, nil),
		fixed("info", check.SeverityInfo, check.StatusPass, nil),
	)
	if critPassInfoFail.Value <= infoPassCritFail.Value {
		t.Fatalf("critical-pass score %d not greater than info-pass score %d",
			critPassInfoFail.Value, infoPassCritFail.Value)
	}
	// 10 of 11 earned → 91 (rounded); 1 of 11 earned → 9 (rounded).
	if critPassInfoFail.Value != 91 {
		t.Fatalf("critical-pass Value=%d, want 91", critPassInfoFail.Value)
	}
	if infoPassCritFail.Value != 9 {
		t.Fatalf("info-pass Value=%d, want 9", infoPassCritFail.Value)
	}
}

func TestScoreImperfectRunNeverReadsAFalseAbsolute(t *testing.T) {
	// Register nCrit critical checks of one status plus one info check of the
	// other, then score the run — driving a real Score() call into each clamp.
	scoreWith := func(nCrit int, critStatus, infoStatus check.Status) Score {
		r := New()
		for i := 0; i < nCrit; i++ {
			r.MustRegister(fixed(fmt.Sprintf("c%d", i), check.SeverityCritical, critStatus, nil))
		}
		r.MustRegister(fixed("info", check.SeverityInfo, infoStatus, nil))
		return runIDs(t, r).Score()
	}

	// 20 passing criticals (200) + 1 failing info: earned=200, possible=201. The
	// raw percentage rounds to exactly 100, so the honesty clamp is what forces
	// 99 — an imperfect install must never brag a perfect score. 20 is the
	// smallest count that drives a real Score() call into the >=100 clamp.
	nearPerfect := scoreWith(20, check.StatusPass, check.StatusFail)
	if nearPerfect.Earned == nearPerfect.Possible {
		t.Fatalf("test setup: expected an imperfect run (Earned=%d Possible=%d)", nearPerfect.Earned, nearPerfect.Possible)
	}
	if nearPerfect.Value != 99 {
		t.Fatalf("near-perfect Value=%d, want 99 (clamp: imperfect never reads 100)", nearPerfect.Value)
	}

	// The mirror: 20 failing criticals + 1 passing info: earned=1, possible=201.
	// The raw percentage rounds to 0, so the clamp forces 1 — some credit was
	// earned, so the score must never read a bare 0.
	nearZero := scoreWith(20, check.StatusFail, check.StatusPass)
	if nearZero.Value != 1 {
		t.Fatalf("near-zero Value=%d, want 1 (clamp: some credit never reads 0)", nearZero.Value)
	}
}

func TestScoreVacuousWhenNothingApplies(t *testing.T) {
	// An empty run, or one where every check is skipped, is vacuously fully
	// hardened rather than a divide-by-zero.
	empty := (&Run{}).Score()
	if empty.Value != 100 || empty.Scored != 0 {
		t.Fatalf("empty run Score=%+v, want Value 100 Scored 0", empty)
	}

	allSkip := scoreOf(t,
		fixed("a", check.SeverityCritical, check.StatusSkip, nil),
		fixed("b", check.SeverityInfo, check.StatusSkip, nil),
	)
	if allSkip.Value != 100 || allSkip.Scored != 0 || allSkip.Possible != 0 {
		t.Fatalf("all-skip Score=%+v, want Value 100 Scored 0 Possible 0", allSkip)
	}
}

func TestScorePercent(t *testing.T) {
	// Exercise the arithmetic and both honesty clamps at their exact boundaries,
	// which are fiddly to hit through whole runs.
	tests := []struct {
		name                   string
		earned, possible, want int
	}{
		{"vacuous", 0, 0, 100},           // nothing to score — vacuously hardened
		{"fully earned", 5, 5, 100},      // perfect
		{"nothing earned", 0, 5, 0},      // bare zero
		{"rounds down", 10, 11, 91},      // 90.9 -> 91
		{"info fail of 11", 1, 11, 9},    // 9.09 -> 9
		{"rounds up", 2, 3, 67},          // 66.7 -> 67
		{"rounds down thirds", 1, 3, 33}, // 33.3 -> 33
		{"clamp high", 200, 201, 99},     // raw 100 but imperfect -> 99
		{"clamp low", 1, 201, 1},         // raw 0 but some credit -> 1
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scorePercent(tt.earned, tt.possible); got != tt.want {
				t.Fatalf("scorePercent(%d, %d) = %d, want %d", tt.earned, tt.possible, got, tt.want)
			}
		})
	}
}

func TestSeverityWeightOrdering(t *testing.T) {
	// The weights must be strictly ordered critical > warning > info; the score's
	// meaning depends on it. An unclassified severity gets the floor weight.
	if severityWeight(check.SeverityCritical) <= severityWeight(check.SeverityWarning) ||
		severityWeight(check.SeverityWarning) <= severityWeight(check.SeverityInfo) {
		t.Fatalf("weights not strictly ordered: crit=%d warn=%d info=%d",
			severityWeight(check.SeverityCritical), severityWeight(check.SeverityWarning),
			severityWeight(check.SeverityInfo))
	}
	if severityWeight(check.Severity("bogus")) != severityWeight(check.SeverityInfo) {
		t.Fatalf("unknown severity weight = %d, want the info floor %d",
			severityWeight(check.Severity("bogus")), severityWeight(check.SeverityInfo))
	}
}
