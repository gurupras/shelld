package vt

import (
	"fmt"
	"strings"
	"testing"
)

// gridState is the whole terminal state a split must not change: every cell's
// glyph, colours and attributes across the history and the screen, plus the
// cursor. It deliberately avoids text, because a blank carrying a background
// reads as a space and a text comparison cannot see it.
func gridState(term *Term) string {
	g := term.Grid()
	var sb strings.Builder
	fmt.Fprintf(&sb, "cursor=%d,%d history=%d cols=%d lines=%d\n",
		g.Cursor.Point.Line, g.Cursor.Point.Column, g.HistorySize(),
		g.Columns(), g.ScreenLines())
	for line := -g.HistorySize(); line <= g.bottommostLine(); line++ {
		row := g.Row(line)
		for c := 0; c < g.Columns(); c++ {
			cell := row.Cell(c)
			fmt.Fprintf(&sb, "%d/%v/%v/%d/%v;",
				cell.Rune(), cell.Fg(), cell.Bg(), cell.Flags(), cell.UnderlineColor())
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// assertSplitInvariant feeds s as one write, then as every possible pair of
// writes, and requires the terminal to end in the same state.
func assertSplitInvariant(t *testing.T, s string) {
	t.Helper()
	term, p := NewTerm(Config{ScrollingHistory: 20}, 24, 3, nil), NewProcessor()
	p.Advance(term, []byte(s))
	want := gridState(term)
	for k := 1; k < len(s); k++ {
		term, p := NewTerm(Config{ScrollingHistory: 20}, 24, 3, nil), NewProcessor()
		p.Advance(term, []byte(s[:k]))
		p.Advance(term, []byte(s[k:]))
		if got := gridState(term); got != want {
			t.Errorf("split after %d of %d bytes (%q | %q) differs from one write",
				k, len(s), s[:k], s[k:])
			return // one report per stream is enough to localize it
		}
	}
}

// advancePartialUTF8 buffers the fragment of a rune that a write ended in,
// appends up to three bytes of the next write, then decodes and prints one
// rune. When the buffer was not wholly well-formed it returned the count of
// well-formed bytes rather than the bytes of the rune it printed, so a byte
// between that rune and whatever ended validity was consumed unprinted.
//
// The buffer is four bytes, so only a two-byte rune split after its first
// byte can reach the faulty branch with bytes to spare: a three- or four-byte
// rune's own length already covers the buffer. What follows the gap byte only
// has to end validity -- it need not be a well-formed lead.
func TestSplitWriteKeepsEveryRune(t *testing.T) {
	// Lead runes. The corpus holds exactly three two-byte runes: U+00A0
	// (8706 occurrences), U+00B7 (2072) and U+00E9 (3). U+00A0 heads 368 of
	// the 369 trigger sites in the recorded Codex sessions, U+00B7 heads 31
	// of the 36 in the recorded Claude Code sessions. U+0301 is a combining mark, which reaches
	// the same branch while also exercising zero-width placement.
	leads := []string{" ", "·", "é", "́"}
	// The gap byte is anything below 0x80. Controls matter more than
	// printables: dropping \r changes the whole line rather than one cell.
	gaps := []string{" ", "|", "/", ",", "\r", "\n", "\t", "\x08", "\x0b", "\x0c", "\x1b"}
	// What ends validity. Well-formed leads and malformed bytes reach the
	// branch through different utf8x.Check returns (errLen 0 against 1), so
	// both are needed.
	tails := []string{
		" ", "↓", "─", "\U0001F600", "X",
		"\x80", "\xa0", "\xbf", "\xc1", "\xf5", "\xff", "\xc2", "\xe2", "\xf0",
	}
	for _, lead := range leads {
		for _, gap := range gaps {
			for _, tail := range tails {
				s := lead + gap + tail + "Z"
				t.Run(fmt.Sprintf("%U/%q/%q", []rune(lead)[0], gap, tail), func(t *testing.T) {
					assertSplitInvariant(t, s)
				})
			}
		}
	}
	for _, s := range []string{
		"       ",
		"   ↓",
		"a  ↓b",
		"↑ ↑X",          // three-byte lead: immune, kept as a control
		"\U0001F600 ↑X", // four-byte lead: immune, kept as a control
		"  X",           // nothing ends validity: must also be unchanged
		"   ↓",
		"·\x1b[m↑",
		"·\x1b[31m↑\x1b[m",
		"áb́c",               // combining marks over base characters
		" \r↓ progress\r100%", // CR overwrite around the boundary
		// The eleven literals main carried before this file replaced it, kept
		// verbatim so taking this version loses none of their coverage. Three
		// shapes the generated grid above does not produce: two leads before the
		// gap, a two-byte gap, and a trigger that repeats after a recovered state.
		"· ↑X",    // the case from the recordings
		"· ↑ · ↑", // repeated, so a later split hits a recovered state
		"·· ↑",
		"a· ↑b",
		"↑ ↑X",          // 3-byte lead
		"\U0001F600 ↑X", // 4-byte lead
		"· X",           // no multi-byte rune after the gap
		"·  ↑",          // two bytes of gap
		"é\t…",          // tab in the gap
		"·\x1b[m↑",      // an escape in the gap
		"·\n↑",          // a control byte in the gap
	} {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) { assertSplitInvariant(t, s) })
	}
}

// The same property byte by byte, the worst case for the partial buffer:
// every multi-byte rune arrives one byte per write.
func TestByteAtATimeMatchesWholeWrite(t *testing.T) {
	for _, s := range []string{
		"⠋ Thinking… (5s · ↑ 234 tokens)\r\n❯ ",
		" |  ───  / ",
		"\x1b[31mred\x1b[m · \x1b[1mbold\x1b[m\r\n",
	} {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			term, p := NewTerm(Config{ScrollingHistory: 20}, 40, 4, nil), NewProcessor()
			p.Advance(term, []byte(s))
			want := gridState(term)

			term2, p2 := NewTerm(Config{ScrollingHistory: 20}, 40, 4, nil), NewProcessor()
			for i := 0; i < len(s); i++ {
				p2.Advance(term2, []byte(s[i:i+1]))
			}
			if got := gridState(term2); got != want {
				t.Fatal("byte at a time differs from one write")
			}
		})
	}
}
