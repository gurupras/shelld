package daemon

import "time"

// retryBusy runs op until it succeeds, fails with an error busy does not
// accept, or budget is used up. It waits 10 ms after the first failure and
// doubles each wait up to 500 ms, calling logRetry before each wait. sleep is
// time.Sleep outside tests.
func retryBusy(budget time.Duration, busy func(error) bool, op func() error,
	logRetry func(attempt int, wait time.Duration, err error), sleep func(time.Duration)) error {
	wait, spent := 10*time.Millisecond, time.Duration(0)
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !busy(err) || spent >= budget {
			return err
		}
		wait = min(wait, budget-spent)
		logRetry(attempt, wait, err)
		sleep(wait)
		spent += wait
		wait = min(2*wait, 500*time.Millisecond)
	}
}
