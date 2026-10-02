package vt

import "testing"

// A full reset must leave no cell of any row carrying an old attribute.
//
// Row.reset clears only inner[:occ], and takes the row's last cell as evidence
// for what lies at or past occ. Widening appends default cells, so that last
// cell reads as default while the row's earlier cells still carry a
// back-colour-erase background — and the reset then clears neither.
//
// The surviving cells are blanks, so a glyph comparison cannot see them; they
// are not isEmpty(), so a later narrowing wraps rows that hold no text and
// manufactures history lines out of them.
func TestResetAfterWidenClearsBackColourErase(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 34, 18, nil), NewProcessor()
	feed(term, p, "\x1b[104m\x1b[J") // back-colour erase: bright cyan to the end of the screen
	term.Resize(40, 18)              // widen: the appended cells are default, the rest are not
	feed(term, p, "\x1bc")           // RIS

	g := term.Grid()
	stale := 0
	first := Point{-1, -1}
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		for c := 0; c < row.Len(); c++ {
			if *row.Cell(c) != (Cell{}) {
				stale++
				if first.Line < 0 {
					first = Point{line, c}
				}
			}
		}
	}
	if stale != 0 {
		t.Errorf("RIS left %d cells set; first at line %d column %d with bg %v",
			stale, first.Line, first.Column, g.Row(first.Line).Cell(first.Column).Bg())
	}
}

// The same surviving cells must not turn into history, and must not cost the
// grid the one character it does hold. After a reset the screen is one 'x', so
// narrowing has nothing to wrap and nothing to lose; blanks left behind by the
// reset would both invent rows and push that character off the top.
func TestResetAfterWidenLeavesNoHistoryOnNarrowing(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 34, 18, nil), NewProcessor()
	feed(term, p, "\x1b[104m\x1b[J")
	term.Resize(40, 18)
	feed(term, p, "\x1bcx")
	term.Resize(20, 18)

	g := term.Grid()
	if got := g.HistorySize(); got != 0 {
		t.Errorf("narrowing a reset grid holding one character pushed %d rows into history", got)
	}
	var text []rune
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		for c := 0; c < row.Len(); c++ {
			if ch := row.Cell(c).Rune(); ch != 0 && ch != ' ' {
				text = append(text, ch)
			}
		}
	}
	if string(text) != "x" {
		t.Errorf("narrowing left the glyphs %q, want the one character written", string(text))
	}
}
