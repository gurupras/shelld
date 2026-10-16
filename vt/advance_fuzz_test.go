package vt

import "testing"

// Advance is the single entry point for bytes the terminal has no control over:
// a PTY carries whatever the program on the other end decides to emit, which
// includes the output of a program that is itself malfunctioning. The corpora in
// this package are reasoned about and deliberately narrow -- the grid in
// partialutf8_test.go is four leads by eleven gaps by fourteen tails, chosen from
// what recorded sessions actually contained -- so what they cannot cover is the
// shape nobody thought to write down. These two targets hand that job to a
// fuzzer, one property each.

// A write boundary must not change the terminal state a stream produces.
//
// The oracle is assertSplitInvariant, which is already exhaustive over two-way
// splits, so the fuzzer explores content while the check stays complete for every
// input it is given. That costs work quadratic in the length, which is what the
// cap is for: at 192 bytes one execution builds at most 192 terminals, which is
// slow enough to matter and fast enough for the fuzzer to make progress.
//
// Long inputs are truncated rather than skipped, so that a mutation that only
// appends still exercises the prefix instead of being thrown away -- and since
// the cut can land inside a rune, truncation itself produces the partial-sequence
// shapes this property is most likely to break on. Streams longer than the cap
// are covered by TestSplitWriteKeepsEveryRune's literals and by the
// scrollhistory cases, not here.
func FuzzAdvanceSplitBoundary(f *testing.F) {
	for _, s := range []string{
		"· ↑X",
		"·\x1b[31m↑\x1b[m",
		" \r↓ progress\r100%",
		"áb́c",
		"\x1b[2J\x1b[H",
		"\x1b[10;10Hx",
		"\x1bP1$r0m\x1b\\",       // a DCS string, which has its own parser state
		"\x1b]0;title\x07",       // OSC terminated by BEL
		"\xff\xfe\xc2",           // nothing well-formed at all
		"a\nb\nc\nd\ne\nf\n",     // enough newlines to push lines into history
		"\x1b[?1049h\x1b[?1049l", // the alternate screen, which swaps grids
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 192 {
			s = s[:192]
		}
		assertSplitInvariant(t, s)
	})
}

// Row.occ's documented invariant, which nothing else gates.
//
// row.go states it in prose: occ is an upper bound on the cells modified since
// the last reset, and every cell at or past occ equals the row's last cell.
// reset relies on it, clearing [0, occ) once it has checked that last cell, so an
// understated occ corrupts nothing at the write -- it loses cells at the next
// reset and at the next narrowing. A state comparison cannot catch it either:
// gridState reads cells through Cell(), which never consults occ, so the
// invariant has to be read off the field directly. Hence a second target rather
// than another assertion inside the one above.
func FuzzAdvanceKeepsRowOccupancyInvariant(f *testing.F) {
	for _, seed := range []struct {
		s           string
		cols, lines uint8
	}{
		{"hello\x1b[Kworld", 40, 5},
		{"\x1b[41m\x1b[2J\x1b[H", 8, 2}, // cleared to a background: blanks that are not default cells
		{"\x1b[41mxxxx\x1b[1;3H\x1b[K", 3, 1},
		{"a\nb\nc\nd\ne\nf\ng\n", 1, 1}, // narrowing to one column
		{"\xff\xc2 \x1b[5;5H\x1b[J", 200, 40},
	} {
		f.Add(seed.s, seed.cols, seed.lines)
	}
	f.Fuzz(func(t *testing.T, s string, cols, lines uint8) {
		term, p := NewTerm(Config{ScrollingHistory: 20}, 24, 3, nil), NewProcessor()
		p.Advance(term, []byte(s))
		checkOccupancy(t, term.Grid(), "after Advance")

		// Narrowing is where an understated occ costs cells and widening is where a
		// stale one can resurrect them, so the resize goes to a fuzzer-chosen size
		// and back rather than in one direction. 1 is a legal width and the most
		// hostile one, so the range starts there.
		term.Resize(1+int(cols)%64, 1+int(lines)%16)
		checkOccupancy(t, term.Grid(), "after Resize")
		term.Resize(24, 3)
		checkOccupancy(t, term.Grid(), "after Resize back")
	})
}

// checkOccupancy reads the invariant off Row.occ for every row the grid holds,
// history included, since reflow moves rows between the history and the screen
// and a row that reached the history with a stale hint is just as broken.
func checkOccupancy(t *testing.T, g *Grid, when string) {
	t.Helper()
	for line := -g.HistorySize(); line <= g.bottommostLine(); line++ {
		row := g.Row(line)
		n := row.Len()
		if n == 0 {
			continue
		}
		occ := int(row.occ)
		if occ < 0 || occ > n {
			t.Fatalf("%s: line %d has occ %d, outside [0, %d]", when, line, occ, n)
		}
		// A packed row stores exactly its cells, so occ is their number.
		if row.packed && occ != n {
			t.Fatalf("%s: packed line %d has occ %d for %d cells", when, line, occ, n)
		}
		last := row.CellAt(n - 1)
		for c := occ; c < n; c++ {
			if cell := row.CellAt(c); cell != last {
				t.Fatalf("%s: line %d cell %d is at or past occ %d but differs from the last cell: %+v vs %+v",
					when, line, c, occ, cell, last)
			}
		}
	}
}
