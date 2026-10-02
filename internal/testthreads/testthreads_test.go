package testthreads

import (
	"runtime"
	"testing"
)

// Two tests that need the same raise overlap, and the one that finishes first must
// not put GOMAXPROCS back while the other is still running on it.
//
// The overlap is driven through a stand-in for testing.TB rather than two parallel
// subtests, because a parallel pair only overlaps when -test.parallel is above 1,
// and it defaults to GOMAXPROCS -- so it is 1 in precisely the arm this matters in,
// a suite invoked at GOMAXPROCS=1, and a pair of subtests there would serialize and
// report green having overlapped nothing. Two Need calls with their cleanups run in
// order are the same sequence with nothing left to the scheduler.
func TestAnOverlappingCallerDoesNotRestoreUnderTheFirst(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs 2 cores for Need(2) to raise to rather than skip")
	}
	was := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(was) })

	first, second := &recordingTB{}, &recordingTB{}
	Need(first, 2)
	if got := runtime.GOMAXPROCS(0); got != 2 {
		t.Fatalf("Need(2) left GOMAXPROCS at %d, so nothing below is the state it names", got)
	}
	Need(second, 2)
	if n := first.skips + second.skips; n != 0 {
		t.Fatalf("%d of the 2 callers skipped on a machine with %d cores, so neither took a raise to hold", n, runtime.NumCPU())
	}

	first.runCleanups()
	if got := runtime.GOMAXPROCS(0); got < 2 {
		t.Errorf("GOMAXPROCS fell to %d when the first of two callers finished while the second still needs 2: the second's goroutines can no longer overlap, which is the failure Need exists to prevent", got)
	}
	second.runCleanups()
	if got := runtime.GOMAXPROCS(0); got != 1 {
		t.Errorf("GOMAXPROCS is %d after the last caller finished, want the 1 it found: a raise that outlives its test changes what every later test runs under", got)
	}
}

// A caller that needs no raise holds nothing, so it cannot defer another's restore
// past its own test. It also must not touch GOMAXPROCS at all: setting it, even to
// the value it already has, is what switches the runtime's automatic updating off.
func TestACallerThatNeedsNoRaiseHoldsNothing(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs 2 cores for Need(2) to raise to rather than skip")
	}
	was := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(was) })

	tb := &recordingTB{}
	Need(tb, 2)
	if len(tb.cleanups) != 0 {
		t.Errorf("Need registered %d cleanups with GOMAXPROCS already at 2; each one is a restore of a raise that never happened", len(tb.cleanups))
	}
}

// recordingTB stands in for a test that has called Need, so a test can run the
// cleanup Need registered at a moment of its choosing. Only the three methods Need
// uses are implemented.
//
// The embedded testing.TB is forced rather than convenient: TB carries an unexported
// private() method for the express purpose of stopping anything outside package
// testing from implementing it, so a stand-in has to embed one. Embedding an
// interface left nil is also what turns a method this does not define into a nil
// dereference at run time instead of a compile error at the edit that needed it.
// That panic fails this test and aborts the rest of the package with it, so it is
// loud and cannot be mistaken for a pass -- but it is not a targeted failure, and
// the cost is the tests that had not run yet. If Need grows a fourth dependency,
// define it here rather than reading the panic as a verdict on Need.
//
// Skipf records instead of stopping its caller, unlike the real one, so a fixture
// built on this must arrange for Need not to reach it -- which is what the core
// count is checked for above.
type recordingTB struct {
	testing.TB
	cleanups []func()
	skips    int
}

func (r *recordingTB) Helper()              {}
func (r *recordingTB) Cleanup(f func())     { r.cleanups = append(r.cleanups, f) }
func (r *recordingTB) Skipf(string, ...any) { r.skips++ }

// runCleanups runs what was registered, last first, as testing does.
func (r *recordingTB) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}
