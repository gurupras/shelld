//go:build unix

package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// NOT PINNED HERE: that stop calls signal.Stop. Dropping it survives every test
// in this package, and deliberately so -- once close(quit) has ended the
// goroutine there is nothing left to observe from inside the process, because a
// registration nobody reads from and no registration at all look the same to
// every later signal. It has no consequence in shellctl, which starts one
// watcher per process and exits, and it is left as a known gap rather than
// covered by a test that would be measuring the runtime instead of this code.

// TestWatchResizeReportsAWinchAndStopsAfterStop drives the unix implementation
// with the signal it exists for. Both halves matter to attach: a SIGWINCH that
// does not reach the callback leaves the daemon laying out for the old size for
// the rest of the session, and a callback that still runs after stop writes a
// resize frame to a connection attach has already closed.
func TestWatchResizeReportsAWinchAndStopsAfterStop(t *testing.T) {
	withoutTerminals(t)
	var calls atomic.Int64
	fired := make(chan struct{}, 16)
	stop := watchResize(func() {
		calls.Add(1)
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("sending SIGWINCH to ourselves: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("a SIGWINCH did not reach the callback: a resized terminal would never be reported to the daemon")
	}

	stop()
	was := calls.Load()

	// After stop, watchResize has unregistered the handler and its goroutine has
	// returned, so this signal must go nowhere. Go's default disposition for
	// SIGWINCH once nothing is listening is to ignore it, so this does not end
	// the test process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("sending the second SIGWINCH: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if now := calls.Load(); now != was {
		t.Errorf("the callback ran %d more times after stop: attach's callback writes to a connection that is being closed", now-was)
	}
}
