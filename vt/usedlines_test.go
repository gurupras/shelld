package vt

import (
	"math/rand"
	"strings"
	"testing"
)

// usedLinesWalk is alacritty's clear_viewport scan, cell by cell from the
// bottom-right corner (grid/mod.rs), kept as the reference for usedLines.
func usedLinesWalk(g *Grid) int {
	top := Point{g.topmostLine(), 0}
	p := Point{g.lines - 1, g.columns}
	less := func(a, b Point) bool { return a.Line < b.Line || (a.Line == b.Line && a.Column < b.Column) }
	for less(top, p) {
		if p.Column == 0 {
			p.Column = g.lastColumn()
			p.Line--
		} else {
			p.Column--
		}
		if !g.Cell(p).isEmpty() || p.Line < 0 {
			break
		}
	}
	return p.Line + 1
}

func TestUsedLinesMatchesCellWalk(t *testing.T) {
	for seed := int64(1); seed <= 2000; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cols, lines := 1+rng.Intn(10), 1+rng.Intn(6)
		term := NewTerm(Config{ScrollingHistory: 4}, cols, lines, nil)
		g := term.Grid()
		if rng.Intn(2) == 0 {
			feed(term, NewProcessor(), strings.Repeat("h\r\n", 1+rng.Intn(8)))
			g.resetRegion(0, lines)
		}
		// Sparse cells that are empty (space, tab, default) or not (text, a
		// background, a flag that counts, or one that does not).
		for range rng.Intn(4) {
			c := g.Row(rng.Intn(lines)).at(rng.Intn(cols))
			switch rng.Intn(6) {
			case 0:
				c.SetRune('x')
			case 1:
				c.SetRune('\t')
			case 2:
				c.SetBg(NewNamedColor(Blue))
			case 3:
				c.flags |= FlagWrapline
			case 4:
				c.flags |= FlagBold
			case 5:
				c.ul = NewNamedColor(Red)
			}
		}
		if got, want := g.usedLines(), usedLinesWalk(g); got != want {
			t.Fatalf("seed %d (%dx%d, history %d): usedLines %d, cell walk %d", seed, cols, lines, g.HistorySize(), got, want)
		}
	}
}
