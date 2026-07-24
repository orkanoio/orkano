package doctor

import "time"

// SetNetpolTimingForTest installs a schedule and returns a restore func.
func SetNetpolTimingForTest(t NetpolTiming) (restore func()) {
	orig := netpolTiming
	netpolTiming = t
	return func() { netpolTiming = orig }
}

// ShrinkNetpolTimingForTest collapses every knob so the retry loop runs at test
// speed. budget bounds the whole probe and poll is the per-pod re-read; the
// settle window is derived from budget so a confirmed leak still gets its
// second batch without the test waiting on wall clock.
func ShrinkNetpolTimingForTest(budget, poll time.Duration) (restore func()) {
	return SetNetpolTimingForTest(NetpolTiming{
		WaitBudget:     budget,
		PollInterval:   poll,
		CanarySettle:   0,
		CanaryDeadline: budget,
		SettleBudget:   budget / 2,
		BatchReserve:   0,
		MinLeakBatches: 2,
		MaxBatches:     3,
	})
}

// LiveNetpolTiming is the production schedule, so a test can assert the tuning
// invariants the retry loop depends on.
func LiveNetpolTiming() NetpolTiming { return netpolTiming }

// CanaryCommandForTest renders the canary's shell command.
func CanaryCommandForTest(settle time.Duration) string { return canaryCommand(settle) }
