package shelld

import "github.com/gurupras/shelld/vt"

// A paused client is promised the text it is reading, and an offset into the
// live grid cannot keep that promise. An offset says "so many lines back from
// the tail", which stays true only while the rows behind it stay put: a history
// clear drops them and a width change rewrites them, and neither moves the
// pushed count the offset is corrected from, so the client is quietly shown
// different text. vt.Grid.HistoryPushed says the same thing about itself —
// neither reflow nor a history clear maintains the relationship at all.
//
// So a client that would lose its content gets a private copy of the scrollback
// instead, and reads that from then on. This is what tmux does for copy-mode,
// where the view survives another client clearing the pane, and it is the only
// arrangement that answers "I want this client to still see what it is seeing"
// rather than approximating it.
//
// The copy is taken LAZILY, at the moment something would destroy what the
// client reads, and not when it pauses. That distinction is the whole cost
// argument. Copying on pause would charge every scroll-up: measured on realistic
// scrollback at the 10,000-line limit, a full copy is 9.4 MiB at 80 columns and
// 13.3 MiB at 200, and scrolling up is something a user does constantly.
// Copying when a peer actually clears or resizes underneath a paused client
// charges nothing for the ordinary case and pays once for the rare one.
//
// Rows ageing off the far end of history are deliberately NOT such an event.
// That content is gone for every reader, the offset is clamped to the history
// that still exists, and the client lands on the oldest row held — the same
// finite-history behaviour any terminal has. Freezing there would put a copy on
// the path every line of output takes, which is the one path that must stay
// cheap.

// FrozenHistory is a client's own copy of the scrollback and screen, taken at
// one instant and never changed afterwards.
//
// One copy is shared by every view frozen at the same instant, so nothing may
// write to it. Its state is unexported for that reason rather than for tidiness:
// a caller that reached in and changed a cell would change what every other
// paused client is reading. Content is read through SnapshotInto, which crops at
// the caller's own declared size and not at the size the copy was taken at, so the
// copy's own geometry is not something a caller has to know; HistoryLen and Pushed
// report the two bounds a caller does need.
//
// It is stored as a single rectangle of slots, oldest row first, because that is
// what the renderer consumes: serving a view is then a crop out of the
// rectangle, with the same padding rules as a live capture, and no part of the
// paused path has to reach into the terminal. The cost of a rectangle is the
// trailing blanks on short rows, which is the deliberate trade — it buys a read
// path with no per-row indirection for a copy that is only ever made when a peer
// disturbs a paused client.
type FrozenHistory struct {
	// cols is the terminal's width when the copy was taken, and rows is every
	// row it holds: the scrollback plus the screen.
	cols, rows int
	// screen is how many of the trailing rows were the viewport. It is what
	// turns an offset into a row index, since an offset counts back from the
	// live tail rather than from the start of the copy.
	screen int
	// pushed is the grid's HistoryPushed at the instant of the copy; see Pushed.
	pushed int64
	// cells is rows rows of cols slots, row-major, exactly as GridSnapshot
	// stores them.
	cells []CellSlot
	// marks resolves the slots' mark ids; see GridSnapshot.Marks.
	marks [][]rune
}

// freezeHistory copies the whole scrollback and screen out of term.
//
// It captures at the terminal's width rather than any client's, so one copy
// serves every paused client whatever size each declared: a client narrower than
// the terminal crops, and a wider one pads, which is what it already gets from a
// live capture.
//
// It copies the primary screen and not the active one. A program on the
// alternate screen has not moved a viewer holding a position in history — that
// history is still in the primary grid, and a resize still reflows it — so the
// alternate grid, which has no scrollback at all, is never what to copy.
func freezeHistory(term *vt.Term) *FrozenHistory {
	grid := term.PrimaryGrid()
	cols, screen := grid.Columns(), grid.ScreenLines()
	hist := grid.HistorySize()
	f := &FrozenHistory{
		cols:   cols,
		rows:   hist + screen,
		screen: screen,
		pushed: term.HistoryPushed(),
		cells:  make([]CellSlot, (hist+screen)*cols),
		marks:  term.MarkTable(),
	}
	// Oldest first, so row i is grid line i-hist: the copy reads top to bottom
	// in the order a viewer scrolls through it.
	var scratch []vt.Cell
	for i := 0; i < f.rows; i++ {
		row := f.cells[i*cols : (i+1)*cols]
		// Pairing matches a live capture: only rows holding a wide half need it.
		if rowSlots(row, grid.Row(i-hist), &scratch)&(attrWide|attrWideSpacer) != 0 {
			pairWide(row)
		}
	}
	return f
}

// HistoryLen is the number of scrollback rows in the copy, which is the largest
// offset a client reading it can scroll to.
func (f *FrozenHistory) HistoryLen() int { return f.rows - f.screen }

// Pushed is the grid's HistoryPushed at the instant of the copy, which is where a
// client following that count stops following it: everything pushed up to here
// moved the rows the client is reading, and nothing pushed after here touches
// them at all.
//
// A client that pauses and is then frozen has to correct its offset for whatever
// was pushed in between, and this is what it corrects against. The live grid
// cannot answer that question, because the rows it is being asked about are no
// longer in it.
func (f *FrozenHistory) Pushed() int64 { return f.pushed }

// SnapshotInto crops the view at offset and a declared size out of the copy,
// reusing dst's cells when they are large enough.
//
// It mirrors the live capture exactly: rows and columns past the frozen
// terminal's own size are padding, an offset addresses rows back from the tail,
// and a view into history carries no cursor. Anything the live path would show
// as padding must be padding here too, or a client's frame would change shape at
// the moment it froze.
func (f *FrozenHistory) SnapshotInto(dst *GridSnapshot, offset, outCols, outRows int) *GridSnapshot {
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
	dst.marks = f.marks
	// A view into a frozen copy has no live cursor, exactly as an offset view of
	// a live grid has none.
	dst.CursorRow, dst.CursorCol, dst.CursorVisible = 0, 0, false

	offset = max(0, min(offset, f.HistoryLen()))
	copyRows := min(outRows, f.screen)
	copyCols := min(outCols, f.cols)
	// Row r of the view is grid line r-offset, which sits this far into the copy.
	base := f.HistoryLen() - offset
	for r := 0; r < outRows; r++ {
		out := dst.Cells[r*outCols : (r+1)*outCols]
		if src := base + r; r < copyRows && src >= 0 && src < f.rows {
			copy(out[:copyCols], f.cells[src*f.cols:src*f.cols+copyCols])
			// The copy's rows are paired already, so the one pair a crop can split
			// is a wide character in the last column: its spacer is outside the
			// view, and a client draws the two halves together or not at all. That
			// is pairWide's rule, which here has exactly one cell to apply to.
			if last := copyCols - 1; last >= 0 && copyCols < f.cols && out[last].Wide() {
				out[last].Ch = ' '
				out[last].Attrs &^= attrWide | attrMarks
			}
			out = out[copyCols:]
		}
		for i := range out {
			out[i] = padSlot
		}
	}
	return dst
}

// RenderDiff appends the ANSI that brings a viewer holding prev to cur, where
// both are views of this copy.
//
// It is VtGrid.RenderDiffSnapshots for a frozen view, and differs only in where
// the repaint it falls back to reads its cells: a frozen view has no terminal to
// re-read, so the copy's own slots are the source. A frozen view is a view into
// history, so no frame from here places or shows the cursor.
func (f *FrozenHistory) RenderDiff(out []byte, prev, cur *GridSnapshot) []byte {
	return renderDiff(out, prev, cur, false, func(out []byte) []byte {
		return renderFullFrozen(out, cur)
	})
}

// renderFullFrozen appends a self-contained repaint of a frozen view to out.
//
// It is renderFull for a snapshot rather than a terminal. The frame clears the
// screen and writes each row's text, skips a row with nothing on it, and parks
// the cursor at the home position still hidden — which is what renderFull does
// for a view into history.
//
// Its blank test reads what a slot carries, which is one term short of the live
// path's: isBlank also requires a cell's underline color to be unset, and a slot
// records that a cell is underlined but not in what color. The two can therefore
// disagree about one cell — a space carrying an underline color with no underline
// style — and that disagreement cannot be seen. Such a cell draws nothing, since a
// color only tints an underline that is actually drawn; a slot cannot emit the
// color in the first place, so a repaint that did emit the cell would emit a plain
// space; and every later diff reads the same slots, so the repaint and the diffs
// after it agree on where a row's text ends. The live path is left reading cells
// because that is where the term exists, not because the answer differs.
func renderFullFrozen(out []byte, snap *GridSnapshot) []byte {
	out = append(out, "\x1b[?25l\x1b[2J\x1b[H\x1b[0m\x1b[0m"...)
	style := padSlot // the style ESC[0m just selected
	// at is the row the client's cursor sits on, so a row that emits nothing
	// costs no bytes and the frame never runs past the last row of text — which
	// on a client whose declared height exceeds the copy's would scroll its
	// screen.
	at := 0
	for r := 0; r < snap.Rows; r++ {
		row := snap.Cells[r*snap.Cols : (r+1)*snap.Cols]
		last := -1
		for c := range row {
			if !slotIsBlank(row[c]) {
				last = c
			}
		}
		if last < 0 {
			continue
		}
		for ; at < r; at++ {
			out = append(out, "\r\n"...)
		}
		for c := 0; c <= last; c++ {
			s := row[c]
			// The copy's wide halves are paired already, so a spacer here always
			// follows the wide character it is drawn with.
			if s.WideSpacer() {
				continue
			}
			if !s.sameStyle(style) {
				out = appendSGR(out, s)
				style = s
			}
			out = appendGlyph(out, s.Ch, snap.Marks(s))
		}
	}
	return append(out, "\x1b[0m\x1b[1;1H"...)
}

// slotIsBlank reports whether a slot would draw nothing over a freshly cleared
// screen, which is where a row's emitted text ends. It is isBlank for a slot;
// see renderFullFrozen for the one term of style it cannot see.
func slotIsBlank(s CellSlot) bool {
	return (s.Ch == ' ' || s.Ch == 0) && s.Attrs&attrMarks == 0 &&
		s.Bg == vt.NewNamedColor(vt.Background) &&
		s.Attrs&(attrInverse|attrUnderline|attrStrike) == 0
}

// PausedView is one client's claim on the content it is reading: while it is
// registered with a grid, anything about to destroy that content hands it a
// private copy of the scrollback first.
//
// A view is registered when a client stops following the live tail and released
// when it starts again. Until it is handed a copy it reads the live grid, which
// is the ordinary case and costs nothing; once it has one it reads only that,
// and later events cannot reach it.
//
// Like the rest of VtGrid, a view is not safe for concurrent use: register,
// release and read it under whatever serializes access to the grid.
type PausedView struct {
	grid    *VtGrid
	history *FrozenHistory
}

// NewPausedView registers a view of this grid's history and returns it. Release
// it when the client returns to the live view, or the grid keeps copying history
// for a reader that is no longer there.
//
// Registering a view does not copy anything and does not pause the grid; it says
// that one reader's content must survive, so that the next event which would
// destroy that content copies it out first.
func (g *VtGrid) NewPausedView() *PausedView {
	v := &PausedView{grid: g}
	g.pausedViews = append(g.pausedViews, v)
	return v
}

// Release unregisters the view. A nil view and an already-released one are both
// no-ops, so a caller can release on every path that leaves the paused state
// without tracking which of them got there.
func (v *PausedView) Release() {
	if v == nil || v.grid == nil {
		return
	}
	g := v.grid
	v.grid, v.history = nil, nil
	for i, other := range g.pausedViews {
		if other == v {
			last := len(g.pausedViews) - 1
			g.pausedViews[i] = g.pausedViews[last]
			g.pausedViews[last] = nil
			g.pausedViews = g.pausedViews[:last]
			return
		}
	}
}

// Frozen is the copy the view was handed, or nil while the live grid still holds
// what the client is reading.
func (v *PausedView) Frozen() *FrozenHistory {
	if v == nil {
		return nil
	}
	return v.history
}

// freezePausedViews hands every registered view that does not already have one a
// copy of the history as it is now. It runs just before something rewrites or
// drops those rows.
//
// One copy serves all of them: they are all reading this same grid at this same
// instant, and a copy is never written to afterwards. A view that already holds
// one is left alone, since its content is in that copy and not in this grid.
func (g *VtGrid) freezePausedViews() {
	var f *FrozenHistory
	for _, v := range g.pausedViews {
		if v.history != nil {
			continue
		}
		if f == nil {
			f = freezeHistory(g.term)
		}
		v.history = f
	}
}
