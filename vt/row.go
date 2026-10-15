package vt

// Row is one grid line. occ is an upper bound on the cells modified since the
// last reset: every cell at or past occ equals the row's last cell, so a reset
// only has to clear [0, occ) once it has checked that last cell.
//
// The invariant is equality with the last cell, not being the default cell: a
// row the program painted with a background and then cleared is blanks
// carrying that background, and occ can sit before them. Anything that needs the
// untracked tail to be DEFAULT — shrink's fast path below — has to establish
// that separately, which is why its guard tests the last cell against the zero
// cell as well as testing occ.
type Row struct {
	inner []Cell
	// occ is int32, not int, so that dirty below fits in the padding a
	// 24-byte slice header plus a 4-byte count already leaves: Row is copied per
	// row by every reflow pass, and widening it to 40 bytes shows up there.
	occ int32
	// dirty records that the capacity past inner's length may hold non-default
	// cells, so a later grow has to clear what it reuses. shrink clears that
	// tail itself when doing so is free, and sets this when it is not; the flag
	// is deliberately conservative, since a false positive costs one clear and a
	// false negative resurrects cells.
	dirty bool
}

func newRow(columns int) Row { return Row{inner: make([]Cell, columns)} }

// Len is the number of cells the row stores. A viewport row stores one per
// column; a history row may store fewer, and every column past Len holds the
// default cell (see compact).
func (r *Row) Len() int { return len(r.inner) }

// Cells returns the row's stored cells for reading, limited to its length,
// which for a history row can be less than the grid's width.
// Writing through it would bypass the occupancy tracking reset relies on.
func (r *Row) Cells() []Cell { return r.inner[:len(r.inner):len(r.inner)] }

// Cell returns the cell at col for reading. A column past what the row stores
// reads as a default cell; the pointer is then to a fresh copy, so a write
// through it is lost. A harness that writes through Cell calls Expand first.
func (r *Row) Cell(col int) *Cell {
	if col >= len(r.inner) {
		return new(Cell)
	}
	return &r.inner[col]
}

// Expand makes the row store at least columns cells, so that Cell returns a
// pointer into the row for every column. Nothing in a terminal needs it; it is
// for harnesses that fabricate a grid by writing through Cell, in the shape of
// MarkOccupied.
func (r *Row) Expand(columns int) { r.grow(columns) }

// trimmedLen is the length compact cuts the row to: one past its last non-zero
// cell when the row's last cell is the zero cell and that one does not carry
// FlagWrapline, and its whole length otherwise. Every cell at or past occ equals the last cell, so when that cell is
// zero the scan back starts at occ.
func (r *Row) trimmedLen() int {
	n := len(r.inner)
	if n == 0 || r.inner[n-1] != (Cell{}) {
		return n
	}
	i := min(int(r.occ), n)
	for i > 0 && r.inner[i-1] == (Cell{}) {
		i--
	}
	// A wrapped line's flag is read off the row's last cell (growColumns'
	// shouldReflow), so a row whose content ends in it is kept whole: trimmed,
	// it would read as a line that wraps where the full row reads as one that
	// does not.
	if i > 0 && r.inner[i-1].flags&FlagWrapline != 0 {
		return n
	}
	return i
}

// compact moves a history row onto an array no longer than its content and
// returns the array it gave up, or nil when it kept its own.
//
// History is where a terminal's memory goes: 10,000 lines of history at 200
// columns are 40 MB stored a full row each, and the lines a shell prints are
// mostly short. A compacted row stores the cells up to its last non-zero one, and
// the default cells past it are implicit: Cell reads them back, and the readers
// that walk Cells pad with them. Only the zero cell is dropped, because it is the
// cell grow appends, so a compacted row and the row it came from read the same at
// every column and widen to the same row. A row ending in anything else — blanks
// carrying a background, a wrapped line's last cell — is stored whole.
//
// buf, when not nil, is an array to reuse for the content: one a row just
// evicted from history gave up. It is reused when its capacity is at least the
// content and at most compactReuseFactor times it, so a reused array never holds
// more than that factor over what it carries, and it is cut to its length so
// nothing reaches the cells past it.
//
// A row that has nothing to trim keeps its array unless that array holds more
// than compactReuseFactor times its length, which is what a narrowing leaves:
// truncate keeps up to shrinkKeepFactor times the new width for a later widen to
// reuse, and a history row is never widened again.
func (r *Row) compact(buf []Cell) []Cell {
	n := r.trimmedLen()
	if n == len(r.inner) && cap(r.inner) <= compactReuseFactor*n {
		return nil
	}
	var cells []Cell
	if n > 0 {
		if cap(buf) >= n && cap(buf) <= compactReuseFactor*n {
			cells = buf[:n:n]
		} else {
			cells = make([]Cell, n)
		}
		copy(cells, r.inner)
	}
	old := r.inner
	*r = Row{inner: cells, occ: int32(n)}
	return old
}

// compactReuseFactor bounds the capacity a compacted row may hold, as a multiple
// of what it stores.
const compactReuseFactor = 2

// MarkOccupied declares the row's first n cells occupied. Nothing in a terminal
// needs it — every write inside this package goes through at, span, lastMut or
// reset, which maintain the hint themselves — but a harness that fabricates a
// grid by writing through the pointer Cell returns does, since a row whose
// occupancy is understated loses those cells at the next reset and at the next
// narrowing.
func (r *Row) MarkOccupied(n int) {
	if n > int(r.occ) {
		r.occ = int32(min(n, len(r.inner)))
	}
}

// at returns the cell at col for writing.
func (r *Row) at(col int) *Cell {
	if col+1 > int(r.occ) {
		r.occ = int32(col + 1)
	}
	return &r.inner[col]
}

// span returns cells [from, to) for writing.
func (r *Row) span(from, to int) []Cell {
	if to > int(r.occ) {
		r.occ = int32(to)
	}
	return r.inner[from:to]
}

// grow extends the row to columns default cells, reusing capacity left by an
// earlier shrink.
func (r *Row) grow(columns int) {
	n := len(r.inner)
	if n >= columns {
		return
	}
	// The cells this adds are default ones, so they only equal the row's last
	// cell — which is what occ promises about them — if that cell is a default
	// one too. When it is not, the row's own tail has to come inside occ,
	// or reset would take the last cell of a widened row as evidence that the
	// painted blanks before it need no clearing.
	//
	// r.inner[n-1] needs no n > 0 guard: occ is never negative (moveFrontTo
	// clamps it), so int(r.occ) < n is false for an empty row and short-circuits
	// before the index. Keep that true if you change how occ is maintained —
	// this arm is what keeps a widened row's painted blanks inside occ, so the
	// shrink fast path below cannot mistake them for a default tail.
	if int(r.occ) < n && !r.inner[n-1].isEmpty() {
		r.occ = int32(n)
	}
	if cap(r.inner) >= columns {
		r.inner = r.inner[:columns]
		// Only a row whose tail an earlier shrink left behind needs clearing.
		// The common case — a shrink that had nothing to abandon — hands the
		// capacity back already zeroed, and re-widening a whole history of rows
		// is where clearing a row at a time shows up.
		if r.dirty {
			if rowGrowClearedTail != nil {
				rowGrowClearedTail(*r, columns)
			}
			clear(r.inner[n:])
			// The cells past columns can still be dirty, so only a grow that
			// consumes the whole capacity clears the flag.
			r.dirty = columns < cap(r.inner)
		}
		return
	}
	r.inner = append(r.inner, make([]Cell, columns-n)...)
	r.markTail()
}

// markTail records conservatively whether the capacity past the row's length can
// hold anything but default cells. Reslicing to the full capacity is the only way
// to be sure, so a row that has any spare capacity is assumed dirty rather than
// trusting append to have zeroed the part it did not need.
//
// The precise version — assigning instead of oring here, and dropping the call
// from appendCells — is also correct, and is deliberately not what ships. Cells
// past the length come from one of two places: a shrink put them there, and
// every one of shrink's exits leaves this flag right, or nothing wrote them and
// they came out of growslice, which memclrs [len, cap) for a pointer-free
// element type. Cell is pointer-free, so that region is zero by construction.
// The conservative form gives that up to avoid depending on a runtime internal:
// its false positive costs one clear of a region grow was about to blank anyway,
// where a false negative resurrects a cell a narrowing dropped.
func (r *Row) markTail() {
	r.dirty = r.dirty || len(r.inner) < cap(r.inner)
}

// shrink truncates the row to columns and returns the removed cells up to
// the last non-empty one, or nil when nothing non-empty was removed.
func (r *Row) shrink(columns int) []Cell {
	if len(r.inner) <= columns {
		return nil
	}
	// Every cell at or past occ equals the row's last cell, so when occ is inside
	// the kept part and that last cell is the default blank, every removed cell is
	// a default blank too: there is nothing to return, and the tail this leaves in
	// the capacity is already zeroed. Skipping the scan matters because it is
	// proportional to the removed width and runs on every row of the history.
	if fastShrink && int(r.occ) <= columns && r.inner[len(r.inner)-1] == (Cell{}) {
		if rowShrinkTookFastPath != nil {
			rowShrinkTookFastPath(*r, columns)
		}
		// Whether truncate keeps the array makes no difference here: every removed cell
		// is the zero cell, so a kept tail is already zeroed and a dropped one takes
		// nothing with it. The row is not dirty either way.
		r.truncate(columns)
		return nil
	}
	removed := r.inner[columns:]
	// n is one past the last cell reflow has to keep, i.e. the last non-empty one.
	// This scan is proportional to the removed width and runs on every row of the
	// history, so it carries nothing else.
	n := 0
	for i := len(removed) - 1; i >= 0; i-- {
		if !removed[i].isEmpty() {
			n = i + 1
			break
		}
	}
	var out []Cell
	if n > 0 {
		out = make([]Cell, n)
		copy(out, removed[:n])
	}
	// A row that gives its array back has nothing behind its length and needs
	// neither of the branches below: the cells removed above went with the array,
	// so there is nothing to clear and nothing for a later grow to resurrect.
	if r.truncate(columns) {
		if n > 0 {
			// Clearing what was just copied out would cost a memclr proportional to the
			// row's content, on a path that is already allocating and copying. That one is
			// left to grow, which pays only if the row is ever widened again.
			r.dirty = true
		} else {
			// Nothing had to be kept, so every removed cell is blank — but blank is not
			// zero: a tab is empty without being zero, and so is a blank carrying a
			// background. Clearing up to the last of those leaves the capacity tail
			// zeroed, so a later grow can reuse it untouched, and it costs only what the
			// row actually held. The bound is a second scan rather than a second variable
			// in the loop above because it is needed only when that loop found nothing,
			// and that loop is the hot one.
			z := 0
			for i := len(removed) - 1; i >= 0; i-- {
				if removed[i] != (Cell{}) {
					z = i + 1
					break
				}
			}
			if z > 0 {
				clear(removed[:z])
			}
		}
	}
	if int(r.occ) > columns {
		r.occ = int32(columns)
	}
	return out
}

// truncate cuts the row to columns cells and reports whether it kept its array.
//
// Keeping it is the usual answer, because a window that narrows tends to widen
// again and grow reuses what is there instead of allocating. Past a point it is the
// wrong answer: the array a row keeps is as wide as the row ever was, so a sharp
// narrowing would hold the pre-narrow grid rather than the new one for as long as
// the history keeps the row. Narrowing a 1024-column history of full lines to 80
// leaves 2001 rows still on their 1024-cell arrays, which is 36 MiB of capacity for
// 3 MiB of cells: the whole grid holds 76 MiB where its cells are 40 MiB, and heap
// in use reads 83 MiB against the 47 MiB it reads with this.
// TestShrinkColumnsBoundsRetainedCapacity is the bound, at a size that runs in a
// test — narrowing 400 columns to 20, where without this 201 rows hold 3054 KiB for
// 1562 KiB of cells.
//
// A row holding more than shrinkKeepFactor times the new width therefore moves to an
// array of exactly that width, which bounds what a narrowing retains at the factor
// times what it needs. The test is on the capacity rather than on the old width, so a
// window dragged narrower in small steps still gives its array back once the steps
// have added up past the factor, and gives it back at most once per factor of
// narrowing.
func (r *Row) truncate(columns int) bool {
	if cap(r.inner) <= shrinkKeepFactor*columns {
		r.inner = r.inner[:columns]
		return true
	}
	kept := make([]Cell, columns)
	copy(kept, r.inner)
	r.inner = kept
	// cap equals len, so no tail can hold anything: the cells past columns stayed
	// with the old array and nothing reaches them.
	r.dirty = false
	return false
}

// shrinkKeepFactor is how much capacity a row may keep, as a multiple of its
// length, before shrink moves it to an array of exactly the new width.
//
// Giving an array back costs an allocation and a copy per history row on the
// narrowing, and a second allocation per row on the widen that follows, since a row
// comes out of truncate with no spare capacity at all. That is why the factor is not
// 1, and why it is not 2: at 2, a 600-row history narrowed from 200 columns to 80 and
// widened back goes from 8 allocations and 85 KB per cycle to 1258 and 3.8 MB — for a
// narrowing a client makes by attaching from a smaller window — and the same widths
// over wrapped content go from 3010 allocations and 7.8 MB to 6660 and 21.8 MB. At 3
// or more both are back to what main allocates, to the byte.
//
// 4 is a step above the sharpest of those, so a narrowing of up to four times keeps
// its reuse and anything sharper pays once to stop holding the old grid. That payment
// is the same 1258 allocations and 14 MB, for a 1024-column history at 80 columns,
// against 36 MiB the rows would hold for as long as the history keeps them.
// BenchmarkNarrowWidenHistory is both shapes.
const shrinkKeepFactor = 4

// fastShrink is off only inside TestShrinkFastPathMatchesScan, which runs the
// same bytes and resizes through both paths and compares the whole observable
// state. It is not configuration: nothing outside that test writes it, and the
// scan is kept as its oracle.
var fastShrink = true

// rowGrowClearedTail is called each time grow clears the capacity it reuses, with
// the row and the new width. Skipping that clear on rows an earlier shrink left
// clean is the other half of this optimisation, and it is invisible to every
// result-comparing assertion: clearing always is simply slower. nil outside tests.
var rowGrowClearedTail func(Row, int)

// rowShrinkTookFastPath is called each time shrink skips the tail scan, with the
// row and the width. It lets a test assert the fast path actually fires and check
// its precondition on real traffic, neither of which any result-comparing
// assertion can see: an inflated occ, or a guard that never passes, leaves every
// result correct and silently turns the optimisation off. nil outside tests, in
// the shape of shrinkColumnsPassDone.
var rowShrinkTookFastPath func(Row, int)

// reset clears the row to blanks carrying template's background.
//
// Calling fillBlank puts this function over the inline budget: go build
// -gcflags='-m -m' costs it at 108 against a limit of 80 — 100 of that without the
// counter fillBlankCopies adds to the fill — so the grid's eight blanking sites pay
// a call per row. The reset that shipped before the doubling landed did inline, at 71,
// but its fill was the store loop and cost 24; nothing measured here is a reset that
// writes today's fill out and still fits. That call is measured to be free at the
// sizes a screen has: blanking 50 rows of 200 columns runs 1943 to 1994 ns with the
// old body marked go:noinline against 1959 to 2017 ns with it inlined, six
// interleaved pairs, so the call is not what the row's reset spends its time on.
// Restructuring the blanking so that it inlines again is a real alternative with a
// real cost; see fillBlank for the candidate that was measured and how the trade went.
func (r *Row) reset(template *Cell) {
	n := len(r.inner)
	if n == 0 {
		return
	}
	if r.inner[n-1].bg != template.bg || int(r.occ) > n {
		r.occ = int32(n)
	}
	fillBlank(r.inner[:r.occ], template.bg)
	r.occ = 0
}

// fillBlank sets cells to blanks with background bg (a Cell's stored bg).
//
// The default background is the zero Cell — that is what the storage encoding is
// arranged for — so it is a clear, which the compiler turns into memclr. Any other
// background has to write a real 20-byte value into every cell, and there no memset
// exists to reach for, so the obvious code is a store per cell. Instead a first block is
// written that way and then doubled with copy, which is the runtime's vectorised
// memmove: each copy's source and destination are disjoint, the source ending exactly
// where the destination begins, so this is a memmove over a doubling prefix and not the
// overlapping self-copy that would propagate one cell at a time.
//
// Against the store loop it replaced, as two arms of one binary, that is 1.16x at 48
// cells, 1.33x at 64, 1.70x at 120, 2.05x at 200 and 2.44x at 400 — measured on the
// hook-free arm, so that the counter below is not folded in; the arm that carries it, the
// one that ships, reads 1.15x, 1.32x, 1.68x, 2.02x and 2.40x against the same store loop.
// Inlined as they ship, in one order-balanced window of eight pairs, a 50-line colored
// erase is 1.04x at 32 columns, 1.14x at 64, 1.41x at 120, 1.55x at 200 and 1.92x at 400,
// those five with the two arms' ranges disjoint, and 1.01x with them overlapping at 8
// columns, where there is nothing to double; the ratio is monotone across the six widths
// that window ran, which did not include 24 or 48. Reset over 50 rows of 200 columns is
// 1.78x. Below 48 cells the doubling does not pay: it is measurably slower than the store
// loop at 17 cells by 9.9% and at 33 by 3.9%, and at every other width under 48 the two
// cannot be told apart at all. A repaint's erases are wider than that, which is the trade
// this function makes.
// Every percentage here and below that compares two arms is taken as a share of the
// slower of the pair, which is also the convention fillforms_test.go's tables use.
//
// The seed is a min rather than a length test choosing between two bodies, and the reason
// is inline cost rather than speed. A span shorter than the seed runs the same two loops
// as any other: it writes the whole span in the first and finds the second's test already
// false, so it pays one min and one compare and nothing branches. go build -gcflags='-m
// -m' costs that at 53 against the length test's 61, or at 61 against 69 once each
// carries the counter under fillBlankCopies. All four inline at all seven call sites
// against a budget of 80, so the form has never decided that; the min is simply the one
// left with room, and the counter is what spent it. On speed the two forms cannot be
// separated: BenchmarkFillForms runs both in one binary at twelve widths, and at none of
// them does the gap between the forms exceed the gap between two arms that provably
// execute the same instructions at that width — at three widths because it *is* that gap.
//
// The seed's value is 16, a measured optimum and not a round number. A larger seed writes
// more of the span a store at a time; a smaller one reaches the doubling sooner but pays
// a memmove call to move very few cells, and both directions cost. Six values — 8, 16,
// 24, 32, 64 and 128 — are swept as an argument to one shared body inside one binary, so
// each compiles to the code a build with that constant would and no comparison crosses
// builds. That matters more than it sounds: two builds of the same store-loop arm read
// 179 and 192 ns at 400 cells, and across the three builds this sweep went through one
// seed arm's median moves by as much as 5.0% between them — a seed of 8 at 24 cells —
// which is larger than most of the seed differences below.
//
// From 48 cells up, 16 is the fastest of the six. Measured against the hook-free 16 arm,
// its rivals cost, at 48, 64, 120, 200 and 400 cells: a seed of 8, 6.4, 4.7, 3.4, 2.2 and
// 0.7% more; 24, 6.2, 9.6, 5.3, 4.8 and 1.5%; 32, 3.4, 11.1, 8.9, 5.3 and 3.3%; while 64
// costs 12.3 to 28.2% across those widths and 128 costs 17.2 to 40.8%. Each of those pairs
// separates, which is not the same as being worth acting on. The bound to read them
// against is 1.41%, the widest gap anywhere in this sweep between two arms that differ
// only in the seed's immediate; it is measured at 33 cells, because past 64 no two of the
// seeds both cover the span and so no such pair exists at 120, 200 or 400, and a
// sweep-wide maximum is the conservative stand-in there. Against it, the smallest of the
// rivals' costs — a seed of 8 at 400 cells, 0.7% — is well inside the bound, so 16's
// margin over a seed of 8 rests on 48 through 200 cells and not on 400, and 24's 1.5% at
// 400 is only just outside.
//
// Below 48 cells the span's remainder decides rather than the seed's size, and 16 does
// not win. At 17 cells every seed of 24 or more is 6.6 to 7.0% faster, because 16 is the
// one that has to memmove a single trailing cell; at 24 cells those same seeds are 6.4 to
// 7.5% slower, because 16 is the one that gets to double. At or below 16 cells the min
// clamps every seed to the span and the value is not observable at all.
//
// Confirmed where it ships, rather than through the function value the sweep needs:
// reset over 50 rows of 200 columns with the fill inlined runs 2602 to 2628 ns at a seed
// of 16 against 2709 to 2738 at 32 — 3.97%, ranges disjoint, the same sign on all eight
// pairs of an order-balanced window. That one comparison is two builds, since the shipped
// seed is a constant, which is what its control arm is for: the default-background arm
// takes the memclr above and cannot reach the seed, and it moved 0.72% with overlapping
// ranges but seven of the eight pairs in one direction, the 16 build slower. That offset
// runs against this measurement, so the seed's own effect is if anything a little larger
// than 3.97%. The sweep predicts about that much: the 2.5 ns it puts between the two
// seeds at 200 cells is 4.6% of the 54.5 ns a row of this op takes in the 32 build.
//
// A warning for whoever retunes this. At 6 to 19 ns, a fill of a few cells is smaller
// than the ways two arms of a benchmark differ without differing in what they compute:
// arms whose source differs while their executed instructions do not read up to 11.8%
// apart in this sweep, and two builds of one arm moved 7%. So sweep the value as an
// argument inside one binary against the controls fillforms_test.go sets out, include a
// width one cell past each candidate, and treat any figure from below 48 cells or from
// two separate builds as deciding nothing. Several published figures in earlier drafts of
// this comment died to exactly that, in both directions.
//
// What this function costs is somewhere no benchmark of the fill can see, and it is
// recorded here rather than smoothed over — but recorded without a number, because the
// number is the part that did not survive. Replaying recorded sessions is slower with this
// function than with the store-per-cell loop it replaced, on three sessions measured that
// way (queued-then-question-menu, compact, file-popup) with the arms order-balanced. Three
// separate sets of percentages were taken for that one comparison, they disagree, together
// they span something between one and four percent, and none of their logs is left, so
// there is nothing here to decide between them with. Treat the cost as established in sign
// and unquantified: re-running the replay harness over those three sessions is what would
// put a figure on it, and until that happens no figure in this comment should be read as
// one.
//
// What does survive says where the cost is not. It is not the doubling: there is no
// colored fill in any of the five sessions, and of the 91 full-screen erases the three
// codex sessions issue between them — 36, 36 and 19, the three whose templates were
// audited — every one carries a default template and so takes the memclr above, with no
// EL, ECH, DCH, ICH, IL or DL anywhere in the five streams to reach this function by
// another route. Nor is it the blanking: reset over 50 rows of 200 columns runs 1934 to
// 1979 ns with the store-per-cell loop and 1918 to 1957 with this function on a default
// background, and 4645 to 4741 against 2623 to 2642 with a colored one, both arms inlined
// as they ship and both figures from one measurement window, so no drift between
// harnesses is folded into them. BenchmarkEDPaths is those two arms, next to the cell walk
// that a benchmark of repeated erases spends most of its time in and would report as the
// fill's.
//
// The part of the replay cost that is explained is the per-row call, and it is a small
// part. The candidate that removes it does not make reset inline — reset stays over budget
// at 110 there — it splits reset into a Row.resetSpan holding the bookkeeping, which does
// inline at 49, and puts both halves inside a Grid.resetLines loop, so the grid's eight
// blanking sites pay one call for a whole range of lines instead of one per row. That
// recovered part of the cost on some of the three sessions and nothing that could be told
// from zero on others, and its percentages are gone with the rest: they came from a second
// order-balanced run, and the smallest of them was inside that run's own baseline drift.
// So the split is worth re-measuring beside the cost it targets, not quoting from here.
// What it did not recover rose in functions neither version touches, the parser's ASCII
// loop and memclrNoHeapPointers among them, which is the signature of code placement
// rather than of a fill. Padding inserted to move code around did not reproduce it either,
// so placement is not established, and a sweep wide enough to establish it would cost more
// than the few points at stake. The trade is therefore stated rather than resolved: a
// colored erase is worth 1.04x to 1.92x over 32 to 400 columns, a session that never
// colors one pays a few percent for reasons that are not in this function, and the split
// that pays one call per range of lines is the fallback if that ever comes to matter more
// than the erase does.
//
// TestFillBlankDoublesInsteadOfStoring holds the bounds, since every seed fills
// identical cells and so no assertion on results can see the value.
func fillBlank(cells []Cell, bg Color) {
	if bg == 0 {
		clear(cells)
		return
	}
	blank := Cell{bg: bg}
	seed := min(len(cells), fillBlankSeed)
	for i := range cells[:seed] {
		cells[i] = blank
	}
	for filled := seed; filled < len(cells); {
		filled += copy(cells[filled:], cells[:filled])
		if fillBlankCopies != nil {
			*fillBlankCopies++
		}
	}
}

// fillBlankCopies, when non-nil, counts the copies fillBlank's doubling loop makes.
// nil outside tests, where TestFillBlankDoublesInsteadOfStoring reads it.
//
// The doubling is invisible to every result-comparing assertion — a store per cell
// produces the identical cells, which is the property the equivalence tests exist to
// pin — so without this, two mutants passed the whole suite, and each dies at a
// different assertion that reads it: replacing the loop with that store, which costs
// 1.92x on a 400-column colored erase, reds on the copies a colored span is required to
// make; deleting the default background's memclr so that both backgrounds take the
// doubling reds on the copies a default-background span is required not to make, and
// costs nothing on a colored erase because a colored erase never took the memclr, which
// is why a figure from that benchmark cannot be the one quoted against it. Dropping the
// seed to 1 is a third such mutant but not this counter's to catch: the range check at
// the top of TestFillBlankDoublesInsteadOfStoring fails on the constant alone, five
// lines before the counter is installed, so it would still kill it if this variable did
// not exist. It is the same reason rowGrowClearedTail and rowShrinkTookFastPath exist,
// in the same shape.
//
// It is a pointer the test owns rather than a counter here, so that nothing in the
// package holds mutable state two Terms could fill through at once. The check reads
// one global and sits inside the doubling loop, which runs at most once per doubling
// and never at all on the default background's path: fillBlank's inline cost is 61
// against a budget of 80 with it, 53 without, so all seven call sites still inline it.
// What it costs in time is one load and branch per doubling, and BenchmarkFillForms does
// not resolve it. That sweep reads it as 1.4% at 400 cells and 1.1% at 200 against an arm
// which is this function's body without the counter, with disjoint ranges — but the same
// two arms part by 3.2% at 1 cell, also disjoint and the other way, at a width where the
// loop is never entered and the check cannot run. Separate source is worth more than this
// here, which fillBlank's controls set out; the bound that holds is the inline cost above.
// No recorded session executes it at all: not one of the five contains a colored fill, so
// none of them reaches the doubling loop the check sits in.
var fillBlankCopies *int

// fillBlankSeed is how many cells fillBlank writes one at a time before it starts
// doubling them with copy. See fillBlank for the measurements behind the value.
const fillBlankSeed = 16

// A seed below one has to be caught by the compiler rather than by a test, because
// the failure is not a wrong answer: fillBlank's doubling loop would copy nothing,
// never advance, and spin. Every test that fills a colored span would hang, whichever
// ran first, so the suite would time out somewhere unrelated instead of pointing here.
// The conversion does not compile for a non-positive seed.
const _ = uint(fillBlankSeed - 1)

func (r *Row) lastMut() *Cell {
	r.occ = int32(len(r.inner))
	return &r.inner[len(r.inner)-1]
}

func (r *Row) appendCells(cells []Cell) {
	r.occ += int32(len(cells))
	r.inner = append(r.inner, cells...)
	r.markTail()
}

func (r *Row) appendCell(c Cell) {
	r.occ++
	r.inner = append(r.inner, c)
	r.markTail()
}

func (r *Row) appendFront(cells []Cell) {
	r.occ += int32(len(cells))
	out := make([]Cell, 0, len(cells)+len(r.inner))
	out = append(out, cells...)
	r.inner = append(out, r.inner...)
	r.markTail()
}

func (r *Row) isClear() bool {
	for i := range r.inner {
		if !r.inner[i].isEmpty() {
			return false
		}
	}
	return true
}

// moveFrontTo appends the row's first at cells to dst and drops them from the
// row, which must not be dst.
//
// The cells are handed over as a slice of the row's own storage rather than as a
// copy, and what is left slides down inside that same storage, so a reflow that
// joins a wrapped line back onto the row above it allocates nothing per row. The
// version this replaces made an array for the cells moved, only for the append into
// dst to copy them straight back out of it, and a second for the remainder, which
// became the row. That second array is empty, and so free, whenever the join takes a
// whole row — the common case, but not the only one: over a randomised sweep of
// widths, histories and content, 4,143 of 12,577 joins took part of a row and paid
// for both. Sliding leaves the moved cells sitting in the capacity past the
// row's new length, which is exactly what dirty is for: grow clears the capacity
// it reuses when the flag is set, so a moved cell cannot come back.
//
// That clear is the other side of this trade rather than a cost on top of it, and
// the array is why: the row keeps the one it already had, which is the array the
// copying version threw away. Allocating an exact-size remainder left the row with
// no spare capacity, so a later widen allocated again; keeping it leaves capacity to
// reuse, so that widen reuses it and clears the part it reuses. The clear is
// therefore real work the copying version did not do, standing in for an allocation
// and a copy of the same row.
//
// How much more clearing that is depends on the traffic and on the widths a client
// resizes through, and the honest summary is that it is common rather than rare:
// counting grow's clears across 50 combinations of line length, history cap and
// resize sequence, 21 of them differ. One clean case to hold in mind is 300-character
// lines from a width of 40, where widening 80 to 200 takes grow's reuse branch for
// all 300 rows — each holding 120 cells in an array of 204 — and the copying version
// appended on every one. Timing that step, six interleaved pairs: 420 to 680 us
// against 1155 to 1357 us, with 631 allocations and 2.6 MB against 2131 and 5.4 MB.
// The ranges do not overlap, and the untouched narrowing benchmark ran 198 to 326 us
// in one arm against 251 to 327 in the other over the same pairs, so the gap is
// outside what this box's noise explains. BenchmarkRewidenColumnsReflow is that step,
// TestGrowClearsRowsAJoinDirtied its clear count.
//
// The trade still pays where the clearing is at its worst. Of those 50 combinations
// the widest gap is 351 clears against 51 — seven times as many — on 176-character
// lines with a 4000-row history and six resizes that never return to a width already
// seen. Timed, six interleaved pairs, that sequence runs 1877 to 2510 us against 2492
// to 3121 us, with 6732 allocations and 9.9 MB against 11233 and 13.7 MB: slower in
// no pair, though close enough at the ends that the ranges touch.
// BenchmarkWorstClearSequence is that sequence.
func (r *Row) moveFrontTo(dst *Row, at int) {
	dst.appendCells(r.inner[:at])
	r.occ = max(r.occ-int32(at), 0)
	r.inner = r.inner[:copy(r.inner, r.inner[at:])]
	r.markTail()
}
