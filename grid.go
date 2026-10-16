package shelld

import (
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/gurupras/shelld/vt"
)

// Color-table indices for the dynamic colors answered to OSC 10/11/12
// queries. OSC 4 palette queries use 0..255 directly.
const (
	IdxForeground = int(vt.Foreground)
	IdxBackground = int(vt.Background)
	IdxCursor     = int(vt.CursorColor)
)

// ColorState holds the colors reported for OSC 4/10/11/12 queries, pushed
// by the client so an embedded TUI in auto-theme mode detects the
// UI palette.
type ColorState struct {
	mu sync.Mutex
	m  map[int]vt.RGB
}

// Set records the color at a table index: 0..255 for a palette entry, or
// IdxForeground, IdxBackground or IdxCursor for a dynamic one. An index that was
// never set is not answered at all, so a program querying it sees a timeout
// rather than a wrong color.
func (c *ColorState) Set(index int, rgb vt.RGB) {
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[int]vt.RGB)
	}
	c.m[index] = rgb
	c.mu.Unlock()
}

func (c *ColorState) get(index int) (vt.RGB, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rgb, ok := c.m[index]
	return rgb, ok
}

// oscListener collects the terminal's replies, in query order, into a buffer
// the PTY read loop queues for the program after releasing the grid lock:
// color queries answered from ColorState, and everything else the VT answers:
// device attributes (DA1, DA2), status and cursor position (DSR, CPR), mode
// reports (DECRQM, which reports synchronized output, mode 2026, as supported)
// and the text area size (CSI 18 t). shelld-rs answers the same set, which
// the differential test compares.
type oscListener struct {
	colors *ColorState
	out    []byte
}

func (l *oscListener) PtyWrite(reply string) {
	l.out = append(l.out, reply...)
}

func (l *oscListener) ColorRequest(index int, format func(vt.RGB) string) {
	if rgb, ok := l.colors.get(index); ok {
		l.out = append(l.out, format(rgb)...)
	}
}

// VtGrid is a terminal emulator plus the ANSI repaint/diff emitters. It is
// not safe for concurrent use; Window serializes access.
type VtGrid struct {
	term     *vt.Term
	parser   *vt.Processor
	colors   *ColorState
	listener *oscListener
	// gen increments on every mutation, letting renderers skip rebuilding a
	// snapshot of an unchanged grid.
	gen uint64
	// pausedViews are the clients reading a position in history rather than the
	// live tail. While there are none nothing is ever copied; see PausedView.
	pausedViews []*PausedView
}

func clampDim(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// resolveDeclared reconciles one dimension of a client's declared viewport
// against the grid it will be drawn from. Zero means "no preference", which is
// all a client that has not measured itself can say; above the grid's own size
// it is asking for cells that do not exist. Both resolve to the grid, and a
// declaration below it is a real crop and is kept.
//
// Resolving the upper end is not a policy about oversized clients. snapshotInto
// sizes its cell buffer on the declaration and fills everything past the grid
// with the constant padSlot, and that padding reaches no client: renderFull
// paints from the term, and a diff compares the same constant on both sides.
// Measured on an 80x24 term over 50 changing frames, a client declaring
// 1024x1024 (which MaxCols/MaxRows admit) produced byte-identical output to one
// declaring 80x24 -- a 93 B repaint then 56 B of diffs, both legs -- for 1.564 ms
// per frame against 3.887 us and 32.0 MiB against 0.1 MiB, all of it padding
// written and then compared against itself.
//
// Resolve before comparing a declared size against a snapshot's stored
// geometry. Two guards do that comparison -- LiveFramer's generation check and
// the paused path's unchanged check -- and a resolved snapshot never matches a
// raw declaration, so resolving below either one disables it.
func resolveDeclared(declared, avail int) int {
	if declared <= 0 || declared > avail {
		return avail
	}
	return declared
}

// ResolveDeclared reconciles a client's declared viewport against the grid it
// will be drawn from, for a caller outside this package that holds the same VT
// lock it read the grid under. See resolveDeclared.
func ResolveDeclared(g *VtGrid, cols, rows int) (int, int) {
	return resolveDeclared(cols, g.Cols()), resolveDeclared(rows, g.Lines())
}

// NewVtGrid creates a grid with 10000 lines of history. Zero dimensions
// default to 80×24.
func NewVtGrid(cols, rows int) *VtGrid {
	colors := &ColorState{}
	l := &oscListener{colors: colors}
	g := &VtGrid{parser: vt.NewProcessor(), colors: colors, listener: l}
	cfg := vt.Config{
		ScrollingHistory: 10_000,
		// A wholesale drop of history is one of the two events a client reading
		// history cannot be followed through; the other is the reflow in Resize.
		BeforeHistoryClear: g.freezePausedViews,
	}
	g.term = vt.NewTerm(cfg, clampDim(cols, 80), clampDim(rows, 24), l)
	return g
}

// Colors is the color table used for OSC query replies.
func (g *VtGrid) Colors() *ColorState { return g.colors }

// Term exposes the emulator.
func (g *VtGrid) Term() *vt.Term { return g.term }

// Gen is the mutation counter.
func (g *VtGrid) Gen() uint64 { return g.gen }

// Advance feeds PTY bytes through the parser.
func (g *VtGrid) Advance(b []byte) {
	g.parser.Advance(g.term, b)
	g.gen++
}

// EndExpiredSync applies what a synchronized update buffered once its
// deadline (vt.SyncUpdateTimeout) has passed, as the program ending the
// update would have, and reports whether it did.
func (g *VtGrid) EndExpiredSync() bool {
	if d, ok := g.parser.SyncDeadline(); !ok || time.Now().Before(d) {
		return false
	}
	return g.EndSync()
}

// EndSync applies what a pending synchronized update buffered, deadline or
// not, and reports whether one was pending. For EOF: nothing more will be
// read to end the update.
func (g *VtGrid) EndSync() bool {
	if !g.parser.SyncPending() {
		return false
	}
	g.parser.StopSync(g.term)
	g.gen++
	return true
}

// SyncDeadline reports when the pending synchronized update, if any, is due
// to be ended by EndExpiredSync.
func (g *VtGrid) SyncDeadline() (time.Time, bool) { return g.parser.SyncDeadline() }

// TakeReplies returns and clears the replies to terminal queries.
func (g *VtGrid) TakeReplies() []byte {
	if len(g.listener.out) == 0 {
		return nil
	}
	out := g.listener.out
	g.listener.out = nil
	return out
}

// Resize changes the terminal's size, zero dimensions defaulting to 80x24 as in
// NewVtGrid. Columns reflow: a line wrapped by running past the old right edge is
// rejoined and re-wrapped at the new one.
//
// A shrink in rows is not a drop of the bottom rows. Each screen keeps a window
// of the new height, placed so the cursor's row is still on it; the screens differ
// in where that window sits and in what becomes of the rows outside it.
//
// On the primary screen the window slides down, and the rows it leaves above it
// go to history. That is what an inline TUI assumes when it repaints after
// SIGWINCH: it keeps status rows below its cursor and expects the output above the
// new screen to be in scrollback. Two things bound how far the window slides, and
// neither of them is where the content ends. Only the rows above the cursor can go
// to history. And what goes from the bottom — discarded, not stored — is the blank
// tail below the cursor less one row, or as much as the cursor's own row forces,
// whichever is the larger, and never more rows than the shrink removes.
//
// So a full 10-row screen shrunk to 5 loses five rows and stores none with the
// cursor on row 1, and stores five and loses none with the cursor on row 10 —
// and a screen whose text ends at row 4, shrunk with the cursor on row 8, puts
// all four rows of text in history and leaves five blank rows on screen. See
// vt.Term.KeepRowsOnShrink for the bound and both of its ends.
//
// The alternate screen has no history to push into and its programs repaint in
// full, so the rows outside the window are destroyed rather than stored, and the
// window keeps the top rows, sliding down only when the cursor would otherwise
// fall off its bottom. vt.Term.KeepRowsOnShrink, which this calls first, is where
// the alternate screen is excluded, and says where the primary screen's rule goes
// further than tmux, alacritty and xterm.js do, and why it has to.
//
// A client reading a position in history is handed a copy of it before a width
// change, and only before a width change: reflow rewrites the history rows an
// offset addresses, splitting and rejoining them, and moves HistoryPushed by
// nothing that describes the rewrite. A change in rows alone moves whole rows
// between the screen and history, which is exactly what that count reports, so
// such a client can still be followed through it. See PausedView.
func (g *VtGrid) Resize(cols, rows int) {
	cols, rows = clampDim(cols, 80), clampDim(rows, 24)
	// Against the width the terminal will actually take, not the one asked for,
	// so a request below MinColumns twice running is not a second freeze.
	if max(cols, vt.MinColumns) != g.term.Columns() {
		g.freezePausedViews()
	}
	g.term.KeepRowsOnShrink(rows)
	g.term.Resize(cols, rows)
	g.gen++
}

// Cols is the terminal's width in columns.
func (g *VtGrid) Cols() int { return g.term.Columns() }

// Lines is the viewport's height in rows, not counting history.
func (g *VtGrid) Lines() int { return g.term.ScreenLines() }

// ScrollbackLen is the number of history lines above the viewport, on whichever
// screen is active — so it reads 0 while AltScreenActive, which see.
func (g *VtGrid) ScrollbackLen() int { return g.term.HistorySize() }

// HistoryPushed counts lines pushed into the primary screen's history, whichever
// screen is active, so it is the one history figure that survives a switch to the
// alternate screen.
func (g *VtGrid) HistoryPushed() int64 { return g.term.HistoryPushed() }

// AltScreenActive reports DECSET 1049: the program is drawing on the alternate
// screen, which has no history of its own and which such a program repaints in
// full.
//
// The two accessors above answer differently while it is active, and the
// difference is worth knowing. ScrollbackLen reads the active screen and so reads
// 0, which means clampOffset clamps every history offset to 0 and SnapshotAt and
// RenderDiffAt hand back the live alternate screen rather than the history the
// caller asked for, with nothing in the result to say so. A client that offers to
// scroll back should check this first.
func (g *VtGrid) AltScreenActive() bool { return g.term.Mode()&vt.ModeAltScreen != 0 }

// MouseReporting reports whether the program asked for mouse events at all, by
// any of the reporting modes. A client that forwards clicks should check this
// first, then SgrMouse for which encoding to use.
func (g *VtGrid) MouseReporting() bool { return g.term.Mode()&vt.ModeMouse != 0 }

// SgrMouse reports DECSET 1006: mouse events are encoded as SGR, which unlike
// the default X10 encoding can carry a coordinate past column 223.
//
// It is not the only such encoding. The terminal also tracks DECSET 1005, UTF-8
// mouse coordinates, which VtGrid does not expose — so a caller that checks
// MouseReporting and then SgrMouse sends X10 coordinates to a program that asked
// for 1005, and that program reads a click past column 95 at the wrong place: a
// 1005 coordinate is the same single byte as X10 up to there and a multi-byte
// sequence beyond it.
//
// This daemon is one of those callers, with a bound of its own. Its ticker reads
// this in handleCtrl (internal/daemon/attach.go:749) and hands it to wheelInput as
// the sgr argument, which falls back to X10 for anything else — but wheelInput
// reports at the grid centre, so a wheel notch is misplaced only on a client at
// least 192 columns or 192 rows across. Programs that ask for 1005 and not 1006
// are rare, which is why the gap has been left rather than closed; see
// vt.ModeUTF8Mouse if it needs closing.
func (g *VtGrid) SgrMouse() bool { return g.term.Mode()&vt.ModeSGRMouse != 0 }

// AlternateScroll reports DECSET 1007: the wheel on the alternate screen
// scrolls by sending arrow keys when mouse reporting is off.
func (g *VtGrid) AlternateScroll() bool { return g.term.Mode()&vt.ModeAlternateScroll != 0 }

// AppCursor reports DECCKM: arrow keys use SS3 (ESC O) rather than CSI.
func (g *VtGrid) AppCursor() bool { return g.term.Mode()&vt.ModeAppCursor != 0 }

// AppKeypad reports DECKPAM: keypad keys use SS3 rather than their characters.
func (g *VtGrid) AppKeypad() bool { return g.term.Mode()&vt.ModeAppKeypad != 0 }

// BracketedPaste reports DECSET 2004: the program wants pastes bracketed.
func (g *VtGrid) BracketedPaste() bool { return g.term.Mode()&vt.ModeBracketedPaste != 0 }

func (g *VtGrid) clampOffset(offset int) int { return max(min(offset, g.ScrollbackLen()), 0) }

// RenderFrame emits a self-contained ANSI repaint of the live viewport.
func (g *VtGrid) RenderFrame() []byte {
	return renderFull(nil, g.term, 0, g.Cols(), g.Lines())
}

// RenderFrameAt renders the view offset lines into history (clamped); the
// cursor is hidden for offset > 0.
func (g *VtGrid) RenderFrameAt(offset int) []byte {
	return renderFull(nil, g.term, g.clampOffset(offset), g.Cols(), g.Lines())
}

// RenderFrameCropped renders the live viewport at a declared size, padding
// or cropping from the top-left.
func (g *VtGrid) RenderFrameCropped(cols, rows int) []byte {
	return renderFull(nil, g.term, 0, cols, rows)
}

// Snapshot captures the live viewport as cells, for comparison or for use as a
// diff baseline. It allocates; SnapshotInto reuses a buffer.
func (g *VtGrid) Snapshot() *GridSnapshot {
	return snapshotInto(nil, g.term, 0, g.Cols(), g.Lines())
}

// SnapshotAt captures the view offset lines into history, clamped to the history
// that exists.
func (g *VtGrid) SnapshotAt(offset int) *GridSnapshot {
	return snapshotInto(nil, g.term, g.clampOffset(offset), g.Cols(), g.Lines())
}

// SnapshotCropped captures the live viewport at a declared size, padding or
// cropping from the top-left, which is what a client smaller than the terminal
// sees.
func (g *VtGrid) SnapshotCropped(cols, rows int) *GridSnapshot {
	return snapshotInto(nil, g.term, 0, cols, rows)
}

// RenderDiff returns the ANSI that brings a viewer holding prev to the
// current live viewport, and the new baseline snapshot.
//
// The returned snapshot is the baseline for the next call, and must be passed
// back: a caller that keeps its old one emits a diff against a state the viewer
// left behind.
//
// prev must not be nil; EmptySnapshot is how a caller with no baseline spells
// one, and it is what a viewer holding nothing yet gets a full repaint from. A
// repaint is also what a geometry change produces, since a diff between two
// different geometries is not defined, so a caller need not special-case a
// resize: passing the snapshot from before it is enough.
func (g *VtGrid) RenderDiff(prev *GridSnapshot) ([]byte, *GridSnapshot) {
	return g.RenderDiffAtCropped(prev, 0, g.Cols(), g.Lines())
}

// RenderDiffAt is RenderDiff for the view offset lines into history.
func (g *VtGrid) RenderDiffAt(prev *GridSnapshot, offset int) ([]byte, *GridSnapshot) {
	return g.RenderDiffAtCropped(prev, offset, g.Cols(), g.Lines())
}

// RenderDiffCropped is RenderDiff at a client's declared size.
func (g *VtGrid) RenderDiffCropped(prev *GridSnapshot, cols, rows int) ([]byte, *GridSnapshot) {
	return g.RenderDiffAtCropped(prev, 0, cols, rows)
}

// RenderDiffAtCropped is the general form the other three call: a diff against
// prev for the view offset lines into history, at a declared size. offset is
// clamped to the history that exists.
func (g *VtGrid) RenderDiffAtCropped(prev *GridSnapshot, offset, cols, rows int) ([]byte, *GridSnapshot) {
	offset = g.clampOffset(offset)
	cur := snapshotInto(nil, g.term, offset, cols, rows)
	return renderDiff(nil, prev, cur, offset == 0, func(out []byte) []byte {
		return renderFull(out, g.term, offset, cols, rows)
	}), cur
}

// SnapshotInto captures the grid at offset and declared size into dst
// (reused when large enough).
func (g *VtGrid) SnapshotInto(dst *GridSnapshot, offset, cols, rows int) *GridSnapshot {
	return snapshotInto(dst, g.term, g.clampOffset(offset), cols, rows)
}

// RenderFullInto appends a self-contained repaint at offset and declared
// size to out.
func (g *VtGrid) RenderFullInto(out []byte, offset, cols, rows int) []byte {
	return renderFull(out, g.term, g.clampOffset(offset), cols, rows)
}

// RenderDiffSnapshots appends the diff from prev to cur, where cur was just
// captured from this grid at offset.
func (g *VtGrid) RenderDiffSnapshots(out []byte, prev, cur *GridSnapshot, offset int) []byte {
	offset = g.clampOffset(offset)
	return renderDiff(out, prev, cur, offset == 0, func(out []byte) []byte {
		return renderFull(out, g.term, offset, cur.Cols, cur.Rows)
	})
}

// CellSlot is one cell of a snapshot: glyph, SGR-relevant style, and wide
// markers. It has no padding, so snapshots compare with memcmp.
type CellSlot struct {
	// Ch is the character drawn in the cell. A blank cell inside the terminal
	// holds a space: slotFromCell normalises the grid's unwritten zero rune, so
	// two snapshots of one screen compare equal whatever path each cell took.
	// Outside the terminal — the padding a client that declared more columns or
	// rows than the terminal has gets — Ch is the zero rune, which the emitters
	// draw as a space like any other control character. So a caller comparing
	// Ch itself should read a zero as a blank too.
	Ch rune
	// Fg and Bg are the cell's colors, as the program named them: a role such as
	// vt.Foreground stays a role, and is not resolved to an RGB value here.
	Fg vt.Color
	Bg vt.Color
	// Attrs is the style bits, at vt.Flags' bit positions. Test it with the
	// attr* constants; the five underline styles all appear as attrUnderline,
	// since the diff emitter draws one underline. The top 16 bits are not
	// style: they are the id of the zero-width characters drawn after Ch (see
	// GridSnapshot.Marks), there so that a change to a cell's marks alone is a
	// change to the slot.
	Attrs uint32
}

// Attribute bits take vt.Flags' bit positions (which are alacritty's), so a
// cell's flags become a slot's attributes with one mask; the other underline
// styles fold into attrUnderline.
const (
	attrBold       = uint32(vt.FlagBold)
	attrDim        = uint32(vt.FlagDim)
	attrItalic     = uint32(vt.FlagItalic)
	attrUnderline  = uint32(vt.FlagUnderline)
	attrInverse    = uint32(vt.FlagInverse)
	attrStrike     = uint32(vt.FlagStrikeout)
	attrHidden     = uint32(vt.FlagHidden)
	attrWideSpacer = uint32(vt.FlagWideCharSpacer)
	attrWide       = uint32(vt.FlagWideChar)

	// attrMarks holds the cell's vt mark id, which vt.Flags' 16 bits leave
	// room for.
	attrMarkShift = 16
	attrMarks     = uint32(0xffff) << attrMarkShift
)

const (
	styleMask = attrBold | attrDim | attrItalic | attrUnderline | attrInverse | attrStrike | attrHidden
	// slotFlags are the flags a slot keeps as they are.
	slotFlags = vt.Flags(styleMask | attrWideSpacer | attrWide)
	// otherUnderlines are the underline styles a slot records as attrUnderline.
	otherUnderlines = vt.AllUnderlines &^ vt.FlagUnderline
)

// padSlot fills snapshot cells outside the grid (a declared size larger
// than the grid).
var padSlot = CellSlot{Fg: vt.NewNamedColor(vt.Foreground), Bg: vt.NewNamedColor(vt.Background)}

// blankSlot is a default cell's slot. A history row stores only its content,
// and the columns past it are default cells, which is not padSlot: a default
// cell's character is a space.
var blankSlot = slotFromCell(&vt.Cell{})

// rowSlots converts a row cropped to len(dst) columns into dst, filling the
// columns the row does not store with blankSlot, and returns the OR of the
// stored cells' attributes. scratch holds the cells of a packed history row,
// decoded (vt.Row.View), and is reused across calls.
func rowSlots(dst []CellSlot, row *vt.Row, scratch *[]vt.Cell) uint32 {
	cells := row.View(scratch)
	cells = cells[:min(len(cells), len(dst))]
	attrs := slotsFromCells(dst, cells)
	for i := len(cells); i < len(dst); i++ {
		dst[i] = blankSlot
	}
	return attrs
}

// WideSpacer reports whether the slot is the right half of a double-width
// character, which has no glyph of its own.
func (s CellSlot) WideSpacer() bool { return s.Attrs&attrWideSpacer != 0 }

// Wide reports whether the slot holds a double-width character, so it advances
// the client's cursor by two columns rather than one.
func (s CellSlot) Wide() bool { return s.Attrs&attrWide != 0 }

// A client draws a wide character only together with its spacer. Sent alone,
// a wide character in the client's last column wraps onto the next row, and
// elsewhere writes a spacer the snapshot does not record; a spacer alone is
// never sent, leaving whatever the client had there. Either way the client
// drifts from the diff baseline for good. So snapshots and full repaints blank
// the half of a wide character that lost its partner: to the crop, to a
// resize that cut the row, or to an erase or deletion that stopped between the
// halves. The blank keeps the wide character's style.

const wideFlags = vt.FlagWideChar | vt.FlagWideCharSpacer

// pairWide applies that rule to a snapshot row.
func pairWide(row []CellSlot) {
	for c := range row {
		switch {
		case row[c].Wide() && (c+1 == len(row) || !row[c+1].WideSpacer()):
			// The marks went with the glyph that is not being drawn: a
			// combining mark applies to whatever precedes it, so kept they
			// would decorate the blank.
			row[c].Ch = ' '
			row[c].Attrs &^= attrWide | attrMarks
		case row[c].WideSpacer() && (c == 0 || !row[c-1].Wide()):
			row[c].Attrs &^= attrWideSpacer
		}
	}
}

// wideHalf applies the rule to cell c of a row cropped to cols: skip is a
// spacer that follows its wide character (drawn with it), blank is a wide
// character without its spacer. cells are the row's stored cells; a column
// past them is a default cell, which is neither half.
func wideHalf(cells []vt.Cell, c, cols int) (skip, blank bool) {
	switch cells[c].Flags() & wideFlags {
	case vt.FlagWideCharSpacer:
		return c > 0 && cells[c-1].Flags()&vt.FlagWideChar != 0, false
	case vt.FlagWideChar:
		return false, c+1 == cols || c+1 >= len(cells) || cells[c+1].Flags()&vt.FlagWideCharSpacer == 0
	}
	return false, false
}

func (s CellSlot) sameStyle(o CellSlot) bool {
	return s.Fg == o.Fg && s.Bg == o.Bg && s.Attrs&styleMask == o.Attrs&styleMask
}

func slotFromCell(c *vt.Cell) CellSlot {
	f := c.Flags()
	a := uint32(f & slotFlags)
	if f&otherUnderlines != 0 {
		a |= attrUnderline
	}
	a |= uint32(c.MarkID()) << attrMarkShift
	ch := c.Rune()
	if ch == 0 {
		ch = ' '
	}
	return CellSlot{Ch: ch, Fg: c.Fg(), Bg: c.Bg(), Attrs: a}
}

// slotsFromCells converts a row's cells into dst, which has the same length,
// and returns the OR of the slots' attributes. Kept out of line; see
// countChangedCells.
//
//go:noinline
func slotsFromCells(dst []CellSlot, cells []vt.Cell) (attrs uint32) {
	dst = dst[:len(cells)]
	for i := range cells {
		s := slotFromCell(&cells[i])
		attrs |= s.Attrs
		dst[i] = s
	}
	return attrs
}

// GridSnapshot is the grid as a viewer last saw it: the baseline a diff is
// computed against, and the only state a caller has to keep per viewer.
type GridSnapshot struct {
	// Cols and Rows are the declared size the snapshot was taken at, which is
	// the client's size and not necessarily the terminal's.
	//
	// A diff is only defined between two snapshots of one geometry, and the two
	// consumers enforce that to different depths. RenderDiff compares these
	// fields and falls back to a full repaint when they differ, so it is always
	// safe. CountChanged compares only cell counts, so it catches 80x24 against
	// 80x25 with a panic but reads 80x24 against 40x48 as one geometry and
	// returns a count computed against misaligned rows. A caller that resizes
	// must pass a fresh baseline, not rely on being told.
	Cols, Rows int
	// Cells is Rows rows of Cols cells, row-major. Its length is exactly
	// Cols*Rows.
	Cells []CellSlot
	// CursorRow and CursorCol are zero-based and clamped into the declared size,
	// so a cursor past a client's right edge reads as sitting on it. They are
	// 0, 0 for a snapshot taken with a history offset, where there is no live
	// cursor to place.
	CursorRow int
	CursorCol int
	// CursorVisible is DECTCEM, forced false for a snapshot taken with a history
	// offset: a viewer scrolled back is not looking at where typing would land.
	CursorVisible bool
	// marks resolves the slots' mark ids: the term's table as it stood when the
	// snapshot was taken (see vt.Term.MarkTable).
	marks [][]rune
}

// Marks returns the zero-width characters (combining marks, variation
// selectors, ZWJs) that complete the glyph in slot s of this snapshot, drawn
// immediately after s.Ch; nil for none. The slice must not be modified.
func (g *GridSnapshot) Marks(s CellSlot) []rune {
	id := s.Attrs >> attrMarkShift
	if id == 0 || int(id) >= len(g.marks) {
		return nil
	}
	return g.marks[id]
}

// EmptySnapshot is the "nothing sent yet" baseline; a diff against it is a
// full repaint.
func EmptySnapshot() *GridSnapshot { return &GridSnapshot{CursorVisible: true} }

// CountChanged returns how many cells differ between two snapshots of the
// same geometry. It panics if their cell counts differ.
//
// Equal cell counts are not equal geometry: 40x48 and 80x24 both hold 1920
// cells, so a caller that resized between the two snapshots gets a count
// computed against misaligned rows rather than a panic. See GridSnapshot.Cols.
//
// The only non-test caller in this package, LiveFramer.Frame, is safe from the
// panic and the misalignment both, by earlier terms of the one || chain that ends
// in this call (liveframe.go:87-88). !f.ok covers the panic on its own: Invalidate
// clears that flag and empties the cells in the same call, and the only place it
// becomes true stores a filled snapshot alongside it, so an empty prev implies
// !f.ok. The len(prev.Cells) == 0 beside it is defence in depth. What the
// misalignment needs is its own term, prev.Cols != cur.Cols || prev.Rows !=
// cur.Rows. A caller writing that test differently has to make both checks itself.
func CountChanged(prev, cur *GridSnapshot) int {
	a, b := prev.Cells, cur.Cells
	if len(a) != len(b) {
		panic("shelld: CountChanged on snapshots of different sizes")
	}
	// A tick usually changes a few rows; equal rows cost one comparison.
	cols := max(cur.Cols, 1)
	n := 0
	for len(a) > 0 {
		k := min(cols, len(a))
		if !slotsEqual(a[:k], b[:k]) {
			n += countChangedCells(a[:k], b[:k])
		}
		a, b = a[k:], b[k:]
	}
	return n
}

// slotDiff is zero exactly when two slots are equal. Folding the four field
// comparisons into one value leaves a single branch per cell in the loops
// that use it.
func slotDiff(x, y *CellSlot) uint32 {
	return uint32(x.Ch^y.Ch) | uint32(x.Fg^y.Fg) | uint32(x.Bg^y.Bg) | (x.Attrs ^ y.Attrs)
}

// countChangedCells counts differing slots.
//
// The per-cell loops of snapshots and diffs (countChangedCells,
// slotsFromCells, skipUnchanged) are kept out of line. Over local slices in
// a frame of their own the compiler keeps the loop state in registers and
// drops the per-cell bounds checks, except skipUnchanged's on cur[c] (c is a
// signed parameter); inlined into a large caller, the loop index and
// temporaries spill to the stack every iteration. With go1.26 the directives
// measure as no-ops: slotsFromCells and skipUnchanged exceed the inlining
// budget anyway, and countChangedCells, which would inline into
// CountChanged, runs as fast either way. They guard against a larger budget
// or PGO pulling the loops back in.
//
//go:noinline
func countChangedCells(a, b []CellSlot) int {
	b = b[:len(a)]
	n := 0
	for i := range a {
		if slotDiff(&a[i], &b[i]) != 0 {
			n++
		}
	}
	return n
}

func slotsEqual(a, b []CellSlot) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	const size = int(unsafe.Sizeof(CellSlot{}))
	ab := unsafe.Slice((*byte)(unsafe.Pointer(&a[0])), len(a)*size)
	bb := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), len(b)*size)
	return string(ab) == string(bb)
}

// snapshotDamaged is snapshotInto at offset 0 for a dst that already holds
// this term's live view at the same size as of damage epoch since: only the
// rows the term reports changed after since are copied again.
func snapshotDamaged(dst *GridSnapshot, term *vt.Term, outCols, outRows int, since uint64) *GridSnapshot {
	dst.marks = term.MarkTable()
	grid := term.Grid()
	copyRows := min(outRows, grid.ScreenLines())
	copyCols := min(outCols, grid.Columns())
	var scratch []vt.Cell
	for r := 0; r < copyRows; r++ {
		if !term.LineChangedSince(r, since) {
			continue
		}
		row := dst.Cells[r*outCols : r*outCols+copyCols]
		if rowSlots(row, grid.Row(r), &scratch)&(attrWide|attrWideSpacer) != 0 {
			pairWide(row)
		}
	}
	cur := grid.Cursor.Point
	dst.CursorRow = min(max(cur.Line, 0), max(outRows-1, 0))
	dst.CursorCol = min(cur.Column, max(outCols-1, 0))
	dst.CursorVisible = term.Mode()&vt.ModeShowCursor != 0
	return dst
}

// countChangedSince is CountChanged over only the rows term reports changed
// after since, prev being the snapshot taken at since: the others are equal.
func countChangedSince(prev, cur *GridSnapshot, term *vt.Term, since uint64) int {
	cols := max(cur.Cols, 1)
	n := 0
	for r := 0; r < cur.Rows; r++ {
		if !term.LineChangedSince(r, since) {
			continue
		}
		a, b := prev.Cells[r*cols:(r+1)*cols], cur.Cells[r*cols:(r+1)*cols]
		if !slotsEqual(a, b) {
			n += countChangedCells(a, b)
		}
	}
	return n
}

// snapshotInto captures the term at a declared size. offset 0 is the live
// view (cursor from the term); offset > 0 is a paused history view with the
// cursor hidden at the origin. dst is reused when non-nil.
func snapshotInto(dst *GridSnapshot, term *vt.Term, offset, outCols, outRows int) *GridSnapshot {
	if dst == nil {
		dst = &GridSnapshot{}
	}
	n := outCols * outRows
	if cap(dst.Cells) >= n {
		dst.Cells = dst.Cells[:n]
	} else {
		dst.Cells = make([]CellSlot, n)
	}
	dst.Cols, dst.Rows = outCols, outRows
	dst.marks = term.MarkTable()

	grid := term.Grid()
	copyRows := min(outRows, grid.ScreenLines())
	copyCols := min(outCols, grid.Columns())
	var scratch []vt.Cell
	for r := 0; r < outRows; r++ {
		out := dst.Cells[r*outCols : (r+1)*outCols]
		if r < copyRows {
			// Pairing runs on the row cropped to the client's width, and only
			// for rows holding a wide-character half.
			row := out[:copyCols]
			if rowSlots(row, grid.Row(r-offset), &scratch)&(attrWide|attrWideSpacer) != 0 {
				pairWide(row)
			}
			out = out[copyCols:]
		}
		for i := range out {
			out[i] = padSlot
		}
	}

	if offset > 0 {
		dst.CursorRow, dst.CursorCol, dst.CursorVisible = 0, 0, false
		return dst
	}
	cur := grid.Cursor.Point
	dst.CursorRow = min(max(cur.Line, 0), max(outRows-1, 0))
	dst.CursorCol = min(cur.Column, max(outCols-1, 0))
	dst.CursorVisible = term.Mode()&vt.ModeShowCursor != 0
	return dst
}

func isBlank(c *vt.Cell) bool {
	ch := c.Rune()
	// A space carrying a mark draws something, so it ends a row's content.
	return (ch == ' ' || ch == 0) && c.MarkID() == 0 &&
		c.Bg() == vt.NewNamedColor(vt.Background) &&
		c.UnderlineColor() == vt.NoColor &&
		c.Flags()&(vt.FlagInverse|vt.AllUnderlines|vt.FlagStrikeout) == 0
}

func appendCUP(out []byte, row, col int) []byte {
	out = append(out, "\x1b["...)
	out = strconv.AppendInt(out, int64(row), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(col), 10)
	return append(out, 'H')
}

// renderFull emits a self-contained repaint at a declared size. Live
// (offset 0) frames end with the cursor placed and shown; paused frames park
// the cursor at the origin and leave it hidden.
func renderFull(out []byte, term *vt.Term, offset, outCols, outRows int) []byte {
	grid := term.Grid()
	copyRows := min(outRows, grid.ScreenLines())
	copyCols := min(outCols, grid.Columns())

	out = append(out, "\x1b[?25l\x1b[2J\x1b[H\x1b[0m\x1b[0m"...)
	cur := padSlot // default style
	var scratch []vt.Cell
	for r := 0; r < copyRows; r++ {
		if r > 0 {
			out = append(out, "\r\n"...)
		}
		cells := grid.Row(r - offset).View(&scratch)
		last := -1
		// Columns a history row does not store are default cells, which are blank.
		for c := 0; c < min(copyCols, len(cells)); c++ {
			if !isBlank(&cells[c]) {
				last = c
			}
		}
		for c := 0; c <= last; c++ {
			cell := &cells[c]
			slot := slotFromCell(cell)
			if cell.Flags()&wideFlags != 0 {
				skip, blank := wideHalf(cells, c, copyCols)
				if skip {
					continue
				}
				if blank {
					slot.Ch = ' '
					slot.Attrs &^= attrMarks
				}
			}
			if !slot.sameStyle(cur) {
				out = appendSGR(out, slot)
				cur = slot
			}
			var marks []rune
			if slot.Attrs&attrMarks != 0 {
				marks = term.Marks(cell.MarkID())
			}
			out = appendGlyph(out, slot.Ch, marks)
		}
	}
	out = append(out, "\x1b[0m"...)

	if offset > 0 {
		return append(out, "\x1b[1;1H"...)
	}
	p := grid.Cursor.Point
	row := min(max(p.Line, 0), max(outRows-1, 0))
	col := min(p.Column, max(outCols-1, 0))
	out = appendCUP(out, row+1, col+1)
	// The frame began by hiding the cursor; show it again only if the
	// application has not hidden it, matching what a snapshot records.
	if term.Mode()&vt.ModeShowCursor != 0 {
		out = append(out, "\x1b[?25h"...)
	}
	return out
}

// skipUnchanged returns the first column at or after c whose cell renderDiff
// emits: a changed cell that is not a wide-character spacer. A spacer or an
// unchanged narrow cell advances one column, an unchanged wide cell two.
// Kept out of line; see countChangedCells.
//
//go:noinline
func skipUnchanged(cur, prev []CellSlot, c int) int {
	prev = prev[:len(cur)]
	for c < len(cur) {
		x := &cur[c]
		diff := slotDiff(x, &prev[c])
		if diff|x.Attrs&(attrWide|attrWideSpacer) == 0 {
			c++ // an unchanged narrow cell, the common case: one branch
			continue
		}
		switch {
		case x.WideSpacer():
			c++
		case diff != 0:
			return c
		default: // an unchanged wide cell
			c += 2
		}
	}
	return c
}

// renderDiff emits the ANSI that turns prev into cur. A geometry change has no
// diff to emit and falls back to full, which appends a self-contained repaint of
// the same view. Output is empty when nothing changed; no clear-screen is ever
// emitted.
//
// live says cur is the tail of the grid rather than a position in history, which
// is what decides whether the cursor is part of the frame. full is a parameter
// because where a repaint reads its cells is the one thing that differs between a
// view of a live grid and a view of a frozen copy of one; see
// FrozenHistory.RenderDiff.
func renderDiff(out []byte, prev, cur *GridSnapshot, live bool, full func([]byte) []byte) []byte {
	if prev.Cols != cur.Cols || prev.Rows != cur.Rows || len(prev.Cells) == 0 {
		return full(out)
	}
	cellsEqual := slotsEqual(prev.Cells, cur.Cells)
	if cellsEqual && prev.CursorVisible == cur.CursorVisible &&
		(!live || (prev.CursorRow == cur.CursorRow && prev.CursorCol == cur.CursorCol)) {
		return out
	}

	start := len(out)
	if live {
		// Reserve room for a possible cursor-hide prefix; filled in below.
		out = append(out, "\x1b[?25l"...)
	}
	bodyStart := len(out)

	cols, rows := cur.Cols, cur.Rows
	var style CellSlot
	styleKnown := false
	atRow, atCol := -1, -1
	anyEmitted := false
	if !cellsEqual {
		for r := 0; r < rows; r++ {
			curRow := cur.Cells[r*cols : (r+1)*cols]
			prevRow := prev.Cells[r*cols : (r+1)*cols]
			if slotsEqual(curRow, prevRow) {
				continue
			}
			for c := skipUnchanged(curRow, prevRow, 0); c < cols; c = skipUnchanged(curRow, prevRow, c) {
				cs := curRow[c]
				if atRow != r || atCol != c {
					out = appendCUP(out, r+1, c+1)
					atRow, atCol = r, c
				}
				if !styleKnown || !cs.sameStyle(style) {
					out = appendSGR(out, cs)
					style = cs
					styleKnown = true
				}
				out = appendGlyph(out, cs.Ch, cur.Marks(cs))
				anyEmitted = true
				adv := 1
				if cs.Wide() {
					adv = 2
				}
				atCol += adv
				c += adv
			}
		}
	}

	if !live {
		if cur.CursorVisible != prev.CursorVisible {
			out = appendCursorVisibility(out, cur.CursorVisible)
		}
		if anyEmitted {
			out = append(out, "\x1b[0m\x1b[1;1H"...)
		}
		return out
	}

	needsWrap := anyEmitted && (cur.CursorVisible || prev.CursorVisible)
	if !needsWrap {
		// Drop the reserved hide prefix.
		out = append(out[:start], out[bodyStart:]...)
	}
	if anyEmitted {
		out = append(out, "\x1b[0m"...)
	}
	out = appendCUP(out, cur.CursorRow+1, cur.CursorCol+1)
	if needsWrap {
		if cur.CursorVisible {
			out = append(out, "\x1b[?25h"...)
		}
	} else if cur.CursorVisible != prev.CursorVisible {
		out = appendCursorVisibility(out, cur.CursorVisible)
	}
	return out
}

// liveCursor is where a live client's cursor is: on cell (row, col), or at
// col == cols with a wrap pending after the last column. A position is sure
// when the client counted the same columns as the snapshot to get there; it
// is not after drawing a glyph whose width a client may count differently.
type liveCursor struct {
	row, col int
	sure     bool
}

// unsureWidth reports whether a client may count the width of a drawn glyph
// differently from the snapshot: a wide character, any non-ASCII one (an
// emoji or East Asian ambiguous character can be one column in the grid and
// two in xterm.js, or the reverse), or one with marks (a variation selector
// or ZWJ can change a glyph's width in a client).
func unsureWidth(s CellSlot) bool {
	return s.Ch >= 0x80 || s.Attrs&(attrWide|attrMarks) != 0
}

// renderLiveDiff appends the cells that changed from prev to cur, of the same
// geometry, for a live client that shows prev in the default pen with its
// cursor at at. It emits no cursor visibility and no final cursor move, ends
// in the default pen, and returns where drawing left the client's cursor.
//
// Moves are the shortest that stay safe. On the same row, up to three
// unchanged narrow ASCII cells in the current pen are drawn again, otherwise
// the cursor moves right by a count; anywhere else it is positioned
// absolutely. Relative moves rely on the client's column, so after a glyph of
// unsure width the next move is absolute and the returned position is not
// sure. Cells of one contiguous run follow each other without moves, as in
// renderDiff.
func renderLiveDiff(out []byte, prev, cur *GridSnapshot, at liveCursor) ([]byte, liveCursor) {
	cols, rows := cur.Cols, cur.Rows
	style := padSlot
	if !at.sure {
		at.row = -1 // the first cell drawn is positioned absolutely
	}
	for r := 0; r < rows; r++ {
		curRow := cur.Cells[r*cols : (r+1)*cols]
		prevRow := prev.Cells[r*cols : (r+1)*cols]
		if slotsEqual(curRow, prevRow) {
			continue
		}
		for c := skipUnchanged(curRow, prevRow, 0); c < cols; c = skipUnchanged(curRow, prevRow, c) {
			cs := curRow[c]
			if at.row != r || at.col != c {
				out = appendMove(out, curRow, style, r, c, at)
				at = liveCursor{r, c, true}
			}
			if !cs.sameStyle(style) {
				out = appendSGR(out, cs)
				style = cs
			}
			out = appendGlyph(out, cs.Ch, cur.Marks(cs))
			if unsureWidth(cs) {
				at.sure = false
			}
			adv := 1
			if cs.Wide() {
				adv = 2
			}
			at.col += adv
			c += adv
		}
	}
	if !style.sameStyle(padSlot) {
		out = append(out, "\x1b[0m"...)
	}
	return out, at
}

// appendMove moves a live client's cursor from at to cell (r, c), the next
// cell to draw in row, with style the current pen (see renderLiveDiff).
func appendMove(out []byte, row []CellSlot, style CellSlot, r, c int, at liveCursor) []byte {
	if !at.sure || at.row != r || at.col >= c {
		return appendCUP(out, r+1, c+1)
	}
	n := c - at.col
	if n <= 3 && bridgeable(row[at.col:c], style) {
		for _, s := range row[at.col:c] {
			out = appendRune(out, s.Ch)
		}
		return out
	}
	if n == 1 {
		return append(out, "\x1b[C"...)
	}
	out = append(out, "\x1b["...)
	out = strconv.AppendInt(out, int64(n), 10)
	return append(out, 'C')
}

// bridgeable reports whether drawing cells again in the pen style leaves them
// as they are: narrow ASCII cells (appendRune draws control characters and
// padding as spaces) without marks, in that style.
func bridgeable(cells []CellSlot, style CellSlot) bool {
	for _, s := range cells {
		if s.Ch >= 0x80 || s.Attrs&(attrWide|attrWideSpacer|attrMarks) != 0 || !s.sameStyle(style) {
			return false
		}
	}
	return true
}

func appendCursorVisibility(out []byte, visible bool) []byte {
	if visible {
		return append(out, "\x1b[?25h"...)
	}
	return append(out, "\x1b[?25l"...)
}

// appendGlyph draws a cell: its character and then the zero-width characters
// that combine with it. A character appendRune draws as a space is not what
// the marks combined with, so they are dropped with it, as the reference does.
func appendGlyph(out []byte, r rune, marks []rune) []byte {
	out = appendRune(out, r)
	if r < 0x20 || r == 0x7f {
		return out
	}
	for _, m := range marks {
		out = append(out, string(m)...)
	}
	return out
}

func appendRune(out []byte, r rune) []byte {
	if r < 0x20 || r == 0x7f {
		// A control character sent to a client would act instead of print:
		// a tab, which vt keeps in the blank cell where it started (as
		// alacritty does), would move the client's cursor to the next tab
		// stop and shift the rest of the run. It looks like a space.
		return append(out, ' ')
	}
	if r < 0x80 {
		return append(out, byte(r))
	}
	return append(out, string(r)...)
}

// appendSGR resets and re-applies a cell's style. Always resetting first is
// shorter than a minimal transition for typical screens.
func appendSGR(out []byte, s CellSlot) []byte {
	out = append(out, "\x1b[0m"...)
	if s.Attrs&attrBold != 0 {
		out = append(out, "\x1b[1m"...)
	}
	if s.Attrs&attrDim != 0 {
		out = append(out, "\x1b[2m"...)
	}
	if s.Attrs&attrItalic != 0 {
		out = append(out, "\x1b[3m"...)
	}
	if s.Attrs&attrUnderline != 0 {
		out = append(out, "\x1b[4m"...)
	}
	if s.Attrs&attrInverse != 0 {
		out = append(out, "\x1b[7m"...)
	}
	if s.Attrs&attrHidden != 0 {
		out = append(out, "\x1b[8m"...)
	}
	if s.Attrs&attrStrike != 0 {
		out = append(out, "\x1b[9m"...)
	}
	out = appendColor(out, s.Fg, true)
	return appendColor(out, s.Bg, false)
}

func appendColor(out []byte, c vt.Color, fg bool) []byte {
	switch c.Kind() {
	case vt.KindNamed:
		code, ok := namedToSGR(c.Named(), fg)
		if !ok {
			return out
		}
		out = append(out, "\x1b["...)
		out = strconv.AppendInt(out, int64(code), 10)
		return append(out, 'm')
	case vt.KindIndexed:
		if fg {
			out = append(out, "\x1b[38;5;"...)
		} else {
			out = append(out, "\x1b[48;5;"...)
		}
		out = strconv.AppendInt(out, int64(c.Index()), 10)
		return append(out, 'm')
	case vt.KindRGB:
		rgb := c.RGB()
		if fg {
			out = append(out, "\x1b[38;2;"...)
		} else {
			out = append(out, "\x1b[48;2;"...)
		}
		out = strconv.AppendInt(out, int64(rgb.R), 10)
		out = append(out, ';')
		out = strconv.AppendInt(out, int64(rgb.G), 10)
		out = append(out, ';')
		out = strconv.AppendInt(out, int64(rgb.B), 10)
		return append(out, 'm')
	}
	return out
}

func namedToSGR(n vt.NamedColor, fg bool) (int, bool) {
	base := 40
	bright := 100
	if fg {
		base, bright = 30, 90
	}
	switch {
	case n <= vt.White:
		return base + int(n), true
	case n <= vt.BrightWhite:
		return bright + int(n-vt.BrightBlack), true
	case n >= vt.DimBlack && n <= vt.DimWhite:
		return base + int(n-vt.DimBlack), true
	}
	// Foreground, Background, Cursor, BrightForeground, DimForeground: the
	// preceding reset already applied the default.
	return 0, false
}
