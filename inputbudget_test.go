package shelld

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/testthreads"
)

// A reserve that exactly fills the budget is admitted: the limit bounds what may
// be held, not what may be claimed. One character out in the comparison turns
// the last chunk a budget has room for into ErrInputQueueFull, and none of the
// concurrent fixtures below can see that — they cross the boundary rather than
// landing on it, so no reserve in them ever fits exactly. Single-threaded on
// purpose: the property has nothing to do with contention, which also means it
// still holds the line in builds the fixtures below are too slow for.
func TestReserveAdmitsAChunkThatExactlyFits(t *testing.T) {
	const limit = 100
	b := NewInputBudget(limit)
	if !b.reserve(60) {
		t.Fatal("60 of 100 was refused into an empty budget")
	}
	if !b.reserve(40) {
		t.Error("a reserve that exactly filled the budget was refused")
	}
	if b.reserve(1) {
		t.Error("a full budget admitted one more byte")
	}
	if got := b.used.Load(); got != limit {
		t.Errorf("used %d after filling a %d-byte budget exactly", got, limit)
	}
}

// used must never exceed the limit, at any instant. That is exactly what the
// compare-and-swap buys and what the add-then-back-out it replaced broke: the
// add publishes used+n to every window sharing the budget before undoing it, so
// the budget really does hold an over-limit total for the length of the
// back-out, and a window reserving inside that window is refused with room to
// spare.
//
// Sampled from watcher goroutines rather than inferred from refusal counts.
// Counting refusals (TestReserveDoesNotRefuseInputTheBudgetHasRoomFor) needs the
// refusal and the fitting reserve to land within a few nanoseconds of each
// other, and a run where the scheduler never overlaps the two proves nothing;
// the over-limit total is published on every single refusal, so a watcher that
// samples used finds it in every run.
func TestUsedNeverExceedsTheLimit(t *testing.T) {
	testthreads.Need(t, 4)
	const limit = 4096
	b := NewInputBudget(limit)

	// Mixed sizes so a reserve's limit test really flips under it: with one size
	// the budget divides evenly and the boundary is crossed at the same total
	// every time.
	sizes := []int64{8, 64, 512, 1024, 2048, 4096}
	workers := 2 * runtime.GOMAXPROCS(0)
	if workers > 24 {
		workers = 24
	}

	var excursions, samples atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			n := sizes[k%len(sizes)]
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < 500; j++ {
					if b.reserve(n) {
						if u := b.used.Load(); u > limit {
							excursions.Add(1)
						}
						b.release(n)
					}
				}
			}
		}(i)
	}
	// Watchers do nothing but read, so they catch an excursion no worker
	// happened to be looking at. They are the reason this test is not a race
	// between two particular goroutines.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < 10000; j++ {
					u := b.used.Load()
					samples.Add(1)
					if u > limit || u < 0 {
						excursions.Add(1)
					}
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := excursions.Load(); got != 0 {
		t.Errorf("used left [0,%d] %d times over %d samples",
			limit, got, samples.Load())
	}
	// Premise: the watchers really did look. A run that sampled nothing would
	// report no excursions for the wrong reason.
	if got := samples.Load(); got < 10000 {
		t.Errorf("the watchers took %d samples; the fixture did not run", got)
	}
	// And nothing is left behind: a refusal that failed to undo its own claim
	// leaks room on every refusal and eventually refuses everything.
	if got := b.used.Load(); got != 0 {
		t.Errorf("used=%d after every reserve was released, want 0", got)
	}
}

// The harm the invariant above causes, asserted where a caller would see it: a
// reserve that fails must not make a concurrent reserve fail. The two are
// asymmetric — a shared budget is the one bound every window sharing it sees, so
// one window pushing a chunk too large for the remaining room refuses another
// window's keystroke, and that caller gets ErrInputQueueFull with the budget
// under its limit. Reaching it needs the budget near full, which is why the
// fixture commits most of it up front rather than starting from empty.
//
// This is the readable statement of the bug and not the load-bearing guard:
// whether it catches an over-publishing reserve depends on the two goroutines
// overlapping, which is why they start on a barrier and why the assertion above
// exists as well.
func TestReserveDoesNotRefuseInputTheBudgetHasRoomFor(t *testing.T) {
	testthreads.Need(t, 2)
	const (
		limit     = 1000
		committed = 900 // held for the whole test, never released
		tooBig    = 200 // 900+200 > 1000: always refused, correctly
		fits      = 50  // 900+50 <= 1000: must never be refused
		rounds    = 200000
	)
	b := NewInputBudget(limit)
	if !b.reserve(committed) {
		t.Fatal("the fixture could not commit its baseline")
	}
	// Premise: the two sizes really are on opposite sides of the limit, so a
	// refusal of fits is always spurious and a refusal of tooBig never is.
	if b.reserve(tooBig) {
		t.Fatal("tooBig was admitted; the fixture does not exercise a failing reserve")
	}
	if !b.reserve(fits) {
		t.Fatal("fits was refused with no concurrency; the fixture is wrong")
	}
	b.release(fits)

	var spurious, overAdmitted atomic.Int64
	// Both goroutines are on a CPU and past their own setup before either
	// starts reserving. Without this the fast one can finish its rounds before
	// the other is scheduled, and the test then passes having never put two
	// reserves in flight.
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ready.Done()
		<-start
		for i := 0; i < rounds; i++ {
			if b.reserve(tooBig) {
				// Over-admission, which this bug is not; counted so the test
				// cannot pass by having stopped refusing altogether.
				overAdmitted.Add(1)
				b.release(tooBig)
			}
		}
	}()
	go func() {
		defer wg.Done()
		ready.Done()
		<-start
		for i := 0; i < rounds; i++ {
			if !b.reserve(fits) {
				spurious.Add(1)
				continue
			}
			b.release(fits)
		}
	}()
	ready.Wait()
	close(start)
	wg.Wait()

	if got := spurious.Load(); got != 0 {
		t.Errorf("%d of %d reserves were refused with %d bytes free of %d"+
			" — the budget had room for every one", got, rounds, limit-committed, limit)
	}
	if got := overAdmitted.Load(); got != 0 {
		t.Errorf("a reserve of %d was admitted %d times with only %d bytes free",
			tooBig, got, limit-committed)
	}
	// A failed reserve must leave nothing behind, or the budget leaks room on
	// every refusal and eventually refuses everything.
	if got := b.used.Load(); got != committed {
		t.Errorf("budget used %d after the round trip, want the %d still committed",
			got, committed)
	}
}
