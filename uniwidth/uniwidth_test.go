package uniwidth

import (
	"bufio"
	"os"
	"testing"
)

// TestMatchesRustTable checks every codepoint against a range dump produced
// by the Rust unicode-width 0.2.2 crate (`start end width`, 255 = None).
func TestMatchesRustTable(t *testing.T) {
	f, err := os.Open("testdata/unicode-width-0.2.2.ranges")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	checked := 0
	for sc.Scan() {
		var start, end, w int
		if _, err := fmtSscan(sc.Text(), &start, &end, &w); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		want := w
		if w == 255 {
			want = None
		}
		for cp := start; cp <= end; cp++ {
			if cp >= 0xd800 && cp <= 0xdfff {
				continue // surrogates are not scalar values
			}
			if got := Width(rune(cp)); got != want {
				t.Fatalf("U+%04X: got %d want %d", cp, got, want)
			}
			checked++
		}
	}
	if checked < 0x10f000 {
		t.Fatalf("only checked %d codepoints", checked)
	}
}

func TestSpotChecks(t *testing.T) {
	cases := map[rune]int{'a': 1, '中': 2, '́': 0, '\x1b': None, 0x7f: None, '🙂': 2, '❯': 1}
	for r, want := range cases {
		if got := Width(r); got != want {
			t.Errorf("Width(%U) = %d, want %d", r, got, want)
		}
	}
}

func BenchmarkWidthASCII(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Width(rune('a' + i%26))
	}
}

func BenchmarkWidthCJK(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Width(rune(0x4e00 + i%2000))
	}
}
