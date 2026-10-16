package vt

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// shrinkDiffState is everything a caller can observe: both grids' cells, the
// history, the cursor and the geometry. occ is deliberately absent — the fast path
// changes it — so the sequences below interleave resizes with more output, which
// turns any occ divergence into wrong cells after the next scroll or erase.
func shrinkDiffState(t *Term) string {
	var b strings.Builder
	var scratch []Cell
	for _, g := range []*Grid{t.grid, t.inactiveGrid} {
		fmt.Fprintf(&b, "grid %dx%d hist=%d pushed=%d cur=%v saved=%v wrap=%v\n",
			g.columns, g.lines, g.HistorySize(), g.pushed, g.Cursor.Point, g.SavedCursor.Point, g.Cursor.InputNeedsWrap)
		for line := -g.HistorySize(); line < g.lines; line++ {
			row := g.raw.row(line)
			fmt.Fprintf(&b, "%d:", line)
			// View, not inner: a packed history row stores its cells past a 0 length.
			cells := row.View(&scratch)
			for i := range cells {
				b.WriteString(cellKey(&cells[i]))
				b.WriteByte(' ')
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// shrinkDiffChunk is output with the properties the fast path reasons about: an
// active background across a newline (reset leaves a whole row of non-default
// blanks while lowering occ to zero), wide characters at wrap boundaries, erases,
// and lines both shorter and longer than any width under test.
//
// Cases 9 to 14 are bare carriage returns, which the rest of this corpus and the
// recorded casts both lack: of the 131 recordings the cast tests replay, 130
// contain no bare CR at all, because the agents draw with cursor addressing. An
// overwrite shorter than what it replaces leaves the older content in the row,
// past the cursor and inside occ, so occ describes cells the last write never
// touched.
//
// They are state coverage and nothing stronger, which is measured rather than
// assumed. Across the 19 mutants of this package's power driver, run at the 3000
// seeds below, the set this test kills is the same with these six cases as with
// the nine that preceded them. Restricting the corpus to them alone does kill the
// two guard mutants — and so does the same corpus with the bare CRs stripped back
// out, which is what says those kills come from the long line rather than from the
// overwrite.
//
// There is a reason to expect that. Nothing in Row reads the cursor, and at, span
// and lastMut only ever RAISE occ, so a row that was overwritten carries occ at
// the extent of its LONGEST write rather than its latest one. A CR can only
// inflate occ relative to what was last written, and inflating occ only makes
// int(r.occ) <= columns harder to pass — it makes the fast path decline, which is
// the safe direction. The cases stay because the rows they build are otherwise
// unreachable here (stale content mid-row, an orphaned wide-glyph trail half) and
// the next change to shrink may read something a CR does move. They do not stay
// as evidence.
//
// One caution came out of the same measurement. This oracle is probabilistic, and
// two mutants sit on its boundary at 3000 seeds: deleting grow's partial-clear
// bookkeeping is caught with one PCG stream and missed with another, and clearing
// dirty on the fast path is missed at 200 seeds and caught at 3000. Both corpora
// catch the first at 12000. Neither is a coverage hole — TestPartialRegrowKeepsDirty
// kills both deterministically — so the seed count stays where it is. But a
// difference in what this test kills says nothing about a corpus until the stream
// constant has been varied too, which is how the one difference between these
// corpora turned out to be a seed accident.
func shrinkDiffChunk(rnd *rand.Rand) string {
	switch rnd.IntN(16) {
	case 0:
		return "\x1b[41m" + strings.Repeat("a", 1+rnd.IntN(30)) + "\r\n"
	case 1: // background left active across many newlines
		return "\x1b[44m" + strings.Repeat("\r\n", 1+rnd.IntN(8))
	case 2:
		return "\x1b[m" + strings.Repeat("b", 1+rnd.IntN(300)) + "\r\n"
	case 3:
		return strings.Repeat("漢", 1+rnd.IntN(120)) + "\r\n"
	case 4:
		return "\x1b[K\x1b[J" + strings.Repeat("c", 1+rnd.IntN(50)) + "\r\n"
	case 5:
		return "x\tz\ty\r\n"
	case 6:
		return "\x1b[7m\x1b[4:3m" + strings.Repeat("d", 1+rnd.IntN(200)) + "\x1b[m\r\n"
	case 7:
		return fmt.Sprintf("\x1b[%d;%dH", 1+rnd.IntN(40), 1+rnd.IntN(100)) + "moved"
	case 8:
		return "\x1b[45m\x1b[2K\r\n" + strings.Repeat("e", 1+rnd.IntN(400))
	case 9: // A progress bar overwriting itself: every CR but the first leaves the
		// tail of the longer previous write in the row, past the cursor and inside
		// occ. Written without a trailing newline so the row stays in that state
		// when the resize arrives. n only ever decreases, so every overwrite is
		// strictly shorter than what it replaces — an equal-length one leaves no
		// stale tail and is just case 2 with extra steps. The first length is above
		// every width this test resizes to, so the shape outlives the wrap.
		var b strings.Builder
		for n := 20 + rnd.IntN(200); n > 0; n -= 1 + rnd.IntN(20) {
			b.WriteString("\r" + strings.Repeat("#", n))
		}
		return b.String()
	case 10: // CR then erase-to-end under a background: the one way the occupied
		// part of a row shrinks in place, and what it leaves behind is blanks
		// CARRYING a background — empty without being zero, which is the exact
		// distinction the guard's second term rests on.
		return "\x1b[46m" + strings.Repeat("g", 1+rnd.IntN(120)) +
			"\r" + strings.Repeat("h", 1+rnd.IntN(10)) + "\x1b[K"
	case 11: // An overwrite landing inside an already-wrapped logical line: the CR
		// returns to the start of the last screen row rather than of the logical
		// line, so the rows before it keep their content while this one is
		// rewritten short. 240 exceeds every width this test resizes to.
		return strings.Repeat("i", 250+rnd.IntN(200)) + "\r" + strings.Repeat("j", 1+rnd.IntN(40))
	case 12: // CR at exactly the pending-wrap column, without the chunk having to
		// know the width: a CUP past the last column clamps to it, one glyph there
		// sets InputNeedsWrap, and the CR has to cancel it. shrinkDiffState prints
		// InputNeedsWrap, so a divergence in that bit alone is caught.
		return "\x1b[999Gk\r" + strings.Repeat("l", 1+rnd.IntN(8))
	case 13: // An overwrite landing on the LEAD half of a two-cell glyph, which
		// orphans its trailing half: a cell no writer produces on its own, and one
		// that is neither default nor ordinary content.
		return strings.Repeat("漢", 5+rnd.IntN(60)) + "\r" + strings.Repeat("m", 1+rnd.IntN(30)) + "\r\n"
	case 14: // The one CR shape that is real traffic rather than constructed:
		// claude-code emits "\r\r\n", 22 times in the single corpus cast that
		// contains any CR at all. The erase comes after the CR and under a fresh
		// background, so the tail is attributed blanks written by EL rather than by
		// a reset — the other way a row's last cell is non-default while occ is low.
		// Contributed by review rather than constructed here; kept separate from
		// case 10 so the two orderings of CR and EL are both covered.
		return strings.Repeat("z", 10+rnd.IntN(200)) + "\r\x1b[46m\x1b[K" +
			strings.Repeat("w", 1+rnd.IntN(20)) + "\x1b[m\r\r\n"
	default:
		return strings.Repeat("f", 1+rnd.IntN(20)) + "\r\n"
	}
}

// TestShrinkFastPathMatchesScan is the fast path's oracle: the same bytes and the
// same resizes, once with the scan and once with the skip, compared on the whole
// observable state of both grids. The scan is kept in shrink for this reason as
// much as for the rows the guard declines.
//
// The precondition is also checked at every use, not just the outcome, because a
// row the guard misjudges can produce a wrong cell several resizes later — or none
// at all, if a later erase happens to overwrite it, which is a pass that means
// nothing.
func TestShrinkFastPathMatchesScan(t *testing.T) {
	seeds := uint64(3000)
	if testing.Short() {
		seeds = 200
	}
	for seed := range seeds {
		steps := make([]string, 0, 16)
		sizes := make([][2]int, 0, 8)
		rnd := rand.New(rand.NewPCG(seed, 0x5eed))
		nsteps := 1 + rnd.IntN(5)
		for range nsteps {
			var chunk strings.Builder
			for range 1 + rnd.IntN(12) {
				chunk.WriteString(shrinkDiffChunk(rnd))
			}
			steps = append(steps, chunk.String())
			sizes = append(sizes, [2]int{2 + rnd.IntN(220), 1 + rnd.IntN(50)})
		}
		startCols, startLines := 2+rnd.IntN(220), 1+rnd.IntN(50)
		history := rnd.IntN(400)

		run := func(fast bool) string {
			// Save and restore rather than restoring the default: writing back true
			// made this test repair a flipped default for every test that runs after
			// it, which hid the whole optimisation being switched off.
			prev := fastShrink
			fastShrink = fast
			if fast {
				rowShrinkTookFastPath = func(r Row, columns int) { checkFastPathPrecondition(t, &r, columns) }
			}
			defer func() { fastShrink, rowShrinkTookFastPath = prev, nil }()
			term := NewTerm(Config{ScrollingHistory: history}, startCols, startLines, nil)
			p := NewProcessor()
			for i, s := range steps {
				feed(term, p, s)
				term.Resize(sizes[i][0], sizes[i][1])
			}
			feed(term, p, "final\r\n\x1b[42m\x1b[2J")
			return shrinkDiffState(term)
		}
		want, got := run(false), run(true)
		if want != got {
			t.Fatalf("seed %d: fast path diverged\nstart %dx%d history %d sizes %v",
				seed, startCols, startLines, history, sizes)
		}
	}
}
