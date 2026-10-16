package vt

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// Shrinking the columns of a grid whose history holds long lines must keep
// memory proportional to the new width. Each wrapped row used to keep the
// whole rest of its line as capacity (about L²/2c cells per line of length
// L at c columns), so narrowing a wide window with full history retained
// hundreds of MiB to GiBs. The sizes here keep the old behaviour's cost to
// ~80 MiB.
func TestShrinkColumnsMemoryIsLinear(t *testing.T) {
	const wide, narrow, history = 400, 20, 1000
	term := NewTerm(Config{ScrollingHistory: history}, wide, 24, nil)
	p := NewProcessor()
	line := strings.Repeat("x", wide-1) + "漢\r\n" // a wide cell at the boundary, too
	for range history + 24 {
		feed(term, p, line)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	term.Resize(narrow, 24)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapInuse) - int64(before.HeapInuse)

	// The narrow grid itself: history plus screen rows of narrow cells.
	cellSize := int64(unsafe.Sizeof(Cell{}))
	grid := int64(history+24) * narrow * cellSize
	if retained > 4*grid+(1<<20) {
		t.Errorf("resize %d -> %d columns retained %d KiB; the %d-column grid is %d KiB", wide, narrow, retained>>10, narrow, grid>>10)
	}
	// Splitting copies each cell a bounded number of times: the old wide
	// rows once more, plus rows for the narrow grid.
	wideCells := int64(history+24) * wide * cellSize
	if limit := 3*wideCells + 4*grid; int64(allocated) > limit {
		t.Errorf("resize %d -> %d columns allocated %d KiB, want at most %d KiB", wide, narrow, allocated>>10, limit>>10)
	}
	t.Logf("resize %d -> %d columns: retained %d KiB, allocated %d KiB (narrow grid %d KiB)", wide, narrow, retained>>10, allocated>>10, grid>>10)
	if got := term.grid.HistorySize(); got != history {
		t.Errorf("history %d lines, want the limit %d", got, history)
	}
	runtime.KeepAlive(term)
}

// The pass must not keep what the resize drops reachable: each original row
// is released once walked, rows past the history limit are dropped as they
// are placed, and the stored rows sit in an array of exactly their number.
// These do not show up as heap growth in the test above, whose freed wide
// history hides them, but at 1024 -> 20 columns keeping the dropped rows
// reachable retained 236 MiB.
func TestShrinkColumnsDropsWhatItDoesNotKeep(t *testing.T) {
	const wide, narrow, history, lines = 400, 20, 1000, 24
	term := NewTerm(Config{ScrollingHistory: history}, wide, lines, nil)
	p := NewProcessor()
	for range history + lines {
		feed(term, p, strings.Repeat("x", wide)+"\r\n")
	}
	limit := history + lines
	var walked, placed int
	shrinkColumnsPassDone = func(rows, newRaw []Row) {
		for i := range rows {
			if rows[i].inner != nil {
				walked++
			}
		}
		placed = max(placed, len(newRaw)) // the primary grid; the alternate one follows
	}
	defer func() { shrinkColumnsPassDone = nil }()
	term.Resize(narrow, lines)
	if walked != 0 {
		t.Errorf("%d original rows still referenced after the pass", walked)
	}
	// Each line splits into wide/narrow rows; dropping starts at twice the limit.
	if max := 2*limit + wide/narrow; placed > max {
		t.Errorf("%d rows held at the end of the pass, want at most %d (history limit %d)", placed, max, limit)
	}
	raw := &term.grid.raw
	if len(raw.inner) != limit || cap(raw.inner) != len(raw.inner) {
		t.Errorf("stored %d rows in an array of capacity %d, want %d in exactly %d", len(raw.inner), cap(raw.inner), limit, limit)
	}
}

// A row keeps its array across a shrink so that widening back reuses it, which is
// free until the narrowing is sharp: the array is as wide as the row ever was, so
// every row of a history narrowed from 400 columns to 20 would hold twenty times
// what it now needs, for as long as the history keeps the row. The sizes here give
// the history room for every row the split produces, so nothing is hidden by the
// history limit dropping the rows that hold the old arrays.
func TestShrinkColumnsBoundsRetainedCapacity(t *testing.T) {
	const wide, narrow, lines = 400, 20, 200
	// Everything below is bounded by the factor times the narrow width, so the shape
	// has to be a sharper narrowing than the factor or the bound is vacuous: at a
	// factor of 20 a row still holding all 400 of its old cells would be inside it.
	if shrinkKeepFactor*narrow >= wide {
		t.Fatalf("shrinkKeepFactor is %d, so narrowing %d -> %d columns is inside it and these bounds hold whatever shrink does: sharpen the shape",
			shrinkKeepFactor, wide, narrow)
	}
	term := NewTerm(Config{ScrollingHistory: lines * wide / narrow}, wide, 24, nil)
	p := NewProcessor()
	for range lines {
		feed(term, p, strings.Repeat("x", wide)+"\r\n")
	}
	term.Resize(narrow, 24)

	raw := &term.grid.raw
	var held, needed, over int
	for i := range raw.inner {
		row := &raw.inner[i]
		held += cap(row.inner)
		// A packed row's cells are bytes in its array past a 0 length, and the
		// array is all it needs.
		if row.packed {
			needed += cap(row.inner)
		} else {
			needed += len(row.inner)
		}
		if cap(row.inner) > shrinkKeepFactor*narrow {
			over++
		}
	}
	cellSize := int(unsafe.Sizeof(Cell{}))
	if over > 0 {
		t.Errorf("%d of %d rows kept more than %d cells after narrowing %d -> %d columns: %d KiB held for %d KiB of cells",
			over, len(raw.inner), shrinkKeepFactor*narrow, wide, narrow, held*cellSize>>10, needed*cellSize>>10)
	}
	// The same fact as a whole-grid bound, which is what a client sees: the factor
	// is per row, so it is also the factor over the grid.
	if held > shrinkKeepFactor*needed {
		t.Errorf("the narrowed grid holds %d KiB for %d KiB of cells, more than %dx",
			held*cellSize>>10, needed*cellSize>>10, shrinkKeepFactor)
	}
	t.Logf("narrowing %d -> %d over %d lines: %d rows holding %d KiB for %d KiB of cells",
		wide, narrow, lines, len(raw.inner), held*cellSize>>10, needed*cellSize>>10)
	runtime.KeepAlive(term)
}
