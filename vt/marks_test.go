package vt

import (
	"strings"
	"testing"
)

func marksAt(t *Term, line, col int) []rune {
	return t.Marks(t.Grid().Row(line).Cell(col).MarkID())
}

// TestMarksAreStoredInOrderAndInterned: a cell keeps its zero-width characters
// in the order they arrived, and two cells with the same sequence share one id.
func TestMarksAreStoredInOrderAndInterned(t *testing.T) {
	term := NewTerm(Config{}, 10, 2, nil)
	NewProcessor().Advance(term, []byte("é̂ é̂"))
	if got := string(marksAt(term, 0, 0)); got != "́̂" {
		t.Fatalf("marks = %+q", got)
	}
	a, b := term.Grid().Row(0).Cell(0).MarkID(), term.Grid().Row(0).Cell(2).MarkID()
	if a == 0 || a != b {
		t.Fatalf("ids %d and %d, want one shared nonzero id", a, b)
	}
	if id := term.Grid().Row(0).Cell(1).MarkID(); id != 0 {
		t.Fatalf("an unmarked cell has id %d", id)
	}
}

// TestOverwritingACellDropsItsMarks: printing over a marked cell, on either
// write path, leaves the new character without the old one's marks.
func TestOverwritingACellDropsItsMarks(t *testing.T) {
	for name, over := range map[string]string{"ascii": "a", "unicode": "ü", "wide": "中"} {
		term := NewTerm(Config{}, 10, 2, nil)
		NewProcessor().Advance(term, []byte("é\r"+over))
		if m := marksAt(term, 0, 0); m != nil {
			t.Errorf("%s: overwritten cell kept marks %+q", name, m)
		}
	}
}

// TestMarkBounds: a mark that would take a cell past 32 bytes of UTF-8, base
// included, is dropped, as is one past the per-Term number of distinct
// sequences, while the cell still counts as having absorbed it.
func TestMarkBounds(t *testing.T) {
	acute, tag := "\u0301", "\U000E0067" // 2 and 4 bytes
	flag := "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"
	for _, c := range []struct {
		name, in string
		want     int
	}{
		{"one-byte base", "e" + strings.Repeat(acute, 40), 15},                  // 1+15*2 = 31
		{"exactly 32 bytes", "\u00e9" + strings.Repeat(acute, 40), 15},          // 2+15*2 = 32
		{"wide base", "中" + strings.Repeat(acute, 40), 14},                      // 3+14*2 = 31
		{"too big after 31", "e" + strings.Repeat(acute, 15) + tag + acute, 15}, // 31+4, then 31+2
		{"four-byte marks", "e" + strings.Repeat(tag, 9), 7},
		{"no base, column 0", strings.Repeat(acute, 40), 15}, // the blank counts 1
		{"REP of a mark", "e" + acute + "\x1b[65535b", 15},
		{"subdivision flag", flag, 6}, // 4+6*4 = 28
	} {
		term := NewTerm(Config{}, 10, 2, nil)
		NewProcessor().Advance(term, []byte(c.in))
		if n := len(marksAt(term, 0, 0)); n != c.want {
			t.Errorf("%s: the cell kept %d marks, want %d", c.name, n, c.want)
		}
		if term.Grid().Row(0).Cell(0).Flags()&FlagZerowidth == 0 {
			t.Errorf("%s: FlagZerowidth not set", c.name)
		}
	}

	term := NewTerm(Config{}, 10, 2, nil)
	p := NewProcessor()
	// Each base gets a distinct pair of marks, so each needs two entries.
	for i := 0; len(term.MarkTable()) <= maxMarkSequences; i++ {
		p.Advance(term, []byte("\re"+string(rune(0x300+i%0x70))+string(rune(0x1dc0+i/0x70))))
	}
	before := len(term.MarkTable())
	p.Advance(term, []byte("\re⃐⃑"))
	if after := len(term.MarkTable()); after != before || before != maxMarkSequences+1 {
		t.Fatalf("table grew %d -> %d, want it held at %d", before, after, maxMarkSequences+1)
	}
	if term.Grid().Row(0).Cell(0).Flags()&FlagZerowidth == 0 {
		t.Fatal("a dropped mark did not set FlagZerowidth")
	}
}

// TestWritingOverASpacerClearsTheWideCharactersMarks: printing into the right
// half of a wide character blanks its left half, marks included, as
// alacritty's clear_wide does.
func TestWritingOverASpacerClearsTheWideCharactersMarks(t *testing.T) {
	term := NewTerm(Config{}, 10, 2, nil)
	NewProcessor().Advance(term, []byte("中́\x1b[1;2Hx"))
	if m := marksAt(term, 0, 0); m != nil {
		t.Fatalf("the blanked left half kept marks %+q", m)
	}
}
