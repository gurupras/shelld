package vt

import (
	"strings"
	"testing"
)

// The string states — SOS, PM, APC, and a DCS the parser gave up on — discard
// their payload and wait for a terminator. Missing one of the two terminators is
// not a parse error that shows up as a wrong glyph: the state consumes every byte
// it is given, so all program output after the sequence disappears until some
// later ESC happens to resynchronize the parser. The window looks hung.
func TestAnIgnoredStringEndsAtEitherTerminator(t *testing.T) {
	// Each opener reaches a state whose payload is thrown away. The DCS one
	// arrives at stateDcsIgnore through advanceDcsParam's 0x3c-0x3f arm.
	for _, opener := range []struct {
		name string
		seq  string
	}{
		{"APC", "\x1b_"},
		{"PM", "\x1b^"},
		{"SOS", "\x1bX"},
		{"an abandoned DCS", "\x1bP1<"},
	} {
		for _, term := range []struct {
			name string
			seq  string
		}{
			// 8-bit ST. advanceDcsPassthrough already honoured this byte, so a
			// DCS the parser kept reading ended and one it gave up on did not.
			{"8-bit ST", "\x9c"},
			// 7-bit ST and the CAN/SUB aborts went through anywhere and worked;
			// they are here so a fix to the 8-bit case cannot cost them.
			{"7-bit ST", "\x1b\\"},
			{"CAN", "\x18"},
			{"SUB", "\x1a"},
		} {
			t.Run(opener.name+" ended by "+term.name, func(t *testing.T) {
				vterm, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
				feed(vterm, p, opener.seq+"payload"+term.seq+"AB")
				// SUB prints a replacement glyph where it aborts, which is its
				// documented behaviour and not what this is testing.
				if got := rowText(vterm, 0); !strings.HasSuffix(got, "AB") {
					t.Errorf("row = %q, want it to end in %q: output after the "+
						"terminator was swallowed", got, "AB")
				}
				if p.state != stateGround {
					t.Errorf("parser state %v, want ground", p.state)
				}
			})
		}
	}
}

// An OSC that is never terminated has no bound of its own: the payload is
// accumulated for a dispatch that never comes, so the rest of the stream becomes
// resident heap in a daemon holding one Processor per window. The cap turns that
// into a bounded loss — the OSC is dropped and the parser resynchronizes.
func TestAnUnterminatedOSCIsAbandonedAtTheCap(t *testing.T) {
	t.Run("past the cap the parser returns to ground", func(t *testing.T) {
		vterm, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
		// oscRaw holds the whole raw payload, params included, so the "0" of
		// "\x1b]0;" is one of its bytes: the x that trips the cap is number
		// maxOSCRaw, and it is the byte dropped. Feeding exactly that many
		// leaves nothing of the payload to print.
		feed(vterm, p, "\x1b]0;"+strings.Repeat("x", maxOSCRaw)+"AB")
		if got := rowText(vterm, 0); got != "AB" {
			t.Errorf("row = %q, want %q: the stream never recovered", got, "AB")
		}
		if p.state != stateGround {
			t.Errorf("parser state %v, want ground", p.state)
		}
		// Released, not truncated: oscEnd keeps its backing array for reuse,
		// which would pin the peak for the Processor's life.
		if p.oscRaw != nil {
			t.Errorf("oscRaw retained %d bytes, want the array released", len(p.oscRaw))
		}
	})

	// The other side of the bound, so the cap cannot be quietly tightened onto
	// payloads it is meant to carry: one byte short of it, the OSC is still
	// open and still eating the stream.
	t.Run("short of the cap the OSC is still open", func(t *testing.T) {
		vterm, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
		feed(vterm, p, "\x1b]0;"+strings.Repeat("x", maxOSCRaw-8)+"AB")
		if got := rowText(vterm, 0); got != "" {
			t.Errorf("row = %q, want it empty: an OSC under the cap must keep reading", got)
		}
		if p.state != stateOscString {
			t.Errorf("parser state %v, want the OSC string state", p.state)
		}
	})

	// And an ordinary terminated OSC still dispatches. OSC 4;7;? is the shortest
	// one with an observable effect.
	t.Run("a terminated OSC still dispatches", func(t *testing.T) {
		r := &recorder{}
		vterm, p := NewTerm(Config{}, 10, 2, r), NewProcessor()
		feed(vterm, p, "\x1b]4;7;?\x1b\\AB")
		if len(r.colors) != 1 || r.colors[0] != 7 {
			t.Errorf("color requests %v, want [7]", r.colors)
		}
		if got := rowText(vterm, 0); got != "AB" {
			t.Errorf("row = %q, want %q", got, "AB")
		}
	})
}
