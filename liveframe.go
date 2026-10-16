package shelld

import "github.com/gurupras/shelld/vt"

// LiveFramer builds the messages one client of a window's live view receives:
// on each tick a frame with what changed since the client's last message, and
// once output settles the cursor it kept hidden. It holds its own model of the
// client: the screen last sent, whether the cursor is shown, and where the
// cursor is.
//
// The cursor rule: a client never sees the cursor at a position the
// application only passed through. A tick woken by output that finds the
// application's cursor somewhere other than at the previous frame, or shown
// while the client's is hidden, hides the cursor until output settles (a
// burst, ended by Settle). A tick woken by output that leaves a shown cursor
// where it was keeps showing it there, as a terminal running the application
// would. So while output flows the cursor is only ever kept where it was or
// hidden; it appears once output is quiet, or on a tick not woken by output.
//
// Every message outside a burst ends with the client's cursor on the
// application's cell, shown or hidden as the application has it: the message
// moves it there unless the model knows it already is. (A hidden cursor's
// position still matters to a client: xterm.js places IME composition there.)
// During a burst the hidden cursor stays where drawing left it, and Settle
// places it. Within one message the cursor may move while cells are drawn. A
// small frame is one write the client parses before it paints; a frame whose
// drawing exceeds SyncMin bytes is bracketed in DEC 2026, with the cursor
// hidden while drawing.
//
// Every message ends in the default pen, so a diff starts from it. The client
// must receive nothing else on the binary stream between live messages; any
// other view (a paused frame) must be followed by Invalidate.
type LiveFramer struct {
	// SyncMin is the largest drawing (cells, pen and moves, before the
	// cursor's final move and visibility) sent without the DEC 2026 bracket.
	// Zero means DefaultSyncMin.
	SyncMin int

	last, scratch *GridSnapshot
	gen           uint64
	ok            bool // last is the client's screen, captured at gen

	// The damage epoch each buffer was captured at, and the term it was
	// captured from; a buffer whose ...OK is false is copied in full.
	lastE, scratchE   uint64
	lastOK, scratchOK bool
	term              *vt.Term

	burst bool       // the cursor is kept hidden until Settle
	shown bool       // the client shows the cursor
	at    liveCursor // the client's cursor
}

// damageFrames lets Frame copy only the rows damaged since a buffer was last
// captured, instead of snapshotting the whole screen. Tests turn it off to
// compare the two.
var damageFrames = true

// DefaultSyncMin is LiveFramer.SyncMin's default.
const DefaultSyncMin = 256

var (
	syncBegin  = []byte("\x1b[?2026h")
	syncEnd    = []byte("\x1b[?2026l")
	cursorShow = []byte("\x1b[?25h")
	cursorHide = []byte("\x1b[?25l")
)

// Invalidate forgets the client: the next frame is a full repaint, and no
// burst is in progress (after a resize, or when the client returns from a
// paused view).
func (f *LiveFramer) Invalidate() {
	f.ok, f.burst = false, false
	f.lastOK, f.scratchOK = false, false
	f.at = liveCursor{}
	if f.last != nil {
		f.last.Cells = f.last.Cells[:0]
	}
}

// Bursting reports whether the cursor is being kept hidden until Settle. It
// is the tick loop's one signal to settle, and may change on any Frame, one
// that sends nothing included.
func (f *LiveFramer) Bursting() bool { return f.burst }

// Frame appends this tick's message for a cols×rows client to dst. dirty says
// output woke the tick. rendered is false when the grid and the client's size
// did not change since the last frame, and dst is then returned as is; full
// reports a repaint. A rendered frame can still be empty.
func (f *LiveFramer) Frame(dst []byte, g *VtGrid, cols, rows int, dirty bool) (out []byte, rendered, full bool) {
	if f.last == nil {
		f.last, f.scratch = &GridSnapshot{}, &GridSnapshot{}
	}
	// Once, above the guard, because the two consumers below take the pair
	// separately: SnapshotInto sizes its cell buffer on it and renderFull paints
	// at it, so resolving in one of them leaves the snapshot's geometry
	// disagreeing with the repaint's. Above the guard also keeps f.last.Cols,
	// which holds what the last snapshot was taken at, comparable with cols.
	cols, rows = resolveDeclared(cols, g.Cols()), resolveDeclared(rows, g.Lines())
	gen := g.Gen()
	if f.ok && gen == f.gen && f.last.Cols == cols && f.last.Rows == rows {
		return dst, false, false
	}
	var cur *GridSnapshot
	prev := f.last
	damaged := damageFrames && f.ok && f.term == g.term && f.lastOK && f.scratchOK &&
		f.scratch.Cols == cols && f.scratch.Rows == rows && len(f.scratch.Cells) == cols*rows &&
		prev.Cols == cols && prev.Rows == rows && len(prev.Cells) == cols*rows
	if damaged {
		cur = snapshotDamaged(f.scratch, g.term, cols, rows, f.scratchE)
	} else {
		cur = g.SnapshotInto(f.scratch, 0, cols, rows)
	}
	total := cur.Cols * cur.Rows
	var changed int
	if damaged {
		changed = countChangedSince(prev, cur, g.term, f.lastE)
	} else if f.ok && len(prev.Cells) != 0 && prev.Cols == cur.Cols && prev.Rows == cur.Rows {
		changed = CountChanged(prev, cur)
	}
	full = !f.ok || len(prev.Cells) == 0 || prev.Cols != cur.Cols || prev.Rows != cur.Rows ||
		total > 0 && changed > total/2
	if damageFrames {
		e := g.term.DamageEpoch()
		f.scratchE, f.scratchOK = f.lastE, f.lastOK && f.term == g.term
		f.lastE, f.lastOK, f.term = e, true, g.term
	}
	moved := !f.ok || cur.CursorRow != prev.CursorRow || cur.CursorCol != prev.CursorCol

	start := len(dst)
	// Reserve the bracket and a hide; dropped below when not needed.
	dst = append(dst, syncBegin...)
	dst = append(dst, cursorHide...)
	body := len(dst)
	shown, at := f.shown, f.at
	if full {
		// A repaint hides the cursor, places it, and shows it again if the
		// application shows it.
		dst = renderFull(dst, g.term, 0, cols, rows)
		shown, at = cur.CursorVisible, liveCursor{cur.CursorRow, cur.CursorCol, true}
	} else {
		dst, at = renderLiveDiff(dst, prev, cur, at)
	}
	f.last, f.scratch = cur, prev
	f.gen, f.ok = gen, true

	if dirty && (moved || cur.CursorVisible && !f.shown) {
		f.burst = true
	}
	want := cur.CursorVisible && !f.burst
	if full && shown && !want {
		dst = dst[:len(dst)-len(cursorShow)]
		shown = false
	}
	big := len(dst)-body > f.syncMin()
	switch {
	case big && !full && shown:
		shown = false // keep the reserved hide while drawing
	case big:
		dst = append(dst[:start+len(syncBegin)], dst[body:]...)
	default:
		dst = append(dst[:start], dst[body:]...)
	}
	dst = f.finish(dst, cur, want, shown, at)
	if big {
		dst = append(dst, syncEnd...)
	}
	return dst, true, full
}

// Settle ends a burst once output has been quiet. It appends the message
// that brings the client up to date: whatever changed since the last frame,
// and the cursor shown on the application's cell if the application shows it.
func (f *LiveFramer) Settle(dst []byte, g *VtGrid, cols, rows int) []byte {
	f.burst = false
	out, rendered, _ := f.Frame(dst, g, cols, rows, false)
	if rendered {
		return out
	}
	return f.finish(out, f.last, f.last.CursorVisible, f.shown, f.at)
}

// finish appends the end of a message: outside a burst, the cursor moved to
// cur's cursor cell unless the client's cursor is known to be there; then
// shown with want, hidden otherwise. shown and at are the client's state
// before it. It records the client's state after it.
func (f *LiveFramer) finish(dst []byte, cur *GridSnapshot, want, shown bool, at liveCursor) []byte {
	if !f.burst && (!at.sure || at.row != cur.CursorRow || at.col != cur.CursorCol) {
		dst = appendCUP(dst, cur.CursorRow+1, cur.CursorCol+1)
		at = liveCursor{cur.CursorRow, cur.CursorCol, true}
	}
	if want {
		if !shown {
			dst = append(dst, cursorShow...)
			shown = true
		}
	} else if shown {
		dst = append(dst, cursorHide...)
		shown = false
	}
	f.shown, f.at = shown, at
	return dst
}

func (f *LiveFramer) syncMin() int {
	if f.SyncMin == 0 {
		return DefaultSyncMin
	}
	return f.SyncMin
}
