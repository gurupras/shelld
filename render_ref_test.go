package shelld

// A frozen copy of the snapshot and render code of main at 81ce82f (grid.go
// after the fix/stale-cells merge, 470f31f, which pairs wide-character
// halves), so the go-render optimizations can be checked byte for byte
// against it. The names take a ref prefix, and CellSlot's Wide, WideSpacer
// and sameStyle methods are written as attribute tests and refSameStyle.
// Re-frozen from that grid.go when rebasing onto the merge; the earlier copy
// froze ef5aa67. refAppendRune also carries fix/render-tab's rule (control
// characters render as spaces), re-frozen when that merged (45db28f).
// Combining marks were added with fix/combining-marks: a slot carries the
// cell's mark id in Attrs' top 16 bits, as CellSlot does, and the marks are
// drawn after a base drawn as itself.
// Change this file only together with an intended change of the rendered
// bytes.

import (
	"strconv"
	"unsafe"

	"github.com/gurupras/shelld/vt"
)

const (
	refAttrBold uint32 = 1 << iota
	refAttrDim
	refAttrItalic
	refAttrUnderline
	refAttrInverse
	refAttrStrike
	refAttrHidden
	refAttrWideSpacer
	refAttrWide
)

const (
	refAttrMarkShift = 16
	refAttrMarks     = uint32(0xffff) << refAttrMarkShift
)

const refStyleMask = refAttrBold | refAttrDim | refAttrItalic | refAttrUnderline | refAttrInverse | refAttrStrike | refAttrHidden

var refPadSlot = CellSlot{Fg: vt.NewNamedColor(vt.Foreground), Bg: vt.NewNamedColor(vt.Background)}

const refWideFlags = vt.FlagWideChar | vt.FlagWideCharSpacer

// refPairWide blanks the half of a wide character that lost its partner in a
// (cropped) snapshot row: a wide character without its spacer becomes a
// space, and an orphan spacer loses its spacer flag.
func refPairWide(row []CellSlot) {
	for c := range row {
		switch {
		case (row[c].Attrs&refAttrWide != 0) && (c+1 == len(row) || !(row[c+1].Attrs&refAttrWideSpacer != 0)):
			row[c].Ch = ' '
			row[c].Attrs &^= refAttrWide | refAttrMarks
		case (row[c].Attrs&refAttrWideSpacer != 0) && (c == 0 || !(row[c-1].Attrs&refAttrWide != 0)):
			row[c].Attrs &^= refAttrWideSpacer
		}
	}
}

// refWideHalf applies the rule to cell c of a row cropped to cols: skip is a
// spacer that follows its wide character (drawn with it), blank is a wide
// character without its spacer.
func refWideHalf(row *vt.Row, c, cols int) (skip, blank bool) {
	switch row.Cell(c).Flags() & refWideFlags {
	case vt.FlagWideCharSpacer:
		return c > 0 && row.Cell(c-1).Flags()&vt.FlagWideChar != 0, false
	case vt.FlagWideChar:
		return false, c+1 == cols || row.Cell(c+1).Flags()&vt.FlagWideCharSpacer == 0
	}
	return false, false
}

func refSameStyle(s, o CellSlot) bool {
	return s.Fg == o.Fg && s.Bg == o.Bg && s.Attrs&refStyleMask == o.Attrs&refStyleMask
}

func refSlotFromCell(c *vt.Cell) CellSlot {
	f := c.Flags()
	var a uint32
	if f&vt.FlagBold != 0 {
		a |= refAttrBold
	}
	if f&vt.FlagDim != 0 {
		a |= refAttrDim
	}
	if f&vt.FlagItalic != 0 {
		a |= refAttrItalic
	}
	if f&vt.AllUnderlines != 0 {
		a |= refAttrUnderline
	}
	if f&vt.FlagInverse != 0 {
		a |= refAttrInverse
	}
	if f&vt.FlagStrikeout != 0 {
		a |= refAttrStrike
	}
	if f&vt.FlagHidden != 0 {
		a |= refAttrHidden
	}
	if f&vt.FlagWideCharSpacer != 0 {
		a |= refAttrWideSpacer
	}
	if f&vt.FlagWideChar != 0 {
		a |= refAttrWide
	}
	a |= uint32(c.MarkID()) << refAttrMarkShift
	ch := c.Rune()
	if ch == 0 {
		ch = ' '
	}
	return CellSlot{Ch: ch, Fg: c.Fg(), Bg: c.Bg(), Attrs: a}
}

func refSlotsEqual(a, b []CellSlot) bool {
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

func refSnapshotInto(dst *GridSnapshot, term *vt.Term, offset, outCols, outRows int) *GridSnapshot {
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
	for r := 0; r < outRows; r++ {
		out := dst.Cells[r*outCols : (r+1)*outCols]
		c := 0
		if r < copyRows {
			row := grid.Row(r - offset)
			var flags vt.Flags
			for ; c < copyCols; c++ {
				cell := row.Cell(c)
				flags |= cell.Flags()
				out[c] = refSlotFromCell(cell)
			}
			if flags&refWideFlags != 0 {
				refPairWide(out[:copyCols])
			}
		}
		for ; c < outCols; c++ {
			out[c] = refPadSlot
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

func refIsBlank(c *vt.Cell) bool {
	ch := c.Rune()
	return (ch == ' ' || ch == 0) && c.MarkID() == 0 &&
		c.Bg() == vt.NewNamedColor(vt.Background) &&
		c.UnderlineColor() == vt.NoColor &&
		c.Flags()&(vt.FlagInverse|vt.AllUnderlines|vt.FlagStrikeout) == 0
}

func refAppendCUP(out []byte, row, col int) []byte {
	out = append(out, "\x1b["...)
	out = strconv.AppendInt(out, int64(row), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(col), 10)
	return append(out, 'H')
}

func refRenderFull(out []byte, term *vt.Term, offset, outCols, outRows int) []byte {
	grid := term.Grid()
	copyRows := min(outRows, grid.ScreenLines())
	copyCols := min(outCols, grid.Columns())

	out = append(out, "\x1b[?25l\x1b[2J\x1b[H\x1b[0m\x1b[0m"...)
	cur := refPadSlot // default style
	for r := 0; r < copyRows; r++ {
		if r > 0 {
			out = append(out, "\r\n"...)
		}
		row := grid.Row(r - offset)
		last := -1
		for c := 0; c < copyCols; c++ {
			if !refIsBlank(row.Cell(c)) {
				last = c
			}
		}
		for c := 0; c <= last; c++ {
			cell := row.Cell(c)
			slot := refSlotFromCell(cell)
			if cell.Flags()&refWideFlags != 0 {
				skip, blank := refWideHalf(row, c, copyCols)
				if skip {
					continue
				}
				if blank {
					slot.Ch = ' '
					slot.Attrs &^= refAttrMarks
				}
			}
			if !refSameStyle(slot, cur) {
				out = refAppendSGR(out, slot)
				cur = slot
			}
			out = refAppendRune(out, slot.Ch)
			if slot.Attrs&refAttrMarks != 0 {
				out = refAppendMarks(out, slot.Ch, term.Marks(cell.MarkID()))
			}
		}
	}
	out = append(out, "\x1b[0m"...)

	if offset > 0 {
		return append(out, "\x1b[1;1H"...)
	}
	p := grid.Cursor.Point
	row := min(max(p.Line, 0), max(outRows-1, 0))
	col := min(p.Column, max(outCols-1, 0))
	out = refAppendCUP(out, row+1, col+1)
	// The frame began by hiding the cursor; show it again only if the
	// application has not hidden it, matching what a snapshot records.
	if term.Mode()&vt.ModeShowCursor != 0 {
		out = append(out, "\x1b[?25h"...)
	}
	return out
}

func refRenderDiff(out []byte, prev, cur *GridSnapshot, term *vt.Term, offset int) []byte {
	live := offset == 0
	if prev.Cols != cur.Cols || prev.Rows != cur.Rows || len(prev.Cells) == 0 {
		return refRenderFull(out, term, offset, cur.Cols, cur.Rows)
	}
	cellsEqual := refSlotsEqual(prev.Cells, cur.Cells)
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
			if refSlotsEqual(curRow, prevRow) {
				continue
			}
			for c := 0; c < cols; {
				cs := curRow[c]
				if cs.Attrs&refAttrWideSpacer != 0 {
					c++
					continue
				}
				if cs == prevRow[c] {
					if cs.Attrs&refAttrWide != 0 {
						c += 2
					} else {
						c++
					}
					continue
				}
				if atRow != r || atCol != c {
					out = refAppendCUP(out, r+1, c+1)
					atRow, atCol = r, c
				}
				if !styleKnown || !refSameStyle(cs, style) {
					out = refAppendSGR(out, cs)
					style = cs
					styleKnown = true
				}
				ch := cs.Ch
				if ch == 0 {
					ch = ' '
				}
				out = refAppendRune(out, ch)
				out = refAppendMarks(out, ch, cur.Marks(cs))
				anyEmitted = true
				adv := 1
				if cs.Attrs&refAttrWide != 0 {
					adv = 2
				}
				atCol += adv
				c += adv
			}
		}
	}

	if !live {
		if cur.CursorVisible != prev.CursorVisible {
			out = refAppendCursorVisibility(out, cur.CursorVisible)
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
	out = refAppendCUP(out, cur.CursorRow+1, cur.CursorCol+1)
	if needsWrap {
		if cur.CursorVisible {
			out = append(out, "\x1b[?25h"...)
		}
	} else if cur.CursorVisible != prev.CursorVisible {
		out = refAppendCursorVisibility(out, cur.CursorVisible)
	}
	return out
}

func refAppendCursorVisibility(out []byte, visible bool) []byte {
	if visible {
		return append(out, "\x1b[?25h"...)
	}
	return append(out, "\x1b[?25l"...)
}

// refAppendMarks draws marks after base r, unless r was drawn as a space.
func refAppendMarks(out []byte, r rune, marks []rune) []byte {
	if r < 0x20 || r == 0x7f {
		return out
	}
	return append(out, string(marks)...)
}

func refAppendRune(out []byte, r rune) []byte {
	if r < 0x20 || r == 0x7f {
		// Re-frozen from main's appendRune after the fix/render-tab merge
		// (6708836): control characters render as spaces.
		return append(out, ' ')
	}
	if r < 0x80 {
		return append(out, byte(r))
	}
	return append(out, string(r)...)
}

// refAppendSGR resets and re-applies a cell's style. Always resetting first is
// shorter than a minimal transition for typical screens.
func refAppendSGR(out []byte, s CellSlot) []byte {
	out = append(out, "\x1b[0m"...)
	if s.Attrs&refAttrBold != 0 {
		out = append(out, "\x1b[1m"...)
	}
	if s.Attrs&refAttrDim != 0 {
		out = append(out, "\x1b[2m"...)
	}
	if s.Attrs&refAttrItalic != 0 {
		out = append(out, "\x1b[3m"...)
	}
	if s.Attrs&refAttrUnderline != 0 {
		out = append(out, "\x1b[4m"...)
	}
	if s.Attrs&refAttrInverse != 0 {
		out = append(out, "\x1b[7m"...)
	}
	if s.Attrs&refAttrHidden != 0 {
		out = append(out, "\x1b[8m"...)
	}
	if s.Attrs&refAttrStrike != 0 {
		out = append(out, "\x1b[9m"...)
	}
	out = refAppendColor(out, s.Fg, true)
	return refAppendColor(out, s.Bg, false)
}

func refAppendColor(out []byte, c vt.Color, fg bool) []byte {
	switch c.Kind() {
	case vt.KindNamed:
		code, ok := refNamedToSGR(c.Named(), fg)
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

func refNamedToSGR(n vt.NamedColor, fg bool) (int, bool) {
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
