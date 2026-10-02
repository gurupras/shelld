package vt

import "testing"

// Three CSI handlers clamp a parameter against the grid, and each one of them
// clamped against the wrong bound. What the three share is that the clamp keeps
// the arithmetic in range either way, so no index panics and nothing downstream
// complains: the only visible effect is cells, or lines, the sequence did not
// name. Each test below pairs the over-large parameter with an in-range one that
// exercises the same handler, because a clamp is only correct if it leaves the
// ordinary case alone.

// DCH deletes from the cursor rightwards and can never touch a cell to its left.
// deleteChars fills from columns-count, so a count clamped to the row width
// rather than to the width remaining moved that fill left of the cursor.
func TestDeleteCharsClampsToTheCellsAfterTheCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
		want string
	}{
		// 5 cells remain at column 5, so any count at or above 5 is the same
		// delete-to-end-of-line. 10 is the row width, 99 is past it.
		{"count past the row width", "\x1b[99P", "01234"},
		{"count equal to the row width", "\x1b[10P", "01234"},
		{"count equal to the cells remaining", "\x1b[5P", "01234"},
		// The in-range control: DCH 2 at column 5 closes the gap by two.
		{"count within the cells remaining", "\x1b[2P", "01234789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
			feed(term, p, "0123456789\x1b[1;6H")
			if got := term.Grid().Cursor.Point.Column; got != 5 {
				t.Fatalf("setup: cursor column %d, want 5", got)
			}
			feed(term, p, tc.seq)
			if got := rowText(term, 0); got != tc.want {
				t.Errorf("row after %q = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// ED 1 erases from the start of the screen through the cursor. On line 1 there is
// exactly one line above the cursor, line 0, and a `> 1` guard skipped it — so the
// sequence left on screen the one line it was asked to clear, which for a
// full-screen redraw is a line of stale output the client then believes is live.
func TestClearAboveErasesLineZeroFromLineOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor string
		want   string
	}{
		// The off-by-one: from line 1, line 0 is above the cursor.
		{"from line 1", "\x1b[2;2H", "|  b|ccc"},
		// Line 0 has nothing above it, so only the cursor's own line is touched.
		// This is the case the guard exists for and it must stay a no-op above.
		{"from line 0", "\x1b[1;2H", "  a|bbb|ccc"},
		// And from the last line both lines above go.
		{"from line 2", "\x1b[3;2H", "||  c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
			feed(term, p, "aaa\r\nbbb\r\nccc")
			if got := screenText(term); got != "aaa|bbb|ccc" {
				t.Fatalf("setup: screen %q, want \"aaa|bbb|ccc\"", got)
			}
			feed(term, p, tc.cursor+"\x1b[1J")
			if got := screenText(term); got != tc.want {
				t.Errorf("screen after %q ED 1 = %q, want %q", tc.cursor, got, tc.want)
			}
		})
	}
}

// DECSTBM's top clamped to lines rather than lines-1, which yields the empty
// region [lines, lines). Nothing rejects an empty region; it is scrollUp and every
// later linefeed that quietly move zero lines, so the window stops scrolling for
// good. A stale region from a taller window is the ordinary way to reach it — the
// sequence is valid and the client is not doing anything wrong.
func TestSetScrollingRegionLeavesTheRegionNonEmpty(t *testing.T) {
	for _, tc := range []struct {
		name          string
		seq           string
		top, bottom   int
		wantAfterScrl string
	}{
		// Both ends past a 3-line screen: the region has to keep a line.
		{"a region from a taller window", "\x1b[30;40r", 2, 3, "1|2|"},
		// The exact boundary: top-1 == lines is the smallest top the old clamp
		// mapped onto an empty region, and one line lower it agreed with the fix.
		{"top one past the last line", "\x1b[4;9r", 2, 3, "1|2|"},
		// The in-range control: an ordinary full-screen region still scrolls.
		{"a region within the screen", "\x1b[1;3r", 0, 3, "2|3|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
			feed(term, p, tc.seq)
			if term.scrollTop != tc.top || term.scrollBottom != tc.bottom {
				t.Errorf("%q set region [%d,%d), want [%d,%d)",
					tc.seq, term.scrollTop, term.scrollBottom, tc.top, tc.bottom)
			}
			if term.scrollTop >= term.scrollBottom {
				t.Fatalf("%q set the empty region [%d,%d): nothing will scroll again",
					tc.seq, term.scrollTop, term.scrollBottom)
			}
			// The consequence, rather than the field: a region that kept a line
			// still moves it.
			feed(term, p, "1\r\n2\r\n3\x1b[S")
			if got := screenText(term); got != tc.wantAfterScrl {
				t.Errorf("screen after SU = %q, want %q", got, tc.wantAfterScrl)
			}
		})
	}
}
