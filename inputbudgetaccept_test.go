//go:build !race

package shelld

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gurupras/shelld/internal/testthreads"
)

// The accept side has to stay exact while the refusal side is fixed: a CAS loop
// that re-tested stale state, or one that dropped the limit check on retry,
// would trade the false refusal for admitting past the limit. Every reserve here
// fits on its own and only the last few do not fit together, so the number
// admitted is the capacity and nothing else.
//
// The boundary is crossed once per round, with a fresh budget each time and the
// writers spinning on the round counter rather than parked on a channel. Both
// halves of that are load-bearing, and both were measured:
//
//   - Crossing it once — 16 writers filling one budget and racing to the end —
//     catches a reserve that loads, tests the limit and then adds in 0 runs out
//     of 200, and a CAS loop that retries without re-testing in 0 out of 200.
//     Almost every attempt in such a run is made against a budget that has long
//     been full, and carries no power at all.
//   - Releasing each round by closing a channel instead of spinning catches the
//     load-then-add in 3 rounds of 200 and the CAS that does not re-test in 2.
//     Parking the writers means the runtime readies them one at a time, and by
//     the time the last is running the first is already past the boundary.
//
// As written — a crossing a round, writers spinning — 198 rounds of 200 catch the
// load-then-add and 199 of 200 catch the CAS that does not re-test, so one run
// settles it either way. An off-by-one in the limit test is caught by all 200.
//
// Built out under the race detector, which instruments every atomic load and
// turns the spin into an eight-minute hang. The mutations it catches are caught
// there too: TestUsedNeverExceedsTheLimit finds both over-admitting reserves in
// 0.2 s, because a budget that admits past its limit then holds an over-limit
// total for a watcher to read, and TestReserveAdmitsAChunkThatExactlyFits is the
// off-by-one's deterministic twin.
func TestReserveNeverAdmitsPastTheLimit(t *testing.T) {
	testthreads.Need(t, 4)
	const (
		chunk    = 64
		capacity = 32
		limit    = chunk * capacity
		rounds   = 200
	)
	// One fewer writer than there are threads, so the round can finish on a
	// thread of its own. With as many writers as threads the round ends only once
	// the runtime deschedules one of them, which costs milliseconds a round
	// instead of microseconds.
	writers := runtime.GOMAXPROCS(0) - 1
	if writers > 16 {
		writers = 16
	}
	// Enough attempts a round to cross the boundary with room to spare either
	// side of it, whatever the writer count came out as.
	each := 2*capacity/writers + 1

	var budget atomic.Pointer[InputBudget]
	var admitted atomic.Int64
	var round atomic.Int64 // the round the writers may start
	var finished sync.WaitGroup
	var wg sync.WaitGroup

	budget.Store(NewInputBudget(limit))
	round.Store(-1)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := int64(0); r < rounds; r++ {
				for round.Load() < r {
				}
				b := budget.Load()
				for j := 0; j < each; j++ {
					if b.reserve(chunk) {
						admitted.Add(1)
					}
				}
				finished.Done()
			}
		}()
	}

	bad, worst, worstUsed := 0, int64(0), int64(0)
	for r := int64(0); r < rounds; r++ {
		b := NewInputBudget(limit)
		budget.Store(b)
		admitted.Store(0)
		finished.Add(writers)
		round.Store(r)
		finished.Wait()
		got, used := admitted.Load(), b.used.Load()
		if got != capacity || used != limit {
			bad++
			if got > worst {
				worst, worstUsed = got, used
			}
		}
	}
	wg.Wait()

	if bad != 0 {
		t.Errorf("%d of %d rounds admitted the wrong number of chunks;"+
			" worst was %d chunks of %d into a %d-byte budget, leaving used=%d, want %d and %d",
			bad, rounds, worst, chunk, limit, worstUsed, capacity, limit)
	}
}
