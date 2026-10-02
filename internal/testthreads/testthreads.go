// Package testthreads lets a test that can only observe its property with
// several goroutines running at once say so, and have it be true.
//
// It exists because GOMAXPROCS is a property of how the suite was invoked and
// not of the machine it runs on. A test whose fixture needs two goroutines to
// overlap does not fail under GOMAXPROCS=1 by finding a bug; it fails, or worse
// passes, by never having run the thing it asserts.
package testthreads

import (
	"runtime"
	"sync"
	"testing"
)

var (
	mu      sync.Mutex
	raised  int    // callers currently holding a raise, across every test
	restore func() // set while raised > 0: puts GOMAXPROCS back as the first found it
)

// Need makes sure at least n goroutines can run at once for the rest of the
// test, and skips only when the machine genuinely cannot.
//
// It raises GOMAXPROCS rather than skipping when it is below n, because skipping
// is the outcome that hides things: a suite run under GOMAXPROCS=1 would report
// every such test green having executed no part of what it asserts, and a
// fixture's pre-fix code usually passes it single-threaded. Only ever raises, and
// restores what it found when the last caller that needed the raise ends.
//
// The last, not each: GOMAXPROCS is process-wide, so overlapping callers share one
// setting, and the value to put back is the one from before any of them raised it.
// Restoring per caller lets the first to finish drop the setting out from under the
// second -- which is the failure this helper exists to prevent, arriving from the
// helper itself. No test in the suite calls Need from a parallel test today, and
// that is not what makes this safe: package shelld has eleven t.Parallel call
// sites, so one of those tests growing a Need is an ordinary edit and not a
// redesign.
//
// What is not restored is the runtime's automatic GOMAXPROCS updating, which
// runtime.GOMAXPROCS switches off for the life of the process.
// runtime.SetDefaultGOMAXPROCS would switch it back on, and it is the wrong repair
// for both of the states that get here: it ignores the GOMAXPROCS environment
// variable, so under `GOMAXPROCS=1 go test` it would abandon the setting the suite
// was invoked with, and it cannot tell an earlier explicit GOMAXPROCS call from the
// automatic default either. The runtime offers no way to ask which was in force. So
// the number goes back and the tracking does not, and what that costs is a binary
// under a cgroup quota below n keeping n afterwards instead of following the quota
// -- which changes how much of the machine the rest of the suite uses, and no
// assertion any of it makes.
func Need(tb testing.TB, n int) {
	tb.Helper()
	if runtime.NumCPU() < n {
		tb.Skipf("needs %d threads running at once, the machine has %d cores",
			n, runtime.NumCPU())
	}

	mu.Lock()
	defer mu.Unlock()
	if raised == 0 && runtime.GOMAXPROCS(0) >= n {
		return // nothing to raise, so nothing to hold and nothing to put back
	}
	if raised == 0 {
		prev := runtime.GOMAXPROCS(0)
		restore = func() { runtime.GOMAXPROCS(prev) }
	}
	raised++
	if runtime.GOMAXPROCS(0) < n {
		runtime.GOMAXPROCS(n)
	}
	tb.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		raised--
		if raised == 0 {
			restore()
			restore = nil
		}
	})
}
