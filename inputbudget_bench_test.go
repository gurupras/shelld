package shelld

import (
	"sync/atomic"
	"testing"
)

// reserve is on the per-keystroke path — push calls it under the window lock for
// every write — so replacing one atomic add with a load plus a compare-and-swap
// has to be priced rather than assumed free. The uncontended arm is the ordinary
// keystroke: one window, the budget far from its limit.
func BenchmarkInputBudgetReserveRelease(b *testing.B) {
	bg := NewInputBudget(DefaultInputBudget)
	for i := 0; i < b.N; i++ {
		if bg.reserve(8) {
			bg.release(8)
		}
	}
}

// The contended arm is many windows sharing one budget, which is what they do
// when the caller passes none of its own. A CAS retries where an add cannot, so
// this is the arm where the primitive itself costs something: 59 ns against the
// add's 32 on a 24-thread box. The rate makes that irrelevant — the call happens
// once per queued write, not once per byte.
func BenchmarkInputBudgetReserveReleaseParallel(b *testing.B) {
	bg := NewInputBudget(DefaultInputBudget)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if bg.reserve(8) {
				bg.release(8)
			}
		}
	})
}

// A budget at its limit is the arm where the two differ in kind, and it is the
// regime the false-refusal bug lives in: every reserve fails, so the
// add-then-back-out pays two atomic writes and dirties the line for every other
// window, while the CAS pays one load and writes nothing at all. 0.08 ns against
// the add's 32 — the line stays shared in every core's cache, so the load never
// leaves it. The figure is per call across all 24 threads; a single thread is
// around 2.5 ns a call, which is the loop and the load and nothing else.
func BenchmarkInputBudgetReserveRefusedParallel(b *testing.B) {
	const limit = 1024
	bg := NewInputBudget(limit)
	if !bg.reserve(limit) {
		b.Fatal("could not fill the budget")
	}
	// Counted rather than failed inside the body: FailNow may only be called
	// from the goroutine running the benchmark, and RunParallel's body is not.
	var admitted atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if bg.reserve(8) {
				admitted.Add(1)
			}
		}
	})
	if got := admitted.Load(); got != 0 {
		b.Fatalf("admitted %d reserves into a full budget", got)
	}
}

// The realistic per-keystroke cost: one push, one next, one done, the whole
// accounting round trip for a single write. reserve runs under the window's own
// mutex here, so this arm says whether the extra atomic is visible at all next to
// the lock it already sits inside.
func BenchmarkInputQueueKeystrokeRoundTrip(b *testing.B) {
	q := newInputQueue(0, NewInputBudget(DefaultInputBudget))
	key := []byte("x")
	for i := 0; i < b.N; i++ {
		if err := q.push(inputChunk{data: key}); err != nil {
			b.Fatal(err)
		}
		c, ok := q.next()
		if !ok {
			b.Fatal("queue closed")
		}
		q.done(c)
	}
}

// One window per thread over a shared budget, each doing the full round trip:
// the cross-window contention a shared budget really sees, as opposed to a tight
// loop on the primitive alone. This is the only arm where the CAS is visible end
// to end — 62 ns/op against the add's 47 — and it is 16 million queued writes a
// second across the box, so it prices a rate no client produces: a window's
// input is keystrokes and pastes, and a paste is one push of many bytes.
func BenchmarkInputQueueKeystrokeRoundTripSharedBudget(b *testing.B) {
	shared := NewInputBudget(DefaultInputBudget)
	var failed atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		q := newInputQueue(0, shared) // one window per goroutine
		key := []byte("x")
		for pb.Next() {
			if err := q.push(inputChunk{data: key}); err != nil {
				failed.Add(1)
				return
			}
			c, ok := q.next()
			if !ok {
				failed.Add(1)
				return
			}
			q.done(c)
		}
	})
	if got := failed.Load(); got != 0 {
		b.Fatalf("%d goroutines could not complete a round trip", got)
	}
}
