package vt

import (
	"strings"
	"testing"
)

// Scrolling history: the invariants that hold across the paths which move
// lines into, out of, and past the scrollback.
//
// These exist because the reverse-scroll paths were reachable only from
// TestRenderClientMatchesSnapshot, which builds its client with vt.Config{} —
// scrollback disabled — and asserts a viewport diff. Both halves of that miss
// the same thing: every defect below leaves the viewport correct and shows up
// only above line 0 or in HistoryPushed. So each test states what history
// holds, not just how many rows it has.

// histText joins the scrollback from oldest to newest.
func histText(t *Term) string {
	var rows []string
	for line := -t.HistorySize(); line < 0; line++ {
		rows = append(rows, rowText(t, line))
	}
	return strings.Join(rows, "|")
}

// screenText joins the viewport from top to bottom.
func screenText(t *Term) string {
	var rows []string
	for line := 0; line < t.ScreenLines(); line++ {
		rows = append(rows, rowText(t, line))
	}
	return strings.Join(rows, "|")
}

// fiveOverThree fills a three-line screen from a five-line stream, leaving
// "1|2" in history, "3|4|5" on screen and HistoryPushed at 2.
func fiveOverThree(t *testing.T, history int) (*Term, *Processor) {
	t.Helper()
	term, p := NewTerm(Config{ScrollingHistory: history}, 10, 3, nil), NewProcessor()
	feed(term, p, "1\r\n2\r\n3\r\n4\r\n5")
	if h, s, pushed := histText(term), screenText(term), term.HistoryPushed(); h != "1|2" || s != "3|4|5" || pushed != 2 {
		t.Fatalf("setup: history %q screen %q pushed %d, want \"1|2\" \"3|4|5\" 2", h, s, pushed)
	}
	return term, p
}

// TestReverseScrollLeavesHistoryAlone covers the three ways a stream scrolls
// the screen downwards. None of them may add to history, drop from it, or move
// HistoryPushed: the lines leaving the screen fall off its bottom, and the
// count exists so a viewer holding a history offset can follow its content.
func TestReverseScrollLeavesHistoryAlone(t *testing.T) {
	for _, tc := range []struct {
		name, seq, screen string
	}{
		{"IL at the top of the screen", "\x1b[H\x1b[L", "|3|4"},
		{"IL of two lines", "\x1b[H\x1b[2L", "||3"},
		{"SD", "\x1b[H\x1b[T", "|3|4"},
		{"RI with the cursor on the top line", "\x1b[H\x1bM", "|3|4"},
		{"DL below the top line", "\x1b[2;1H\x1b[M", "3|5|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := fiveOverThree(t, 5)
			feed(term, p, tc.seq)
			if got := histText(term); got != "1|2" {
				t.Errorf("history %q, want \"1|2\" — scrolling down must not disturb the scrollback", got)
			}
			if got := term.HistorySize(); got != 2 {
				t.Errorf("HistorySize %d, want 2", got)
			}
			if got := term.HistoryPushed(); got != 2 {
				t.Errorf("HistoryPushed %d, want 2 — no line left the top of the screen", got)
			}
			if got := screenText(term); got != tc.screen {
				t.Errorf("screen %q, want %q", got, tc.screen)
			}
		})
	}
}

// TestScrollDownWithoutHistoryKeepsRowsOutsideTheRegion pins the branch
// scrollDown takes when there is no scrollback to rotate through: it shuffles
// the rows below the region out of the way, rotates the whole ring, and puts
// them back. The rows outside the region must come out where they went in.
func TestScrollDownWithoutHistoryKeepsRowsOutsideTheRegion(t *testing.T) {
	term, p := NewTerm(Config{}, 10, 6, nil), NewProcessor()
	feed(term, p, "A\r\nB\r\nC\r\nD\r\nE\r\nF")
	if got := screenText(term); got != "A|B|C|D|E|F" {
		t.Fatalf("setup: screen %q", got)
	}
	feed(term, p, "\x1b[3;5r\x1b[3;1H\x1b[L") // region is rows 3..5; insert one line at its top
	if got := screenText(term); got != "A|B||C|D|F" {
		t.Errorf("screen %q, want \"A|B||C|D|F\": C and D move down inside the region, E falls out of it, and A, B and F are outside it", got)
	}
	if h, pushed := term.HistorySize(), term.HistoryPushed(); h != 0 || pushed != 0 {
		t.Errorf("HistorySize %d HistoryPushed %d, want 0 0 with scrollback disabled", h, pushed)
	}
}

// TestHistoryPushedCountsEveryLineOfAMultiLineScroll guards the count against
// being incremented per scroll rather than per line. A viewer applies the
// change in this count to its own offset, so one SU of three lines has to move
// it by three.
func TestHistoryPushedCountsEveryLineOfAMultiLineScroll(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 5, nil), NewProcessor()
	feed(term, p, "1\r\n2\r\n3\r\n4\r\n5")
	if pushed := term.HistoryPushed(); pushed != 0 {
		t.Fatalf("setup: pushed %d, want 0 — five lines fit on a five-line screen", pushed)
	}
	feed(term, p, "\x1b[3S")
	if got := term.HistoryPushed(); got != 3 {
		t.Errorf("HistoryPushed %d after SU 3, want 3", got)
	}
	if got := histText(term); got != "1|2|3" {
		t.Errorf("history %q, want \"1|2|3\"", got)
	}
	if got := screenText(term); got != "4|5|||" {
		t.Errorf("screen %q, want \"4|5|||\"", got)
	}
}

// TestTopAnchoredRegionScrolledByItsHeightEntersHistory covers the one case
// where the fast path for a region shorter than the scroll would be wrong. A
// region anchored at the top of the screen scrolls into history however tall
// it is, and blanking it in place would look identical on screen.
func TestTopAnchoredRegionScrolledByItsHeightEntersHistory(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 5, nil), NewProcessor()
	feed(term, p, "A\r\nB\r\nC\r\nD\r\nE")
	feed(term, p, "\x1b[1;3r\x1b[3S") // region is rows 1..3, scrolled by its full height
	if got := histText(term); got != "A|B|C" {
		t.Errorf("history %q, want \"A|B|C\" — a region at the top of the screen scrolls into history", got)
	}
	if got := term.HistoryPushed(); got != 3 {
		t.Errorf("HistoryPushed %d, want 3", got)
	}
	if got := screenText(term); got != "|||D|E" {
		t.Errorf("screen %q, want \"|||D|E\" — rows below the region do not move", got)
	}
}

// TestHistoryAtItsLimitDropsTheOldestLine checks what the scrollback holds once
// it is full, not only how tall it is: the limit drops from the far end.
func TestHistoryAtItsLimitDropsTheOldestLine(t *testing.T) {
	term, p := fiveOverThree(t, 2)
	feed(term, p, "\r\n6")
	if got := term.HistorySize(); got != 2 {
		t.Errorf("HistorySize %d, want 2 — the configured limit", got)
	}
	if got := histText(term); got != "2|3" {
		t.Errorf("history %q, want \"2|3\" — line 1 is past the limit", got)
	}
	if got := term.HistoryPushed(); got != 3 {
		t.Errorf("HistoryPushed %d, want 3 — it counts lines the limit has dropped", got)
	}
	if got := screenText(term); got != "4|5|6" {
		t.Errorf("screen %q, want \"4|5|6\"", got)
	}
}

// TestNoScrollbackNeverPushes checks that a terminal configured without history
// reports none, however much it scrolls.
func TestNoScrollbackNeverPushes(t *testing.T) {
	term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
	feed(term, p, strings.Repeat("x\r\n", 20))
	if h, pushed := term.HistorySize(), term.HistoryPushed(); h != 0 || pushed != 0 {
		t.Errorf("HistorySize %d HistoryPushed %d after 20 scrolls, want 0 0", h, pushed)
	}
}

// TestClearSavedLinesEmptiesHistory covers ED 3, the only sequence that drops
// the scrollback without touching the screen. HistoryPushed is deliberately not
// asserted: a history clear is documented not to maintain its relationship to
// the content a viewer is holding.
func TestClearSavedLinesEmptiesHistory(t *testing.T) {
	term, p := fiveOverThree(t, 5)
	feed(term, p, "\x1b[3J")
	if got := term.HistorySize(); got != 0 {
		t.Errorf("HistorySize %d after ED 3, want 0", got)
	}
	if got := screenText(term); got != "3|4|5" {
		t.Errorf("screen %q, want \"3|4|5\" — ED 3 clears history, not the screen", got)
	}
}

// TestGrowingScreenPullsHistoryBackOntoTheScreen covers a taller screen taking
// its new lines from history rather than blanking them: the scrollback empties
// from its newest end onto the top of the screen, in order, and the cursor rides
// along with the content under it.
func TestGrowingScreenPullsHistoryBackOntoTheScreen(t *testing.T) {
	term, p := fiveOverThree(t, 100)
	term.Resize(10, 5)
	if got := screenText(term); got != "1|2|3|4|5" {
		t.Errorf("screen %q, want \"1|2|3|4|5\" — the two history rows return above the screen's content", got)
	}
	if got := term.HistorySize(); got != 0 {
		t.Errorf("HistorySize %d, want 0 — both rows were pulled back out", got)
	}
	if got := term.HistoryPushed(); got != 0 {
		t.Errorf("HistoryPushed %d, want 0 — pulling a row back out undoes its push", got)
	}
	feed(term, p, "X")
	if got := screenText(term); got != "1|2|3|4|5X" {
		t.Errorf("screen %q after writing one character, want it appended to \"5\": the cursor moved down with its own row", got)
	}
}

// TestGrowingScreenWithoutHistoryKeepsContentAnchored covers the other half of
// growLines: with no history to pull from, the new lines are blanks and the
// content stays where it is, cursor included.
//
// So the count does not move either. growLines rotates the blank lines from the
// top of the screen to the bottom through scrollUp, which counts what it rotates
// as a push, and growLines puts the count back: the rotation moves blanks past
// the content, not content off the screen.
func TestGrowingScreenWithoutHistoryKeepsContentAnchored(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 3, nil), NewProcessor()
	feed(term, p, "1\r\n2")
	term.Resize(10, 5)
	if got := screenText(term); got != "1|2|||" {
		t.Errorf("screen %q, want \"1|2|||\" — the new lines arrive blank below the content", got)
	}
	if got := term.HistorySize(); got != 0 {
		t.Errorf("HistorySize %d, want 0 — growing must not manufacture scrollback", got)
	}
	if got := term.HistoryPushed(); got != 0 {
		t.Errorf("HistoryPushed %d, want 0 — no line left the screen, only blanks moved past the content", got)
	}
	feed(term, p, "X")
	if got := screenText(term); got != "1|2X|||" {
		t.Errorf("screen %q, want the character appended to \"2\": the cursor did not move", got)
	}
}

// TestShrinkingScreenPushesTheRowsAboveTheCursor covers a shorter screen having
// to put the cursor's row back on screen: the rows above it go to history, and
// the count moves by exactly as many.
func TestShrinkingScreenPushesTheRowsAboveTheCursor(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 5, nil), NewProcessor()
	feed(term, p, "1\r\n2\r\n3\r\n4\r\n5")
	term.Resize(10, 3)
	if got := histText(term); got != "1|2" {
		t.Errorf("history %q, want \"1|2\" — the two rows above the screen's new top", got)
	}
	if got := screenText(term); got != "3|4|5" {
		t.Errorf("screen %q, want \"3|4|5\"", got)
	}
	if got := term.HistoryPushed(); got != 2 {
		t.Errorf("HistoryPushed %d, want 2", got)
	}
	feed(term, p, "X")
	if got := screenText(term); got != "3|4|5X" {
		t.Errorf("screen %q after writing one character, want it appended to \"5\": the cursor kept its row", got)
	}
}

// TestShrinkingScreenDropsRowsBelowTheCursor pins the other shrink case. With
// the cursor above the new bottom nothing has to move to keep it on screen, so
// the rows below it are cut rather than pushed: they are not in history
// afterwards and the count does not move. KeepRowsOnShrink is the knob that
// trades this away.
func TestShrinkingScreenDropsRowsBelowTheCursor(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 5, nil), NewProcessor()
	feed(term, p, "1\r\n2\r\n3\r\n4\r\n5\x1b[2;1H")
	term.Resize(10, 3)
	if got := screenText(term); got != "1|2|3" {
		t.Errorf("screen %q, want \"1|2|3\"", got)
	}
	if h, pushed := term.HistorySize(), term.HistoryPushed(); h != 0 || pushed != 0 {
		t.Errorf("HistorySize %d HistoryPushed %d, want 0 0 — rows 4 and 5 were cut, not scrolled off the top", h, pushed)
	}
}

// TestAltScreenScrollingLeavesPrimaryHistoryIntact covers a full-screen program
// scrolling for as long as it likes: the primary screen's scrollback is the
// user's, and neither its content nor the count a viewer tracks may move while
// the alternate screen is up.
func TestAltScreenScrollingLeavesPrimaryHistoryIntact(t *testing.T) {
	term, p := fiveOverThree(t, 10)
	feed(term, p, "\x1b[?1049h")
	feed(term, p, strings.Repeat("alt\r\n", 30))
	if h := term.HistorySize(); h != 0 {
		t.Errorf("HistorySize %d on the alternate screen, want 0", h)
	}
	if got := term.HistoryPushed(); got != 2 {
		t.Errorf("HistoryPushed %d on the alternate screen, want the primary screen's 2", got)
	}
	feed(term, p, "\x1b[?1049l")
	if got := histText(term); got != "1|2" {
		t.Errorf("history %q after leaving the alternate screen, want \"1|2\"", got)
	}
	if got := term.HistoryPushed(); got != 2 {
		t.Errorf("HistoryPushed %d, want 2 — 30 scrolls on the alternate screen push nothing", got)
	}
	if got := screenText(term); got != "3|4|5" {
		t.Errorf("screen %q, want \"3|4|5\"", got)
	}
}

// TestReflowThenGrowTakesTheCountBelowZero covers the one path that drives the
// count negative. Narrowing splits wrapped rows, which manufactures history
// rows without pushing anything, so HistorySize outruns the count. A taller
// screen then pulls rows back out of that larger history and subtracts every
// one of them, including rows no push ever counted.
//
// The count is signed for exactly this reason. The negative value is not a row
// count and nothing reads it as one; what it records is that content has moved
// three rows nearer the tail than the pushes alone account for, which the next
// reading has to be differenced against.
func TestReflowThenGrowTakesTheCountBelowZero(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 3, nil), NewProcessor()
	feed(term, p, "AAAABBBB\r\nCCCCDDDD\r\nEEEEFFFF\r\nGGGGHHHH\r\nIIIIJJJJ")
	if h, pushed := term.HistorySize(), term.HistoryPushed(); h != 2 || pushed != 2 {
		t.Fatalf("setup: HistorySize %d HistoryPushed %d, want 2 2", h, pushed)
	}

	term.Resize(4, 3) // every 8-column row reflows into two 4-column rows
	if h, pushed := int64(term.HistorySize()), term.HistoryPushed(); h <= pushed {
		t.Fatalf("setup: HistorySize %d HistoryPushed %d, want history to outrun the count — otherwise this test cannot reach a negative count", h, pushed)
	}

	term.Resize(4, 8) // pull five rows back out of a history of seven
	if got := term.HistoryPushed(); got != -3 {
		t.Errorf("HistoryPushed %d, want -3: five rows came back out of history and only two were ever pushed", got)
	}
}

// TestCountBelowZeroStillTracksContentExactly is the invariant the sign exists
// to preserve, asserted on content rather than on the number. A viewer holds a
// row and a reading of the count; after the whole narrow-grow-output sequence,
// the change in the count must equal the number of rows that row actually moved
// away from the live tail — including across the stretch where the count was
// negative.
//
// A count floored at zero passes the test above and fails this one: flooring
// discards the excursion, so the later difference overstates the movement by
// however far below zero the count would have gone, and a paused viewer lands
// that many rows too deep in history.
func TestCountBelowZeroStillTracksContentExactly(t *testing.T) {
	const anchor = "EEEE"
	lineOf := func(term *Term) int { return lineOfRow(t, term, anchor) }

	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 3, nil), NewProcessor()
	feed(term, p, "AAAABBBB\r\nCCCCDDDD\r\nEEEEFFFF\r\nGGGGHHHH\r\nIIIIJJJJ")
	term.Resize(4, 3)

	// The viewer's window starts on the anchor row; offsets count back from the
	// live tail, so its offset is the anchor's negated line.
	before, readingBefore := -lineOf(term), term.HistoryPushed()

	// No reading is taken between these two: a tick can miss the whole excursion,
	// and the count has to survive being sampled only at the ends.
	term.Resize(4, 8)
	feed(term, p, "\r\nKKKK\r\nLLLL\r\nMMMM\r\nNNNN\r\nOOOO\r\nPPPP\r\nQQQQ\r\nRRRR\r\nSSSS")

	after := -lineOf(term)
	if moved, counted := int64(after-before), term.HistoryPushed()-readingBefore; moved != counted {
		t.Errorf("%q moved %d rows from the tail but the count moved %d: a viewer applying the count lands %d rows out",
			anchor, moved, counted, counted-moved)
	}
}

// lineOfRow is the grid line a followed row sits on, so a test can measure how
// far that row moved rather than asserting the count against itself. A row's
// distance from the live tail is its negated line, which is also the history
// offset a viewer holding it would need.
func lineOfRow(t *testing.T, term *Term, anchor string) int {
	t.Helper()
	for line := -term.HistorySize(); line < term.ScreenLines(); line++ {
		if strings.TrimRight(rowText(term, line), " ") == anchor {
			return line
		}
	}
	t.Fatalf("row %q left the grid, so this test can no longer follow it", anchor)
	return 0
}

// TestGrowingPastItsHistoryDoesNotCountTheBlanks is the same content invariant
// for the other way a grow goes wrong, and it needs no reflow: the width never
// changes here. When the screen grows by more rows than history can fill, the
// remainder arrive blank above the content and growLines rotates them below it
// through scrollUp. That rotation moves blanks past the content; no line leaves
// the screen, so it must not reach the count.
//
// Counting it inflates the count by exactly the shortfall — ten rows here — and
// a paused viewer differencing two readings applies those ten rows as a scroll
// that never happened, landing ten rows too deep in history. The inflation is
// not visible as a rise: with enough history the reclaim outweighs it and the
// count still falls, just not by as much as the content moved. The shortfall is
// the precondition, not the direction.
func TestGrowingPastItsHistoryDoesNotCountTheBlanks(t *testing.T) {
	const anchor = "4"
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 5, nil), NewProcessor()
	feed(term, p, "1\r\n2\r\n3\r\n4\r\n5\r\n6\r\n7\r\n8\r\n9\r\n10")
	if h := term.HistorySize(); h >= 15 {
		t.Fatalf("setup: HistorySize %d covers the 15 rows the grow adds, so nothing is left blank and there is no shortfall to test", h)
	}

	before, readingBefore := -lineOfRow(t, term, anchor), term.HistoryPushed()

	// One reading at each end, as a tick that misses the resize would take them.
	term.Resize(10, 20)
	feed(term, p, "\r\na\r\nb\r\nc\r\nd\r\ne\r\nf\r\ng\r\nh\r\ni\r\nj\r\nk\r\nl\r\nm\r\nn")

	after := -lineOfRow(t, term, anchor)
	if after >= term.HistorySize() {
		t.Fatalf("setup: %q ended as the oldest row in %d lines of history, where an offset that overshot would land on it anyway", anchor, term.HistorySize())
	}
	if moved, counted := int64(after-before), term.HistoryPushed()-readingBefore; moved != counted {
		t.Errorf("%q moved %d rows from the tail but the count moved %d: a viewer applying the count lands %d rows out",
			anchor, moved, counted, counted-moved)
	}
}
