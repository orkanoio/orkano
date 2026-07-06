package doctor

import "time"

// SetNetpolTimingForTest shrinks the netpol probe's wait budget and poll
// interval so the timeout path is testable; it returns a restore func.
func SetNetpolTimingForTest(budget, poll time.Duration) (restore func()) {
	origBudget, origPoll := netpolWaitBudget, netpolPollInterval
	netpolWaitBudget, netpolPollInterval = budget, poll
	return func() {
		netpolWaitBudget, netpolPollInterval = origBudget, origPoll
	}
}
