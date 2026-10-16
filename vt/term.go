package vt

import (
	"fmt"

	"github.com/gurupras/shelld/uniwidth"
)

// Mode is the set of terminal modes. Bit values match alacritty's TermMode.
type Mode uint32

// The terminal modes, with the sequence that sets each one. Unless noted, a
// mode is a DEC private mode: CSI ? n h sets it and CSI ? n l clears it.
//
// A caller reads these to learn what the program running in the window expects
// of whoever is displaying it — which keys to send, whether to report the
// mouse, whether to bracket a paste. That is most of why Term.Mode is public.
const (
	// ModeShowCursor (DECTCEM, 25) — draw the cursor. Set by default.
	ModeShowCursor Mode = 1 << iota
	// ModeAppCursor (DECCKM, 1) — arrow keys send SS3 (ESC O A) instead of CSI.
	ModeAppCursor
	// ModeAppKeypad — the numeric keypad sends SS3 sequences instead of digits.
	// Set by ESC = (DECKPAM) and cleared by ESC > (DECKPNM); it has no CSI form.
	ModeAppKeypad
	// ModeMouseReportClick (1000) — report button press and release.
	ModeMouseReportClick
	// ModeBracketedPaste (2004) — wrap pasted text in ESC [ 200~ and ESC [ 201~
	// so the program can tell it from typing.
	ModeBracketedPaste
	// ModeSGRMouse (1006) — encode mouse reports as CSI < b ; x ; y M, which
	// unlike the default encoding is not limited to 223 columns. Mutually
	// exclusive with ModeUTF8Mouse: setting either clears the other.
	ModeSGRMouse
	// ModeMouseMotion (1003) — report every motion event, button or not.
	ModeMouseMotion
	// ModeLineWrap (DECAWM, 7) — a character written in the last column wraps
	// to the next line rather than overwriting in place. Set by default.
	ModeLineWrap
	// ModeLineFeedNewLine (LNM, 20) — a line feed also returns the carriage.
	// This is an ANSI mode, set by CSI 20 h without the '?'.
	ModeLineFeedNewLine
	// ModeOrigin (DECOM, 6) — cursor addressing is relative to the scrolling
	// region rather than the screen. Setting it homes the cursor.
	ModeOrigin
	// ModeInsert (IRM, 4) — a printed character shifts the rest of the line
	// right instead of overwriting it. An ANSI mode, like ModeLineFeedNewLine.
	ModeInsert
	// ModeFocusInOut (1004) — report focus changes as CSI I and CSI O.
	ModeFocusInOut
	// ModeAltScreen (1049) — the alternate screen buffer is active: a second
	// grid with no scrollback, which full-screen programs use so that leaving
	// them restores the shell's scrollback untouched.
	ModeAltScreen
	// ModeMouseDrag (1002) — report motion while a button is held.
	ModeMouseDrag
	// ModeUTF8Mouse (1005) — encode mouse coordinates as UTF-8. Mutually
	// exclusive with ModeSGRMouse, which supersedes it.
	ModeUTF8Mouse
	// ModeAlternateScroll (1007) — on the alternate screen, turn wheel events
	// into arrow keys so that a pager scrolls without mouse reporting. Set by
	// default.
	ModeAlternateScroll
	// ModeVi is alacritty's vi-mode bit. No sequence sets it and nothing here
	// reads it; it exists so that Mode's bit values stay alacritty's, and
	// because it is the one mode a reset preserves.
	ModeVi
	// ModeUrgencyHints (1042) — a bell should raise an urgency hint on the
	// window. Set by default; acting on it is the caller's business.
	ModeUrgencyHints
)

// ModeMouse is any mouse-reporting mode: test against it to ask whether the
// program wants mouse events at all, without naming the three levels.
const ModeMouse = ModeMouseReportClick | ModeMouseMotion | ModeMouseDrag

const defaultMode = ModeShowCursor | ModeLineWrap | ModeAlternateScroll | ModeUrgencyHints

const initialTabstops = 8

// Listener receives the terminal's outbound events. Both callbacks run
// synchronously inside Processor.Advance, so they must not call back into
// the Term.
type Listener interface {
	// PtyWrite delivers a reply the application expects on its input
	// (device attributes, cursor position report, mode reports).
	PtyWrite(reply string)
	// ColorRequest asks for the color at index (0..255 palette, 256
	// foreground, 257 background, 258 cursor); format renders the reply.
	ColorRequest(index int, format func(RGB) string)
}

// Config configures a Term.
type Config struct {
	// ScrollingHistory is the maximum number of history lines on the
	// primary screen (alacritty's default is 10000).
	ScrollingHistory int
	// BeforeHistoryClear, if set, is called immediately before the primary
	// screen's history is dropped wholesale — by ESC[3J or by a full reset —
	// while the rows are still readable.
	//
	// It exists for a viewer holding a position in history. Such a viewer
	// follows its content through HistoryPushed, and a wholesale drop is the one
	// movement that count does not describe, so a viewer that has to keep
	// showing what it was showing can only do so by copying the rows out here.
	// Like Listener's callbacks it runs inside Processor.Advance, so it must not
	// call back into the Term; reading the grid is what it is for.
	BeforeHistoryClear func()
}

// Term is the terminal state: primary and alternate grids, modes, scroll
// region, tab stops and charsets.
type Term struct {
	grid         *Grid // active
	inactiveGrid *Grid

	activeCharset int
	tabs          []bool
	mode          Mode
	scrollTop     int // scroll region [scrollTop, scrollBottom)
	scrollBottom  int
	// cursorBlink is the blinking bit of the application-set cursor style
	// (DECSCUSR / DECSET 12); only observable through DECRQM.
	cursorBlink bool

	listener Listener
	marks    markTable
	// beforeHistoryClear is Config.BeforeHistoryClear; see it.
	beforeHistoryClear func()
}

// MinColumns is the narrowest terminal, as in alacritty: a wide character
// takes two columns, and at one a write indexes past the row and a reflow
// never finishes. NewTerm and Resize raise narrower widths to it, and fewer
// than one line to one.
const MinColumns = 2

// NewTerm creates a terminal of the given size. listener may be nil.
func NewTerm(cfg Config, columns, lines int, listener Listener) *Term {
	columns, lines = max(columns, MinColumns), max(lines, 1)
	t := &Term{
		grid:         newGrid(lines, columns, cfg.ScrollingHistory),
		inactiveGrid: newGrid(lines, columns, 0),
		mode:         defaultMode,
		scrollBottom: lines,
		listener:     listener,

		beforeHistoryClear: cfg.BeforeHistoryClear,
	}
	t.tabs = newTabs(columns)
	return t
}

func newTabs(columns int) []bool {
	tabs := make([]bool, columns)
	for i := range tabs {
		tabs[i] = i%initialTabstops == 0
	}
	return tabs
}

// Grid is the active screen buffer.
func (t *Term) Grid() *Grid { return t.grid }

// PrimaryGrid is the primary screen buffer, whichever screen is active. It is
// the one that has a scrollback, so it is what a viewer holding a position in
// history is reading — a program taking the alternate screen does not move that
// viewer, and a resize reflows the primary either way.
func (t *Term) PrimaryGrid() *Grid {
	if t.mode&ModeAltScreen != 0 {
		return t.inactiveGrid
	}
	return t.grid
}

// Mode reports the active modes.
func (t *Term) Mode() Mode { return t.mode }

// Columns is the terminal width.
func (t *Term) Columns() int { return t.grid.columns }

// ScreenLines is the terminal height.
func (t *Term) ScreenLines() int { return t.grid.lines }

// HistoryPushed is the primary screen's Grid.HistoryPushed, whichever screen
// is active.
func (t *Term) HistoryPushed() int64 {
	if t.mode&ModeAltScreen != 0 {
		return t.inactiveGrid.pushed
	}
	return t.grid.pushed
}

// HistorySize is the scrollback length of the active grid (always 0 on the
// alternate screen).
func (t *Term) HistorySize() int { return t.grid.HistorySize() }

func (t *Term) lastColumn() int     { return t.grid.columns - 1 }
func (t *Term) bottommostLine() int { return t.grid.lines - 1 }

// Resize changes the terminal size, reflowing the primary screen. Sizes below
// MinColumns columns or one line are raised to them.
func (t *Term) Resize(columns, lines int) {
	columns, lines = max(columns, MinColumns), max(lines, 1)
	oldCols, oldLines := t.grid.columns, t.grid.lines
	if oldCols == columns && oldLines == lines {
		return
	}
	isAlt := t.mode&ModeAltScreen != 0
	t.grid.resize(!isAlt, lines, columns)
	t.inactiveGrid.resize(isAlt, lines, columns)
	if oldCols != columns {
		t.tabs = resizeTabs(t.tabs, columns)
	}
	t.scrollTop, t.scrollBottom = 0, t.grid.lines
}

func resizeTabs(tabs []bool, columns int) []bool {
	if columns <= len(tabs) {
		return tabs[:columns]
	}
	for i := len(tabs); i < columns; i++ {
		tabs = append(tabs, i%initialTabstops == 0)
	}
	return tabs
}

func (t *Term) swapAlt() {
	if t.mode&ModeAltScreen == 0 {
		t.inactiveGrid.Cursor = t.grid.Cursor
		t.grid.SavedCursor = t.grid.Cursor
		t.inactiveGrid.resetRegion(0, t.inactiveGrid.lines)
	}
	t.grid, t.inactiveGrid = t.inactiveGrid, t.grid
	t.mode ^= ModeAltScreen
}

func (t *Term) scrollDownRelative(origin, lines int) {
	lines = min(lines, t.scrollBottom-t.scrollTop)
	lines = min(lines, t.scrollBottom-origin)
	t.grid.scrollDown(origin, t.scrollBottom, lines)
}

func (t *Term) scrollUpRelative(origin, lines int) {
	lines = min(lines, t.scrollBottom-t.scrollTop)
	t.grid.scrollUp(origin, t.scrollBottom, lines)
}

func (t *Term) deccolm() {
	t.setScrollingRegion(1, 0)
	t.grid.resetRegion(0, t.grid.lines)
}

func (t *Term) wrapline() {
	if t.mode&ModeLineWrap == 0 {
		return
	}
	t.grid.cursorCell().flags |= FlagWrapline
	if t.grid.Cursor.Point.Line+1 >= t.scrollBottom {
		t.linefeed()
	} else {
		t.grid.Cursor.Point.Line++
	}
	t.grid.Cursor.Point.Column = 0
	t.grid.Cursor.InputNeedsWrap = false
}

func (t *Term) writeAtCursor(c rune) {
	g := t.grid
	if g.Cursor.Charsets[t.activeCharset] == CharsetLineDrawing {
		c = mapLineDrawing(c)
	}
	tmpl := &g.Cursor.Template
	p := g.Cursor.Point
	row := g.raw.row(p.Line)
	cell := row.at(p.Column)

	if cell.flags&(FlagWideChar|FlagWideCharSpacer) != 0 {
		wide := cell.flags&FlagWideChar != 0
		if wide && p.Column < t.lastColumn() {
			row.at(p.Column + 1).flags &^= FlagWideCharSpacer
		} else if p.Column > 0 {
			row.at(p.Column - 1).clearWide()
		}
		// The line above can be history, which stores no cells past its content;
		// a column it does not store holds a default cell, with no flag to clear.
		if p.Column <= 1 && p.Line != g.topmostLine() {
			if above := g.raw.row(p.Line - 1); t.lastColumn() < above.Len() &&
				above.CellAt(t.lastColumn()).flags&FlagLeadingWideCharSpacer != 0 {
				// A history row is packed; it is unpacked to be written and
				// stored again as a scroll would store it.
				packed := above.packed
				above.unpack()
				above.at(t.lastColumn()).flags &^= FlagLeadingWideCharSpacer
				if packed {
					countPack(func(s *PackStats) *int { return &s.Rewrites })
					above.packOrCompact(nil, g.columns, g.noPack)
				}
			}
		}
	}

	cell.ch = c ^ ' '
	cell.fg = tmpl.fg
	cell.bg = tmpl.bg
	cell.flags = tmpl.flags
	cell.ul = tmpl.ul
	cell.zw = 0
}

// input prints one character at the cursor.
func (t *Term) input(c rune) {
	width := uniwidth.Width(c)
	if width == uniwidth.None {
		return
	}
	g := t.grid

	if width == 0 {
		col := g.Cursor.Point.Column
		if !g.Cursor.InputNeedsWrap {
			col = max(col-1, 0)
		}
		row := g.raw.row(g.Cursor.Point.Line)
		if row.inner[col].flags&FlagWideCharSpacer != 0 {
			col = max(col-1, 0)
		}
		cell := row.at(col)
		cell.flags |= FlagZerowidth
		cell.zw = t.marks.push(cell.Rune(), cell.zw, c)
		return
	}

	if g.Cursor.InputNeedsWrap {
		t.wrapline()
	}

	columns := g.columns
	if t.mode&ModeInsert != 0 && g.Cursor.Point.Column+width < columns {
		row := g.raw.row(g.Cursor.Point.Line)
		cells := row.span(0, columns)
		col := g.Cursor.Point.Column
		for i := columns - width - 1; i >= col; i-- {
			cells[i+width], cells[i] = cells[i], cells[i+width]
		}
	}

	if width == 1 {
		t.writeAtCursor(c)
	} else {
		if g.Cursor.Point.Column+1 >= columns {
			if t.mode&ModeLineWrap != 0 {
				g.Cursor.Template.flags |= FlagLeadingWideCharSpacer
				t.writeAtCursor(' ')
				g.Cursor.Template.flags &^= FlagLeadingWideCharSpacer
				t.wrapline()
			} else {
				g.Cursor.InputNeedsWrap = true
				return
			}
		}
		g.Cursor.Template.flags |= FlagWideChar
		t.writeAtCursor(c)
		g.Cursor.Template.flags &^= FlagWideChar

		g.Cursor.Point.Column++
		g.Cursor.Template.flags |= FlagWideCharSpacer
		t.writeAtCursor(' ')
		g.Cursor.Template.flags &^= FlagWideCharSpacer
	}

	if g.Cursor.Point.Column+1 < columns {
		g.Cursor.Point.Column++
	} else {
		g.Cursor.InputNeedsWrap = true
	}
}

// inputASCII prints a run of printable ASCII (0x20..0x7e). It is input()
// specialized for width-1 characters outside insert mode and line drawing;
// callers fall back to input() otherwise.
func (t *Term) inputASCII(s []byte) {
	g := t.grid
	columns := g.columns
	for len(s) > 0 {
		if g.Cursor.InputNeedsWrap {
			t.wrapline()
		}
		p := g.Cursor.Point
		row := g.raw.row(p.Line)
		// Characters that fit before the last column, in one span.
		n := min(len(s), columns-p.Column)
		cells := row.span(p.Column, p.Column+n)
		tmpl := g.Cursor.Template
		i := 0
		for ; i < n; i++ {
			cell := &cells[i]
			if cell.flags&(FlagWideChar|FlagWideCharSpacer) != 0 {
				break
			}
			// Copy the template, then set the character. A composite literal
			// (Cell{ch: …, fg: tmpl.fg, …}) compiles to a stack temporary
			// filled field by field and copied with 16-byte loads that cannot
			// be store-forwarded, stalling every character: parse_warm ascii
			// ran 2.4x slower. tmpl is stored once per span, so its loads
			// forward.
			*cell = tmpl
			cell.ch = rune(s[i]) ^ ' '
		}
		if i > 0 {
			// Advance past the i written cells exactly as i calls to input
			// would: the column stops at the last column with wrap pending.
			last := p.Column + i - 1
			if last+1 < columns {
				g.Cursor.Point.Column = last + 1
			} else {
				g.Cursor.Point.Column = last
				g.Cursor.InputNeedsWrap = true
			}
			s = s[i:]
		}
		if i < n {
			// A wide cell is being overwritten; take the general path.
			t.input(rune(s[0]))
			s = s[1:]
		}
	}
}

// goTo is absolute cursor addressing — CUP, and the row half of it. Under DECOM
// the row is counted from the scrolling region's top and bounded by its bottom, so
// a program that has set a region addresses rows within it; otherwise a row is
// counted from the top of the screen.
//
// Only absolute addressing comes through here. The offset DECOM applies is a
// translation of the coordinate system, which is meaningless applied to a
// distance: added to one it would displace every relative move by the region's
// top row. moveVertically bounds those instead.
func (t *Term) goTo(line, col int) {
	yOffset, maxY := 0, t.bottommostLine()
	if t.mode&ModeOrigin != 0 {
		yOffset, maxY = t.scrollTop, t.scrollBottom-1
	}
	t.grid.Cursor.Point.Line = max(min(line+yOffset, maxY), 0)
	t.grid.Cursor.Point.Column = min(col, t.lastColumn())
	t.grid.Cursor.InputNeedsWrap = false
}

// gotoLine is VPA, the row half of absolute addressing, so it does go through
// goTo.
func (t *Term) gotoLine(line int) { t.goTo(line, t.grid.Cursor.Point.Column) }

// gotoCol is CHA, which addresses a column and leaves the row alone. Handing the
// current row back to goTo would not: under DECOM that row already counts from
// the region's top, and goTo would add the region's top to it again and move a
// cursor CHA must not move.
func (t *Term) gotoCol(col int) {
	t.grid.Cursor.Point.Column = min(col, t.lastColumn())
	t.grid.Cursor.InputNeedsWrap = false
}

// moveVertically is the body of the four relative vertical moves — CUU, CUD, CPL
// and CNL — and what it exists to get right is the bound. A cursor inside the
// scrolling region stays inside it; one outside is bounded by the screen. A
// program that sets a region owns those rows and keeps what is outside them, so a
// move that ran past an edge would put the cursor on a row the program is not
// expecting to be written, and the next thing it prints lands there: the status
// line a pager keeps below its region, most often.
//
// The bound does not depend on DECOM. DECOM changes what an absolute row number
// counts from; the region is what limits where the cursor may go, and it limits it
// either way.
func (t *Term) moveVertically(lines int) {
	line := t.grid.Cursor.Point.Line
	lo, hi := 0, t.bottommostLine()
	if line >= t.scrollTop {
		lo = t.scrollTop
	}
	if line < t.scrollBottom {
		hi = t.scrollBottom - 1
	}
	t.grid.Cursor.Point.Line = max(min(line+lines, hi), lo)
	t.grid.Cursor.InputNeedsWrap = false
}

func (t *Term) insertBlank(count int) {
	g := t.grid
	columns := g.columns
	count = min(count, columns-g.Cursor.Point.Column)
	source := g.Cursor.Point.Column
	dest := source + count
	numCells := columns - dest
	cells := g.raw.row(g.Cursor.Point.Line).span(0, columns)
	for off := numCells - 1; off >= 0; off-- {
		cells[dest+off], cells[source+off] = cells[source+off], cells[dest+off]
	}
	fillBlank(cells[source:dest], g.Cursor.Template.bg)
}

func (t *Term) moveUp(lines int)   { t.moveVertically(-lines) }
func (t *Term) moveDown(lines int) { t.moveVertically(lines) }

func (t *Term) moveForward(cols int) {
	t.grid.Cursor.Point.Column = min(t.grid.Cursor.Point.Column+cols, t.lastColumn())
	t.grid.Cursor.InputNeedsWrap = false
}

func (t *Term) moveBackward(cols int) {
	t.grid.Cursor.Point.Column = max(t.grid.Cursor.Point.Column-cols, 0)
	t.grid.Cursor.InputNeedsWrap = false
}

func (t *Term) ptyWrite(s string) {
	if t.listener != nil {
		t.listener.PtyWrite(s)
	}
}

// alacrittyVersion is alacritty_terminal 0.26.0 in the DA2 reply encoding.
const alacrittyVersion = 2600

func (t *Term) identifyTerminal(intermediate byte) {
	switch intermediate {
	case 0:
		t.ptyWrite("\x1b[?6c")
	case '>':
		t.ptyWrite(fmt.Sprintf("\x1b[>0;%d;1c", alacrittyVersion))
	}
}

func (t *Term) deviceStatus(arg int) {
	switch arg {
	case 5:
		t.ptyWrite("\x1b[0n")
	case 6:
		p := t.grid.Cursor.Point
		t.ptyWrite(fmt.Sprintf("\x1b[%d;%dR", p.Line+1, p.Column+1))
	}
}

// moveDownAndCR and moveUpAndCR are CNL and CPL: the same relative move as CUD and
// CUU followed by a carriage return, so they take the same bound.
func (t *Term) moveDownAndCR(lines int) { t.moveVertically(lines); t.gotoCol(0) }
func (t *Term) moveUpAndCR(lines int)   { t.moveVertically(-lines); t.gotoCol(0) }

func (t *Term) putTab(count int) {
	g := t.grid
	if g.Cursor.InputNeedsWrap {
		t.wrapline()
		return
	}
	for g.Cursor.Point.Column < g.columns && count != 0 {
		count--
		c := '\t'
		if g.Cursor.Charsets[t.activeCharset] == CharsetLineDrawing {
			c = mapLineDrawing(c)
		}
		cell := g.cursorCell()
		if cell.Rune() == ' ' {
			cell.SetRune(c)
		}
		for g.Cursor.Point.Column+1 != g.columns {
			g.Cursor.Point.Column++
			if t.tabs[g.Cursor.Point.Column] {
				break
			}
		}
	}
}

func (t *Term) backspace() {
	if t.grid.Cursor.Point.Column > 0 {
		t.grid.Cursor.Point.Column--
		t.grid.Cursor.InputNeedsWrap = false
	}
}

func (t *Term) carriageReturn() {
	t.grid.Cursor.Point.Column = 0
	t.grid.Cursor.InputNeedsWrap = false
}

func (t *Term) linefeed() {
	next := t.grid.Cursor.Point.Line + 1
	if next == t.scrollBottom {
		t.scrollUp(1)
	} else if next < t.grid.lines {
		t.grid.Cursor.Point.Line++
	}
}

func (t *Term) setHorizontalTabstop() { t.tabs[t.grid.Cursor.Point.Column] = true }

func (t *Term) scrollUp(lines int)   { t.scrollUpRelative(t.scrollTop, lines) }
func (t *Term) scrollDown(lines int) { t.scrollDownRelative(t.scrollTop, lines) }

func (t *Term) insertBlankLines(lines int) {
	origin := t.grid.Cursor.Point.Line
	if origin >= t.scrollTop && origin < t.scrollBottom {
		t.scrollDownRelative(origin, lines)
	}
}

func (t *Term) deleteLines(lines int) {
	origin := t.grid.Cursor.Point.Line
	lines = min(t.grid.lines-origin, lines)
	if lines > 0 && origin >= t.scrollTop && origin < t.scrollBottom {
		t.scrollUpRelative(origin, lines)
	}
}

func (t *Term) eraseChars(count int) {
	g := t.grid
	start := g.Cursor.Point.Column
	end := min(start+count, g.columns)
	fillBlank(g.raw.row(g.Cursor.Point.Line).span(start, end), g.Cursor.Template.bg)
}

func (t *Term) deleteChars(count int) {
	g := t.grid
	columns := g.columns
	start := g.Cursor.Point.Column
	// Clamp to the cells at or after the cursor, not to the whole row: the fill
	// below starts at columns-count, so a count clamped to columns would blank
	// cells to the LEFT of the cursor whenever it exceeded the columns
	// remaining. \x1b[80P in column 5 of an 80-column row erased the row.
	//
	// A documented divergence: the reference is min(count, columns)
	// (alacritty_terminal 0.26 term/mod.rs:1544). See the package doc.
	count = min(count, columns-start)
	end := min(start+count, columns-1)
	numCells := columns - end
	cells := g.raw.row(g.Cursor.Point.Line).span(0, columns)
	for off := 0; off < numCells; off++ {
		cells[start+off], cells[end+off] = cells[end+off], cells[start+off]
	}
	fillBlank(cells[columns-count:], g.Cursor.Template.bg)
}

func (t *Term) moveBackwardTabs(count int) {
	g := t.grid
	for ; count > 0; count-- {
		col := g.Cursor.Point.Column
		if col == 0 {
			break
		}
		found := false
		for i := col - 1; i >= 0; i-- {
			if t.tabs[i] {
				col, found = i, true
				break
			}
		}
		// No stop to the left (TBC 3 clears them all): the cursor stays put,
		// and so would every remaining count, each rescanning the row. CBT's
		// count is a u16, so without this a few bytes cost millions of steps.
		if !found {
			break
		}
		g.Cursor.Point.Column = col
	}
}

func (t *Term) moveForwardTabs(count int) {
	g := t.grid
	numCols := g.columns
	for ; count > 0; count-- {
		col := g.Cursor.Point.Column
		if col == numCols-1 {
			break
		}
		for i := col + 1; i < numCols; i++ {
			col = i
			if t.tabs[col] {
				break
			}
		}
		g.Cursor.Point.Column = col
	}
}

func (t *Term) saveCursorPosition()    { t.grid.SavedCursor = t.grid.Cursor }
func (t *Term) restoreCursorPosition() { t.grid.Cursor = t.grid.SavedCursor }

const (
	clearRight = iota
	clearLeft
	clearAll
)

func (t *Term) clearLine(mode int) {
	g := t.grid
	p := g.Cursor.Point
	var left, right int
	switch mode {
	case clearRight:
		if g.Cursor.InputNeedsWrap {
			return
		}
		left, right = p.Column, g.columns
	case clearLeft:
		left, right = 0, p.Column+1
	default:
		left, right = 0, g.columns
	}
	fillBlank(g.raw.row(p.Line).span(left, right), g.Cursor.Template.bg)
}

func (t *Term) dynamicColorSequence(prefix string, index int, terminator string) {
	if t.listener == nil {
		return
	}
	t.listener.ColorRequest(index, func(c RGB) string {
		return fmt.Sprintf("\x1b]%s;rgb:%02x%02x/%02x%02x/%02x%02x%s", prefix, c.R, c.R, c.G, c.G, c.B, c.B, terminator)
	})
}

const (
	clearBelow = iota
	clearAbove
	clearScreenAll
	clearSaved
)

func (t *Term) clearScreen(mode int) {
	g := t.grid
	bg := g.Cursor.Template.bg
	switch mode {
	case clearAbove:
		p := g.Cursor.Point
		// > 0, not > 1: on line 1 the single line above the cursor is line 0,
		// and it has to be erased too. resetRegion(0, 0) is already a no-op, so
		// the guard only needs to exclude line 0, which has nothing above it.
		//
		// A documented divergence: the reference guards with cursor.line > 1
		// (alacritty_terminal 0.26 term/mod.rs:1761). See the package doc.
		if p.Line > 0 {
			g.resetRegion(0, p.Line)
		}
		end := min(p.Column+1, g.columns)
		fillBlank(g.raw.row(p.Line).span(0, end), bg)
	case clearBelow:
		p := g.Cursor.Point
		fillBlank(g.raw.row(p.Line).span(p.Column, g.columns), bg)
		if p.Line < g.lines-1 {
			g.resetRegion(p.Line+1, g.lines)
		}
	case clearScreenAll:
		if t.mode&ModeAltScreen != 0 {
			g.resetRegion(0, g.lines)
		} else {
			g.clearViewport()
		}
	case clearSaved:
		if t.HistorySize() > 0 {
			t.notifyBeforeHistoryClear()
			g.clearHistory()
		}
	}
}

// notifyBeforeHistoryClear runs Config.BeforeHistoryClear, if one was configured,
// with the history still in place.
func (t *Term) notifyBeforeHistoryClear() {
	if t.beforeHistoryClear != nil {
		t.beforeHistoryClear()
	}
}

func (t *Term) clearTabs(all bool) {
	if all {
		clear(t.tabs)
	} else {
		t.tabs[t.grid.Cursor.Point.Column] = false
	}
}

func (t *Term) resetState() {
	if t.mode&ModeAltScreen != 0 {
		t.grid, t.inactiveGrid = t.inactiveGrid, t.grid
		// The bit goes with the swap, even though the reset below clears every
		// mode anyway: HistoryPushed and PrimaryGrid pick their grid by it, and
		// the notification between here and there reads both.
		t.mode &^= ModeAltScreen
	}
	t.activeCharset = 0
	// The swap above has made the primary screen active, so this reads the
	// history that reset is about to drop whichever screen the program was on.
	if t.HistorySize() > 0 {
		t.notifyBeforeHistoryClear()
	}
	t.grid.reset()
	t.inactiveGrid.reset()
	t.scrollTop, t.scrollBottom = 0, t.grid.lines
	t.tabs = newTabs(t.grid.columns)
	t.mode &= ModeVi
	t.mode |= defaultMode
	t.cursorBlink = false
}

func (t *Term) reverseIndex() {
	if t.grid.Cursor.Point.Line == t.scrollTop {
		t.scrollDown(1)
	} else {
		t.grid.Cursor.Point.Line = max(t.grid.Cursor.Point.Line-1, 0)
	}
}

func (t *Term) setPrivateMode(mode uint16, set bool) {
	switch mode {
	case 1042:
		t.toggle(ModeUrgencyHints, set)
	case 1049:
		if set == (t.mode&ModeAltScreen == 0) {
			t.swapAlt()
		}
	case 25:
		t.toggle(ModeShowCursor, set)
	case 1:
		t.toggle(ModeAppCursor, set)
	case 1000:
		t.setMouseMode(ModeMouseReportClick, set)
	case 1002:
		t.setMouseMode(ModeMouseDrag, set)
	case 1003:
		t.setMouseMode(ModeMouseMotion, set)
	case 1004:
		t.toggle(ModeFocusInOut, set)
	case 2004:
		t.toggle(ModeBracketedPaste, set)
	case 1006:
		if set {
			t.mode &^= ModeUTF8Mouse
		}
		t.toggle(ModeSGRMouse, set)
	case 1005:
		if set {
			t.mode &^= ModeSGRMouse
		}
		t.toggle(ModeUTF8Mouse, set)
	case 1007:
		t.toggle(ModeAlternateScroll, set)
	case 7:
		t.toggle(ModeLineWrap, set)
	case 6:
		t.toggle(ModeOrigin, set)
		if set {
			t.goTo(0, 0)
		}
	case 3:
		t.deccolm()
	case 12:
		t.cursorBlink = set
	}
	// 2026 (synchronized update) is handled by the Processor.
}

func (t *Term) toggle(m Mode, set bool) {
	if set {
		t.mode |= m
	} else {
		t.mode &^= m
	}
}

// Mouse protocols are mutually exclusive when enabled.
func (t *Term) setMouseMode(m Mode, set bool) {
	if set {
		t.mode &^= ModeMouse
		t.mode |= m
	} else {
		t.mode &^= m
	}
}

// reportPrivateMode answers DECRQM for a private mode.
func (t *Term) reportPrivateMode(mode uint16) {
	state := 0 // not supported
	has := func(m Mode) int {
		if t.mode&m != 0 {
			return 1
		}
		return 2
	}
	switch mode {
	case 1:
		state = has(ModeAppCursor)
	case 6:
		state = has(ModeOrigin)
	case 7:
		state = has(ModeLineWrap)
	case 12:
		state = 2
		if t.cursorBlink {
			state = 1
		}
	case 25:
		state = has(ModeShowCursor)
	case 1000:
		state = has(ModeMouseReportClick)
	case 1002:
		state = has(ModeMouseDrag)
	case 1003:
		state = has(ModeMouseMotion)
	case 1004:
		state = has(ModeFocusInOut)
	case 1005:
		state = has(ModeUTF8Mouse)
	case 1006:
		state = has(ModeSGRMouse)
	case 1007:
		state = has(ModeAlternateScroll)
	case 1042:
		state = has(ModeUrgencyHints)
	case 1049:
		state = has(ModeAltScreen)
	case 2004:
		state = has(ModeBracketedPaste)
	case 2026:
		state = 2
	}
	t.ptyWrite(fmt.Sprintf("\x1b[?%d;%d$y", mode, state))
}

func (t *Term) setMode(mode uint16, set bool) {
	switch mode {
	case 4:
		t.toggle(ModeInsert, set)
	case 20:
		t.toggle(ModeLineFeedNewLine, set)
	}
}

func (t *Term) reportMode(mode uint16) {
	state := 0
	switch mode {
	case 4:
		state = 2
		if t.mode&ModeInsert != 0 {
			state = 1
		}
	case 20:
		state = 2
		if t.mode&ModeLineFeedNewLine != 0 {
			state = 1
		}
	}
	t.ptyWrite(fmt.Sprintf("\x1b[%d;%d$y", mode, state))
}

// setScrollingRegion is DECSTBM with 1-based bounds; bottom 0 means the last
// line.
func (t *Term) setScrollingRegion(top, bottom int) {
	if bottom == 0 {
		bottom = t.grid.lines
	}
	if top >= bottom {
		return
	}
	lines := t.grid.lines
	// The top clamps to lines-1, not lines: clamping it to lines yields the
	// empty region [lines, lines), and every later linefeed then scrolls zero
	// lines, so the screen never scrolls again. A stale region from a taller
	// window (\x1b[30;40r on 24 lines) has to leave at least one line.
	//
	// A documented divergence: the reference is cmp::min(start, screen_lines)
	// (alacritty_terminal 0.26 term/mod.rs:2174), and linefeed and
	// scrollUpRelative below are transcribed from it faithfully, so the freeze
	// reproduces there too. See the package doc.
	t.scrollTop = min(top-1, lines-1)
	t.scrollBottom = min(bottom, lines)
	t.goTo(0, 0)
}

func (t *Term) textAreaSizeChars() {
	t.ptyWrite(fmt.Sprintf("\x1b[8;%d;%dt", t.grid.lines, t.grid.columns))
}

func mapLineDrawing(c rune) rune {
	switch c {
	case '_':
		return ' '
	case '`':
		return '◆'
	case 'a':
		return '▒'
	case 'b':
		return '␉'
	case 'c':
		return '␌'
	case 'd':
		return '␍'
	case 'e':
		return '␊'
	case 'f':
		return '°'
	case 'g':
		return '±'
	case 'h':
		return '␤'
	case 'i':
		return '␋'
	case 'j':
		return '┘'
	case 'k':
		return '┐'
	case 'l':
		return '┌'
	case 'm':
		return '└'
	case 'n':
		return '┼'
	case 'o':
		return '⎺'
	case 'p':
		return '⎻'
	case 'q':
		return '─'
	case 'r':
		return '⎼'
	case 's':
		return '⎽'
	case 't':
		return '├'
	case 'u':
		return '┤'
	case 'v':
		return '┴'
	case 'w':
		return '┬'
	case 'x':
		return '│'
	case 'y':
		return '≤'
	case 'z':
		return '≥'
	case '{':
		return 'π'
	case '|':
		return '≠'
	case '}':
		return '£'
	case '~':
		return '·'
	}
	return c
}

// KeepRowsOnShrink prepares the primary screen for shrinking to lines rows.
//
// Resize on its own keeps a window of the new height, placed at the top of the
// screen and slid down only as far as it takes to keep the cursor on it; the rows
// above the window go to history, as much of it as Config.ScrollingHistory left
// room for, and the rows below the window are destroyed. An inline
// TUI such as Claude Code keeps status rows below its cursor and, on SIGWINCH,
// clears the screen and repaints only the bottom of its output — so the rows it
// clears survive only if they reached history first, and Resize's own slide
// stores just the rows the cursor forced, which is fewer than the TUI is about to
// overwrite. KeepRowsOnShrink stores the rest up front, scrolling the shrink's
// whole deficit off the top, less all but one of the blank rows at the bottom of
// the screen.
//
// The cursor bounds how far it can go, and that bound is where the guarantee
// stops. The cursor's own row is never pushed, so at most the rows above it go to
// history, and a shrink needing more than that destroys the difference from the
// bottom whether those rows hold anything or not: a full 10-row screen shrunk to
// 5 with the cursor on row 1 pushes nothing and loses five rows of text, and the
// same shrink with the cursor on row 10 pushes five and loses none.
//
// Blank means the run of rows below the cursor's own row whose cells are all
// empty, counted up from the very bottom. Both ends of that matter: a blank row
// with anything below it is not in the run, and neither is the cursor's row, even
// when it is as empty as the rows under it. Empty is Cell.isEmpty: a space or a
// tab in default colors with no attribute a renderer would paint on a blank, so
// bold, dim, italic and hidden leave a cell empty while a color, any underline
// style, inverse and strikeout do not. Two edges of that are easy to miss. An
// underline color on its own leaves a cell empty, because isEmpty does not read
// it. And a row of blanks that ran off the right edge is content on FlagWrapline
// alone — the flag lands on the row that overflowed, not on the row it spilled
// into, which stays blank and does not stop the run. So what the shrink can
// reclaim is a row of spaces or tabs, carrying none of the attributes above, that
// did not run off the right edge; a status row a TUI painted in color, or with any
// of them, holds its place.
//
// One row of that blank run is held back, so the push runs a row further than the
// run alone would allow: the oldest row still on screen ends up one line clear of
// the viewport's top instead of sitting on it. That is a cushion against the
// repaint, not a rule about the bottom edge — an inline TUI clears and redraws the
// top of the window it was just given, so a row left on the new row 0 is
// overwritten there while a row one line above it survives in history. A later
// grow can hand it back, since a grow fills from history first, taking as many
// lines as it added or the whole of history if that is less. So a 10-row screen
// whose text ends on row 4, shrunk to 5, pushes one row of text with the cursor
// on row 5 rather than none, and with the cursor on row 8 pushes all four and
// leaves a blank screen. Where every row below the cursor is blank, that one row
// is the most calling this adds, and often it adds nothing.
// TestKeepRowsOnShrinkAgainstAPlainResize measures that: over every screen of
// three to nine rows, every pattern of blank and occupied rows, every cursor row
// and every target — 59,376 cases — 14,266 have every row below the cursor blank,
// and of those this pushes exactly one row more than a plain Resize in 5,164 and
// the same number in the other 9,102. One row is the whole spread there, never
// two, so on that shape the cushion is all of the difference from Resize and
// everything above about a deficit and a blank tail comes to nothing. The push
// earns its keep on the other shape, a blank run that stops short of the cursor:
// it exceeds a plain Resize's in 31,636 of the 45,110 remaining cases, and there
// the spread is wider — one row in 16,060 of them, two in 10,936, three in 4,256
// and four in 384. The predicate is vacuous with the cursor on the bottom row,
// which is 7,168 of the 14,266 and never a case that adds a row: all 5,164 have a
// row below the cursor.
//
// Nothing in the arithmetic here derives the cushion, and one row is what the
// evidence shows to be enough rather than a bound: a recorded Claude Code
// session resized three times, loses two of its 150 output items without it, one traceable to the
// cushion at each of the two height shrinks. Neither row dies at the shrink
// itself, which is the point: with the cushion off, the first is ten lines into
// history when its resize returns and the second is on the new row 0, and what
// takes them is a repaint thousands of bytes later — the first only after the
// intervening grow has pulled it back onto row 0, where the second already was.
// Both then go the same way, overwritten in place by the next item. It is also a
// trade. Two of the other recordings, claude-mixed and claude-mixed2, duplicate
// three more rows between them with the cushion than without, which the replay
// logs and does not fail.
//
// Against tmux — screen_resize_y with eat_empty set, which deletes rows from the
// bottom down to the cursor's own row without looking at what is on them, and
// pushes the remaining max(0, cursor+1-target) — this pushes at least as many
// rows, for every target of a row or more: the deficit less the blank tail is
// never smaller than tmux's count, because the tail cannot reach the cursor's
// row, and neither is the cursor's own row, which is the only other bound. So
// wherever the two screens differ it is because this pushed further, a row tmux
// still shows sitting in history here or a row tmux destroyed still being here.
//
// That the further push also keeps more holds only while the history has room for
// it. screen_resize_y does not trim at all — it raises hsize past the limit —
// where a push here evicts the oldest rows as soon as ScrollingHistory is full, so
// just after a shrink against a full history there are rows tmux still holds and
// this one has dropped. tmux's overrun is transient, though:
// grid_view_scroll_region_up calls grid_collect_history before every scroll, and
// that frees the oldest tenth of the limit in one batch, so a single linefeed later
// tmux has dropped more than this did. The push count is the part that holds
// unconditionally, and it is what
// TestKeepRowsOnShrinkPushesAtLeastAsMuchAsTmux asserts;
// TestKeepRowsOnShrinkAgainstTmux pins whole screens against measured tmux output
// with the history limit well clear.
//
// The alternate screen, which has no history and whose programs repaint
// entirely, is left alone. A saved cursor (DECSC) whose row goes to history is
// clamped to the top row rather than followed into it, so a restore after a
// shrink can land on a line other than the one saved. Call it before Resize.
func (t *Term) KeepRowsOnShrink(lines int) {
	g := t.grid
	if t.mode&ModeAltScreen != 0 || lines >= g.lines {
		return
	}
	c := g.Cursor.Point.Line
	blank := 0
	for line := g.lines - 1; line > c && g.raw.row(line).isClear(); line-- {
		blank++
	}
	// One row of the blank tail is not given up, which is what leaves the oldest
	// row this pushes one line clear of the viewport. See the cushion above.
	giveUp := max(blank-1, 0)
	push := min(g.lines-lines-giveUp, c)
	if push <= 0 {
		return
	}
	g.scrollUp(0, g.lines, push)
	g.Cursor.Point.Line -= push
	g.SavedCursor.Point.Line = max(g.SavedCursor.Point.Line-push, 0)
}
