package vt

import (
	"fmt"
	"strings"
	"testing"
)

// TestHistoryStoresOnlyContent is the reason Row.compact exists, stated as the
// cells the history holds: short lines scrolled off a wide screen keep arrays
// about their own length, not the screen's. A history stored a full row each
// holds 200 cells per line here, against the 37 the line has.
func TestHistoryStoresOnlyContent(t *testing.T) {
	const line = "\x1b[32m2026-09-15 20:00:00\x1b[0m INFO line" // 29 cells on screen
	term, p := NewTerm(Config{ScrollingHistory: 2000}, 200, 50, nil), NewProcessor()
	for i := range 5000 {
		feed(term, p, fmt.Sprintf("%s %06d\r\n", line, i))
	}
	g := term.Grid()
	if g.HistorySize() != 2000 {
		t.Fatalf("history holds %d lines, want 2000", g.HistorySize())
	}
	const want = 36 // the line's 29 cells and " 000000"
	var stored, capacity int
	for l := -g.HistorySize(); l < 0; l++ {
		r := g.Row(l)
		if r.Len() != want {
			t.Fatalf("history line %d stores %d cells, want %d", l, r.Len(), want)
		}
		stored += r.Len()
		capacity += cap(r.inner)
	}
	// Recycled arrays may be up to compactReuseFactor times what they carry.
	if capacity > compactReuseFactor*stored {
		t.Errorf("history arrays hold %d cells of capacity for %d stored", capacity, stored)
	}
	for l := 0; l < g.ScreenLines(); l++ {
		if n := g.Row(l).Len(); n != g.Columns() {
			t.Errorf("viewport line %d stores %d cells, want %d", l, n, g.Columns())
		}
	}
	t.Logf("%d cells stored, %d capacity, over %d lines", stored, capacity, g.HistorySize())
}

// TestCompactKeepsWhatItCannotDrop: compact drops only a run of zero cells, and
// never one that would leave a FlagWrapline cell last, since growColumns reads
// that flag off the last cell a row stores.
func TestCompactKeepsWhatItCannotDrop(t *testing.T) {
	text := func(s string, n int) Row {
		r := newRow(n)
		for i, c := range s {
			r.at(i).SetRune(c)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		row  func() Row
		want int
	}{
		{"short content", func() Row { return text("abc", 10) }, 3},
		{"blank", func() Row { return newRow(10) }, 0},
		{"full", func() Row { return text("abcdefghij", 10) }, 10},
		{"coloured blanks", func() Row {
			r := text("abc", 10)
			fillBlank(r.span(3, 10), NewIndexedColor(4)^defaultBg)
			return r
		}, 10},
		{"wrapline before zeros", func() Row {
			r := text("abc", 10)
			r.at(2).flags |= FlagWrapline
			return r
		}, 10},
		{"empty", func() Row { return Row{} }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.row()
			before := make([]Cell, r.Len())
			copy(before, r.inner)
			r.compact(nil)
			if r.Len() != tc.want {
				t.Fatalf("compacted to %d cells, want %d", r.Len(), tc.want)
			}
			for c := range before {
				if *r.Cell(c) != before[c] {
					t.Fatalf("column %d reads %+v after compact, was %+v", c, *r.Cell(c), before[c])
				}
			}
			if int(r.occ) > r.Len() {
				t.Errorf("occ %d past the %d cells stored", r.occ, r.Len())
			}
		})
	}
}

// TestShortSlotEntersViewport: on a screen without history, scrolling down
// rotates ring slots into the top of the viewport, and after a resize to a
// taller screen those can be slots the ring grew by, which store no cells. A
// write to the line they land on has to find a full row.
func TestShortSlotEntersViewport(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 1000}, 80, 10, nil), NewProcessor()
	feed(term, p, "\x1b[?1049h")
	term.Resize(80, 20)
	feed(term, p, "\x1b[H\x1bM\x1bMhello\x1b[2T\x1b[1;1Hx")
	g := term.Grid()
	for l := 0; l < g.ScreenLines(); l++ {
		if n := g.Row(l).Len(); n != 80 {
			t.Errorf("viewport line %d stores %d cells", l, n)
		}
	}
	if got := g.Row(2).Cell(0).Rune(); got != 'h' {
		t.Errorf("line 2 starts with %q, want 'h'", got)
	}
}

// TestNarrowedHistoryGivesBackItsArrays: a narrowing leaves a history row on
// the array of the width it had, up to shrinkKeepFactor times the new one, for
// a widen to reuse; a history row is never widened, so normalisation moves it
// to an array of its own length.
func TestNarrowedHistoryGivesBackItsArrays(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 2000}, 200, 24, nil), NewProcessor()
	feed(term, p, strings.Repeat(strings.Repeat("x", 199)+"\x1b[44m \x1b[0m\r\n", 500))
	term.Resize(80, 24)
	g := term.Grid()
	for l := -g.HistorySize(); l < 0; l++ {
		r := g.Row(l)
		if cap(r.inner) > compactReuseFactor*r.Len() {
			t.Fatalf("history line %d holds %d cells of capacity for %d", l, cap(r.inner), r.Len())
		}
	}
}

// TestScrollReusesFreedArrays: once the history is full, scrolling trades
// arrays with the lines it evicts (see rowPool), so lines whose lengths recur
// cost no allocation however their shapes alternate. A wrapped line pushes a
// full row and evicts a short one, and a short line the reverse; paired one for
// one, each such push freed one array and allocated another, a full row's worth
// for every wrapped line.
func TestScrollReusesFreedArrays(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 300}, 80, 24, nil), NewProcessor()
	cycle := []byte(strings.Repeat("w", 100) + "\r\nshort\r\n" + strings.Repeat("m", 40) + "\r\n")
	for range 400 {
		p.Advance(term, cycle)
	}
	if h := term.Grid().HistorySize(); h != 300 {
		t.Fatalf("history holds %d lines, want 300", h)
	}
	if n := testing.AllocsPerRun(200, func() { p.Advance(term, cycle) }); n != 0 {
		t.Errorf("scrolling a full history allocates %.1f times per cycle of four lines, want 0", n)
	}
}

// TestRecycledArraysAreNotShared: an array the pool hands out belongs to the
// one row that takes it. Lines of many lengths, some wrapping, keep the pool
// trading arrays of every size; were one handed to two rows, a later line
// would overwrite an earlier one's cells, which the text of every history line
// shows.
func TestRecycledArraysAreNotShared(t *testing.T) {
	const cols, hist = 120, 400
	term, p := NewTerm(Config{ScrollingHistory: hist}, cols, 10, nil), NewProcessor()
	var want []string // one entry per screen row each line takes
	for i := range 3000 {
		s := fmt.Sprintf("%05d%s", i, strings.Repeat(string(rune('a'+i%26)), (i*37)%170))
		feed(term, p, s+"\r\n")
		for ; len(s) > cols; s = s[cols:] {
			want = append(want, s[:cols])
		}
		want = append(want, s)
	}
	g := term.Grid()
	if g.HistorySize() != hist {
		t.Fatalf("history holds %d lines, want %d", g.HistorySize(), hist)
	}
	// The cursor sits on the blank row after the last line, at the bottom.
	want = want[len(want)-hist-(g.ScreenLines()-1):]
	for l := -hist; l < g.ScreenLines()-1; l++ {
		if got := rowText(term, l); got != want[l+hist] {
			t.Fatalf("line %d reads %q, want %q", l, got, want[l+hist])
		}
	}
}
