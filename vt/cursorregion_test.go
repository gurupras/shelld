package vt

import (
	"strings"
	"testing"
)

// Where the cursor lands under a scrolling region is one of the few places this
// library deliberately does not do what alacritty_terminal 0.26.0 does, so the
// expectations below are not read off either implementation: every one of them was
// measured from tmux 3.8-rc, on a private socket, by printing the sequence into a
// pane and asking display-message for #{cursor_y},#{cursor_x}. The rule they
// describe is tmux's and xterm's, and it is two rules that alacritty runs together:
//
//   - DECOM (ModeOrigin) translates the coordinate system that absolute addressing
//     counts in. It belongs to CUP and VPA and to nothing else.
//   - The scrolling region bounds where a relative move may put the cursor,
//     whether or not DECOM is set. A cursor inside the region stays inside it; one
//     outside is bounded by the screen and may move into the region.
//
// alacritty routes the relative moves and CHA through goto, which applies DECOM's
// translation to all of them and bounds none of them by the region. That displaces
// every relative move by the region's top row while DECOM is set, moves the cursor
// off its row on a CHA that must not move it at all, and — the case that needs no
// DECOM and so is the one real programs meet — lets CUU and CUD run out of the
// region entirely, onto rows the program is keeping.
const (
	region = "\x1b[6;12r" // rows 6..12 1-based, so 5..11 counting from zero
	decom  = "\x1b[?6h"
)

// TestCursorMovesWithinTheScrollingRegionAsTmuxDoes is the whole of the measured
// table. Each row names where it starts relative to the region, because that is
// what the rule turns on, and the four clauses of the bound are each the only
// thing that answers at least one row.
func TestCursorMovesWithinTheScrollingRegionAsTmuxDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
		line int
	}{
		// Absolute addressing under DECOM: counted from the region's top row and
		// bounded by its bottom. This part alacritty already has right.
		{"DECOM homes the cursor to the region's top", region + decom, 5},
		{"CUP 1;1 addresses the region's top row", region + decom + "\x1b[1;1H", 5},
		{"CUP 4;1 counts four rows into the region", region + decom + "\x1b[4;1H", 8},
		{"CUP 20;1 is bounded by the region's bottom", region + decom + "\x1b[20;1H", 11},
		{"VPA 1 addresses the region's top row", region + decom + "\x1b[4;1H\x1b[1d", 5},
		// A zero parameter is a one, so nothing reaches goTo with a row below the
		// region: the low bound there stays the screen's top and needs no second.
		{"VPA 0 is VPA 1", region + decom + "\x1b[4;1H\x1b[0d", 5},
		{"CUP 0;0 is CUP 1;1", region + decom + "\x1b[4;1H\x1b[0;0H", 5},

		// CHA addresses a column. Under DECOM alacritty adds the region's top row
		// to a row that already counts from it and moves the cursor down.
		{"CHA leaves the row alone under DECOM", region + decom + "\x1b[4;1H\x1b[1G", 8},
		{"CHA leaves the row alone without DECOM", region + "\x1b[9;1H\x1b[1G", 8},

		// Relative moves under DECOM. alacritty translates each of these by the
		// region's top row, which sends every one of them to the region's bottom.
		{"CUU 1 moves one row up under DECOM", region + decom + "\x1b[4;1H\x1b[A", 7},
		{"CUD 1 moves one row down under DECOM", region + decom + "\x1b[4;1H\x1b[B", 9},
		{"CPL 1 moves one row up under DECOM", region + decom + "\x1b[4;1H\x1b[F", 7},
		{"CNL 1 moves one row down under DECOM", region + decom + "\x1b[4;1H\x1b[E", 9},
		{"CUU 0 is CUU 1", region + decom + "\x1b[4;1H\x1b[0A", 7},
		{"CUD 0 is CUD 1", region + decom + "\x1b[4;1H\x1b[0B", 9},

		// The bound on a relative move, from each of the three places the cursor
		// can start. These need no DECOM, which is what makes them the cases a
		// real program meets: a pager sets a region and keeps a status line below
		// it, and a CUD that ran past the region would print into it.
		{"CUU from inside stops at the region's top", region + "\x1b[9;1H\x1b[9A", 5},
		{"CUD from inside stops at the region's bottom", region + "\x1b[9;1H\x1b[20B", 11},
		{"CPL from inside stops at the region's top", region + "\x1b[9;1H\x1b[9F", 5},
		{"CNL from inside stops at the region's bottom", region + "\x1b[9;1H\x1b[20E", 11},
		// Above the region the low bound is the screen's top, not the region's:
		// bound it by the region and a cursor above one could never reach row 0.
		{"CUU from above the region stops at the screen's top", region + "\x1b[4;1H\x1b[9A", 0},
		// ...but the high bound is still the region's, so a move down from above
		// it stops on entry rather than crossing it.
		{"CUD from above the region stops at the region's bottom", region + "\x1b[3;1H\x1b[20B", 11},
		// Below the region, symmetrically: bounded by the screen going down, by
		// the region going up, and a move up may cross into the region.
		{"CUD from below the region stops at the screen's bottom", region + "\x1b[20;1H\x1b[20B", 23},
		{"CUU from below the region stops at the region's top", region + "\x1b[20;1H\x1b[20A", 5},
		{"CUU from below the region may enter it", region + "\x1b[20;1H\x1b[9A", 10},

		// With no region set the bound is the screen at both ends, which is the
		// case every one of these has to keep behaving as it always did.
		{"CUU with no region stops at the screen's top", "\x1b[9;1H\x1b[20A", 0},
		{"CUD with no region stops at the screen's bottom", "\x1b[9;1H\x1b[40B", 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := NewTerm(Config{}, 80, 24, nil), NewProcessor()
			feed(term, p, tc.seq)
			if got := term.Grid().Cursor.Point.Line; got != tc.line {
				t.Errorf("%s: cursor on row %d, tmux 3.8-rc puts it on row %d", tc.name, got, tc.line)
			}
		})
	}
}

// whereIsRune is the one cell holding r, or (-1, -1). The moves are asserted by
// where the character they were followed by came out, so a failure has to be able
// to say where it went instead rather than only that it was not where it belonged.
func whereIsRune(t *Term, r rune) (line, col int) {
	g := t.Grid()
	for l := range g.ScreenLines() {
		for c := range g.Columns() {
			if g.Row(l).Cell(c).Rune() == r {
				return l, c
			}
		}
	}
	return -1, -1
}

// TestACursorMoveClearsAPendingWrap is the other half of what the moves write. A
// character printed in the last column leaves the wrap pending rather than wrapping
// at once, so that a line ending exactly at the edge does not cost a blank row; a
// cursor move has to cancel that, or the next character the program prints is
// carried a row past wherever the move put the cursor.
//
// It is asserted by where a character comes out rather than by reading the flag,
// because that is what tmux can be asked and so what these two expectations were
// measured from. Both moves are needed: CUU leaves the full row, so an uncancelled
// wrap is visible as the character returning to it, while CHA stays on that row and
// an uncancelled wrap carries the character to the next one — which is why an
// assertion about the row the cursor started on cannot see CHA's case at all.
func TestACursorMoveClearsAPendingWrap(t *testing.T) {
	full := strings.Repeat("x", 80)
	for _, tc := range []struct {
		name, move string
		line, col  int
	}{
		{"CUU", "\x1b[A", 1, 79},
		{"CHA", "\x1b[80G", 2, 79},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, p := NewTerm(Config{}, 80, 24, nil), NewProcessor()
			feed(term, p, "\x1b[3;1H"+full)
			if row := rowText(term, 2); row != full {
				t.Fatalf("setup: row 3 holds %q, so there is no pending wrap to cancel", row)
			}
			feed(term, p, tc.move+"Y")

			if l, c := whereIsRune(term, 'Y'); l != tc.line || c != tc.col {
				t.Errorf("after %s the next character came out at row %d column %d, tmux 3.8-rc puts it at row %d column %d: the move left the pending wrap set, so what a program prints next is carried a row past where the move put the cursor",
					tc.name, l, c, tc.line, tc.col)
			}
		})
	}
}
