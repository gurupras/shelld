package vt

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// rowsWithAttrs renders every row a resize can touch, history included, as text
// plus a marker on each cell that is not carrying the default colours. A stale
// cell is usually a BLANK carrying a background, which is invisible to any
// comparison of rendered text, so the marker is the point of this rather than the
// text.
func rowsWithAttrs(t *Term) []string {
	g := t.Grid()
	out := make([]string, 0, g.HistorySize()+t.ScreenLines())
	for line := -g.HistorySize(); line < t.ScreenLines(); line++ {
		var b strings.Builder
		for _, c := range g.Row(line).Cells() {
			b.WriteRune(c.Rune())
			if c.Bg() != defaultBg || c.Fg() != defaultFg {
				b.WriteString("<a>")
			}
		}
		out = append(out, strings.TrimRight(b.String(), " \t"))
	}
	return out
}

// contentCells is every non-empty cell of every row, oldest first, with the wrap
// flag masked out. That sequence is exactly what reflow moves between rows: row
// boundaries and trailing blank runs change with the width, the cells themselves do
// not. It is the only oracle available at an intermediate width, because a terminal
// fed the same bytes AT that width is not one — wrapping as you write and reflowing
// legitimately disagree about tab stops and about the blanks a background paints.
func contentCells(t *Term) []string {
	g := t.Grid()
	out := make([]string, 0, 256)
	for line := -g.HistorySize(); line < t.ScreenLines(); line++ {
		for _, c := range g.Row(line).Cells() {
			// The wrap flag is dropped BEFORE asking whether the cell is empty: it
			// lands on whatever cell ends a row, so a blank at a wrap boundary counts
			// as content with the flag on and does not without it, and reflow moves
			// those boundaries by design.
			c.flags &^= FlagWrapline
			if c.isEmpty() {
				continue
			}
			out = append(out, cellKey(&c))
		}
	}
	return out
}

// diffContent names the first cell two content sequences disagree on.
func diffContent(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("%d cells vs %d, first difference at %d: got %s, want %s",
				len(got), len(want), i, got[i], want[i])
		}
	}
	if len(got) == len(want) {
		return ""
	}
	longer, at := "got", len(want)
	if len(want) > len(got) {
		longer, at = "want", len(got)
	}
	return fmt.Sprintf("%d cells vs %d, identical up to %d and then only %s continues",
		len(got), len(want), at, longer)
}

// TestResizeRoundTripKeepsRowsClean pins the invariant that lets grow reuse the
// capacity a shrink left behind: the cells past a row's length are default, so
// re-widening produces blanks rather than whatever used to be there.
//
// The oracle at each width is a terminal fed the same bytes at that width without
// ever being resized. BOTH widths are checked, and the narrow one is the important
// half: shrink only reslices, so cells it wrongly drops are still sitting in the
// capacity and the widening reslices them straight back — a narrowing that lost 70
// columns of every line passes the round trip and fails only in the middle. (Found
// by the mutant that drops the guard's occupancy term: the round trip was green for
// two of the three cases below.)
func TestResizeRoundTripKeepsRowsClean(t *testing.T) {
	const wide, narrow, lines = 200, 80, 8

	for _, tc := range []struct {
		name string
		text string
	}{
		{"plain", strings.Repeat("abcdefghij", 15)},
		{"colored", "\x1b[41m" + strings.Repeat("abcdefghij", 15) + "\x1b[m"},
		{"tabs", strings.Repeat("ab\tcd\tef\t", 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Real traffic through shrink, so the fast path's own claim is worth
			// checking here too: it names the offending row, where a comparison of
			// rows can only report the text that went missing.
			rowShrinkTookFastPath = func(r Row, columns int) { checkFastPathPrecondition(t, &r, columns) }
			defer func() { rowShrinkTookFastPath = nil }()

			write := func(term *Term, proc *Processor) {
				for i := 0; i < 12; i++ {
					feed(term, proc, tc.text+"\r\n")
				}
			}
			compare := func(stage string, got *Term, columns int) {
				want := NewTerm(Config{ScrollingHistory: 64}, columns, lines, nil)
				write(want, NewProcessor())
				g, w := rowsWithAttrs(got), rowsWithAttrs(want)
				if len(g) != len(w) {
					t.Fatalf("%s: row count at %d columns: got %d, want %d", stage, columns, len(g), len(w))
				}
				for i := range g {
					if g[i] != w[i] {
						t.Fatalf("%s: row %d at %d columns:\n got %q\nwant %q", stage, i, columns, g[i], w[i])
					}
				}
			}

			got := NewTerm(Config{ScrollingHistory: 64}, wide, lines, nil)
			write(got, NewProcessor())
			content := contentCells(got)

			got.Resize(narrow, lines)
			if d := diffContent(contentCells(got), content); d != "" {
				t.Fatalf("narrowing to %d columns changed the content: %s", narrow, d)
			}
			got.Resize(wide, lines)
			if d := diffContent(contentCells(got), content); d != "" {
				t.Fatalf("widening back to %d columns changed the content: %s", wide, d)
			}
			compare("after widening back", got, wide)
		})
	}
}

// TestShrinkLeavesDefaultCapacity states the contract grow depends on: after a
// shrink, the capacity past the row's length holds default cells unless the row is
// flagged dirty, and either way growing back into it yields blanks.
//
// Removing shrink's clear, the dirty flag, or the fast path's requirement that the
// row end in the default blank makes this fail.
func TestShrinkLeavesDefaultCapacity(t *testing.T) {
	// Built with the setters, not with field literals: a Cell stores ch XOR ' ' and
	// bg XOR the default background, so Cell{ch: '\t'} is the glyph ')' and not a
	// tab at all. Writing these by hand turned both tab cases into ordinary content
	// and left the empty-but-not-zero bound with no unit coverage.
	blank := Cell{}
	var tab, red, text Cell
	tab.SetRune('\t')
	red.SetBg(NewNamedColor(Red))
	text.SetRune('x')
	if !tab.isEmpty() || tab == blank || red.isEmpty() || text.isEmpty() {
		t.Fatalf("fixtures do not have the properties the cases below rely on: tab %+v red %+v text %+v", tab, red, text)
	}

	for _, tc := range []struct {
		name  string
		cells []Cell
		occ   int
	}{
		{"trailing blanks", []Cell{text, text, blank, blank, blank, blank}, 2},
		{"content in the removed part", []Cell{text, text, text, text, text, text}, 6},
		// The row ends in the default blank while content sits before it, past
		// columns: the last cell alone says the tail is droppable and only occ says
		// otherwise, so this is the case a guard missing its occ term gets wrong.
		{"content behind a default last cell", []Cell{text, text, text, text, text, blank}, 6},
		// A tab is empty without being the zero cell, so a clear bounded by the
		// last non-empty cell would leave these behind.
		{"trailing tabs", []Cell{text, text, tab, tab, tab, tab}, 6},
		{"tabs the occupancy hint does not cover", []Cell{text, tab, tab, tab, tab, tab}, 1},
		// occ is 0 with non-default cells present: what reset leaves behind.
		{"blanks under a background, occ zero", []Cell{red, red, red, red, red, red}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Row{inner: append([]Cell(nil), tc.cells...), occ: int32(tc.occ)}
			out := r.shrink(3)
			if len(r.inner) != 3 {
				t.Fatalf("length after shrink: got %d, want 3", len(r.inner))
			}
			// What reflow gets back: the removed cells up to the last non-empty one.
			// Computed from the case rather than written out, so a case added above
			// cannot silently skip this. A shrink that returns too little drops
			// content on a narrowing, which is the fast path's whole risk.
			want := []Cell{}
			for i := len(tc.cells) - 1; i >= 3; i-- {
				if !tc.cells[i].isEmpty() {
					want = tc.cells[3 : i+1]
					break
				}
			}
			if len(out) != len(want) {
				t.Fatalf("shrink returned %d cells, want %d: got %+v want %+v", len(out), len(want), out, want)
			}
			for i := range want {
				if out[i] != want[i] {
					t.Errorf("returned cell %d: got %+v, want %+v", i, out[i], want[i])
				}
			}
			// Leaving occ past the length is correctness-neutral — it makes the tail
			// invariant vacuous and the fast path's guard decline — so it fails
			// silently, by turning the optimisation off on most of a narrowing rather
			// than by producing a wrong cell.
			if int(r.occ) > len(r.inner) {
				t.Errorf("shrink left occ %d past the length %d: the fast path will decline every such row",
					r.occ, len(r.inner))
			}
			if tail := r.inner[len(r.inner):cap(r.inner)]; !r.dirty {
				for i := range tail {
					if tail[i] != blank {
						t.Fatalf("row is not dirty but capacity cell %d is not default: %+v (tail %+v)",
							i, tail[i], tail)
					}
				}
			}
			// The observable consequence, dirty or not.
			r.grow(6)
			for i := 3; i < 6; i++ {
				if r.inner[i] != blank {
					t.Errorf("cell %d after shrink+grow: got %+v, want default", i, r.inner[i])
				}
			}
		})
	}
}

// TestPartialRegrowKeepsDirty covers a row that grows back into only PART of the
// capacity a shrink left. grow clears what it hands out, but the cells past the
// new length are still whatever the earlier shrink abandoned there, so the flag
// has to stay set. Clearing it unconditionally leaves a row whose tail is stale
// while it claims to be clean, and the next grow resurrects it.
//
// The shape needs two shrinks and two grows: the first shrink dirties the tail,
// the partial grow is where the flag is decided, the second shrink takes the fast
// path (which does not clear), and the final grow is where a wrong flag becomes
// visible content.
//
// The attributed-blank case is the one that matters, and it is deliberately not
// just the text case with a different fill: a stale cell that is a BLANK carrying
// a background is invisible to any assertion that compares rendered text, because
// it renders as a space. It is the same shape as the client-visible defect this
// work has to avoid re-creating, one level down.
func TestPartialRegrowKeepsDirty(t *testing.T) {
	blank := Cell{}
	var textCell, bgCell Cell
	textCell.SetRune('x')
	bgCell.SetBg(NewNamedColor(Red))
	for _, tc := range []struct {
		name string
		fill Cell
	}{
		{"text", textCell},
		{"blank carrying a background", bgCell},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fill.isEmpty() {
				t.Fatalf("fill %+v is empty: shrink would have nothing to abandon", tc.fill)
			}
			cells := make([]Cell, 0, 6)
			for range 6 {
				cells = append(cells, tc.fill)
			}
			r := Row{inner: cells, occ: 6}
			if cap(r.inner) != 6 {
				t.Fatalf("test needs a row with cap 6, got %d", cap(r.inner))
			}

			r.shrink(3) // leaves the fill at inner[3:6] behind the length
			r.grow(5)   // consumes 2 of the 3 spare cells: inner[5] is still filled
			if !r.dirty {
				t.Fatalf("a row grown back into part of its capacity must stay dirty: len %d cap %d",
					len(r.inner), cap(r.inner))
			}
			r.shrink(4) // ends in a default blank, so the fast path takes it and clears nothing
			if !r.dirty {
				t.Fatalf("the fast path must not clear the flag: len %d cap %d",
					len(r.inner), cap(r.inner))
			}
			r.grow(6)
			for i := 3; i < 6; i++ {
				if r.inner[i] != blank {
					t.Errorf("cell %d after shrink/grow/shrink/grow: got %+v, want default",
						i, r.inner[i])
				}
			}
		})
	}
}

// TestGrowFixupKeepsBackgroundBlanks is the fast path's guard in one row. A reset
// under a background leaves blanks that carry it without raising occ; growing
// appends DEFAULT cells, so the row's last cell becomes the zero cell and the fast
// path's guard sees a row it would truncate without looking — while the background
// blanks sit in the middle, past occ. Only grow's occ fixup keeps them, so shrink
// must return them.
//
// An intermediate shrink back to the original width hides this, because it leaves
// a background-carrying last cell and the guard declines by accident.
func TestGrowFixupKeepsBackgroundBlanks(t *testing.T) {
	cells := make([]Cell, 6)
	for i := range cells {
		cells[i].SetBg(NewNamedColor(Red))
	}
	r := Row{inner: cells, occ: 0}
	r.grow(10)
	out := r.shrink(3)
	if len(out) != 3 {
		t.Fatalf("shrink dropped background blanks past occ: got %d cells, want 3 (%+v)", len(out), out)
	}
	for i, c := range out {
		if c.Bg() != NewNamedColor(Red) {
			t.Errorf("returned cell %d lost its background: got %+v", i, c)
		}
	}
}

// TestMarkOccupiedKeepsFabricatedCells pins the exported escape hatch, which no
// other test can see fail. A harness that writes cells through the pointer Cell
// returns leaves the occupancy hint understated, and that is not a rendering
// difference — every reader goes through Cells() — so it only shows up as cells
// silently lost at the next narrowing or reset. The render property test compares
// against a reference computed from the same terminal, so a loss there cancels.
func TestMarkOccupiedKeepsFabricatedCells(t *testing.T) {
	fabricate := func() Row {
		r := newRow(6)
		for i := range r.inner {
			r.inner[i].SetRune('x') // as the harness does: straight into the cell
		}
		return r
	}

	r := fabricate()
	r.MarkOccupied(r.Len())
	if out := r.shrink(3); len(out) != 3 {
		t.Errorf("shrink after MarkOccupied returned %d cells, want 3: a narrowing dropped fabricated content", len(out))
	}

	r = fabricate()
	r.MarkOccupied(r.Len())
	var template Cell
	r.reset(&template)
	for i := range r.inner {
		if r.inner[i] != (Cell{}) {
			t.Errorf("cell %d survived a reset after MarkOccupied: %+v", i, r.inner[i])
			break
		}
	}
}

// TestShrinkFastPathFires guards the fast path against being silently disabled.
// Every other test here asserts that the results are right, and they stay right
// when the fast path never runs — so a change that leaves occ past a row's length,
// or tightens the guard until nothing passes it, costs the whole optimisation
// without turning a single test red.
func TestShrinkFastPathFires(t *testing.T) {
	var hits int
	rowShrinkTookFastPath = func(r Row, columns int) {
		hits++
		checkFastPathPrecondition(t, &r, columns)
	}
	defer func() { rowShrinkTookFastPath = nil }()

	// A screen of short lines narrowed well below the width: the shape the
	// optimisation exists for, where almost every row is mostly trailing blanks.
	//
	// The history of such lines no longer reaches the fast path. A history row
	// is compacted to its content (Row.compact), so it arrives at shrink no
	// wider than the narrow width and shrink returns before the guard. What
	// still arrives at full width is the viewport, so that is the population
	// this counts; the history's side is asserted below as the property that
	// took it away, since a history stored at full width would take the fast
	// path again and pass the count.
	const line = "2026-09-15 20:00:00 INFO line"
	term := NewTerm(Config{ScrollingHistory: 2000}, 200, 50, nil)
	p := NewProcessor()
	for range 2000 {
		feed(term, p, line+"\r\n")
	}
	g := term.Grid()
	for l := -g.HistorySize(); l < 0; l++ {
		if n := g.Row(l).Len(); n > len(line) {
			t.Fatalf("history line %d stores %d cells for a %d-character line: history is not compacted", l, n, len(line))
		}
	}
	term.Resize(80, 24)

	// Every viewport row but the cursor's empty one holds a line, so all but a
	// couple of the 50 take it.
	if want := g.ScreenLines() - 2; hits < want {
		t.Errorf("fast path fired %d times narrowing 50 blank-tailed viewport rows from 200 to 80 columns; want at least %d", hits, want)
	}
	t.Logf("fast path fired %d times", hits)
}

// TestGrowClearsOnlyRowsShrinkDirtied is the dirty flag's reason for existing,
// stated as a count rather than as a result: growing back into a capacity an
// earlier shrink already left zeroed must not clear it again. Dropping the flag and
// clearing every row is correct, so nothing else here can see it — the same blind
// spot TestShrinkFastPathFires covers for the other half.
//
// The second arm is the control: rows whose content DID reach the removed part
// leave a tail shrink refuses to clear, and those must be cleared on the way back,
// so a counter that reads zero for both workloads is measuring nothing.
func TestGrowClearsOnlyRowsShrinkDirtied(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
		// emptyNonZeroTail says the workload must actually produce cells past the
		// narrow width that are empty without being the zero cell. Without the
		// premise the arm reads zero because there was nothing to clear, and the
		// mutant that clears them anyway would go unnoticed.
		emptyNonZeroTail bool
	}{
		{"lines that fit the narrow width", "2026-09-15 20:00:00 INFO line", "no clears", false},
		// Cells that are empty without being the zero cell: shrink has to clear
		// those itself, since reflow keeps nothing and the row is not dirty
		// afterwards. Treating them like content instead is correct and costs a
		// clear per row on the way back, which only a counter can see.
		{"lines of tabs, empty but not zero", strings.Repeat("\t", 40), "no clears", true},
		{"lines that spill past it", strings.Repeat("abcdefghij", 15), "clears", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clears int
			rowGrowClearedTail = func(Row, int) { clears++ }
			defer func() { rowGrowClearedTail = nil }()

			term := NewTerm(Config{ScrollingHistory: 2000}, 200, 24, nil)
			// The subject is a plain row's tail, which a packed history row does
			// not have: it is stored without one and never grown back.
			term.Grid().SetNoPack(true)
			p := NewProcessor()
			for range 400 {
				feed(term, p, tc.line+"\r\n")
			}
			if tc.emptyNonZeroTail {
				g, found := term.Grid(), 0
				for line := -g.HistorySize(); line < term.ScreenLines(); line++ {
					for _, c := range g.Row(line).inner[80:] {
						if c.isEmpty() && c != (Cell{}) {
							found++
						}
					}
				}
				if found == 0 {
					t.Fatalf("workload produced no empty-but-non-zero cell past column 80: this arm would read zero clears for the wrong reason")
				}
			}
			term.Resize(80, 24)
			term.Resize(200, 24)

			switch {
			case tc.want == "no clears" && clears != 0:
				t.Errorf("re-widening a history of lines shorter than 80 columns cleared %d rows; shrink left every one of those tails zeroed", clears)
			case tc.want == "clears" && clears == 0:
				t.Errorf("re-widening a history of wrapped rows cleared nothing: the counter cannot see a clear, so the other arm proves nothing")
			}
			t.Logf("%d grow clears", clears)
		})
	}
}

// A narrowing is not the only thing that leaves a row with capacity to clear: so
// does a widen that joins, because the row a join empties keeps its array instead of
// being given an exact-size replacement. Nothing here ever narrows, so the count
// below is that on its own, and it is what makes a second widen the step worth
// timing — BenchmarkRewidenColumnsReflow is the same shape.
//
// The control is lines that fit the starting width: nothing wraps, so no row is
// joined, none is left with capacity past its length, and the same second widen
// clears nothing. Without it a count of zero and a hook that is never installed look
// the same.
//
// History rows are compacted (Row.compact) and a widen grows only the viewport,
// so the rows a widen can reuse capacity on are the viewport's, and both counts
// are bounded by its height. The join can now dirty a viewport row too: a
// compacted row joined onto the one above moves only the cells it stores, which
// can leave that row short of the width with the array append grew it into.
//
// What this does not test is the clear itself. The hook fires before it, and in this
// shape the same pass writes over the region anyway, so a grow that skipped the clear
// entirely still passes here — TestShrinkLeavesDefaultCapacity and
// TestPartialRegrowKeepsDirty are the two that fail, and TestShrinkFastPathMatchesScan
// is what fails if moveFrontTo stops marking the tail. The count here is the number
// of rows that took grow's reuse branch, and the text check is a smoke check on the
// round trip.
func TestGrowClearsRowsAJoinDirtied(t *testing.T) {
	const wrapped = 300 // characters per line, and lines: eight rows deep at width 40
	for _, tc := range []struct {
		name  string
		line  string
		joins bool
	}{
		{"lines that wrap, so widening joins them", strings.Repeat("the quick brown fox jumps over the lazy dog ", 7)[:wrapped], true},
		{"lines that fit the start width", "short line", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clears int
			rowGrowClearedTail = func(Row, int) { clears++ }
			defer func() { rowGrowClearedTail = nil }()

			term, p := NewTerm(Config{ScrollingHistory: 10000}, 40, 24, nil), NewProcessor()
			feed(term, p, strings.Repeat(tc.line+"\r\n", wrapped))
			term.Resize(80, 24) // joins the wrapped rows, which is what sets the flag
			first := clears
			term.Resize(200, 24) // reuses the capacity the join left behind
			second := clears - first
			switch screen := term.ScreenLines(); {
			case first > screen || second > screen:
				t.Errorf("the widens cleared %d and %d rows, more than the %d viewport rows: a history row was widened", first, second, screen)
			case tc.joins && second == 0:
				t.Errorf("widening 80 to 200 over %d wrapped lines cleared nothing: the counter cannot see a clear, so the control proves nothing", wrapped)
			case !tc.joins && clears != 0:
				t.Errorf("widening a history of lines that never wrapped cleared %d rows; no join dirtied any of them", clears)
			}
			t.Logf("%d clears joining, %d widening again", first, second)

			g, rows := term.Grid(), (len(tc.line)+199)/200
			var got strings.Builder
			for line := range rows {
				row := g.Row(-g.HistorySize() + line)
				for c := range g.Columns() {
					got.WriteRune(row.Cell(c).Rune())
				}
			}
			if want := tc.line + strings.Repeat(" ", rows*200-len(tc.line)); got.String() != want {
				t.Errorf("the first line of history after the round trip:\n got %q\nwant %q", got.String(), want)
			}
		})
	}
}

// checkFastPathPrecondition is the fast path's own claim, checked at the moment it
// is used: every cell it is about to drop is the DEFAULT cell. Installed by the
// tests that drive real traffic through shrink, which is the only way to see a row
// the guard misjudges — a wrong result downstream can be several resizes away from
// the row that caused it.
//
// Default, not merely empty, because the fast path makes two promises at once: it
// returns nothing to reflow (so no dropped cell may hold content) and it leaves the
// row not dirty (so no dropped cell may hold anything a later grow would hand out).
// A tab and a blank carrying a background are empty and satisfy only the first.
func checkFastPathPrecondition(t *testing.T, r *Row, columns int) {
	t.Helper()
	for i := columns; i < len(r.inner); i++ {
		if r.inner[i] != (Cell{}) {
			what := "content reflow will never see"
			if r.inner[i].isEmpty() {
				what = "an empty but non-default cell the tail keeps while the row claims to be clean"
			}
			t.Fatalf("shrink(%d) fast path would drop cell %d, %s: len=%d occ=%d cell=%+v last=%+v",
				columns, i, what, len(r.inner), r.occ, r.inner[i], r.inner[len(r.inner)-1])
		}
	}
}

// TestNarrowWidenKeepsRowsOffTheHeap pins the other half of what makes a narrowing
// cheap, and the half no result-comparing assertion can reach: reflow copies Row
// STRUCTS a row at a time (growColumns reverses the history into a []Row and splits
// each row's front off), so neither grow nor shrink may let its receiver escape.
//
// The first version of this change did. Both test seams were typed func(*Row, int),
// and handing the receiver to a function the compiler cannot see is enough for
// escape analysis to move every Row local in growColumns and shrinkColumns to the
// heap: one extra allocation per history row (a 600-row narrow+widen went from 8
// allocations to 1258, and a 12,000-row resize storm from 15,329 allocs/op to
// 25,554), which cost 12-16% on histories that wrap — while every test stayed green.
// Hence a bound, on the shape where the fast path leaves nothing legitimate to
// allocate: a history of short lines, where narrowing drops only blanks.
func TestNarrowWidenKeepsRowsOffTheHeap(t *testing.T) {
	const rows = 600
	term := NewTerm(Config{ScrollingHistory: rows}, 200, 24, nil)
	p := NewProcessor()
	for range rows {
		feed(term, p, "2026-09-15 20:00:00 INFO line\r\n")
	}

	allocs := testing.AllocsPerRun(3, func() {
		term.Resize(80, 24)
		term.Resize(200, 24)
	})

	// Measured 8 on this shape, on main and here alike: the two []Row arrays each
	// pass builds, and nothing per row. The bound is loose enough not to depend on
	// that exact number and far below the per-row figure an escape produces.
	if allocs > 64 {
		t.Errorf("narrow+widen of a %d-row history of short lines allocated %.0f times; "+
			"want under 64 (a per-row allocation means grow or shrink leaked its receiver)",
			rows, allocs)
	}
	t.Logf("allocations per narrow+widen of %d rows: %.0f", rows, allocs)
}

// TestRowFitsInFourWords pins Row's size for the same reason: every reflow pass
// copies one Row struct per history row, so a field that spills the struct out of
// its padding is paid 12,000 times per resize. dirty was added as the fifth field
// of a struct whose first four are a 24-byte slice header and a count, and with occ
// left as int it took Row from 32 bytes to 40 — worth 2-6% on the shapes that copy
// the most rows. It fits only because occ is int32.
func TestRowFitsInFourWords(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skipf("size pinned for 64-bit builds; int is %d bits here", strconv.IntSize)
	}
	if got := unsafe.Sizeof(Row{}); got != 32 {
		t.Errorf("unsafe.Sizeof(Row{}) = %d, want 32: a new field has to fit in the "+
			"padding after occ, or reflow pays for it per row", got)
	}
}
