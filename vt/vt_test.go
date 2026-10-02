package vt

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"testing"
)

// Cross-implementation fidelity against alacritty_terminal is covered by
// the shelld package's differential test; these check the library API directly.

type recorder struct {
	writes []string
	colors []int
}

func (r *recorder) PtyWrite(s string) { r.writes = append(r.writes, s) }
func (r *recorder) ColorRequest(i int, f func(RGB) string) {
	r.colors = append(r.colors, i)
	r.writes = append(r.writes, f(RGB{0x12, 0x34, 0x56}))
}

func feed(t *Term, p *Processor, s string) { p.Advance(t, []byte(s)) }

func rowText(t *Term, line int) string {
	g := t.Grid()
	var sb strings.Builder
	for c := 0; c < g.Columns(); c++ {
		cell := g.Row(line).Cell(c)
		if cell.Flags()&FlagWideCharSpacer != 0 {
			continue
		}
		sb.WriteRune(cell.Rune())
	}
	return strings.TrimRight(sb.String(), " ")
}

// cellKey is a cell's whole stored value as one comparable string, for the
// differential oracles that compare grids cell by cell.
//
// Two of the conversions to integers are load-bearing and the other two keep the
// fields reading alike; none of them is tidiness. Color and Flags both have a
// String method, and fmt runs a Stringer for %x and %q as well as for %v, so
// formatting a field directly prints the hex of a name. For two of the
// fields, fg and bg, that loses information: they hold the color XORed
// against that slot's default, which
// shifts the kind byte and collapses two whole classes of color onto one string
// each. A real named color's kind becomes zero — KindNone, whose name is "None" —
// so the default and all of SGR 30-37/39/40-47/49/90-107 alike render as "None" —
// 49 belonging in that list for the same reason 39 does, since it stores a zero,
// which is the default background and not an absent color. An RGB color's kind
// becomes KindIndexed, whose name reads the low byte alone, so every color sharing
// a blue value renders as one "Indexed(n)" — n being blue XORed with the default's
// own low byte, which differs between fg and bg. An oracle keyed on that cannot see
// any two members of either class differ.
//
// The other two fields are not blind, and are converted anyway so that every field
// reads alike and neither can go blind quietly. ul is a color stored as it comes
// rather than XORed, so its name is its own value. Flags has a name for each of its
// sixteen bits, which makes the name injective — %x of a Flags is 65,536 distinct
// strings over all 65,536 values, measured — but only for as long as flagNames
// covers the whole width, which is exactly what the comment on flagNames warns
// about: widen Flags and every pair differing only in an unnamed bit reads alike.
//
// The rune is hex for a related reason, and %q was the trap there. It quotes any
// rune that is not a valid code point as U+FFFD, so the 2048 surrogates, everything
// past U+10FFFF, everything negative and a real U+FFFD all print as one string:
// 2050 values in the Unicode range alone. A byte stream cannot produce those, since
// the decoder replaces them before a cell is written, but SetRune is exported and
// takes any rune, so a fabricated grid can hold one and an oracle keyed on %q would
// pass it against any other. Hex over the whole rune is injective, and the human
// form is in the failure messages rather than in the key.
func cellKey(c *Cell) string {
	return fmt.Sprintf("%x/%x/%x/%x/%x", uint32(c.Rune()), uint32(c.fg), uint32(c.bg), uint32(c.ul), uint16(c.flags))
}

// The oracles above are only as good as cellKey, and the way it fails is silent:
// two cells that really differ hash to one string and every comparison keyed on
// it passes. Formatting the stored fields directly did exactly that, so this pins
// the discrimination rather than the formatting.
//
// The color list covers both classes that collapsed, which needs care: the named
// ones all collide with each other, but the RGB ones collide only when they share
// the blue byte, so a pair differing in blue alone would be discriminated even by
// the blind key and would leave that half of the repair untested. Hence three colors
// differing in red and green over one blue.
//
// The rune axis is here because without one the key's first field is untested: every
// cell carrying the same character, a key that dropped the rune entirely passed.
// It holds the four runes %q collapses — a surrogate, one past the last code point,
// a negative one and a real U+FFFD — and U+0000 with the space, the pair whose
// stored forms are each other's, since the rune is stored XORed with the space.
//
// That last pair is an encoding axis, not a discrimination one. Keying on the stored
// c.ch rather than on c.Rune() is an XOR by a constant either way, so it is just as
// injective and no cell pair can fail it; this test does not kill that change and is
// not meant to. Which of the two the key reads is a question of what it means, not of
// what it can see.
func TestCellKeySeesEveryCellDifference(t *testing.T) {
	colors := []Color{defaultFg, defaultBg, NewNamedColor(Red), NewNamedColor(Green),
		NewNamedColor(BrightRed), NewNamedColor(Background), NewNamedColor(1000),
		NewIndexedColor(9), NewIndexedColor(10), NewRGBColor(1, 2, 3), NewRGBColor(9, 2, 3),
		NewRGBColor(1, 9, 3), NewRGBColor(1, 2, 4), NoColor}
	runes := []rune{'h', 0, ' ', '中', 0xFFFD, 0xD800, 0x110000, -1}
	seen := map[string]string{}
	for _, r := range runes {
		for _, fg := range colors {
			for _, bg := range colors {
				for _, ul := range colors {
					for _, flags := range []Flags{0, FlagBold, FlagWrapline, FlagBold | FlagStrikeout} {
						var c Cell
						c.SetRune(r)
						c.SetFg(fg)
						c.SetBg(bg)
						c.ul = ul
						c.SetFlags(flags)
						want := fmt.Sprintf("rune %U fg %v bg %v ul %v flags %v", r, fg, bg, ul, flags)
						if got, ok := seen[cellKey(&c)]; ok && got != want {
							t.Fatalf("cellKey %q is blind to %s versus %s", cellKey(&c), got, want)
						}
						seen[cellKey(&c)] = want
					}
				}
			}
		}
	}
}

func TestPrintWrapAndScrollback(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 3, nil), NewProcessor()
	feed(term, p, "0123456789abc\r\nline2\r\nline3\r\nline4")
	if term.HistorySize() != 2 || rowText(term, -2) != "0123456789" || rowText(term, -1) != "abc" {
		t.Fatalf("history: %q %q hist=%d", rowText(term, -2), rowText(term, -1), term.HistorySize())
	}
	if rowText(term, 0) != "line2" || rowText(term, 2) != "line4" {
		t.Fatalf("viewport: %q %q", rowText(term, 0), rowText(term, 2))
	}
	if term.Grid().Row(-2).Cell(9).Flags()&FlagWrapline == 0 {
		t.Fatal("wrapped line must carry WRAPLINE")
	}
}

func TestHistoryPushedCountsPastTheLimit(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 5}, 10, 3, nil), NewProcessor()
	feed(term, p, strings.Repeat("x\r\n", 22))
	if term.HistorySize() != 5 || term.Grid().HistoryPushed() != 20 {
		t.Fatalf("hist=%d pushed=%d", term.HistorySize(), term.Grid().HistoryPushed())
	}
	feed(term, p, "\x1b[1;2r\x1b[2;1H\n\n") // region starting at the top still pushes
	feed(term, p, "\x1b[2;3r\x1b[3;1H\n")   // region below the top does not
	if got := term.Grid().HistoryPushed(); got != 22 {
		t.Fatalf("pushed=%d", got)
	}
}

func TestSplitUTF8AndWideChars(t *testing.T) {
	term, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
	s := []byte("a中b")
	for i := range s {
		p.Advance(term, s[i:i+1]) // one byte at a time
	}
	g := term.Grid()
	if g.Row(0).Cell(1).Rune() != '中' || g.Row(0).Cell(1).Flags()&FlagWideChar == 0 || g.Row(0).Cell(2).Flags()&FlagWideCharSpacer == 0 || g.Row(0).Cell(3).Rune() != 'b' {
		t.Fatalf("row: %q", rowText(term, 0))
	}
	feed(term, p, "\r\n\xe2\x82Z")
	if rowText(term, 1) != "�Z" {
		t.Fatalf("truncated sequence becomes one replacement: %q", rowText(term, 1))
	}
}

func TestSGRAttributes(t *testing.T) {
	term, p := NewTerm(Config{}, 20, 2, nil), NewProcessor()
	feed(term, p, "\x1b[1;4:3;38;2;1;2;3;48;5;200mX\x1b[0mY")
	x, y := term.Grid().Row(0).Cell(0), term.Grid().Row(0).Cell(1)
	if x.Flags()&(FlagBold|FlagUndercurl) != FlagBold|FlagUndercurl || x.Fg() != NewRGBColor(1, 2, 3) || x.Bg() != NewIndexedColor(200) {
		t.Fatalf("X: flags=%x fg=%x bg=%x", x.Flags(), x.Fg(), x.Bg())
	}
	if y.Flags() != 0 || y.Fg() != NewNamedColor(Foreground) || y.Bg() != NewNamedColor(Background) {
		t.Fatal("reset")
	}
}

func TestAltScreenHasNoHistory(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 2, nil), NewProcessor()
	feed(term, p, "a\r\nb\r\nc\r\n")
	feed(term, p, "\x1b[?1049h")
	if term.Mode()&ModeAltScreen == 0 || term.HistorySize() != 0 {
		t.Fatal("alt screen active with no history")
	}
	feed(term, p, "\x1b[?1049l")
	if term.HistorySize() == 0 {
		t.Fatal("primary history restored")
	}
}

func TestSynchronizedUpdateBuffers(t *testing.T) {
	term, p := NewTerm(Config{}, 20, 2, nil), NewProcessor()
	feed(term, p, "\x1b[?2026hhidden")
	if !p.SyncPending() || rowText(term, 0) != "" {
		t.Fatal("output inside a synchronized update must be buffered")
	}
	feed(term, p, "\x1b[?2026l")
	if p.SyncPending() || rowText(term, 0) != "hidden" {
		t.Fatalf("ESU applies the update: %q", rowText(term, 0))
	}
	feed(term, p, "\x1b[?2026h more")
	p.StopSync(term)
	if rowText(term, 0) != "hidden more" {
		t.Fatalf("StopSync flushes: %q", rowText(term, 0))
	}
}

func TestReflowOnResize(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 4, nil), NewProcessor()
	feed(term, p, "abcdefghijKLM")
	term.Resize(20, 4)
	if rowText(term, 0) != "abcdefghijKLM" {
		t.Fatalf("grow joins wrapped line: %q", rowText(term, 0))
	}
	// Shrinking rewraps and, like alacritty, keeps the cursor's line in view by
	// pushing the lines above it into history.
	term.Resize(5, 4)
	if term.HistorySize() != 2 || rowText(term, -2) != "abcde" || rowText(term, -1) != "fghij" || rowText(term, 0) != "KLM" {
		t.Fatalf("shrink rewraps: %q %q %q hist=%d", rowText(term, -2), rowText(term, -1), rowText(term, 0), term.HistorySize())
	}
	if c := term.Grid().Cursor.Point; c.Line != 0 || c.Column != 3 {
		t.Fatalf("cursor %+v", c)
	}
}

func TestKeepRowsOnShrink(t *testing.T) {
	shrink := func(term *Term, rows int) {
		term.KeepRowsOnShrink(rows)
		term.Resize(term.Columns(), rows)
	}
	// Prompt near the top of a mostly empty screen: nothing moves.
	term, p := NewTerm(Config{ScrollingHistory: 100}, 20, 10, nil), NewProcessor()
	feed(term, p, "a\r\nb\r\n$ ")
	shrink(term, 5)
	if term.HistorySize() != 0 || rowText(term, 0) != "a" || term.Grid().Cursor.Point.Line != 2 {
		t.Fatalf("empty screen: hist=%d row0=%q cursor=%+v", term.HistorySize(), rowText(term, 0), term.Grid().Cursor.Point)
	}

	// Inline TUI: output, then status rows below the cursor, then one row
	// left by the trailing newline. The status rows stay; rows above go to
	// history.
	term, p = NewTerm(Config{ScrollingHistory: 100}, 20, 10, nil), NewProcessor()
	feed(term, p, "o1\r\no2\r\no3\r\no4\r\no5\r\n> \r\nstatus1\r\nstatus2\r\n\x1b[6;3H")
	shrink(term, 6)
	if term.HistorySize() != 3 || rowText(term, -1) != "o3" || rowText(term, 0) != "o4" ||
		rowText(term, 3) != "status1" || rowText(term, 4) != "status2" {
		t.Fatalf("inline: hist=%d rows %q %q %q %q", term.HistorySize(), rowText(term, 0), rowText(term, 2), rowText(term, 3), rowText(term, 4))
	}
	if c := term.Grid().Cursor.Point; c.Line != 2 || c.Column != 2 {
		t.Fatalf("cursor %+v", c)
	}

	// The cursor row is never pushed: rows below it are discarded instead.
	term, p = NewTerm(Config{ScrollingHistory: 100}, 20, 6, nil), NewProcessor()
	feed(term, p, "top\r\nx1\r\nx2\r\nx3\r\nx4\r\nx5\x1b[2;1H")
	shrink(term, 2)
	if term.HistorySize() != 1 || rowText(term, 0) != "x1" || term.Grid().Cursor.Point.Line != 0 {
		t.Fatalf("cursor row: hist=%d row0=%q", term.HistorySize(), rowText(term, 0))
	}

	// The alternate screen is left to the application.
	term, p = NewTerm(Config{ScrollingHistory: 100}, 20, 6, nil), NewProcessor()
	feed(term, p, "\x1b[?1049hA\r\nB\r\nC\r\nD\r\nE\x1b[1;1H")
	shrink(term, 3)
	if rowText(term, 0) != "A" {
		t.Fatalf("alt: row0=%q", rowText(term, 0))
	}
}

// The rule KeepRowsOnShrink implements is tmux's plus a margin, and pinning it
// needs the comparison, not just our own side. Every tmux column here was
// measured: tmux next-3.5 run with `-f /dev/null` and window-size manual, one
// 10x10 pane per row, shrunk to 5 rows by resize-window and read back with
// capture-pane. The term below is 8 columns rather than 10, which nothing here
// depends on: every row holds a single character, so no case is near either
// width's wrap.
//
// Where the two differ it is because this one pushed further: a row tmux still
// shows sits in history here, or a row tmux destroyed off the bottom is still
// here. The loop below checks that no row tmux keeps went missing — set membership
// over history and screen together, so the tmuxHist and tmuxView columns are split
// for the reader and the check does not care which side a row is on. The check
// needs the history limit clear of the push — evicting the oldest rows sooner is
// the one way this loses a row tmux would have held, so every case here runs with
// room to spare. TestKeepRowsOnShrinkPushesAtLeastAsMuchAsTmux carries the underlying push
// bound, which has no such condition, past the cases this table happens to list.
func TestKeepRowsOnShrinkAgainstTmux(t *testing.T) {
	// hist and view read oldest-first with '_' for a blank row; view is the
	// whole 5-row screen.
	for _, tc := range []struct {
		content, cursorRow int
		hist, view         string
		cursorLine         int
		tmuxHist, tmuxView string
	}{
		{10, 1, "", "ABCDE", 0, "", "ABCDE"},
		{10, 4, "ABC", "DEFGH", 0, "", "ABCDE"},
		{10, 5, "ABCD", "EFGHI", 0, "", "ABCDE"},
		{10, 6, "ABCDE", "FGHIJ", 0, "A", "BCDEF"},
		{10, 10, "ABCDE", "FGHIJ", 4, "ABCDE", "FGHIJ"},
		{1, 1, "", "A____", 0, "", "A____"},
		{4, 4, "", "ABCD_", 3, "", "ABCD_"},
		{4, 5, "A", "BCD__", 3, "", "ABCD_"},
		{4, 8, "ABCD", "_____", 3, "ABC", "D____"},
		{6, 7, "ABC", "DEF__", 3, "AB", "CDEF_"},
		{6, 8, "ABCD", "EF___", 3, "ABC", "DEF__"},
	} {
		term, p := NewTerm(Config{ScrollingHistory: 100}, 8, 10, nil), NewProcessor()
		for i := 1; i <= tc.content; i++ {
			feed(term, p, fmt.Sprintf("\x1b[%d;1H%c", i, 'A'+i-1))
		}
		feed(term, p, fmt.Sprintf("\x1b[%d;1H", tc.cursorRow))
		term.KeepRowsOnShrink(5)
		term.Resize(term.Columns(), 5)

		var hist, view string
		for line := -term.HistorySize(); line < 0; line++ {
			hist += rowLetter(term, line)
		}
		for line := 0; line < 5; line++ {
			view += rowLetter(term, line)
		}
		if hist != tc.hist || view != tc.view || term.Grid().Cursor.Point.Line != tc.cursorLine {
			t.Errorf("%d rows of text, cursor on row %d: hist %q view %q cursor line %d, want %q %q %d",
				tc.content, tc.cursorRow, hist, view, term.Grid().Cursor.Point.Line, tc.hist, tc.view, tc.cursorLine)
		}
		// Against the rows just read, not against the table's own columns: a
		// change that moves a row out of both history and the screen has to fail
		// here and not only above.
		for _, row := range strings.Split(tc.tmuxHist+tc.tmuxView, "") {
			if row != "_" && !strings.Contains(hist+view, row) {
				t.Errorf("%d rows of text, cursor on row %d: row %q survives in tmux but not in %q+%q",
					tc.content, tc.cursorRow, row, hist, view)
			}
		}
	}
}

// The blank tail KeepRowsOnShrink measures is Row.isClear, so what counts as a
// blank row is Cell.isEmpty, and the two edges of that worth knowing belong in a
// test rather than in prose: an underline color on its own leaves a row blank,
// because isEmpty does not read it, and a row of spaces that overflowed the right
// edge is content on the wrap flag alone, while the row it spilled into stays
// blank. The wrap half is the only guard on that flag in either of the packages
// that could plausibly hold one: drop FlagWrapline from isEmpty's mask and this is
// the only test in ./vt or the root package that fails, reflow and the
// recorded-resize replay included.
//
// Every case is read twice, once through isClear and once through the push count
// a real shrink produces from it, because a row's blankness only matters as a
// number of rows pushed. That second reading is what pins the run's two ends and
// the cushion: counting the cursor's own row, or giving up the whole run rather
// than all but one of it, leaves isClear untouched and used to be caught only by
// the measured-tmux table below.
func TestBlankRowAsTheShrinkCountsIt(t *testing.T) {
	for _, tc := range []struct {
		name, write string
		blank       bool
	}{
		{"spaces", "    ", true},
		{"tabs", "\t\t", true},
		{"bold, dim, italic and hidden", "\x1b[1;2;3;8m    ", true},
		{"underline color alone", "\x1b[58;2;255;0;0m    ", true},
		{"underline style", "\x1b[4m    ", false},
		{"undercurl", "\x1b[4:3m    ", false},
		{"inverse", "\x1b[7m    ", false},
		{"strikeout", "\x1b[9m    ", false},
		{"background color", "\x1b[41m    ", false},
		{"foreground color", "\x1b[31m    ", false},
		{"text", "x", false},
	} {
		// Through a real shrink as well as through isClear, because the two can
		// disagree: isClear answers about one row, and what the shrink does with
		// the answer is a push count. Eight rows to four, three rows of text at
		// the top and the cursor on line 3, with tc.write on line 5 and lines 4, 6
		// and 7 left blank — so a blank line 5 joins the run and the run reaches
		// line 4, giving up three of its four rows and pushing one, where content
		// on line 5 stops the run at two rows, gives up one and pushes three.
		func() {
			term, p := NewTerm(Config{ScrollingHistory: 100}, 8, 8, nil), NewProcessor()
			feed(term, p, "one\r\ntwo\r\nthree")
			feed(term, p, "\x1b[6;1H"+tc.write+"\x1b[0m\x1b[4;1H")
			before := term.Grid().HistoryPushed()
			term.KeepRowsOnShrink(4)
			want := 3
			if tc.blank {
				want = 1
			}
			if got := int(term.Grid().HistoryPushed() - before); got != want {
				t.Errorf("%s on line 5: the shrink pushed %d rows, want %d", tc.name, got, want)
			}
		}()

		term, p := NewTerm(Config{ScrollingHistory: 10}, 8, 3, nil), NewProcessor()
		feed(term, p, tc.write)
		if got := term.Grid().Row(0).isClear(); got != tc.blank {
			t.Errorf("%s: row blank = %v, want %v", tc.name, got, tc.blank)
		}
	}
	// Nine spaces on eight columns: the first row carries the wrap and is content,
	// the row it wrapped into is blank.
	term, p := NewTerm(Config{ScrollingHistory: 10}, 8, 3, nil), NewProcessor()
	feed(term, p, strings.Repeat(" ", 9))
	if term.Grid().Row(0).isClear() || !term.Grid().Row(1).isClear() {
		t.Errorf("wrapped blanks: row 0 blank = %v, row 1 blank = %v, want false true",
			term.Grid().Row(0).isClear(), term.Grid().Row(1).isClear())
	}

	// The run's other end: it stops below the cursor's own row, even when that row
	// is as blank as the rows under it. Eight rows to four, text on lines 0 to 2,
	// the cursor on line 4 and lines 3 to 7 blank — the run is lines 7, 6 and 5, so
	// two rows are given up and two pushed. Counting the cursor's row as well would
	// give up three and push one.
	term, p = NewTerm(Config{ScrollingHistory: 100}, 8, 8, nil), NewProcessor()
	feed(term, p, "one\r\ntwo\r\nthree\x1b[5;1H")
	before := term.Grid().HistoryPushed()
	term.KeepRowsOnShrink(4)
	if got := int(term.Grid().HistoryPushed() - before); got != 2 {
		t.Errorf("with the cursor on a blank line 4: the shrink pushed %d rows, want 2", got)
	}
}

// A table cannot see a case it does not list, and everything said about the two
// screens rests on one inequality: our push is never smaller than tmux's max(0,
// cursor+1-target). Random screens, with content, blank rows and the cursor placed
// independently, exercise both of our bounds — the deficit less the blank tail, and
// the cursor's own row. History here is far larger than any push, which is why this
// reads the push count rather than the screens: with a full history a longer push
// evicts the oldest rows, and the two screens stop being comparable row by row.
//
// Two things about the instrument. The count comes from Grid.pushed, which
// scrollUp only increments when the grid has history at all (maxScrollLimit > 0),
// so the limit in the config is what makes the reading exist; its size past that
// does not enter, since pushed counts rows pushed and not rows still held. And
// this is one-sided: it fails only on a push below tmux's, so a change that pushes
// less than this implementation means to but still at least as much as tmux does is
// invisible here. Removing the cushion and counting the cursor's own row as blank
// are both of that kind, and both leave all 3000 iterations green; the table above,
// TestBlankRowAsTheShrinkCountsIt and TestKeepRowsOnShrinkAgainstAPlainResize are
// what catch them.
func TestKeepRowsOnShrinkPushesAtLeastAsMuchAsTmux(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		rows := 2 + rng.IntN(30)
		target := 1 + rng.IntN(rows-1)
		term, p := NewTerm(Config{ScrollingHistory: 1000}, 8, rows, nil), NewProcessor()
		for line := range rows {
			if rng.IntN(2) == 0 {
				feed(term, p, fmt.Sprintf("\x1b[%d;1Hx", line+1))
			}
		}
		cursor := rng.IntN(rows)
		feed(term, p, fmt.Sprintf("\x1b[%d;1H", cursor+1))

		before := term.Grid().HistoryPushed()
		term.KeepRowsOnShrink(target)
		ours := int(term.Grid().HistoryPushed() - before)
		if tmux := max(0, cursor+1-target); ours < tmux {
			t.Fatalf("%d rows to %d with the cursor on line %d: pushed %d, tmux pushes %d",
				rows, target, cursor, ours, tmux)
		}
	}
}

// Everything KeepRowsOnShrink's own documentation claims about how much it adds
// is a claim about the difference from calling Resize alone, and this is where
// that difference is measured: the whole space of small screens — three to nine
// rows, every pattern of blank and occupied rows, every cursor row and every
// target of one row or more — each case run twice, once through KeepRowsOnShrink
// and the shrink and once through the shrink alone, reading Grid.pushed both
// times. Both arms are in the tree, so the comparison needs no patched copy of
// this package and the numbers below can be reproduced by running this test.
//
// The two invariants are the interesting part. The push is never smaller than a
// plain Resize's, anywhere in the sweep, which is what makes "keeps at least as
// much" true of Resize and not only of tmux. And where every row below the cursor
// is blank it is never more than one row larger — that single row being the
// cushion — so on that shape the deficit-less-blank-tail arithmetic has no effect
// of its own at all. The one-row bound holds only on that shape, which is why it
// is asserted inside the branch: where the blank run stops short of the cursor the
// deficit does the work and the difference runs up to four rows, whose whole
// spread the last check below pins.
//
// The counts are asserted rather than logged because KeepRowsOnShrink's comment
// quotes them; a rule change that moves them should fail here and send the reader
// to that comment. They are not themselves the property under test.
func TestKeepRowsOnShrinkAgainstAPlainResize(t *testing.T) {
	// A screen of rows rows with an 'x' on every row named by mask, and the cursor
	// parked on cursor. Built twice per case: Resize consumes the grid it shrinks,
	// so the two arms cannot share one.
	build := func(rows, mask, cursor int) *Term {
		term, p := NewTerm(Config{ScrollingHistory: 1000}, 8, rows, nil), NewProcessor()
		for line := range rows {
			if mask&(1<<line) != 0 {
				feed(term, p, fmt.Sprintf("\x1b[%d;1Hx", line+1))
			}
		}
		feed(term, p, fmt.Sprintf("\x1b[%d;1H", cursor+1))
		return term
	}
	pushedBy := func(term *Term, shrink func(*Term)) int {
		before := term.Grid().HistoryPushed()
		shrink(term)
		return int(term.Grid().HistoryPushed() - before)
	}

	var total, blankBelow, vacuous, oneMore, oneMoreWithRowBelow, same, shortRun, shortRunMore int
	shortRunSpread := map[int]int{}
	for rows := 3; rows <= 9; rows++ {
		for mask := range 1 << rows {
			for cursor := range rows {
				// Every row below the cursor blank makes the blank run reach the
				// cursor, where the cushion is the only thing left to give. With the
				// cursor on the bottom row there is nothing below it and the condition
				// holds for want of a counterexample.
				blank := mask>>(cursor+1) == 0
				for target := 1; target < rows; target++ {
					total++
					ours := pushedBy(build(rows, mask, cursor), func(term *Term) {
						term.KeepRowsOnShrink(target)
						term.Resize(term.Columns(), target)
					})
					plain := pushedBy(build(rows, mask, cursor), func(term *Term) {
						term.Resize(term.Columns(), target)
					})
					if ours < plain {
						t.Fatalf("%d rows to %d, content %0*b, cursor on line %d: pushed %d, a plain Resize pushes %d",
							rows, target, rows, mask, cursor, ours, plain)
					}
					switch {
					case !blank:
						shortRun++
						shortRunSpread[ours-plain]++
						if ours > plain {
							shortRunMore++
						}
					default:
						blankBelow++
						if cursor == rows-1 {
							vacuous++
						}
						switch ours - plain {
						case 0:
							same++
						case 1:
							oneMore++
							if cursor < rows-1 {
								oneMoreWithRowBelow++
							}
						default:
							t.Fatalf("%d rows to %d, content %0*b, cursor on line %d: pushed %d where a plain Resize pushes %d, more than the one row of cushion",
								rows, target, rows, mask, cursor, ours, plain)
						}
					}
				}
			}
		}
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"cases", total, 59376},
		{"cases with every row below the cursor blank", blankBelow, 14266},
		{"of those, one row past a plain Resize (the cushion)", oneMore, 5164},
		{"of those, the same as a plain Resize", same, 9102},
		{"of those, vacuous under a bottom-row cursor", vacuous, 7168},
		{"of the cushion's cases, with a row below the cursor", oneMoreWithRowBelow, 5164},
		{"cases with the blank run stopping short of the cursor", shortRun, 45110},
		{"of those, past a plain Resize", shortRunMore, 31636},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d; KeepRowsOnShrink's comment quotes these numbers, so a deliberate change to the rule means correcting it there too",
				c.name, c.got, c.want)
		}
	}
	// The blank-below shape's spread is the one-row bound asserted in the loop. This
	// is the other shape's, where nothing bounds it: without it, "the difference runs
	// up to four rows" would be prose with no check behind it.
	if want := map[int]int{0: 13474, 1: 16060, 2: 10936, 3: 4256, 4: 384}; !maps.Equal(shortRunSpread, want) {
		t.Errorf("rows past a plain Resize where the blank run stops short of the cursor: %v, want %v", shortRunSpread, want)
	}
}

// rowLetter is rowText for this file's single-character rows, with a blank row
// reported as "_" so that a whole screen reads as one string.
func rowLetter(t *Term, line int) string {
	if text := rowText(t, line); text != "" {
		return text
	}
	return "_"
}

func TestListenerReplies(t *testing.T) {
	rec := &recorder{}
	term, p := NewTerm(Config{}, 20, 5, rec), NewProcessor()
	feed(term, p, "\x1b[3;7H\x1b[6n\x1b[c\x1b]11;?\x07\x1b[?25$p")
	want := []string{"\x1b[3;7R", "\x1b[?6c", "\x1b]11;rgb:1212/3434/5656\x07", "\x1b[?25;1$y"}
	if strings.Join(rec.writes, "|") != strings.Join(want, "|") || len(rec.colors) != 1 || rec.colors[0] != int(Background) {
		t.Fatalf("replies %q colors %v", rec.writes, rec.colors)
	}
}

func BenchmarkAdvanceASCII(b *testing.B) {
	line := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog ", 3) + "\r\n")
	data := []byte(strings.Repeat(string(line), 200))
	term, p := NewTerm(Config{ScrollingHistory: 10000}, 200, 50, nil), NewProcessor()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		p.Advance(term, data)
	}
}

// The reflow benchmarks resize a history of several hundred wrapped rows, which is
// where a cost per row shows up at all — one client changing its width walks every
// line the history holds. Each iteration builds its own grid because a resize
// consumes the one it is given, so the build is outside the timer; what is left
// inside is one Resize.
//
// Widening is the arm moveFrontTo changed, and what this one measures of it is
// allocation, not time. Six interleaved pairs against a build that still copied:
// 356 to 357 allocations and 1.47 MB against 656 to 658 and 2.09 MB, every
// repetition. The gap is 300 and so is the number of joins: that is one array per
// joined row, not the two frontSplitOff wrote. This content joins whole rows, so
// its remainder was make([]Cell, 0) on all 300 and cost no accounted allocation —
// only the array for the moved cells shows up here. Both cost something where a
// join takes part of a row, which is common in general (4,143 of 12,577 joins over
// a randomised sweep of widths, histories and content) and never happens here.
//
// Nothing is claimed here about ns/op, and the narrowing benchmark below is why.
// It runs code neither arm touched, and across the same six pairs its median came
// out 247 us in one and 292 in the other, a spread as wide as the difference the
// widening arm shows — so on this box a per-op time read off a handful of
// repetitions of this benchmark cannot separate the two implementations.
//
// This benchmark also never reaches the clear that moveFrontTo's dirty flag causes:
// it builds at the width it widens from, so every row's capacity is that starting
// width and grow appends rather than reusing anything, and counting grow's clears
// through it gives zero on either implementation. The re-widen benchmark below is
// the one that pays for the flag, and the one to read a time off.
func BenchmarkGrowColumnsReflow(b *testing.B) {
	data := []byte(strings.Repeat(strings.Repeat("the quick brown fox jumps over the lazy dog ", 4)+"\r\n", 300))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		term, p := NewTerm(Config{ScrollingHistory: 10000}, 100, 50, nil), NewProcessor()
		p.Advance(term, data)
		b.StartTimer()
		term.Resize(200, 50)
	}
}

// Narrowing, the other direction over the same content, for comparison: it splits
// rows rather than joining them and goes nowhere near moveFrontTo. It is also the
// control for the two benchmarks either side of it, since neither implementation
// touches the code it runs: any difference it shows between them is noise.
func BenchmarkShrinkColumnsReflow(b *testing.B) {
	data := []byte(strings.Repeat(strings.Repeat("the quick brown fox jumps over the lazy dog ", 4)+"\r\n", 300))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		term, p := NewTerm(Config{ScrollingHistory: 10000}, 200, 50, nil), NewProcessor()
		p.Advance(term, data)
		b.StartTimer()
		term.Resize(100, 50)
	}
}

// A widen that reuses capacity an earlier widen's join left behind. This is the
// shape that pays for handing cells over in place instead of copying them: a row a
// join empties keeps its array rather than being given an exact-size replacement, so
// the next widen reuses that capacity and clears what it reuses. It is also the shape
// where the saving is big enough to read a time off rather than only an allocation
// count. The 300-character lines wrap eight deep at a width of 40, the widen to 80
// joins them, and the timed step is the widen from there to 200 — which is where all
// 300 clears land, as TestGrowClearsRowsAJoinDirtied checks.
//
// Against a build that still copied, six interleaved pairs: 420 to 680 us against
// 1155 to 1357 us, 631 allocations and 2.6 MB against 2131 and 5.4 MB. The control
// above spanned 198 to 326 us in one arm and 251 to 327 in the other over the same
// pairs, which is the width of this box's noise; these ranges do not overlap.
func BenchmarkRewidenColumnsReflow(b *testing.B) {
	line := strings.Repeat("the quick brown fox jumps over the lazy dog ", 7)[:300]
	data := []byte(strings.Repeat(line+"\r\n", 300))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		term, p := NewTerm(Config{ScrollingHistory: 10000}, 40, 24, nil), NewProcessor()
		p.Advance(term, data)
		term.Resize(80, 24)
		b.StartTimer()
		term.Resize(200, 24)
	}
}

// The shape where moveFrontTo's clearing is furthest above what the version that
// copied did, found by counting grow's clears over 50 combinations of line length,
// history cap and resize sequence: 176-character lines, a 4000-row history and six
// resizes that never return to a width already seen clear 351 rows where copying
// cleared 51. It is here as the other end of the range from the benchmark above —
// that one is where the trade pays most, this is where it pays least — so that a
// change making the clear more expensive shows up against both.
//
// Six interleaved pairs against the copying version: 1877 to 2510 us against 2492 to
// 3121 us, 6732 allocations and 9.9 MB against 11233 and 13.7 MB. Slower in no pair,
// but the ranges touch at the ends, which is what seven times the clearing costs.
func BenchmarkWorstClearSequence(b *testing.B) {
	line := strings.Repeat("the quick brown fox jumps over the lazy dog ", 8)[:176]
	data := []byte(strings.Repeat(line+"\r\n", 300))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		term, p := NewTerm(Config{ScrollingHistory: 4000}, 40, 50, nil), NewProcessor()
		p.Advance(term, data)
		b.StartTimer()
		for _, cols := range []int{60, 50, 70, 45, 90, 41} {
			term.Resize(cols, 50)
		}
	}
}

// BenchmarkNarrowWidenHistory prices shrinkKeepFactor, whose two sides are a
// narrowing that keeps its arrays and one that gives them back. The arms differ only
// in how sharp the narrowing is: 200 columns to 80 is inside the factor, so every row
// keeps its array and the widen back reuses it, and 1024 to 80 is past it, so every
// row moves to an array of the new width and the widen back allocates one per row.
//
// The history is built once and the cycle timed, because a narrow-and-widen returns
// the grid to the width it started at: what each repetition measures is a client
// attaching at a smaller size and detaching again. Read the allocation counts first —
// they are exact, where the times move with the box — and read the inside-the-factor
// arms against main, which they should match to the byte.
//
// Six interleaved pairs against a build that keeps every array: the sharp arm runs 1107
// to 1691 us against 37 to 47, and the two arms inside the factor overlap in both
// directions — 12 to 40 us against 11 to 49, and 778 to 1331 us against 777 to 1147 —
// having allocated the same 8 and 3010 times as that build, to within seven bytes.
func BenchmarkNarrowWidenHistory(b *testing.B) {
	short := "2026-09-15 20:00:00 INFO line"
	wrapped := strings.Repeat("the quick brown fox jumps over the lazy dog ", 7)[:300]
	for _, arm := range []struct {
		name   string
		wide   int
		narrow int
		line   string
	}{
		{"short/200-to-80", 200, 80, short},
		{"wrapped/200-to-80", 200, 80, wrapped},
		{"short/1024-to-80", 1024, 80, short},
	} {
		b.Run(arm.name, func(b *testing.B) {
			const rows = 600
			term, p := NewTerm(Config{ScrollingHistory: rows * 16}, arm.wide, 24, nil), NewProcessor()
			for range rows {
				feed(term, p, arm.line+"\r\n")
			}
			b.ReportAllocs()
			for b.Loop() {
				term.Resize(arm.narrow, 24)
				term.Resize(arm.wide, 24)
			}
		})
	}
}

func TestHistoryPushedFollowsScreenHeight(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 10, nil), NewProcessor()
	feed(term, p, strings.Repeat("x\r\n", 30))
	before := term.Grid().HistoryPushed()
	term.KeepRowsOnShrink(5)
	term.Resize(10, 5)
	if got := term.Grid().HistoryPushed() - before; got != 5 {
		t.Fatalf("shrink pushed %d, want 5", got)
	}
	term.Resize(10, 10)
	if got := term.Grid().HistoryPushed(); got != before {
		t.Fatalf("grow back: pushed %d, want %d", got, before)
	}
}

// The erase path with a colored background, which is what a full-screen
// application does on every repaint: fillBlank cannot clear memory there, so this
// is where its doubling branch earns its keep.
//
// The width is swept because the width is the span, and what the seed gates can only be
// measured by varying the span. At a width at or below both of two seeds the min clamps
// each to the span, so the two write the same cells with the same number of stores and
// differ only in one immediate: the seed cannot be what a difference there measures. It
// does not follow that such a difference is noise: BenchmarkFillForms puts two seeds in
// exactly that position inside one binary and finds up to 1.4 points between them, taken
// as a share of the slower arm, which is why the seed is chosen there and not here. The
// widths below start under the seed and reach twenty-five times past it, which is where
// its cost and its win separate.
func BenchmarkEraseColoredLines(b *testing.B) {
	for _, cols := range []int{8, 24, 32, 48, 64, 120, 200, 400} {
		b.Run(fmt.Sprintf("cols=%d", cols), func(b *testing.B) {
			const rows = 50
			var sb strings.Builder
			sb.WriteString("\x1b[41m")
			for line := 1; line <= rows; line++ {
				fmt.Fprintf(&sb, "\x1b[%d;1H\x1b[K", line)
			}
			data := []byte(sb.String())
			term, p := NewTerm(Config{ScrollingHistory: 1000}, cols, rows, nil), NewProcessor()
			b.SetBytes(int64(cols * rows))
			for b.Loop() {
				p.Advance(term, data)
			}
		})
	}
}

// The fill on its own, which is what the erase sweep above measures through a
// caller. A span of one is the interesting end: a one-cell ICH or DCH fills
// exactly one cell, so whatever the seed costs a short span shows up here
// undiluted, where in the erase sweep it is a few percent of a parse.
func BenchmarkFillBlank(b *testing.B) {
	cells := make([]Cell, 400)
	bg := NewIndexedColor(200)
	for _, n := range []int{1, 2, 8, 32, 200, 400} {
		b.Run(fmt.Sprintf("cells=%d", n), func(b *testing.B) {
			span := cells[:n]
			for b.Loop() {
				fillBlank(span, bg)
			}
		})
	}
}

// TestAStrayC1BytePrintsAReplacementCharacter: a lone 0x80-0x9F byte in ground
// state draws U+FFFD, as tmux and shelld-rs do, rather than vanishing as the
// 8-bit C1 control vte would execute. 0xA0 is the neighbour that always drew
// U+FFFD; a C1 written as valid UTF-8 is still executed and draws nothing.
func TestAStrayC1BytePrintsAReplacementCharacter(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\x80c", "a�c"},
		{"a\x9fc", "a�c"},
		{"a\xa0c", "a�c"},
		{"a\xc2\x85c", "ac"},
	} {
		term := NewTerm(Config{}, 10, 2, nil)
		feed(term, NewProcessor(), c.in)
		if got := rowText(term, 0); got != c.want {
			t.Errorf("%q: row %q, want %q", c.in, got, c.want)
		}
	}
	term, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
	for _, s := range []string{"a", "\x80", "c"} { // the byte alone in its read
		feed(term, p, s)
	}
	if got := rowText(term, 0); got != "a�c" {
		t.Errorf("split reads: row %q, want %q", got, "a�c")
	}
}
