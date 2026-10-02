//go:build linux || darwin

package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/gurupras/shelld"
)

// The tests here drive a paused client through render and handleCtrl rather than
// calling the pieces by hand, because what they are about is a call that a caller
// has to make. resumeView is held down three ways by the tests next door — each of
// its three lines fails a different one — but every one of those tests calls it
// itself, so what they pin is that one call suffices. They say nothing about
// whether anything makes it, and deleting the call at three of its four sites left
// the whole package green. The cost is the same at each: the grid goes on copying
// the window's whole history, once per clear or width change, for a client that is
// no longer reading it, and no ticker points at the claim any more so nothing can
// ever release it.
//
// That is why these need a real window. render and handleCtrl read the grid under
// the window's own lock and write the status tuple from it, so a ticker without one
// cannot reach either.

// renderingTicker is a ticker whose render and handleCtrl can be called: a real
// window for the VT lock and the grid, and an attachment for the crop.
//
// The window runs cat, which produces no output of its own, so the grid holds only
// what a test writes into it. The attachment is an observer so that its declared
// size does not vote on the window's: these tests write history at the window's own
// width and a vote that changed it would reflow that history underneath them.
func renderingTicker(t *testing.T, cols, rows uint16, lines int) (*ticker, *shelld.Window) {
	t.Helper()
	w, err := shelld.NewWindow(shelld.WindowOpts{
		Name:    "render",
		Command: []string{"/bin/cat"},
		Cols:    cols,
		Rows:    rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)

	g := w.LockVT()
	g.Advance(blockRows(int(cols), lines))
	sb := g.ScrollbackLen()
	w.UnlockVT()
	if sb == 0 {
		t.Fatalf("setup: %d lines at %d rows left no history, so there is nothing to pause on", lines, rows)
	}

	att := w.RegisterObserver(cols, rows)
	t.Cleanup(att.Release)
	tk := &ticker{win: w, att: att, live: &shelld.LiveFramer{}, observer: true}
	// The paused baseline run sets up before its first tick. A ticker's zero value
	// has none, and a paused frame is a diff against whatever is there, so a ticker
	// assembled here rather than by run faults on the first one — a property of this
	// harness, not of the code it drives.
	tk.lastSentPaused, tk.pausedScratch = shelld.EmptySnapshot(), &shelld.GridSnapshot{}
	return tk, w
}

// pauseAt puts the client into the paused state the way a client does, through the
// control frame, so that the claim is registered by the code under test.
func pauseAt(t *testing.T, tk *ticker, offset uint32) {
	t.Helper()
	tk.handleCtrl(ctrlEvent{scroll: &scrollMsg{op: "enter", offset: offset, hasOffset: true}})
	if !tk.mode.paused || tk.mode.offset != offset {
		t.Fatalf("setup: the client is %+v, not paused at offset %d", tk.mode, offset)
	}
	if claimsHeldBy(tk.win) == 0 {
		t.Fatalf("setup: pausing registered no claim, so nothing below is about releasing one")
	}
}

// TestRenderReturnsAFrozenClientToTheLiveViewWholly is render's back-to-live
// branch, reached with a copy in hand. Four things happen there and this is the
// only place any of them can be reached: the mode is reset, the claim is released,
// the client's bound and its wheel are switched back to the live grid's, and its
// live view is invalidated.
//
// A client paused at the tail holding a copy is what reaches it. The offset makes
// the branch fire; the copy is what a peer's clear leaves behind, and it is the
// interesting input because the three things the branch does are all about reading
// the grid instead of the copy. A client with no copy reaches the same branch and
// cannot tell whether two of them happened at all.
//
// The alternate screen is taken as well, because the wheel half of it is only
// visible while a program owns scrolling: a client on the live grid must be told to
// hand its wheel over, and a client on a copy must not be, so the answer changes
// with exactly the thing the branch resets.
//
// The paused baseline that resumeView drops along with the claim is not asserted
// here, and cannot be: this test reaches the branch by pausing at offset 0,
// backToLive fires on a zero offset, so this client goes live on its first render
// and never rendered a paused frame to have taken a baseline from.
// TestScrollingBackToTheLiveViewDropsTheClaim renders one before it releases, and
// asserts it there.
func TestRenderReturnsAFrozenClientToTheLiveViewWholly(t *testing.T) {
	tk, w := renderingTicker(t, 16, 3, 12)
	pauseAt(t, tk, 0)

	g := w.LockVT()
	held := g.ScrollbackLen()
	g.Advance([]byte("\x1b[3J"))     // a peer's clear: the claim is handed a copy
	g.Advance([]byte("\x1b[?1049h")) // and a program takes the screen
	frozen, live := tk.frozen(), g.ScrollbackLen()
	w.UnlockVT()
	if frozen == nil {
		t.Fatalf("setup: the clear handed the client no copy, so this test is not about one")
	}
	if frozen.HistoryLen() != held {
		t.Fatalf("setup: the copy holds %d of the %d rows the live grid had, so status[1] below cannot tell the copy's bound from the live grid's",
			frozen.HistoryLen(), held)
	}
	// Not arranged but assumed: the alternate screen carries no scrollback of its
	// own, which is what makes 0 the answer status[1] has to give.
	if live != 0 {
		t.Fatalf("setup: the live grid still offers %d lines of scrollback under the alternate screen, so status[1] below no longer distinguishes it from the copy's %d",
			live, held)
	}

	_, _, status, _ := tk.render(false)

	if tk.mode.paused || status[2] != 0 {
		t.Errorf("the client is still paused (mode %+v, status %v) with nothing left to be paused on", tk.mode, status)
	}
	if n := claimsHeldBy(w); n != 0 {
		t.Errorf("the grid still holds %d claim(s) after the client went live, so it goes on copying this window's whole history once per clear for a reader that is gone", n)
	}
	if status[1] != 0 {
		t.Errorf("the client is told it can scroll back %d lines, which is its old copy's history and not the live grid's %d", status[1], live)
	}
	if status[3] != 1 {
		t.Errorf("the client is back on the live grid while a program owns the screen and was not told to hand over its wheel (status %v), so its own scrolling acts on a grid it is not looking at", status)
	}
}

// TestScrollingBackToTheLiveViewDropsTheClaim is the other caller that leaves the
// paused state without being made to: a client that scrolls down to the tail.
// handleCtrl's own exit path, not render's — deleting its resumeView left the whole
// package green, and the claim it leaves behind is the same permanent one.
//
// It scrolls down rather than sending an explicit exit so that the release is
// decided by applyScroll returning an unpaused mode, which is the ordinary way a
// user ends a scroll-back.
//
// The paused baseline the same call drops is asserted here as well, and it takes a
// copy and a frame off it to have one: pausedSource is written in exactly one
// place, render's paused branch, and what it records there is the copy that frame
// was diffed against. Without the frame the field is nil before the release runs
// and the assertion below passes on that rather than on the release.
//
// TestAClientBackOnTheLiveViewHoldsNoCopy pins the same drop, and pins it for all
// four callers at once by calling resumeView alone; what this adds is that the
// state being dropped is the state a render left, not a state a test assembled.
func TestScrollingBackToTheLiveViewDropsTheClaim(t *testing.T) {
	tk, w := renderingTicker(t, 16, 3, 12)
	pauseAt(t, tk, 4)

	g := w.LockVT()
	g.Advance([]byte("\x1b[3J")) // a peer's clear: the claim is handed a copy
	w.UnlockVT()
	tk.render(false) // and one paused frame off it makes the copy this client's baseline
	if held := frozenFieldsHeldBy(tk); !slices.Equal(held, []string{"pausedSource"}) {
		t.Fatalf("setup: the fields holding the copy are %v, want [pausedSource]: there is no baseline here for the release below to drop", held)
	}

	tk.handleCtrl(ctrlEvent{scroll: &scrollMsg{op: "delta", lines: 4, hasLines: true}})

	if tk.mode.paused {
		t.Fatalf("the client is still paused at %+v after scrolling back down to the tail", tk.mode)
	}
	if n := claimsHeldBy(w); n != 0 {
		t.Errorf("the grid still holds %d claim(s) for a client that scrolled back to the live view, and nothing points at them to release them", n)
	}
	if held := frozenFieldsHeldBy(tk); len(held) != 0 {
		t.Errorf("ticker fields %v still point at a copy after the client went live", held)
	}
}

// TestRenderLeavesAPausedOffsetOnARowThatStillExists is the clamp render writes
// back at the end of its paused branch, and it is asserted through a later scroll
// because nothing about the frame it is written during can see it. Every reader
// bounds the offset again on the way past — the snapshot, the status tuple — so with
// the line deleted this client is sent a byte-identical frame and a byte-identical
// status tuple. What the line decides is what the stored offset means afterwards.
//
// Reaching the state takes a full history, because it is the scrollback cap that
// makes the count and the history disagree: scrollUp counts every line it pushes
// while increaseScrollLimit stops adding rows once the limit is reached, so past the
// cap the count keeps climbing over a history that cannot grow. Then one tick's gap
// holds four pushes and a rows-only grow of six. Six rows come back out of history
// and off the count; the four pushes only went onto the count. reanchor sees the net
// two, and its negative arm bounds what it subtracts by the offset rather than by
// the history that is left, so the client is left addressing a row four deeper than
// the oldest one there is.
//
// The cost is that the client cannot get back. It is told it is 9994 lines from the
// tail, scrolls down by exactly that, and applyScroll takes it to four rather than
// to zero: no unpause, the status still says paused, and every frame from then on is
// the oldest row history holds while the client believes it is somewhere above it.
func TestRenderLeavesAPausedOffsetOnARowThatStillExists(t *testing.T) {
	// A little past NewVtGrid's 10000 lines of history, so the cap is reached. The
	// guard below is what checks that, rather than the number here.
	tk, w := renderingTicker(t, 16, 3, 10_010)
	g := w.LockVT()
	sb0, p0 := g.ScrollbackLen(), g.HistoryPushed()
	w.UnlockVT()
	if p0 <= int64(sb0) {
		t.Fatalf("setup: %d lines pushed into %d of history, so the cap is not reached and the two agree", p0, sb0)
	}
	pauseAt(t, tk, uint32(sb0)) // the oldest row there is

	g = w.LockVT()
	g.Advance([]byte("\r\nx\r\nx\r\nx\r\nx")) // four lines land in the gap...
	g.Resize(16, 9)                           // ...and a peer's screen grows by six rows
	sb1, p1 := g.ScrollbackLen(), g.HistoryPushed()
	frozen := tk.frozen()
	w.UnlockVT()
	if frozen != nil {
		t.Fatalf("setup: the client holds a copy, so reanchor reads the copy's own count and the live grid's disagreement cannot reach it")
	}
	if sb0-sb1 <= int(p0-p1) {
		t.Fatalf("setup: history fell by %d and the count by %d, so the correction lands inside the history that is left and the offset stays a real position",
			sb0-sb1, p0-p1)
	}

	_, _, status, _ := tk.render(false)

	tk.handleCtrl(ctrlEvent{scroll: &scrollMsg{op: "delta", lines: int32(status[0]), hasLines: true}})

	if tk.mode.paused {
		t.Errorf("a client told it was %d lines back scrolled down %d and is still paused %d lines up (mode %+v): its stored offset was past the oldest of the %d rows history holds, and nothing it can do will reach the tail",
			status[0], status[0], tk.mode.offset, tk.mode, sb1)
	}
}

// screenText is a grid's visible screen, one string per row. It is how a client's
// screen is compared with the window's: the same reading of both, so a difference
// between them is a difference in content and not in how it was read.
func screenText(g *shelld.VtGrid, cols, rows int) []string {
	snap := g.SnapshotInto(nil, 0, cols, rows)
	out := make([]string, 0, rows)
	for r := range snap.Rows {
		var b strings.Builder
		for _, c := range snap.Cells[r*snap.Cols : (r+1)*snap.Cols] {
			if c.Ch == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Ch)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// TestAClientReturnedToTheLiveViewIsRepaintedOverItsPausedFrame follows the frames
// into a terminal of their own, because a frame is a diff and no single frame's
// bytes say what it leaves on a screen. What a client has in front of it is the sum
// of every frame it has been sent, and that is the only thing the invalidation at
// the end of render's back-to-live branch is about.
//
// LiveFramer diffs against the last live frame it sent, so after a paused frame its
// baseline describes a screen the client no longer has: it has history rows on it.
// Two things then go wrong without the invalidation, and which one depends on
// whether the grid moved while the client was paused. Unmoved, Frame's own
// early-out sends nothing at all. Moved by less than half the screen, the client is
// sent a diff — and a diff is applied to the screen the baseline describes, so the
// rows the paused frame put there that the diff does not mention are left where
// they are.
//
// This is the second: one short row of text over two blank ones, and a program
// taking the alternate screen to blank it. Five cells change, which is under the
// half-a-screen the framer repaints for, so a repaint here can only come from the
// baseline being dropped. A larger change would repaint either way and the two
// would be indistinguishable.
func TestAClientReturnedToTheLiveViewIsRepaintedOverItsPausedFrame(t *testing.T) {
	const cols, rows = 16, 3
	tk, w := renderingTicker(t, cols, rows, 12)
	g := w.LockVT()
	g.Advance([]byte("\x1b[2J\x1b[Hhello")) // the screen the client is left holding
	w.UnlockVT()

	client := shelld.NewVtGrid(cols, rows)
	send := func() { out, _, _, _ := tk.render(true); client.Advance(out) }

	send() // the live frame
	live := screenText(client, cols, rows)

	pauseAt(t, tk, 4)
	send() // the paused frame, which is history and not that screen
	held := screenText(client, cols, rows)
	if slices.Equal(held, live) {
		t.Fatalf("setup: the paused frame left the same screen as the live one (%q), so a stale baseline would describe it correctly", held)
	}

	g = w.LockVT()
	g.Advance([]byte("\x1b[?1049h")) // a program takes the screen: nothing left to be paused on
	w.UnlockVT()
	send() // back to live

	g = w.LockVT()
	want := screenText(g, cols, rows)
	w.UnlockVT()
	if got := screenText(client, cols, rows); !slices.Equal(got, want) {
		t.Errorf("the client is back on the live view showing %q, and the live screen is %q: the rows its paused frame left behind are still there, because the frame that returned it was a diff against the screen it had before it paused, %q",
			got, want, live)
	}
}
