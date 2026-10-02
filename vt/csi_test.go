package vt

import "testing"

// SGR color parsing consumes parameters exactly as vte's
// attrs_from_sgr_parameters: a color that fails to parse still consumes the
// parameters read up to the failure, and the rest are applied as SGR.
func TestSGRColorParameterConsumption(t *testing.T) {
	for _, tc := range []struct {
		seq   string
		fg    Color
		flags Flags
		ul    Color
	}{
		{"\x1b[38;2;1;2;3m", NewRGBColor(1, 2, 3), 0, NoColor},
		{"\x1b[38;5;200m", NewIndexedColor(200), 0, NoColor},
		{"\x1b[38;2;300;1;2;3m", defaultFg, FlagBold | FlagDim | FlagItalic, NoColor}, // 300 consumed; 1 2 3 apply
		{"\x1b[38;2;1;2m", defaultFg, 0, NoColor},
		{"\x1b[38;5;256;1m", defaultFg, FlagBold, NoColor},
		{"\x1b[38;7;1m", defaultFg, FlagBold, NoColor}, // unknown kind consumes only itself
		{"\x1b[38m\x1b[1m", defaultFg, FlagBold, NoColor},
		{"\x1b[38:2:1:2:3m", NewRGBColor(1, 2, 3), 0, NoColor},
		{"\x1b[38:2::4:5:6m", NewRGBColor(4, 5, 6), 0, NoColor}, // colorspace id skipped
		{"\x1b[38:2:1:2m", defaultFg, 0, NoColor},
		{"\x1b[38:5:9;1m", NewIndexedColor(9), FlagBold, NoColor},
		{"\x1b[38:2:1:2:300m", defaultFg, 0, NoColor},
		{"\x1b[58;2;7;8;9m", defaultFg, 0, NewRGBColor(7, 8, 9)},
		{"\x1b[58:5:3m", defaultFg, 0, NewIndexedColor(3)},
		{"\x1b[4:3m", defaultFg, FlagUndercurl, NoColor},
		{"\x1b[4:2:1m", defaultFg, FlagUnderline, NoColor},
		{"\x1b[31;1;38;5;4;4m", NewIndexedColor(4), FlagBold | FlagUnderline, NoColor},
		// A semicolon color reads each parameter's first value and skips its
		// subparameters.
		{"\x1b[38;2:1:2;3;4;5m", NewRGBColor(3, 4, 5), 0, NoColor},
		{"\x1b[38;5:9:9;7;1m", NewIndexedColor(7), FlagBold, NoColor},
	} {
		term, p := NewTerm(Config{}, 10, 2, nil), NewProcessor()
		feed(term, p, tc.seq)
		tmpl := term.Grid().Cursor.Template
		if tmpl.Fg() != tc.fg || tmpl.Flags() != tc.flags || tmpl.UnderlineColor() != tc.ul {
			t.Errorf("%q: fg %x flags %x ul %x, want fg %x flags %x ul %x", tc.seq, tmpl.Fg(), tmpl.Flags(), tmpl.UnderlineColor(), tc.fg, tc.flags, tc.ul)
		}
	}
}

// Final bytes dispatch only with the intermediates their sequence takes.
func TestCSIIntermediates(t *testing.T) {
	rec := &recorder{}
	term, p := NewTerm(Config{}, 10, 3, rec), NewProcessor()
	feed(term, p, "\x1b[>1m\x1b[?1m\x1b[$1m\x1b[2;2H\x1b[?2J\x1b[>2K") // all ignored
	g := term.Grid()
	if g.Cursor.Template.Flags() != 0 || g.Cursor.Point != (Point{1, 1}) {
		t.Fatalf("intermediate sequences applied: flags %x cursor %+v", g.Cursor.Template.Flags(), g.Cursor.Point)
	}
	feed(term, p, "\x1b[?25l\x1b[4h\x1b[?7$p\x1b[4$p\x1b[5 q\x1b[?12$p\x1b[>c\x1b[c")
	if term.Mode()&ModeShowCursor != 0 || term.Mode()&ModeInsert == 0 {
		t.Fatalf("modes %x", term.Mode())
	}
	// DECSCUSR takes exactly one space intermediate.
	feed(term, p, "\x1b[5 q")
	if !term.cursorBlink {
		t.Fatal("CSI 5 SP q did not set blinking")
	}
	feed(term, p, "\x1b[4$q\x1b[0!q\x1b[2\"q\x1b[?4 q\x1b[4  q")
	if !term.cursorBlink {
		t.Fatal("DECSCUSR dispatched with the wrong intermediates")
	}
	feed(term, p, "\x1b[4 q")
	if term.cursorBlink {
		t.Fatal("CSI 4 SP q did not clear blinking")
	}
	want := []string{"\x1b[?7;1$y", "\x1b[4;1$y", "\x1b[?12;1$y", "\x1b[>0;2600;1c", "\x1b[?6c"}
	if len(rec.writes) != len(want) {
		t.Fatalf("replies %q, want %q", rec.writes, want)
	}
	for i := range want {
		if rec.writes[i] != want[i] {
			t.Fatalf("replies %q, want %q", rec.writes, want)
		}
	}
}

// A CSI split across Advance calls at every byte parses like one call.
func TestCSISplitAcrossChunks(t *testing.T) {
	seq := "ab\x1b[38;2;10;20;30;1;4:3mX\x1b[65535;99999Hc\x1b[1;2;3;4;5;6;7;8;9;10;11;12;13;14;15;16;17;18;19;20;21;22;23;24;25;26;27;28;29;30;31;32;33;34mY"
	whole, pw := NewTerm(Config{}, 20, 4, nil), NewProcessor()
	feed(whole, pw, seq)
	for cut := 1; cut < len(seq); cut++ {
		split, ps := NewTerm(Config{}, 20, 4, nil), NewProcessor()
		feed(split, ps, seq[:cut])
		feed(split, ps, seq[cut:])
		sameGrid(t, "cut "+seq[:cut], whole, split)
		if whole.Grid().Cursor.Template != split.Grid().Cursor.Template {
			t.Fatalf("cut %d: template %+v vs %+v", cut, whole.Grid().Cursor.Template, split.Grid().Cursor.Template)
		}
	}
}

// A private marker after a parameter digit makes the sequence invalid, so
// xterm and alacritty ignore it. The CSI fast path must leave the parser in
// the parameter state when it stops at such a byte, or `\x1b[0?7l` would
// reset line wrap.
func TestCSIMarkerAfterDigitIgnored(t *testing.T) {
	term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
	before := term.Mode()
	if before&ModeLineWrap == 0 || before&ModeShowCursor == 0 {
		t.Fatalf("unexpected default modes %x", before)
	}
	feed(term, p, "\x1b[0?7l\x1b[0?25l\x1b[1?7l")
	if got := term.Mode(); got != before {
		t.Fatalf("marker after digit applied: modes %x, want %x", got, before)
	}
}
