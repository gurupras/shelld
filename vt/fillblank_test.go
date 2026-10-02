package vt

import (
	"strings"
	"testing"
)

// Erasing with the default background leaves zero cells; with a colored one,
// blanks carrying it.
func TestEraseFillsBlanks(t *testing.T) {
	for _, bg := range []struct {
		sgr  string
		want Color
	}{{"\x1b[49m", defaultBg}, {"\x1b[41m", NewNamedColor(Red)}, {"\x1b[48;5;200m", NewIndexedColor(200)}} {
		for _, seq := range []string{"\x1b[K", "\x1b[1K", "\x1b[2K", "\x1b[4X", "\x1b[3P", "\x1b[2@", "\x1b[J", "\x1b[1J"} {
			term, p := NewTerm(Config{}, 12, 3, nil), NewProcessor()
			feed(term, p, "\x1b[1;4;35mabcdefghijkl\r\nmnopqrstuvwx\r\nyz0123456789\x1b[0m\x1b[2;5H"+bg.sgr+seq)
			g := term.Grid()
			blanks := 0
			for line := 0; line < 3; line++ {
				for col := 0; col < 12; col++ {
					c := g.Row(line).Cell(col)
					if c.Rune() != ' ' {
						continue
					}
					blanks++
					if c.Bg() != bg.want || c.Fg() != defaultFg || c.Flags() != 0 || c.UnderlineColor() != NoColor {
						t.Fatalf("%q %q: blank at %d,%d = %+v", bg.sgr, seq, line, col, *c)
					}
					if bg.want == defaultBg && *c != (Cell{}) {
						t.Fatalf("%q %q: default blank at %d,%d is not the zero cell: %+v", bg.sgr, seq, line, col, *c)
					}
				}
			}
			if blanks == 0 {
				t.Fatalf("%q %q erased nothing", bg.sgr, seq)
			}
		}
	}
}

// A colored fill writes its seed a cell at a time and everything past the seed
// with copy, and the erase tests above run at 12 columns, which never reaches the
// copy. Every length around the seed and its doublings is checked here, against
// cells that start dirty so a cell the fill skips reads as the dirty value rather
// than as a blank that happened to be there, and with a guard cell past the span
// so an overrun is visible too. The lengths are derived from the seed rather than
// written out, so changing it moves them instead of making them miss.
func TestFillBlankAroundTheSeed(t *testing.T) {
	bg := NewIndexedColor(200)
	want := Cell{bg: bg}
	dirty := Cell{ch: 'x', fg: 9, bg: 7, flags: FlagBold}
	lengths := []int{0, 1, 2, 3, 200, 401}
	for _, n := range []int{fillBlankSeed, 2 * fillBlankSeed, 4 * fillBlankSeed} {
		lengths = append(lengths, n-1, n, n+1)
	}
	for _, n := range lengths {
		cells := make([]Cell, n+1)
		for i := range cells {
			cells[i] = dirty
		}
		fillBlank(cells[:n], bg)
		for i := range n {
			if cells[i] != want {
				t.Fatalf("length %d: cell %d is %+v, want %+v", n, i, cells[i], want)
			}
		}
		if cells[n] != dirty {
			t.Fatalf("length %d: the fill ran one cell past its span", n)
		}
	}
}

// And once through the erase path at a width that reaches the copy, so that it is
// reached the way a caller reaches it and not only directly. The width is checked
// against the seed rather than assumed to exceed it: with a seed of 200 or more
// this test would pass on a fill that never copied anything, which is precisely
// the failure its name is supposed to rule out.
func TestEraseWidthReachesTheCopy(t *testing.T) {
	const cols = 200
	if cols <= fillBlankSeed {
		t.Fatalf("a seed of %d covers the whole %d-column span, so this test no longer reaches the copy", fillBlankSeed, cols)
	}
	term, p := NewTerm(Config{}, cols, 2, nil), NewProcessor()
	feed(term, p, strings.Repeat("a", cols)+"\x1b[1;1H\x1b[41m\x1b[K")
	for col := range cols {
		if c := term.Grid().Row(0).Cell(col); c.Rune() != ' ' || c.Bg() != NewNamedColor(Red) {
			t.Fatalf("column %d after EL: rune %q bg %v", col, c.Rune(), c.Bg())
		}
	}
}

// Every assertion above compares cells, and a store per cell produces the same cells
// as the doubling: that equivalence is the point of the optimisation, and it is also
// what makes the optimisation invisible. Replacing the doubling loop with one store per
// cell leaves this file and the rest of the package green while costing 1.92x on a
// 400-column colored erase, and deleting the default background's memclr so that both
// backgrounds take the doubling leaves it green too, on a path no colored erase measures.
// So this counts what the loop did, through fillBlankCopies.
//
// The schedule is derived from the seed rather than written out, for the reason
// TestEraseWidthReachesTheCopy states: a test that hardcoded it would pass on a seed
// that covers the whole span and so never copies. What is written out is the demand
// that the longest span here take several copies, which is what a seed grown past the
// spans would break.
func TestFillBlankDoublesInsteadOfStoring(t *testing.T) {
	// The seed itself is a measured constant and not a free parameter, and no
	// assertion on results can see its value: every seed from 1 to 1100 fills the
	// same cells. Both directions cost. Doubling from one cell pays a memmove call
	// per few cells and runs 30% slower at a 32-cell span than the seed here; a seed past
	// the span never doubles at all, which is the whole optimisation; and the sweep
	// that picked this value found a seed of 64 between 12 and 28% slower from 48 cells
	// up, and 128 worse again. Each of those is a share of the slower of the two arms,
	// the convention fillBlank's comment uses, and that comment holds the measurements
	// behind the bounds. So this is a tripwire.
	//
	// It is checked before anything below fills a span, so that an unmeasured seed
	// fails here rather than as a wall of wrong copy counts.
	if fillBlankSeed < 8 || fillBlankSeed > 32 {
		t.Fatalf("fillBlankSeed is %d, outside the 8 to 32 the sweep in fillBlank's comment measures as within reach of the optimum: a seed outside it needs its own measurement, not this test's blessing", fillBlankSeed)
	}

	bg := NewIndexedColor(200)
	copies := 0
	fillBlankCopies = &copies
	defer func() { fillBlankCopies = nil }()

	// want is the doubling schedule: after the seed, each copy doubles what is filled
	// until the span is covered.
	want := func(n int) int {
		c := 0
		for filled := min(n, fillBlankSeed); filled > 0 && filled < n; filled += filled {
			c++
		}
		return c
	}
	for _, n := range []int{0, 1, fillBlankSeed - 1, fillBlankSeed, fillBlankSeed + 1, 3 * fillBlankSeed, 8 * fillBlankSeed} {
		copies = 0
		fillBlank(make([]Cell, n), bg)
		if copies != want(n) {
			t.Errorf("filling %d colored cells made %d copies, want %d: the span past the %d-cell seed was not doubled", n, copies, want(n), fillBlankSeed)
		}
	}
	if w := want(8 * fillBlankSeed); w < 3 {
		t.Errorf("the longest span here needs only %d copies, so this test barely reaches the doubling: the seed of %d has grown past the spans", w, fillBlankSeed)
	}

	// And the default background never reaches the doubling: it is a clear, which is
	// the faster path and the one every erase carrying no color takes.
	copies = 0
	fillBlank(make([]Cell, 8*fillBlankSeed), 0)
	if copies != 0 {
		t.Errorf("filling %d default-background cells made %d copies, want 0: the fill is doubling blanks where it could clear memory", 8*fillBlankSeed, copies)
	}
}

// The zero Cell is the default blank, so clearing memory and writing
// Cell{bg: template.bg} with the default background produce the same cell.
func TestZeroCellIsDefaultBlank(t *testing.T) {
	var z Cell
	if z.Rune() != ' ' || z.Fg() != defaultFg || z.Bg() != defaultBg || z.UnderlineColor() != NoColor || z.Flags() != 0 || !z.isEmpty() {
		t.Fatalf("zero cell %+v: ch %q fg %x bg %x", z, z.Rune(), z.Fg(), z.Bg())
	}
	var tmpl Cell
	tmpl.SetBg(defaultBg)
	if (Cell{bg: tmpl.bg}) != z {
		t.Fatal("a blank with the default background is not the zero cell")
	}
	tmpl.SetBg(NewNamedColor(Red))
	if (Cell{bg: tmpl.bg}) == z || tmpl.bg == 0 {
		t.Fatal("a colored background encodes as zero")
	}
}
