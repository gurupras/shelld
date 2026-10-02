package vt

import (
	"strconv"
	"strings"
	"testing"
)

// The three tests below pin behaviour that the doc comments on Point.Column,
// Charset and Grid.SavedCursor call out as surprising. Each one is the sort of
// thing a reader doubts, so it is asserted rather than only described.

// Point.Column stops at the last column and InputNeedsWrap carries the pending
// wrap, so a Point never holds a column equal to the width.
//
// The rule is implemented twice — once in input and once in the inputASCII fast
// path — so every case here is driven through both. Printable ASCII outside
// insert mode with G0 on ASCII takes the fast path; anything else, here a
// non-ASCII width-1 rune, takes input. Feeding only ASCII leaves a mutation of
// input's copy of the rule undetected, which is how this test was first written.
func TestPointColumnNeverReachesWidth(t *testing.T) {
	const cols = 6
	for _, path := range []struct {
		name string
		char string // one width-1 character
	}{
		{"inputASCII fast path", "a"},
		{"input general path", "é"},
	} {
		t.Run(path.name, func(t *testing.T) {
			// One character at a time, checking after each: the column must stay
			// below the width at every step, including the step that fills the
			// row and the step that wraps.
			term, proc := NewTerm(Config{ScrollingHistory: 4}, cols, 3, nil), NewProcessor()
			for i := 1; i <= 2*cols; i++ {
				feed(term, proc, path.char)
				if got := term.Grid().Cursor.Point.Column; got >= cols {
					t.Fatalf("after %d characters the column is %d, which is not below the width %d", i, got, cols)
				}
			}

			// Having filled the row exactly, the cursor rests on the last column
			// with the wrap pending rather than sitting one past it. The loop above
			// cannot see that: the column is in range either way, and it never reads
			// the flag. It does catch a rule read off the span's length rather than
			// off where the span ends — that one walks the column to the width and
			// fails at the sixth character — so what is left to these two blocks is
			// the flag itself.
			//
			// Fed one character at a time, so the write that fills the row is a span
			// of a single cell. That is what this block holds over the one below:
			// a version setting the flag only for spans longer than one cell passes
			// there and fails here.
			term2, proc2 := NewTerm(Config{ScrollingHistory: 4}, cols, 3, nil), NewProcessor()
			for range cols {
				feed(term2, proc2, path.char)
			}
			c := term2.Grid().Cursor
			if c.Point.Column != cols-1 || !c.InputNeedsWrap {
				t.Fatalf("after filling the row one character at a time: column %d wrap %v, want column %d with wrap pending",
					c.Point.Column, c.InputNeedsWrap, cols-1)
			}

			// The same, fed as one write, because inputASCII advances over a whole
			// run at once. This is the other direction and neither block subsumes
			// the other: a version setting the flag only for a span of one cell
			// passes the block above and fails here.
			term3, proc3 := NewTerm(Config{ScrollingHistory: 4}, cols, 3, nil), NewProcessor()
			feed(term3, proc3, strings.Repeat(path.char, cols))
			if c := term3.Grid().Cursor; c.Point.Column != cols-1 || !c.InputNeedsWrap {
				t.Fatalf("after one write filling the row: column %d wrap %v, want column %d with wrap pending",
					c.Point.Column, c.InputNeedsWrap, cols-1)
			}
		})
	}
}

// An unhandled charset designation leaves the slot as it was. It does not fall
// back to ASCII, so line drawing designated earlier stays in effect.
func TestUnhandledCharsetDesignationLeavesSlot(t *testing.T) {
	term, proc := NewTerm(Config{ScrollingHistory: 4}, 8, 2, nil), NewProcessor()
	feed(term, proc, "\x1b(0") // designate G0 as line drawing
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetLineDrawing {
		t.Fatalf("after ESC ( 0, G0 is %v, want line drawing", got)
	}
	feed(term, proc, "\x1b(A") // UK: not implemented
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetLineDrawing {
		t.Fatalf("after ESC ( A, G0 is %v, want line drawing still (it must not fall back to ASCII)", got)
	}
	feed(term, proc, "\x1b(B") // ASCII: implemented
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetASCII {
		t.Fatalf("after ESC ( B, G0 is %v, want ASCII", got)
	}

	// Two escapes that designate nothing write the slots anyway, which is why the
	// rule above is about designations and not about every escape.
	feed(term, proc, "\x1b(0\x1bc") // line drawing, then RIS
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetASCII {
		t.Fatalf("after RIS, G0 is %v, want ASCII", got)
	}
	feed(term, proc, "\x1b7\x1b(0") // DECSC with G0 on ASCII, then line drawing
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetLineDrawing {
		t.Fatalf("before DECRC, G0 is %v, want line drawing", got)
	}
	feed(term, proc, "\x1b8") // DECRC restores the designations with the cursor
	if got := term.Grid().Cursor.Charsets[0]; got != CharsetASCII {
		t.Fatalf("after DECRC, G0 is %v, want the saved ASCII", got)
	}
}

// Entering the alternate screen overwrites the primary screen's DECSC save with
// the position at the switch, because saving the cursor is part of what DECSET
// 1049 does. A DECRC after the round trip restores the switch position.
func TestDecscIsLostAcrossAltScreen(t *testing.T) {
	term, proc := NewTerm(Config{ScrollingHistory: 10}, 20, 5, nil), NewProcessor()

	feed(term, proc, "\x1b[3;5H") // Point{2,4}
	feed(term, proc, "\x1b7")     // DECSC
	saved := term.Grid().SavedCursor.Point
	feed(term, proc, "\x1b[2;3H") // Point{1,2}
	atSwitch := term.Grid().Cursor.Point
	if saved == atSwitch {
		t.Fatalf("test is void: the saved point and the switch point are equal (%+v)", saved)
	}

	feed(term, proc, "\x1b[?1049h\x1b[?1049l\x1b8") // enter alt, leave, DECRC
	got := term.Grid().Cursor.Point

	if got == saved {
		t.Fatalf("DECRC restored the DECSC save %+v, so it survived the round trip", saved)
	}
	if got != atSwitch {
		t.Fatalf("DECRC restored %+v, want the position at the switch %+v", got, atSwitch)
	}
}

// Color.String is documented as unique over every color the constructors can
// make, which is what lets a caller use it as a comparison key. fmt calls it for
// %x and %q as well as %v, so a %x on a Color prints the hex of this string
// rather than the packed bits, and any collision here would silently merge two
// colors in a comparison built that way.
func TestColorStringIsUniquePerConstructibleColor(t *testing.T) {
	seen := make(map[string]Color, 1<<16)
	add := func(c Color) {
		s := c.String()
		if prev, dup := seen[s]; dup {
			t.Fatalf("%#x and %#x both render as %q", uint32(prev), uint32(c), s)
		}
		seen[s] = c
	}
	add(NoColor)
	for n := 0; n <= 0xffff; n++ {
		add(NewNamedColor(NamedColor(n)))
	}
	for i := 0; i <= 0xff; i++ {
		add(NewIndexedColor(uint8(i)))
	}

	// The RGB cube is too large to hold in a map, but its rendering is a fixed
	// width with a prefix none of the others use, so recovering the components
	// from the string is enough to show it is injective.
	for v := 0; v < 1<<24; v++ {
		c := NewRGBColor(uint8(v>>16), uint8(v>>8), uint8(v))
		s := c.String()
		if len(s) != 7 || s[0] != '#' {
			t.Fatalf("RGB %#06x renders as %q, want a 7-character #rrggbb", v, s)
		}
		back, err := strconv.ParseUint(s[1:], 16, 32)
		if err != nil || int(back) != v {
			t.Fatalf("RGB %#06x renders as %q, which reads back as %#06x (err %v)", v, s, back, err)
		}
	}
	for s := range seen {
		if strings.HasPrefix(s, "#") {
			t.Fatalf("%q shares the RGB prefix, so it can collide with a cube entry", s)
		}
	}

	// A kind outside the four cannot be constructed, but if one is fabricated it
	// must not read as an unset color.
	for k := uint32(4); k < 8; k++ {
		if got := Color(k << 24).String(); got == "None" {
			t.Fatalf("a color of kind %d renders as %q, which is NoColor's rendering", k, got)
		}
	}
}
