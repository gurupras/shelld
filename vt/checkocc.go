package vt

import "fmt"

// OccViolation is one row whose occupancy bound does not hold: the cell at
// Column, which is at or past the row's occ, differs from the row's last cell.
type OccViolation struct {
	// Line is the row's grid line, negative for history, as Grid.Row takes.
	Line int
	// Column is the first cell at or past occ that differs from the last cell.
	// It is zero and means nothing when OutOfRange is set, for the same reason
	// Cell and Last are: the row has no such cell.
	Column int
	// Occ is the row's occupancy bound, and Columns its length.
	Occ, Columns int
	// Cell is the offending cell and Last the row's last cell, which every
	// cell at or past Occ is supposed to equal. Both are zero when OutOfRange
	// is set: there is no cell at Column to report.
	Cell, Last Cell
	// OutOfRange reports the other way the bound can fail: occ past the row's
	// length, so it names no cells at all.
	OutOfRange bool
}

// String describes the violation in terms of the offending cell and the row's
// last cell, which is what occ promised every cell past it would equal.
func (v OccViolation) String() string {
	if v.OutOfRange {
		return fmt.Sprintf("line %d: occ %d is past the row's %d columns", v.Line, v.Occ, v.Columns)
	}
	return fmt.Sprintf("line %d column %d (occ %d of %d): %s, but the row's last cell is %s",
		v.Line, v.Column, v.Occ, v.Columns, describeCell(v.Cell), describeCell(v.Last))
}

func describeCell(c Cell) string {
	ch := c.Rune()
	if ch == 0 {
		ch = ' '
	}
	// Flags is converted before being formatted as a number. fmt calls a
	// Stringer for %x and %q as well as for %v, so %#x on a Flags prints the
	// hex of its name: Flags(0) comes out as 0x30, which reads as a real flag
	// set (Wrapline|WideChar). Both forms are printed because a name is what a
	// human compares and the bits are what a bitwise diff needs.
	return fmt.Sprintf("%q fg %v bg %v flags %v (%#04x)", ch, c.Fg(), c.Bg(), c.Flags(), uint16(c.Flags()))
}

// CheckOcc reports every row of the terminal's grid whose occupancy bound does
// not hold, over the history as well as the screen.
//
// Row.occ is an upper bound on the cells modified since the last reset, and
// what makes it usable is that every cell at or past it equals the row's last
// cell. Row.reset relies on that to clear only [0, occ) after reading the last
// cell; anything that leaves a row with cells past occ that the last cell does
// not stand for makes a reset skip them, and blanks carrying an old background
// then survive where no glyph comparison can see them.
//
// It reads only through the exported accessors, so it is a check a test can run
// after any operation rather than a property of one code path.
//
// CheckOcc and OccViolation are a test-only diagnostic, exported because they
// read the unexported occ and the root package's tests need them across the
// package boundary. Nothing in production calls them.
func CheckOcc(t *Term) []OccViolation {
	g := t.Grid()
	var out []OccViolation
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		n := row.Len()
		if n == 0 {
			continue
		}
		if int(row.occ) > n {
			out = append(out, OccViolation{Line: line, Column: n, Occ: int(row.occ), Columns: n, OutOfRange: true})
			continue
		}
		last := *row.Cell(n - 1)
		for c := int(row.occ); c < n; c++ {
			if cell := *row.Cell(c); cell != last {
				out = append(out, OccViolation{
					Line: line, Column: c, Occ: int(row.occ), Columns: n,
					Cell: cell, Last: last,
				})
				break
			}
		}
	}
	return out
}
