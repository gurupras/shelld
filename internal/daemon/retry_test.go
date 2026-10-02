package daemon

import (
	"errors"
	"testing"
	"time"
)

func TestRetryBusy(t *testing.T) {
	errBusy, errOther := errors.New("busy"), errors.New("other")
	isBusy := func(err error) bool { return errors.Is(err, errBusy) }

	// Busy for 7 attempts, then success: waits 10, 20, 40, 80, 160, 320 ms.
	var waits []time.Duration
	calls := 0
	err := retryBusy(5*time.Second, isBusy, func() error {
		if calls++; calls <= 6 {
			return errBusy
		}
		return nil
	}, func(int, time.Duration, error) {}, func(d time.Duration) { waits = append(waits, d) })
	if err != nil || calls != 7 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	want := []time.Duration{10, 20, 40, 80, 160, 320}
	for i, w := range want {
		if waits[i] != w*time.Millisecond {
			t.Fatalf("waits %v", waits)
		}
	}

	// Always busy: gives up after the 5 s budget, having logged every retry.
	var slept time.Duration
	logged := 0
	err = retryBusy(5*time.Second, isBusy, func() error { return errBusy },
		func(int, time.Duration, error) { logged++ }, func(d time.Duration) { slept += d })
	if !errors.Is(err, errBusy) || slept != 5*time.Second || logged == 0 {
		t.Fatalf("err %v, slept %v, logged %d", err, slept, logged)
	}

	// A different error is returned at once.
	calls = 0
	err = retryBusy(5*time.Second, isBusy, func() error { calls++; return errOther },
		func(int, time.Duration, error) { t.Fatal("retried a non-busy error") }, func(time.Duration) {})
	if !errors.Is(err, errOther) || calls != 1 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}
