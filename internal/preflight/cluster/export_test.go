package cluster

import "time"

// SetLiveProbeTimingForTest shrinks the live-probe budget and poll interval so
// timeout and propagation paths are testable without making the suite slow.
func SetLiveProbeTimingForTest(budget, poll time.Duration) (restore func()) {
	originalBudget, originalPoll := liveProbeWaitBudget, liveProbePollInterval
	liveProbeWaitBudget, liveProbePollInterval = budget, poll
	return func() {
		liveProbeWaitBudget, liveProbePollInterval = originalBudget, originalPoll
	}
}

// SetScratchCleanupTimeoutForTest shortens namespace-deletion verification in
// the cleanup failure test.
func SetScratchCleanupTimeoutForTest(timeout time.Duration) (restore func()) {
	original := scratchCleanupTimeout
	scratchCleanupTimeout = timeout
	return func() {
		scratchCleanupTimeout = original
	}
}
