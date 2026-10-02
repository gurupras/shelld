package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gurupras/shelld"
)

// A paused client is promised the content it is reading, not an offset. These
// tests are the two events that an offset cannot keep that promise through: a
// history clear and a reflow. Both rewrite the rows an offset addresses while
// moving the count the offset is corrected from by nothing that describes the
// rewrite, so a client following the live grid is quietly shown different text
// and nothing reports a fault.
//
// vt.Grid.HistoryPushed says as much about itself — "neither reflow nor a
// history clear maintains the relationship at all". What answers it is not
// better arithmetic but a copy: a paused client registers a claim on what it
// reads, and whichever of these events comes first hands it a private copy of
// history to read instead. Drop the claim from pausedViewer below and both tests
// fail on the wrong text, which is what they are worth.

// pausedViewer is pausedTicker plus the claim a real client registers when it
// pauses. What that claim buys is the subject of both tests: without it the
// client reads the live grid, and there is nothing to keep it on its content
// once a peer rewrites the rows under it.
func pausedViewer(g *shelld.VtGrid, offset uint32) *ticker {
	tk := pausedTicker(g, offset)
	tk.view = g.NewPausedView()
	return tk
}

// shownTo is the first row the client is actually displaying, read from whichever
// source it is reading: the live grid, or its own copy once it has been handed
// one. What a test must compare is the text the client sees, and where that text
// comes from is what the code under test decides.
//
// cols is the width to read the row at, and the caller supplies it rather than it
// being read off either source, because the grid's width is one of the things
// these tests change. A row read at a narrower width would be truncated, so the
// comparison has to happen at the width the text was captured at — which is also
// the width the client's copy was taken at.
func shownTo(tk *ticker, g *shelld.VtGrid, cols int) string {
	if f := tk.frozen(); f != nil {
		off := int(min(tk.mode.offset, uint32(f.HistoryLen())))
		return firstRow(f.SnapshotInto(nil, off, cols, 1))
	}
	return topRow(g, min(tk.mode.offset, uint32(g.ScrollbackLen())))
}

// TestBackToLiveAsksBothWaysOfHavingNothing covers the condition render returns a
// paused client on, one way at a time. Both ways send the client to the live view,
// so a test that only ever supplies both at once passes with either half deleted —
// and the halves are not interchangeable.
//
// Losing the tail case is the expensive one: a client whose offset a peer's taller
// screen pulled down to 0 keeps reporting itself paused and keeps being sent
// cursorless frames of the live tail, with nothing to end it but the user scrolling
// or resizing again. Losing the empty case is recoverable, and recoverable only
// because of the clamp render writes back at the end of its paused branch: the
// client is shown one frame of the alternate screen it is not meant to be reading,
// that clamp puts its offset at 0, and the tail case returns it on the next tick.
func TestBackToLiveAsksBothWaysOfHavingNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   clientMode
		maxOff uint32
		want   bool
	}{
		{"at the tail with history behind it", clientMode{paused: true, offset: 0}, 5, true},
		{"held in history with rows behind it", clientMode{paused: true, offset: 3}, 5, false},
		{"in a history that holds nothing", clientMode{paused: true, offset: 3}, 0, true},
		// Both ways at once, which is the second tick of the alternate-screen case:
		// the clamp has put the offset at 0 and the history is still empty. Either
		// clause alone answers this row, so it pins nothing about them — what it pins
		// is that they are combined with "or". Swap that for the third boolean
		// operator and this is the only input that notices, and such a client is
		// never returned to the live view at all.
		{"at the tail of a history that holds nothing", clientMode{paused: true, offset: 0}, 0, true},
		// Not paused, with both ways of having nothing supplied: the strongest row
		// this table can put to the paused guard, since every other clause is
		// satisfied and only that guard answers false. Drop the guard, or regroup the
		// parentheses so that the empty case escapes it, and an ordinary live client
		// on a window with no scrollback — which is every fresh window — has its mode
		// reset and its live view invalidated on every tick.
		{"not paused at all", clientMode{offset: 0}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mode.backToLive(tc.maxOff); got != tc.want {
				t.Errorf("a client %s: backToLive(%d) = %t, want %t", tc.name, tc.maxOff, got, tc.want)
			}
		})
	}
}

// frozenFieldsHeldBy names the ticker's own fields that still point at a frozen
// copy. It is reflection rather than a check on one named field because what
// matters is that nothing in a ticker holds a copy once the client is live: a
// field added later for some other reason would otherwise bring the leak back
// with nothing to say so.
func frozenFieldsHeldBy(tk *ticker) []string {
	want := reflect.TypeOf((*shelld.FrozenHistory)(nil))
	v := reflect.ValueOf(tk).Elem()
	var held []string
	for i := range v.NumField() {
		if f := v.Field(i); f.Type() == want && !f.IsNil() {
			held = append(held, v.Type().Field(i).Name)
		}
	}
	return held
}

// claimsOn is how many claims the grid still believes it has to copy history for.
//
// It reads the grid's own registry rather than the ticker's pointer at a view,
// because those are two records of one fact and only the grid's decides whether a
// copy gets made. A ticker that forgets its pointer looks released from the client
// side while the grid goes on copying for it forever, and no assertion about a
// ticker's fields can tell the difference. Reflection is what reads it: the
// registry is deliberately not exported, and reflect can take an unexported
// field's length even though it cannot take its value.
func claimsOn(g *shelld.VtGrid) int {
	return reflect.ValueOf(g).Elem().FieldByName("pausedViews").Len()
}

// claimsHeldBy is claimsOn for a test that has the window rather than the grid. It
// takes the lock and gives it back before returning, so a caller can report the
// count without holding it.
func claimsHeldBy(w *shelld.Window) int {
	g := w.LockVT()
	defer w.UnlockVT()
	return claimsOn(g)
}

// TestAResumedClientLeavesNoClaimOnTheGrid is the other half of the retention
// story, and the more expensive half to get wrong. Releasing the claim is what
// stops the grid copying; dropping the ticker's pointer at the view is what stops
// the client reading. Lose the second and the client shows stale content once. Lose
// the first and nothing can ever release that claim again — it is not reachable
// from any ticker — so every later clear or width change copies the window's whole
// history for a reader that is gone, once per pause cycle, for as long as the
// window lives.
//
// Two cycles rather than one, because a leak that grows per cycle is the thing
// being ruled out, and because the second pause is what proves the first cycle
// left nothing behind for pauseView's idempotence guard to trip over.
func TestAResumedClientLeavesNoClaimOnTheGrid(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	if claimsOn(g) == 0 {
		t.Fatalf("setup: pausing registered no claim, so this test would pass on anything")
	}
	tk.resumeView()

	tk.mode.paused = true
	tk.pauseView(g)
	tk.resumeView()

	if n := claimsOn(g); n != 0 {
		t.Errorf("the grid still holds %d claim(s) for a client that is back on the live view, so every later clear or width change copies this window's whole history for a reader that is gone", n)
	}
}

// TestAClientBackOnTheLiveViewHoldsNoCopy is a lifetime test rather than a
// rendering one. A copy is the whole of a window's history, so it is the largest
// thing a client can be holding — and a client scrolls back for a moment and then
// stays attached for days.
//
// Releasing the claim is not enough on its own. The ticker keeps its own pointer
// at the copy it last rendered from, so that a view which cannot have changed
// needs no new frame; once the claim is released and the grid has forgotten the
// copy, that pointer is the only thing left holding it. Which pointer that is, is
// asserted by type rather than by name, so that a field added later for some other
// reason cannot bring the leak back silently.
//
// The rest of the baseline is asserted by what keeping it would do rather than
// field by field. What is left on the client's screen while it is live is a live
// frame, so the next paused frame it is sent has to be a full repaint — and
// RenderDiff repaints in full only when the baseline it is given holds no cells.
// Keep a stale baseline of the same crop and a client re-pausing at the same offset
// on an unchanged grid is sent nothing at all: it sits with the live view on screen
// and paused in its status until some cell happens to change.
//
// It calls resumeView alone, which is the whole of what a caller leaving the
// paused state has to do. There are four such callers, one of them the resize
// branch of handleCtrl, which returns before reaching the end of that function, so
// what this pins is that none of them has to remember a second call — drop the invalidatePaused out of resumeView and this
// fails, which is the only place it can be asserted once for all four.
func TestAClientBackOnTheLiveViewHoldsNoCopy(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	g.Advance([]byte("\x1b[3J"))
	f := tk.frozen()
	if f == nil {
		t.Fatalf("setup: the clear handed the client no copy, so there would be nothing to drop")
	}
	// The whole of what render records after sending a frame from the copy: the
	// frame it sent, the source it came from, and that the client is believed to be
	// showing it. It is set here rather than by a render because render needs an
	// attached client and a live window, and the state it leaves behind is the whole
	// of what this is about.
	tk.lastSentPaused = f.SnapshotInto(nil, int(tk.mode.offset), 16, 3)
	tk.pausedSource, tk.pausedOK = f, true
	if held := frozenFieldsHeldBy(tk); len(held) == 0 {
		t.Fatalf("setup: the ticker holds no copy even before going live, so this test would pass on anything")
	}
	if len(tk.lastSentPaused.Cells) == 0 {
		t.Fatalf("setup: the baseline holds no cells before the client goes live, so dropping it would assert nothing")
	}

	tk.resumeView()

	if tk.frozen() != nil {
		t.Errorf("the client still has a claim registered after going live, so it goes on being copied for")
	}
	if held := frozenFieldsHeldBy(tk); len(held) != 0 {
		t.Errorf("ticker fields %v still point at the copy, keeping its %d rows of history alive for as long as the client stays attached",
			held, f.HistoryLen())
	}
	if n := len(tk.lastSentPaused.Cells); n != 0 {
		t.Errorf("the client's paused baseline still holds %d cells, so its next paused frame is a diff against a screen it no longer has: an unchanged grid sends it nothing and leaves the live view on screen", n)
	}
	if tk.pausedOK {
		t.Errorf("the client is still believed to be showing a paused frame, so render can reuse a baseline taken before it went live")
	}
}

// TestASecondPauseRegistersItsClaimAgain is a client's second scroll-back, which
// every other test here leaves out: they pause once per client, and a user scrolls
// up, comes back to the tail, and scrolls up again.
//
// pauseView registers nothing while the ticker already holds a view, which is what
// makes one pause per tick idempotent. So leaving a released view in place — a
// husk whose grid and copy are already nil — reads as "already claimed" and the
// second pause registers no claim at all. Every assertion about a first pause
// still holds, tk.frozen() is still nil where it should be, and the feature is
// simply off from the second scroll-back onwards.
func TestASecondPauseRegistersItsClaimAgain(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	tk.resumeView()

	tk.mode.paused = true
	tk.pauseView(g)
	g.Advance([]byte("\x1b[3J"))

	if tk.frozen() == nil {
		t.Errorf("the client's second pause registered no claim, so a peer's clear destroyed what it was reading")
	}
}

// TestAPausedClientKeepsItsContentAcrossAHistoryClear is the ESC[3J case. The
// clear drops the whole scrollback without touching the pushed count, so the
// correction reanchor would apply is computed from a count that no longer
// describes the rows underneath it.
//
// The output after the clear is what makes this the interesting input rather than
// the easy one. render returns a paused client to the live view when there is no
// history to be in, so a clear on its own is already handled; a clear with any
// output behind it leaves the history non-empty, that guard does not fire, and an
// offset into the live grid addresses a row of text the client has never seen.
func TestAPausedClientKeepsItsContentAcrossAHistoryClear(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	want := shownTo(tk, g, 16)
	if want == "" {
		t.Fatalf("setup: the client's top row is blank, so this test cannot follow it")
	}

	g.Advance([]byte("\x1b[3J"))
	// Enough to refill history, so that sb == 0 is not what decides this.
	g.Advance([]byte(strings.Repeat("\r\nz", 20)))
	if g.ScrollbackLen() == 0 {
		t.Fatalf("setup: history is empty, so render's bail-out would rescue this rather than the code under test")
	}
	tk.reanchor(g)

	if got := shownTo(tk, g, 16); got != want {
		t.Errorf("a clear under a paused client changed what it reads from %q to %q: its content is at %s",
			want, got, whereIs(g, want))
	}
}

// TestAPausedClientKeepsItsContentWhenOutputPrecedesAClear is the gap between a
// client's tick and the event that freezes it. Output that lands in that gap has
// already moved the client's content by the time the copy is taken, so the copy
// holds it one row further back than the client's offset says — and the correction
// for it is never going to come from the live grid, which no longer holds those
// rows at all.
//
// So the copy carries the count it was taken at, and the client applies the
// outstanding correction against that rather than against the live grid. One line
// is enough to tell the two apart, and one line is what a peer's own shell echoes
// before it runs the command that clears.
func TestAPausedClientKeepsItsContentWhenOutputPrecedesAClear(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	want := shownTo(tk, g, 16)

	g.Advance([]byte("\r\ny")) // the line that lands in the gap
	g.Advance([]byte("\x1b[3J"))
	g.Advance([]byte(strings.Repeat("\r\nz", 20)))
	tk.reanchor(g)

	if got := shownTo(tk, g, 16); got != want {
		t.Errorf("a line pushed before the clear left the client reading %q, want %q: its offset is in the live grid's frame, not its copy's",
			got, want)
	}
}

// TestAPausedClientKeepsItsContentAcrossAPeerResize is the reflow case. A width
// change rewrites history in place: rows are split or rejoined, so the row at a
// given offset is different text.
//
// The width here is changed by a peer, not by this client, which is the case the
// package doc calls out as leaving a view undisturbed. That is true of the
// per-client crop and not of the shared grid the crop reads from.
func TestAPausedClientKeepsItsContentAcrossAPeerResize(t *testing.T) {
	g := blockGrid(t, 16, 3, 12)
	tk := pausedViewer(g, 4)
	want := shownTo(tk, g, 16)
	if want == "" {
		t.Fatalf("setup: the client's top row is blank, so this test cannot follow it")
	}

	g.Resize(8, 3) // a peer's width change; every row splits into its two halves
	tk.reanchor(g)

	if got := shownTo(tk, g, 16); got != want {
		t.Errorf("a peer's resize changed what a paused client reads from %q to %q: its content is at %s",
			want, got, whereIs(g, want))
	}
}
