//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// expandedTableSlots mirrors shelld.descriptorTableSlots, which is unexported, so
// this file cannot read it. A change there reds this test rather than weakening
// it silently, and the failure below says so.
const expandedTableSlots = 4096

// fdSize reads the descriptor table's capacity from /proc/<pid>/status.
//
// FDSize is the table's capacity and not the number of descriptors open, which is
// what makes it the field that can see an expansion at all: the count is unchanged
// by one, since reserveDescriptorTable closes the slot it parked in.
func fdSize(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1 // the daemon exited; the caller's poll decides what that means
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "FDSize:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				t.Fatalf("FDSize %q: %v", rest, err)
			}
			return n
		}
	}
	t.Fatalf("no FDSize in /proc/%d/status", pid)
	return 0
}

// TestDescriptorTableIsExpandedWithoutHoldingUpTheListener covers the half of
// cmdRun's `go shelld.InitDescriptorTable()` that a working daemon does not
// demonstrate on its own.
//
// The goroutine exists so that the expansion's RCU grace period does not land on
// the startup figure -- 225752c paid it ahead of the listen and cost 3.4 ms to
// 18.4 ms exec-to-listen. But a daemon that came up fast because the expansion
// silently stopped happening is indistinguishable, from the outside, from one that
// came up fast because the expansion moved off the critical path: both answer
// quickly and serve windows correctly, and the difference only shows up later as
// the 20 ms create the expansion was introduced to remove. Dropping the keyword's
// call entirely would pass every other test in this package.
//
// So what is asserted here is that the expansion still happens. FDSize is the field
// that can tell: a daemon with no up-front expansion reaches 256 on this box by
// growing organically, against 4096 for one that reserved, so the assertion is
// against a value the unexpanded case does not reach rather than against a bound
// near it.
//
// What is NOT asserted is the latency, and the scope limit is worth stating
// plainly: replacing the goroutine with a plain call -- which is the regression
// this was written alongside -- PASSES this test. The expansion still happens, so
// the assertion below is still satisfied; only the startup figure moves. Nothing
// in this package's suite fails for that change.
//
// It is left uncovered rather than approximated. The quantity is a kernel RCU grace
// period, measured between 5 and 28 ms across reps on one box and a function of
// that kernel's RCU configuration and its load, so a latency threshold here would
// be a bound read off a property of the machine running the test -- green on a box
// where the grace period is short for reasons that have nothing to do with where
// the call sits. The one field that does separate the placements is logged below,
// with the measurement showing why it cannot be a pass condition either. The
// ordering claim rests on measurement across 225752c and its parent instead, and
// the comment at the call site carries those figures.
func TestDescriptorTableIsExpandedWithoutHoldingUpTheListener(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("reading RLIMIT_NOFILE: %v", err)
	}
	// Below the full reservation the daemon clamps to the soft limit, and the
	// expected capacity becomes a function of that limit rather than of the
	// constant. That path has its own test in the shelld package
	// (TestInitDescriptorTableClampsToTheSoftLimit); skipping keeps this one from
	// asserting a number it did not derive.
	if lim.Cur < expandedTableSlots {
		t.Skipf("RLIMIT_NOFILE soft limit is %d, below the %d-slot reservation: the daemon clamps, and the expected capacity is not this constant", lim.Cur, expandedTableSlots)
	}

	p := newPaths(t)
	c := p.startRun(t) // returns once the daemon answers on the socket
	pid := c.Process.Pid

	// Recorded and not asserted, and the numbers are why rather than caution.
	//
	// This field does separate the two placements: with the goroutine it reads 64
	// here, and with the call serialised ahead of the listen it reads 4096, because
	// then the listen is what waited. So it looks like the assertion that would pin
	// the ordering directly -- and it was measured before being trusted. Over 12 runs
	// with the goroutine in place it read 64 ten times and 4096 twice: the expansion
	// sometimes does finish first, since a ~16 ms grace period only has to beat this
	// process's remaining startup, and exec jitter occasionally lets it. Asserting it
	// would be a false red on correct code about one run in six.
	//
	// It is logged because when it reads 64 that is direct evidence the listen did
	// not wait, which no other field in this test gives.
	t.Logf("FDSize when the daemon first answered: %d", fdSize(t, pid))

	// Polled here rather than through waitUntil, because the failure message has to
	// carry the daemon's log as it reads when the deadline expires. waitUntil takes
	// the message as a string, so composing it at the call would read the log before
	// the first poll and report an empty one.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := fdSize(t, pid)
		if got >= expandedTableSlots {
			return
		}
		if got < 0 {
			// Not the expansion's failure: there is no process left to expand
			// anything, and reporting this as a missing reservation would name the
			// wrong cause.
			t.Fatalf("the daemon exited during the test, so nothing here measured the reservation -- %s", logTail(t, p))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the descriptor table capacity stayed at %d, below the %d-slot reservation, so this daemon is back to paying an RCU grace period inside a window create: either cmdRun's goroutine is gone or InitDescriptorTable declined -- %s -- (if descriptorTableSlots changed, expandedTableSlots in this file has to follow it)",
				got, expandedTableSlots, logTail(t, p))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logTail returns the daemon's log for a failure message, since a decline is
// logged rather than fatal and the reason is the whole diagnosis.
//
// A missing file is not evidence that nothing was logged: `run` in the foreground
// logs to stderr, which startRun does not capture, so the decline can be real and
// unreadable here. The message says so rather than reading as "no decline".
func logTail(t *testing.T, p paths) string {
	t.Helper()
	b, err := os.ReadFile(p.logfile)
	if err != nil {
		return "no logfile to read (" + err.Error() + "); a foreground `run` logs to stderr, which this fixture does not capture, so this is not evidence that nothing was logged"
	}
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return "daemon log: " + strings.TrimSpace(string(b))
}
