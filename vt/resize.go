package vt

// resize changes the grid's dimensions, reflowing wrapped lines when reflow
// is set (the primary screen). Port of alacritty grid/resize.rs.
func (g *Grid) resize(reflow bool, lines, columns int) {
	// Resize with an empty template so cells blanked by the resize carry no
	// attributes; restore the template afterwards.
	template := g.Cursor.Template
	g.Cursor.Template = Cell{}

	switch {
	case g.lines < lines:
		g.growLines(lines)
	case g.lines > lines:
		g.shrinkLines(lines)
	}
	// growLines brings history rows, possibly packed, onto the screen. The column
	// passes do not need this today: each write unpacks its row first, and the
	// raw reads they make are of rows a write has just unpacked or of the cursor
	// row, which growLines never takes from history. It keeps them from
	// depending on either, for a walk of the viewport.
	g.unpackViewport()
	switch {
	case g.columns < columns:
		g.growColumns(reflow, columns)
	case g.columns > columns:
		g.shrinkColumns(reflow, columns)
	}
	g.reflowFree = nil
	g.normalizeRows()

	g.Cursor.Template = template
}

// normalizeRows puts every row back in the form the grid keeps it in between
// resizes, which a resize breaks by moving rows across the top of the viewport
// and by reflowing them: each viewport row stores a cell per column, and each
// history row is compacted (see Row.compact). A row already in its form costs a
// comparison or two, so this is a walk over the history and not a copy of it.
//
// Between growLines and the column passes the rows growLines pulled out of
// history are still short, and that is safe because every viewport row the
// column passes index at full width is the cursor's, which growLines moves down
// with its content and so is never one of those.
func (g *Grid) normalizeRows() {
	g.spare.reset()
	for line := g.topmostLine(); line < 0; line++ {
		g.raw.row(line).settle(nil, g.columns, g.noPack)
	}
	for line := 0; line < g.lines; line++ {
		if r := g.raw.row(line); !r.full(g.columns) {
			r.grow(g.columns) // grow unpacks
		}
	}
}

// unpackViewport turns any packed viewport row back into a plain one.
func (g *Grid) unpackViewport() {
	for line := 0; line < g.lines; line++ {
		g.raw.row(line).unpack()
	}
}

// growLines adds viewport lines, pulling them from history while it lasts so
// the cursor stays anchored to its content.
func (g *Grid) growLines(target int) {
	added := target - g.lines
	g.raw.growVisibleLines(target)
	g.lines = target

	fromHistory := min(g.HistorySize(), added)
	if shortfall := added - fromHistory; shortfall > 0 {
		// History ran out, so the rest of the new lines stay blank. They arrived
		// above the content, and this rotates them below it. It reuses scrollUp,
		// which counts what it rotates as a push, but nothing left the screen —
		// only blank lines moved past it — so the count is put back afterwards.
		// Letting that push stand inflates the count by exactly the shortfall,
		// and a paused viewer applies the inflation as a scroll it never saw.
		pushed := g.pushed
		g.scrollUp(0, target, shortfall)
		g.pushed = pushed
	}
	// Lines pulled back out of history are one line nearer the live tail, so
	// they come off the pushed count. This can take the count below zero, and is
	// why it is signed: narrowing the screen splits wrapped rows, manufacturing
	// history rows that no push counted, so HistorySize can exceed the number of
	// lines ever pushed and a grow can reclaim rows nothing ever pushed. Letting
	// the count go negative keeps the difference between two readings equal to
	// the distance content actually moved; flooring it here would discard that,
	// and a paused viewer applying the difference would land too deep in history.
	g.pushed -= int64(fromHistory)
	g.SavedCursor.Point.Line += fromHistory
	g.Cursor.Point.Line += fromHistory
	g.decreaseScrollLimit(added)
}

// shrinkLines removes viewport lines, pushing content above the cursor into
// history so the cursor stays on screen.
func (g *Grid) shrinkLines(target int) {
	if required := g.Cursor.Point.Line + 1 - target; required > 0 {
		g.scrollUp(0, g.lines, required)
		g.Cursor.Point.Line = min(g.Cursor.Point.Line, target-1)
	}
	g.SavedCursor.Point.Line = min(g.SavedCursor.Point.Line, target-1)
	g.raw.rotate(g.lines - target)
	g.raw.shrinkVisibleLines(target)
	g.lines = target
}

func (g *Grid) growColumns(reflow bool, columns int) {
	shouldReflow := func(r *Row) bool {
		n := r.Len()
		return reflow && n > 0 && n < columns && r.CellAt(n-1).flags&FlagWrapline != 0
	}

	oldColumns := g.columns
	g.columns = columns
	if g.Cursor.InputNeedsWrap && reflow {
		g.Cursor.InputNeedsWrap = false
		g.Cursor.Point.Column++
	}

	rows := g.raw.takeAll()
	reversed := make([]Row, 0, len(rows))
	cursorLineDelta := 0

	// rows[0] is the bottom line; walk from the oldest history line down.
	// A row g.lines back from the end of reversed is history whatever later rows
	// do, so it is settled there rather than left plain until normalizeRows. Rows
	// nearer the end may end up on the screen, and are left as they are.
	appendRow := func(row Row) {
		if n := len(reversed) - g.lines; n > 0 {
			g.settleReflow(&reversed[n-1])
		}
		reversed = append(reversed, row)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		rows[i] = Row{}
		var last *Row
		if n := len(reversed); n > 0 && shouldReflow(&reversed[n-1]) {
			last = &reversed[n-1]
		} else {
			appendRow(row)
			continue
		}

		// A join writes last in place, onto an array with room for the
		// cells it takes; moveFrontTo reads row where it is.
		if last.packed {
			g.putPacked(last.unpackWith(g.takeReflow(0, columns)))
		}
		last.lastMut().flags &^= FlagWrapline

		lastLen := last.Len()
		if lastLen >= 1 && last.inner[lastLen-1].flags&FlagLeadingWideCharSpacer != 0 {
			last.shrink(lastLen - 1)
			lastLen--
		}

		numWrapped := columns - lastLen
		n := min(row.Len(), numWrapped)

		// A compacted history row can store fewer cells than the oldColumns a
		// full row holds, none at all when it is blank. The cells it does not
		// store are default ones, so the cell a full row would split at,
		// min(oldColumns, numWrapped)-1, is a wide one only when the row stores
		// it, and moving just the cells it stores leaves the row above reading
		// the same.
		if row.Len() >= min(oldColumns, numWrapped) && row.CellAt(n-1).flags&FlagWideChar != 0 {
			// A wide character cannot be split over the join, so it stays on this row
			// and the row above takes the spacer that stands in for its first half.
			numWrapped--
			row.moveFrontTo(last, n-1)
			last.appendCell(Cell{flags: FlagLeadingWideCharSpacer})
		} else {
			row.moveFrontTo(last, n)
		}

		cursorBufferLine := g.lines - g.Cursor.Point.Line - 1
		if i == cursorBufferLine && reflow {
			target := g.subCursorBoundary(g.Cursor.Point, numWrapped)
			if target.Column == 0 && row.isClear() {
				g.Cursor.InputNeedsWrap = true
				target = g.subCursorBoundary(target, 1)
			}
			g.Cursor.Point.Column = target.Column

			lineDelta := g.Cursor.Point.Line - target.Line
			if lineDelta != 0 && row.isClear() {
				continue
			}
			cursorLineDelta += lineDelta
		} else if row.isClear() {
			if i < cursorBufferLine {
				g.Cursor.Point.Line++
			}
			continue
		}

		last.lastMut().flags |= FlagWrapline
		appendRow(row)
	}

	if len(reversed) < g.lines {
		delta := g.lines - len(reversed)
		g.Cursor.Point.Line = max(g.Cursor.Point.Line-delta, 0)
		for len(reversed) < g.lines {
			reversed = append(reversed, newRow(columns))
		}
	}

	if cursorLineDelta != 0 {
		cursorBufferLine := g.lines - g.Cursor.Point.Line - 1
		available := min(cursorBufferLine, len(reversed)-g.lines)
		overflow := max(cursorLineDelta-available, 0)
		reversed = reversed[:len(reversed)+overflow-cursorLineDelta]
		g.Cursor.Point.Line = max(g.Cursor.Point.Line-overflow, 0)
	}

	// The viewport, the first g.lines rows of the ring, is widened. A history
	// row is left short, which reads as the row widened with default cells,
	// and resize compacts it — except one whose last cell carries
	// FlagWrapline, which a join can leave short of the width: widened, the
	// flag is no longer on the last cell and the row no longer reads as
	// wrapped to the next widening, and compact keeps such a row whole.
	newRaw := make([]Row, len(reversed))
	for i := range reversed {
		row := reversed[len(reversed)-1-i]
		if i < g.lines {
			if !row.full(columns) {
				row.grow(columns) // grow unpacks
			}
		} else if n := row.Len(); n > 0 && n < columns && row.CellAt(n-1).flags&FlagWrapline != 0 {
			if row.packed {
				countPack(func(s *PackStats) *int { return &s.WrapGrows })
			}
			row.grow(columns)
		}
		newRaw[i] = row
	}
	g.raw.replaceInner(newRaw)
}

// shrinkColumnsPassDone, when a test sets it, sees the original rows and the
// rows placed so far once shrinkColumns has walked every line.
var shrinkColumnsPassDone func(rows, placed []Row)

func (g *Grid) shrinkColumns(reflow bool, columns int) {
	g.columns = columns
	if g.Cursor.InputNeedsWrap && reflow {
		g.Cursor.InputNeedsWrap = false
		g.Cursor.Point.Column++
	}

	rows := g.raw.takeAll()
	limit := g.maxScrollLimit + g.lines
	newRaw := make([]Row, 0, min(len(rows), 2*limit))
	var buffered []Cell
	// As in growColumns, a row g.lines back from the end of newRaw is history,
	// and nothing writes to it again.
	settleBehind := func() {
		if n := len(newRaw) - g.lines; n > 0 {
			g.settleReflow(&newRaw[n-1])
		}
	}

	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		rows[i] = Row{}

		if buffered != nil {
			if i == g.lines-g.Cursor.Point.Line-1 {
				g.Cursor.Point.Column += len(buffered)
			}
			row.appendFront(buffered)
			buffered = nil
		}

		// wrapped is the rest of the line still to be placed, a slice of
		// backing starting at off. Rows split off from it get cells of their
		// own, so no row keeps the (possibly much longer) rest reachable,
		// and splitting a line copies each cell once.
		if row.packed && row.Len() > columns {
			// shrink would unpack it onto an array of its own.
			g.putPacked(row.unpackWith(g.takeReflow(0, row.Len())))
		}
		wrapped := row.shrink(columns)
		backing, off := wrapped, 0
		for {
			if wrapped == nil || !reflow {
				cursorBufferLine := g.lines - g.Cursor.Point.Line - 1
				if reflow && i == cursorBufferLine && g.Cursor.Point.Column > columns {
					// Explicit whitespace before the cursor wraps like content.
					wrapped = []Cell{}
					backing, off = wrapped, 0
				} else {
					settleBehind()
					newRaw = append(newRaw, row)
					break
				}
			}

			if row.Len() >= columns && row.inner[columns-1].flags&FlagWideChar != 0 {
				wide := row.inner[columns-1]
				*row.at(columns - 1) = Cell{flags: FlagLeadingWideCharSpacer}
				if off > 0 {
					// The wide cell was copied into row from backing[off-1].
					off--
					wrapped = backing[off : off+len(wrapped)+1]
					wrapped[0] = wide
				} else {
					wrapped = append([]Cell{wide}, wrapped...)
					backing, off = wrapped, 0
				}
			}

			if n := len(wrapped); n > 0 && wrapped[n-1].flags&FlagLeadingWideCharSpacer != 0 {
				if n == 1 {
					row.at(columns - 1).flags |= FlagWrapline
					settleBehind()
					newRaw = append(newRaw, row)
					break
				}
				wrapped[n-2].flags |= FlagWrapline
				wrapped = wrapped[:n-1]
			}

			settleBehind()
			newRaw = append(newRaw, row)
			newRaw[len(newRaw)-1].lastMut().flags |= FlagWrapline

			if n := len(wrapped); n > 0 && wrapped[n-1].flags&FlagWrapline != 0 && i >= 1 && n < columns {
				wrapped[n-1].flags &^= FlagWrapline
				buffered = wrapped
				break
			}

			cursorBufferLine := g.lines - g.Cursor.Point.Line - 1
			if (i == cursorBufferLine && g.Cursor.Point.Column < columns) || i < cursorBufferLine {
				g.Cursor.Point.Line = max(g.Cursor.Point.Line-1, 0)
			}
			if i == cursorBufferLine && g.Cursor.Point.Column >= columns {
				g.Cursor.Point.Column -= columns
			}

			// The next row: the first columns cells of the rest, and what
			// Row.shrink would have returned for it.
			occ := len(wrapped)
			cells := g.takeReflow(columns, columns)
			copy(cells, wrapped)
			row = Row{inner: cells, occ: int32(min(occ, columns))}
			wrapped = nil
			if occ > columns {
				// wrapped always ends in a non-empty cell (Row.shrink trims
				// it, and dropping a trailing spacer leaves a Wrapline cell),
				// so the rest needs no trim.
				wrapped, off = backing[off+columns:off+occ], off+columns
			}

			if len(newRaw) >= 2*limit {
				// Rows beyond the history limit are dropped below anyway;
				// dropping the oldest now bounds what a narrow resize holds.
				n := copy(newRaw, newRaw[len(newRaw)-limit:])
				clear(newRaw[n:])
				newRaw = newRaw[:n]
			}
		}
	}

	if shrinkColumnsPassDone != nil {
		shrinkColumnsPassDone(rows, newRaw)
	}
	reversed := make([]Row, min(len(newRaw), limit))
	for i := range reversed {
		reversed[i] = newRaw[len(newRaw)-1-i]
	}
	g.raw.replaceInner(reversed)
	// The cursor logic below reads the cursor row's last column raw. That row is
	// already full and plain (see unpackViewport above); this does not rely on it.
	for line := 0; line < g.lines; line++ {
		if r := g.raw.row(line); !r.full(columns) {
			r.grow(columns)
		}
	}

	switch {
	case !reflow:
		g.Cursor.Point.Column = min(g.Cursor.Point.Column, columns-1)
	case g.Cursor.Point.Column == columns &&
		g.raw.row(g.Cursor.Point.Line).inner[columns-1].flags&FlagWrapline == 0:
		g.Cursor.InputNeedsWrap = true
		g.Cursor.Point.Column--
	default:
		g.Cursor.Point = g.clampCursorBoundary(g.Cursor.Point)
	}
	g.SavedCursor.Point.Column = min(g.SavedCursor.Point.Column, columns-1)
}
