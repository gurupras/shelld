package vt

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// heapAlloc is the total size of the live heap's objects, which is what a shared
// backing array inflates: the collector keeps a whole allocation alive for a pointer
// anywhere inside it, so a row sliced out of a slab counts the slab's full size.
//
// Two cycles, because the first can leave its own mark work visible as garbage. A read
// carries about 10 KiB of noise either way — other live objects in the test binary, and
// span accounting inside HeapAlloc — so both callers are built to put a signal far above
// that rather than to measure this more precisely.
func heapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// gridContentBytes is what the grid holds: every row's cells, counted once, plus the
// row headers. Capacity is included, so the bound below is generous about slack.
func gridContentBytes(g *Grid) int64 {
	cell := int64(unsafe.Sizeof(Cell{}))
	var n int64
	for i := range g.raw.inner {
		n += int64(cap(g.raw.inner[i].inner)) * cell
	}
	return n + int64(len(g.raw.inner))*int64(unsafe.Sizeof(Row{}))
}

// A window resized over and over while output keeps arriving must not accumulate
// reachable bytes. The grid's content is bounded by the history limit, so its heap is
// too, and a storm is just the same bounded structure rebuilt.
//
// allocRows used to back a whole chunk of rows with one cell slab, capping each row's
// capacity so appends could not reach the neighbour. Capping bounds writes, not
// reachability: one surviving row kept all thousand rows of its slab. initialize
// allocates maxCacheSize rows to add one and a reflowing resize then installs an
// exact-sized array with no spare, so every resize allocated a fresh slab, used a row
// or two of it, and pinned the rest — 4 MiB per resize, without limit. At the sizes
// here that was 306 MiB of growth against a 2 MiB grid, a factor of 154.
//
// Both halves of the workload are needed. Without the drip the grid reaches a fixed
// point after one round trip and reflowing an unchanging history allocates no rows at
// all, so the test would pass with the slab in place.
func TestResizeStormDoesNotRetainPastRows(t *testing.T) {
	const (
		history = 1000
		cols    = 200
		lines   = 50
		narrow  = 120
		short   = 30
		warmup  = 40
		measure = 80
	)
	term := NewTerm(Config{ScrollingHistory: history}, cols, lines, nil)
	p := NewProcessor()
	line := strings.Repeat("x", cols) + "\r\n"
	for range history + lines {
		feed(term, p, line)
	}

	storm := func(n int) {
		for range n {
			feed(term, p, line+line+line+line) // output during the storm, as a shell's is
			term.Resize(narrow, short)
			term.Resize(cols, lines)
		}
	}

	// Warm up first: the fill leaves one partly used chunk, and the first resizes
	// settle the history at what the reflow's limit allows. The claim is about a
	// storm in progress, so both samples are taken inside one.
	storm(warmup)
	var ms runtime.MemStats
	before := heapAlloc()
	runtime.ReadMemStats(&ms)
	allocBefore := ms.TotalAlloc
	contentBefore := gridContentBytes(term.Grid())
	storm(measure)
	after := heapAlloc()
	runtime.ReadMemStats(&ms)
	allocated := int64(ms.TotalAlloc - allocBefore)
	content := max(contentBefore, gridContentBytes(term.Grid()))
	runtime.KeepAlive(term)
	runtime.KeepAlive(p)

	growth := int64(after) - int64(before)
	t.Logf("%d toggles (%d Resize calls): heap %d KiB -> %d KiB (growth %d KiB), allocated %d KiB, grid content %d KiB, history %d lines",
		measure, 2*measure, before>>10, after>>10, growth>>10, allocated>>10, content>>10, term.Grid().HistorySize())

	// A premise, not a result, and it holds either way: for the storm to be able to
	// retain anything it has to allocate rows, which means the drip has to be reaching
	// history and initialize has to be running. The trim inside the reflow settles
	// history well below its limit, so the count of lines is not the thing to check —
	// the allocation is. Measured at these sizes the storm allocates 1,072 MiB against a
	// 2,032 KiB grid, 527x, so the 20x the guard asks for has 26x of room under it.
	if allocated < 20*content {
		t.Fatalf("%d toggles allocated only %d KiB against a %d KiB grid; the storm is not growing history, so it cannot show retention either",
			measure, allocated>>10, content>>10)
	}
	// One whole grid's worth of growth is already far more than a steady state needs;
	// the slab overshot this by a factor of 154.
	if growth > content {
		t.Errorf("%d toggles grew the live heap by %d KiB, more than the whole %d KiB grid; rows from earlier grids are still reachable",
			measure, growth>>10, content>>10)
	}
}

// The invariant the test above rests on, stated directly: each row's cells are its own
// allocation, so the rows a reflow keeps out of a chunk keep only themselves.
//
// It is not redundant with the storm, and the two divide the space cleanly. Putting the
// slab back at a chosen sharing factor of k rows per slab, the storm reds only at the
// whole chunk, where it grows 306 MiB against its 2,032 KiB bound; at k=2, 3, 4, 8 and
// 16 it grows 2 to 1,285 KiB and stays green. This reds at every one of them. The storm
// is the end-to-end symptom and catches only the extreme; this is the property.
//
// The storm's blindness to small k is structural, not a matter of running it longer. Its
// bound is one grid, and a slab leaks only while it holds both a survivor and dead rows,
// so at sharing factor k a survivor costs at most k-1 extra rows. The reflow keeps
// contiguous runs, which puts most survivors in slabs that are wholly live and most dead
// rows in slabs that are wholly dead, so the excess stays a bounded multiple of a bounded
// grid instead of accumulating. At the whole chunk that argument fails outright: one
// surviving row pins 999 rows of a grid that is otherwise entirely gone, every resize,
// without limit. So no measure count makes the storm see k=2, and this test is not a
// faster version of it.
//
// Why it keeps a hundred rows and not one. A read of the live heap carries about 10 KiB
// of noise, so a witness that keeps one row measures a 4 KiB signal underneath it. That
// is what this test used to do, and over 78 fresh processes it read 4, 9 and 14 KiB
// against a 15.6 KiB limit while the k=4 arm read 16 — a coin flip on both sides. It
// never went red, which is the worst shape for a bound: it would have fired first
// somewhere else. Recalibrating cannot fix it, because correct code and k=2 differ by
// one row wherever the line is drawn. It was also sensitive to how it was invoked —
// whole-package runs read 4 KiB in 36 of 36 where -run filtered ones spanned 4 to 14,
// since this test sees a warm heap in one and a cold heap in the other.
//
// Keeping every stride'th row multiplies the signal by the number kept and leaves the
// noise where it is, because the noise belongs to the read and not to the chunk. At
// stride 1000, 100, 20, 10, 5 and 2 the read comes to 2.37x the kept rows' own size at
// one row kept and 1.03x from ten rows up.
//
// What the read comes to has a closed form, which is worth having because it says why no
// k escapes rather than only that none of the k's tried did. Sharing k rows per slab, the
// kept rows land in the distinct slabs {i/k : i = 0, stride, 2*stride, ...}, and each of
// those costs the size class of k*columns cells. For stride 10 and 200 columns that is
// 100 slabs of 4,096 B at k=1, 100 of 8,192 at k=2, and from k=10 up every slab holds a
// kept row, so it is the whole chunk: 4,000 KiB at k=10 falling weakly to 3,908 at
// k=1,000. The arms do not rise to the chunk, they settle onto it from a hair above and
// decline 2.4% as the rounding waste per slab shrinks. The floor is 6.7x this bound, so
// every k in [2, 1000] reds, with the thinnest margin at k=2, which clears the limit by
// 1.37x.
//
// Measured, three fresh processes per arm, against the 585 KiB limit this computes:
// correct code 400 to 410 KiB, k=2 at 800, k=3 at 1,200, k=4 at 1,600, k=8 at 3,200,
// k=16 at 3,968 and one shared slab at 3,912. Every arm reds. Correct code keeps at
// least 175 KiB of headroom and k=2 clears the limit by 215 — both around twenty times
// the noise, where the old form could not see k=2 at all. Do not read the low digit of
// the correct-code arm as stable: 400 reproduces in whole-package runs and a filtered
// one has been seen at 410. It is the margin that reproduces, not the reading.
//
// Two cheaper witnesses were tried and both are unsound, so do not simplify into them.
// Slice headers: rows carved from a slab have non-overlapping [base, base+cap), so an
// overlap check sees nothing, and the natural repair — row i's end equals row i+1's
// base — also fires on independent allocations, because they come out of one
// size-class span and are evenly spaced too. It discriminates only where a row's byte
// size falls short of its size class and leaves a gap: at 200 columns a row is 4,000 B
// in a 4,096 B class, so there are 96 B between neighbours, but at 160, 512 or 1,024
// columns a row is exactly 3,200, 10,240 or 20,480 B, all of them size classes, and
// independent rows are flush against each other — so the check would report sharing for
// correct code at three of the widths a window can actually have. (256 columns is not
// one of them: 5,120 B rounds up to the 5,376 B class.) Write-through aliasing: reading
// past a row's cap and
// comparing with the next row's first cell reads the neighbour under a slab and an
// unrelated object otherwise — but the cells are blank, so the unrelated object usually
// compares equal, and the read is out of bounds besides. The live heap is the only
// sound witness here.
func TestAllocRowsDoesNotShareOneBackingArray(t *testing.T) {
	const (
		n       = maxCacheSize
		columns = 200
		// Scattered, as a reflow keeps a few rows out of a chunk and drops the rest.
		stride = 10
	)
	row := int64(columns) * int64(unsafe.Sizeof(Cell{}))
	base := heapAlloc()
	rows := allocRows(n, columns)
	// The kept rows are copied out, not sliced: `rows[:len]` would hold the whole
	// 1000-entry Row array through an interior pointer, which is the very mechanism
	// under test one level up, and would add a systematic 31 KiB to every arm (Row is 32 B).
	keep := make([]Row, 0, n/stride)
	for i := 0; i < n; i += stride {
		keep = append(keep, rows[i])
	}
	rows = nil
	held := int64(heapAlloc()) - int64(base)
	runtime.KeepAlive(keep)

	own := int64(len(keep)) * row
	limit := own * 3 / 2 // half a row of slack each; the noise is a fortieth of one
	t.Logf("holding %d rows of %d keeps %d KiB reachable; their own cells are %d KiB, limit %d KiB",
		len(keep), n, held>>10, own>>10, limit>>10)

	// A premise, not a result. held is a difference of two readings of a process-wide
	// counter, so anything the runtime frees between them subtracts from it, and the
	// assertion below is one-sided: a read that lost more than it gained passes under a
	// slab as readily as without one. The kept rows cannot hold less than their own
	// cells, so a reading materially under that is a failed measurement rather than a
	// verdict, and it must be loud instead of green. This does not make the bound immune
	// to cancellation — masking k=2 would need 215 KiB to vanish, twenty times the
	// noise — it removes the case where nothing was measured at all.
	if held < own/2 {
		t.Fatalf("holding %d rows of %d read only %d KiB against %d KiB of cells they own: the heap reading did not see the rows, so it cannot rule on sharing either",
			len(keep), n, held>>10, own>>10)
	}
	if held > limit {
		t.Errorf("holding %d rows of %d kept %d KiB reachable, want at most %d KiB: the rows share a backing array",
			len(keep), n, held>>10, limit>>10)
	}
}
