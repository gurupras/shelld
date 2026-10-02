package vt

// Point addresses a cell. Line 0 is the top of the viewport; negative lines
// are scrollback history, so -1 is the line most recently scrolled off. Both
// coordinates are zero-based, unlike the one-based coordinates the escape
// sequences themselves use.
type Point struct {
	// Line counts down from the top of the viewport, into history when negative.
	Line int
	// Column counts from the left edge and never reaches the grid's width: it
	// stops at the last column, and Cursor.InputNeedsWrap carries the fact that
	// the next printable character has to wrap first. That flag is what stands
	// in for the "phantom column" other terminals park the cursor in, so a
	// caller reading a Point does not need to allow for an out-of-range column,
	// and one comparing two Points has to read the flag to tell a cursor resting
	// on the last column from one about to wrap off it.
	//
	// It is an invariant on what a caller can read, not on every assignment: a
	// reflowing resize walks the column past the width on purpose, since a row
	// joined onto the one before it moves the cursor right by the length of what
	// was prepended, and the switch that closes shrinkColumns brings it back
	// inside — which is why that switch tests for a column exactly equal to the
	// width.
	Column int
}

// Charset is a designated graphic character set: what SCS (ESC ( and friends)
// selects, and what SI/SO switch between.
type Charset uint8

// The character sets the grid distinguishes. CharsetASCII draws characters as
// themselves.
//
// These two are the only designations that take effect: ESC ( B and its G1..G3
// forms select ASCII, ESC ( 0 selects line drawing, and every other designation
// falls through the parser's escape dispatch without assigning anything. So an
// unhandled designation — ESC ( A for UK, say — does not fall back to ASCII; it
// leaves the slot designated as whatever it was, which for a slot already
// holding line drawing means the program keeps getting line drawing. Two escapes
// that designate nothing do write the slots: RIS (ESC c) resets all four, and
// DECRC (ESC 8) restores them along with the rest of the saved cursor.
const (
	CharsetASCII Charset = iota
	CharsetLineDrawing
)

// Cursor is the write position plus the attributes new text is drawn with.
// Term keeps four: each of its two screen buffers has a live one and a saved
// one, and only the active buffer's pair is reachable at any moment.
type Cursor struct {
	// Point is where the next character goes.
	Point Point
	// Template is the cell new text is written from: the colors and flags SGR
	// has selected. Its character is unused.
	Template Cell
	// Charsets holds the four designatable slots (G0..G3). The parser tracks
	// which one is active separately, since SI/SO change that without
	// redesignating anything.
	Charsets [4]Charset
	// InputNeedsWrap is set after writing the last column; the next printable
	// character wraps first.
	InputNeedsWrap bool
}

// Grid is one screen buffer: a viewport of lines×columns plus up to
// maxScrollLimit lines of history above it.
type Grid struct {
	// Cursor is the live write position and pen.
	Cursor Cursor
	// SavedCursor is what DECSC stored and DECRC restores. It is per-buffer, so
	// the alternate screen's save and the primary screen's are separate values.
	//
	// The primary screen's does not survive a switch to the alternate screen,
	// though. Entering it (DECSET 1049) overwrites this with the live Cursor
	// before swapping, because saving the cursor is part of what 1049 is defined
	// to do, and leaving does not put back what DECSC had stored earlier. So a
	// DECSC on the primary screen followed by a round trip through the alternate
	// screen and a DECRC restores the position the program was at when it
	// switched, not the one it saved.
	SavedCursor Cursor

	raw            storage
	columns        int
	lines          int
	maxScrollLimit int

	// pushed is the net count of lines that have moved off the top of the
	// screen into history: up for every line scrolled off, down for every line
	// a growing screen pulled back out. It is signed because narrowing the
	// screen can leave more history than was ever pushed, so a grow can reclaim
	// rows no push counted and drive the net below zero. HistoryPushed says what
	// a reader may and may not conclude from it.
	pushed int64
}

func newGrid(lines, columns, maxScrollLimit int) *Grid {
	return &Grid{raw: newStorage(lines, columns), columns: columns, lines: lines, maxScrollLimit: maxScrollLimit}
}

// Columns is the viewport width.
func (g *Grid) Columns() int { return g.columns }

// ScreenLines is the viewport height.
func (g *Grid) ScreenLines() int { return g.lines }

// HistorySize is the number of lines of scrollback above the viewport.
func (g *Grid) HistorySize() int {
	if n := g.raw.len - g.lines; n > 0 {
		return n
	}
	return 0
}

// HistoryPushed is the running count of lines scrolled from the top of the
// screen into history, including lines the history limit has since dropped,
// less lines a taller screen pulled back out of history. A line's distance
// from the live tail changes by exactly the change in this count, so a viewer
// holding a history offset stays on the same content by applying it. Column
// reflow and history clears do not maintain the relationship.
//
// It is signed, and may be negative, because it is a net count rather than a
// total. Narrowing the screen splits wrapped rows, manufacturing history rows
// without pushing anything, so a later grow can reclaim rows no push ever
// counted and carry the net below zero. Widening cannot: it only ever joins
// rows back together, which lowers the history a grow has to draw on.
//
// Only the difference between two readings is meaningful. The absolute value is
// not a row count, and an excursion below zero is recoverable rather than
// short-lived: the rows that caused it are back on screen, and the next lines to
// scroll off carry the count back up through the reading a viewer is holding —
// but a session that goes idle after a resize holds a negative count for as long
// as it stays idle. Nothing needs to wait for it. A viewer differences the
// readings and does not test their sign, so a negative reading and a negative
// difference are both ordinary.
//
// Two things it still does not promise. Neither reflow nor a history clear
// maintains the relationship at all, so across those the difference is only an
// estimate of how far content moved; and the offset a viewer derives has to be
// clamped to the history that actually exists, since a grow can move content
// back onto the screen, where no history offset addresses it.
func (g *Grid) HistoryPushed() int64 { return g.pushed }

// Row returns a line; valid lines are -HistorySize()..ScreenLines()-1.
func (g *Grid) Row(line int) *Row { return g.raw.row(line) }

// Cell returns the cell at p.
func (g *Grid) Cell(p Point) *Cell { return g.raw.row(p.Line).Cell(p.Column) }

func (g *Grid) topmostLine() int    { return -g.HistorySize() }
func (g *Grid) bottommostLine() int { return g.lines - 1 }
func (g *Grid) lastColumn() int     { return g.columns - 1 }

func (g *Grid) cursorCell() *Cell {
	return g.raw.row(g.Cursor.Point.Line).at(g.Cursor.Point.Column)
}

func (g *Grid) increaseScrollLimit(count int) {
	count = min(count, g.maxScrollLimit-g.HistorySize())
	if count != 0 {
		g.raw.initialize(count, g.columns)
	}
}

func (g *Grid) decreaseScrollLimit(count int) {
	count = min(count, g.HistorySize())
	if count != 0 {
		g.raw.shrinkLines(count)
	}
}

// scrollDown moves lines in [start, end) down by positions, clearing at the
// top of the region.
func (g *Grid) scrollDown(start, end, positions int) {
	tmpl := &g.Cursor.Template
	if end-start <= positions {
		for i := start; i < end; i++ {
			g.raw.row(i).reset(tmpl)
		}
		return
	}
	if g.maxScrollLimit == 0 {
		for i := end; i < g.lines; i++ {
			g.raw.swap(i, i-positions)
		}
		g.raw.rotateDown(positions)
		for i := 0; i < positions; i++ {
			g.raw.row(i).reset(tmpl)
		}
		for i := 0; i < start; i++ {
			g.raw.swap(i, i+positions)
		}
		return
	}
	for line := end - 1; line >= start+positions; line-- {
		g.raw.swap(line, line-positions)
	}
	for line := start + positions - 1; line >= start; line-- {
		g.raw.row(line).reset(tmpl)
	}
}

// scrollUp moves lines in [start, end) up by positions. When the region
// starts at the top of the screen the lines scrolled off enter history.
func (g *Grid) scrollUp(start, end, positions int) {
	tmpl := &g.Cursor.Template
	if end-start <= positions && start != 0 {
		for i := start; i < end; i++ {
			g.raw.row(i).reset(tmpl)
		}
		return
	}
	if start == 0 {
		if g.maxScrollLimit > 0 {
			g.pushed += int64(positions)
		}
		g.increaseScrollLimit(positions)
		g.raw.rotate(-positions)
		for i := g.lines - 1; i >= end; i-- {
			g.raw.swap(i, i-positions)
		}
	} else {
		for i := start; i < end-positions; i++ {
			g.raw.swap(i, i+positions)
		}
	}
	for i := end - positions; i < end; i++ {
		g.raw.row(i).reset(tmpl)
	}
}

// clearViewport scrolls the used part of the viewport into history and
// blanks the screen (ED 2 on the primary screen).
func (g *Grid) clearViewport() {
	positions := g.usedLines()
	g.scrollUp(0, g.lines, positions)
	for line := 0; line < g.lines-positions; line++ {
		g.raw.row(line).reset(&g.Cursor.Template)
	}
}

// usedLines is the number of viewport lines up to and including the last one
// holding a non-empty cell: alacritty's cell-by-cell walk back from the
// bottom-right corner (grid/mod.rs clear_viewport), a row at a time. When
// every viewport cell is empty the walk stops on the last history line (0)
// or, without history, on the first cell (1).
func (g *Grid) usedLines() int {
	for line := g.lines - 1; line >= 0; line-- {
		inner := g.raw.row(line).inner
		cells := inner[:g.columns:len(inner)] // panics, like Cell, if a row is short
		for i := len(cells) - 1; i >= 0; i-- {
			if !cells[i].isEmpty() {
				return line + 1
			}
		}
	}
	if g.topmostLine() < 0 {
		return 0
	}
	return 1
}

func (g *Grid) reset() {
	g.clearHistory()
	g.SavedCursor = Cursor{}
	g.Cursor = Cursor{}
	for line := g.topmostLine(); line < g.lines; line++ {
		g.raw.row(line).reset(&g.Cursor.Template)
	}
}

// resetRegion blanks viewport lines [start, end).
func (g *Grid) resetRegion(start, end int) {
	for line := start; line < end; line++ {
		g.raw.row(line).reset(&g.Cursor.Template)
	}
}

func (g *Grid) clearHistory() {
	g.raw.shrinkLines(g.HistorySize())
}

// clampCursorBoundary is Point::grid_clamp(_, Boundary::Cursor).
func (g *Grid) clampCursorBoundary(p Point) Point {
	p.Column = min(p.Column, g.lastColumn())
	if p.Line < 0 {
		return Point{}
	}
	if p.Line > g.bottommostLine() {
		return Point{g.bottommostLine(), g.lastColumn()}
	}
	return p
}

// subCursorBoundary is Point::sub(_, Boundary::Cursor, rhs).
func (g *Grid) subCursorBoundary(p Point, rhs int) Point {
	cols := g.columns
	lineChanges := max(rhs+cols-1-p.Column, 0) / cols
	p.Line -= lineChanges
	p.Column = (cols + p.Column - rhs%cols) % cols
	return g.clampCursorBoundary(p)
}
