package main

import (
	"runtime"
	"testing"
	"time"
)

// TestWatchResizeStopEndsItsGoroutine pins the half of watchResize's contract
// that is the same on both platforms and is invisible in normal use, because
// attach calls stop on its way out and then the process exits anyway. It is the
// half that a change breaks silently: the unix version has two things to undo
// (signal.Stop and close(quit)) and the Windows version has two (close(quit) and
// the ticker it stops on the way out of its own goroutine), and dropping the
// close leaves a goroutine holding a live reference to the callback -- which
// closes over the websocket connection -- for the life of the process.
func TestWatchResizeStopEndsItsGoroutine(t *testing.T) {
	withoutTerminals(t)

	// One watcher started and stopped before the baseline is taken, because the
	// first watchResize in a unix process leaves a goroutine behind that is not
	// its own: signal.Notify starts the runtime's signal-delivery loop, and that
	// one never exits. If stop does nothing at all this warm-up leaks too, which
	// only raises the baseline -- the second watcher still puts the count above
	// it.
	watchResize(func() {})()
	before := goroutinesSettled()

	stop := watchResize(func() {})
	if stop == nil {
		t.Fatal("watchResize returned a nil stop: attach defers it, so nil panics on the way out of an attached session")
	}
	stop()

	if after := goroutinesSettled(); after > before {
		t.Errorf("goroutines: %d before watchResize, %d after stop -- its goroutine is still running and still holding the callback", before, after)
	}
}

// goroutinesSettled waits for the count to hold steady before reporting it, so
// the comparison is not decided by some unrelated goroutine that was still
// starting up or still winding down. It is an upper bound either way: this test
// can show a goroutine that stayed, and cannot show which one.
func goroutinesSettled() int {
	last := -1
	for i := 0; i < 200; i++ {
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
		time.Sleep(5 * time.Millisecond)
	}
	return last
}
