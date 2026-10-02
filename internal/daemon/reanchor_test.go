package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gurupras/shelld"
)

// reanchor's job is to keep a paused client on the content it is reading while
// the grid moves underneath it. It does that from vt.Grid.HistoryPushed, a net
// count that a narrowing resize can drive below zero, so these tests follow a
// recognisable row rather than asserting a number: what matters is that the
// client is still looking at the same text afterwards.

// blockGrid fills a grid with rows of two distinguishable halves, so that
// halving the width splits each row into two rows that are still tellable
// apart. Row i is one letter repeated across the left half and the next letter
// across the right.
func blockGrid(t *testing.T, cols, rows, lines int) *shelld.VtGrid {
	t.Helper()
	if cols%2 != 0 {
		t.Fatalf("blockGrid needs an even width, got %d", cols)
	}
	g := shelld.NewVtGrid(cols, rows)
	g.Advance(blockRows(cols, lines))
	if g.ScrollbackLen() == 0 {
		t.Fatalf("setup: no scrollback, so there is nothing for a paused client to hold")
	}
	return g
}

// blockRows is the output blockGrid writes, so that a test driving a window's own
// grid can write the same rows into it rather than build a grid of its own.
func blockRows(cols, lines int) []byte {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(strings.Repeat(string(rune('A'+2*i%26)), cols/2))
		b.WriteString(strings.Repeat(string(rune('A'+(2*i+1)%26)), cols/2))
	}
	return []byte(b.String())
}

// topRow is the text of the first row a client paused at offset sees: offsets
// count back from the live tail, so it is grid line -offset.
func topRow(g *shelld.VtGrid, offset uint32) string {
	return firstRow(g.SnapshotAt(int(offset)))
}

// firstRow is the text of a snapshot's first row, trailing blanks trimmed. It
// takes a snapshot rather than a grid so that a view of a frozen copy and a view
// of the live grid are read the same way.
func firstRow(snap *shelld.GridSnapshot) string {
	var b strings.Builder
	for _, c := range snap.Cells[:snap.Cols] {
		if c.Ch == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(c.Ch)
	}
	return strings.TrimRight(b.String(), " ")
}

// offsetOf is the offset at which text is the top row, or -1 if no offset shows
// it. Tests use it two ways: to say where a client's content actually went, and
// to refuse to pass by accident — a row sitting at the far end of history is one
// an overshooting offset lands on whatever the code under test did.
func offsetOf(g *shelld.VtGrid, text string) int {
	for off := 0; off <= g.ScrollbackLen(); off++ {
		if topRow(g, uint32(off)) == text {
			return off
		}
	}
	return -1
}

// whereIs describes offsetOf's answer for a failure message.
func whereIs(g *shelld.VtGrid, text string) string {
	if off := offsetOf(g, text); off >= 0 {
		return fmt.Sprintf("offset %d", off)
	}
	return fmt.Sprintf("no offset in %d lines of history", g.ScrollbackLen())
}

// pausedTicker is a ticker holding a paused view offset lines back from the
// live tail, anchored at the grid's current count.
func pausedTicker(g *shelld.VtGrid, offset uint32) *ticker {
	t := &ticker{}
	t.mode = clientMode{paused: true, offset: offset}
	t.pushed = g.HistoryPushed()
	return t
}

// TestReanchorKeepsAPausedClientOnItsContentAcrossReflowAndGrowth is the case
// the count's signedness exists for. Narrowing manufactures history rows that
// no push counted; a taller screen then reclaims more rows than were ever
// pushed, taking the count below where the client's anchor reading sits; and
// further output brings it back up through that reading. No tick lands inside
// the excursion, which is the ordinary case — a client resizes its terminal once
// and the shell repaints, all within one tick window.
//
// The client must end up reading the same text. Flooring the count at zero
// instead of letting it go negative discards the excursion, and the client lands
// that many rows too deep in history; leaving the count unsigned makes the dip
// read as a ~2^64-line push, which pins the client to the far end of history.
func TestReanchorKeepsAPausedClientOnItsContentAcrossReflowAndGrowth(t *testing.T) {
	g := blockGrid(t, 16, 3, 5)
	g.Resize(8, 3) // every row splits into its two halves

	tk := pausedTicker(g, 3)
	want := topRow(g, tk.mode.offset)
	if want == "" {
		t.Fatalf("setup: the client's top row is blank, so this test cannot follow it")
	}

	g.Resize(8, 8) // reclaims five rows from a history of seven
	g.Advance([]byte("\r\n" + strings.Repeat("zzzz\r\n", 9)))
	tk.reanchor(g)

	if got := topRow(g, tk.mode.offset); got != want {
		t.Errorf("client at offset %d now reads %q, want %q, which is at %s",
			tk.mode.offset, got, want, whereIs(g, want))
	}
}

// TestReanchorFollowsOrdinaryOutput is the neighbouring input with no resize in
// it at all: the count only rises, and the client still has to track its row.
// It is what stops reanchor from being written as a no-op.
func TestReanchorFollowsOrdinaryOutput(t *testing.T) {
	g := blockGrid(t, 16, 3, 5)
	tk := pausedTicker(g, 2)
	want := topRow(g, tk.mode.offset)

	g.Advance([]byte("\r\n" + strings.Repeat("zzzz\r\n", 6)))
	tk.reanchor(g)

	if got := topRow(g, tk.mode.offset); got != want {
		t.Errorf("client at offset %d reads %q, want %q — seven lines of output move its content seven rows back",
			tk.mode.offset, got, want)
	}
}

// TestReanchorLeavesALiveClientAlone is the control on the paused guard: the
// identical sequence on a client that is not paused must not move its offset at
// all, whatever the count does.
func TestReanchorLeavesALiveClientAlone(t *testing.T) {
	g := blockGrid(t, 16, 3, 5)
	tk := pausedTicker(g, 5)
	tk.mode.paused = false

	g.Resize(8, 3)
	tk.reanchor(g)
	g.Resize(8, 8)
	tk.reanchor(g)

	if tk.mode.offset != 5 {
		t.Errorf("offset %d, want 5 — a live client holds no history offset to reanchor", tk.mode.offset)
	}
	if tk.pushed != g.HistoryPushed() {
		t.Errorf("tracked count %d, want the grid's %d — it is recorded even when the offset is not adjusted", tk.pushed, g.HistoryPushed())
	}
}

// TestReanchorClampsToTheHistoryThatExists pins the other half of the contract.
// The count says how far content moved; it does not say the content is still
// there. Once history is at its limit it drops rows off the far end as fast as
// new ones arrive, so the count keeps rising while the history stops growing,
// and a client near the oldest row is asked for an offset past the end of it.
//
// Its content is genuinely gone, so it cannot be kept on it. What it must not do
// is hold an offset that addresses nothing: the derived offset is clamped to the
// history that exists, leaving the client on the oldest row still held.
func TestReanchorClampsToTheHistoryThatExists(t *testing.T) {
	g := blockGrid(t, 16, 3, 5)
	g.Advance([]byte(strings.Repeat("\r\nq", 10100))) // fill history to its limit
	sb := g.ScrollbackLen()
	if int64(sb) >= g.HistoryPushed() {
		t.Fatalf("setup: history %d has not reached its limit against %d pushes, so the count cannot outrun it", sb, g.HistoryPushed())
	}

	tk := pausedTicker(g, uint32(sb-1)) // one row from the oldest
	g.Advance([]byte(strings.Repeat("\r\nr", 50)))
	tk.reanchor(g)

	if got := g.ScrollbackLen(); got != sb {
		t.Fatalf("history moved from %d to %d; this test needs it pinned at the limit", sb, got)
	}
	if tk.mode.offset > uint32(sb) {
		t.Errorf("offset %d addresses past the %d lines of history that exist", tk.mode.offset, sb)
	}
}

// TestReanchorNeverUnderflowsTheOffset is the same guard in the other direction:
// a count that falls by more than the client's offset must leave it at the live
// tail, not wrap a uint32 offset around to the far end of history.
func TestReanchorNeverUnderflowsTheOffset(t *testing.T) {
	g := blockGrid(t, 16, 3, 5)
	g.Resize(8, 3)
	tk := pausedTicker(g, 1) // less than the five rows the grow reclaims

	g.Resize(8, 8)
	tk.reanchor(g)

	if tk.mode.offset > uint32(g.ScrollbackLen()) {
		t.Errorf("offset %d exceeds the %d lines of history that exist: the fall past zero wrapped", tk.mode.offset, g.ScrollbackLen())
	}
}

// TestReanchorKeepsAPausedClientOnItsContentAcrossAPureHeightGrow is the same
// promise with the width held fixed, so no reflow is involved and nothing drives
// the count negative. The screen grows by more rows than history can fill, which
// makes growLines rotate the leftover blank lines past the content; counting that
// rotation as a push tells the client its content scrolled away when it did not.
//
// As elsewhere, one reanchor at the end: the grow and the repaint that follows it
// both land inside one tick window, so the client never observes the intermediate
// state where history is briefly empty.
func TestReanchorKeepsAPausedClientOnItsContentAcrossAPureHeightGrow(t *testing.T) {
	g := blockGrid(t, 16, 5, 10)
	tk := pausedTicker(g, 2)
	want := topRow(g, tk.mode.offset)
	if want == "" {
		t.Fatalf("setup: the client's top row is blank, so this test cannot follow it")
	}
	if g.ScrollbackLen() >= 15 {
		t.Fatalf("setup: history %d covers the 15 rows the grow adds, so no blank lines are left to rotate", g.ScrollbackLen())
	}

	g.Resize(16, 20) // width untouched; history supplies 5 of the 15 rows added
	g.Advance([]byte("\r\n" + strings.Repeat("zzzz\r\n", 13)))
	tk.reanchor(g)

	if at := offsetOf(g, want); at < 0 || at == g.ScrollbackLen() {
		t.Fatalf("setup: %q ended at %s of %d, where an offset that overshot lands on it anyway",
			want, whereIs(g, want), g.ScrollbackLen())
	}
	if got := topRow(g, tk.mode.offset); got != want {
		t.Errorf("client at offset %d now reads %q, want %q, which is at %s",
			tk.mode.offset, got, want, whereIs(g, want))
	}
}

// TestReanchorLeavesAPausedOffsetPastHistoryAtTheScrollbackCap is the state the
// two arms are asymmetric for. reanchor's positive arm bounds its result by the
// history that exists and its negative arm does not, so the offset it stores can
// end up addressing a row that history no longer holds. Both the comment on the
// switch and the one on render's write-back say so; this is what makes it a
// measurement rather than an argument.
//
// The mechanism needs the cap and a gap. At the cap a push still counts —
// vt/grid.go's scrollUp adds to the count unconditionally while
// increaseScrollLimit adds nothing once HistorySize is at maxScrollLimit — so
// pushes there move the count without moving the bound. A rows-only grow then
// takes the same number off both. reanchor differences the count once per tick,
// so a tick window holding both moves the count by less than it moves the
// history, and the difference is exactly the pushes that bought no row.
//
// A grow rather than a shrink, and rows rather than columns: only a grow drops
// history, and a width change would freeze the claimed views, which switches the
// bound to the copy's and closes the gap before it opens.
func TestReanchorLeavesAPausedOffsetPastHistoryAtTheScrollbackCap(t *testing.T) {
	// The history limit NewVtGrid applies. Pushes past it are what count for free.
	const limit = 10_000
	const pushes, grow = 5, 7

	g := blockGrid(t, 16, 3, limit+10)
	if sb := g.ScrollbackLen(); sb != limit {
		t.Fatalf("setup: history is %d lines, not the %d-line limit, so a push here still buys a row and the gap cannot open",
			sb, limit)
	}
	// The deepest row history holds, so the whole gap is outside the bound rather
	// than partly absorbed by room above the client.
	tk := pausedTicker(g, limit)

	sb0, p0 := g.ScrollbackLen(), g.HistoryPushed()
	g.Advance([]byte(strings.Repeat("\r\nfree", pushes)))
	sb1, p1 := g.ScrollbackLen(), g.HistoryPushed()
	if sb1 != sb0 || p1-p0 != pushes {
		t.Fatalf("setup: %d pushes at the limit moved history %d->%d and the count by %d, want history unmoved and the count by %d",
			pushes, sb0, sb1, p1-p0, pushes)
	}

	g.Resize(16, 3+grow)
	sb2, p2 := g.ScrollbackLen(), g.HistoryPushed()
	if sb1-sb2 != grow || p1-p2 != grow {
		t.Fatalf("setup: a grow of %d rows took %d off history and %d off the count, want %d off both",
			grow, sb1-sb2, p1-p2, grow)
	}

	// One reanchor for the whole window, which is what a tick does: the client did
	// not tick between the pushes and the grow.
	tk.reanchor(g)

	if got := int(tk.mode.offset) - sb2; got != pushes {
		t.Errorf("reanchor left offset %d against %d rows of history, %d past the end; want %d past, the pushes that counted without adding a row (the count moved %d, history moved %d)",
			tk.mode.offset, sb2, got, pushes, p2-p0, sb2-sb0)
	}
}
