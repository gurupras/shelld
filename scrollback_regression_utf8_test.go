package shelld

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The regression guard for the partial-UTF-8 hold, stated as the property the bug
// violated rather than as the cases that happened to expose it: a PTY read can end
// at any byte, so valid text must render whole no matter where the chunk
// boundaries fall, and a read taken between two chunks must never show damage for
// a character that is merely still arriving.
//
// This asserts that at EVERY byte boundary, not at boundaries chosen by hand. The
// hand-picked cases in scrollback_join_test.go are worth keeping for the diagnostic
// they print, but they are a sample of the space and this is the space: a fix that
// works for a two-byte rune split one way and fails for a four-byte rune split
// another is green on the sample and red here.
//
// Two invariants are checked after every single write:
//
//   - the read is valid UTF-8 and contains no U+FFFD, because the input contains
//     no invalid byte at any prefix -- only incomplete sequences, which are not
//     the same thing;
//   - the read is a byte prefix of the whole string. This is the load-bearing one.
//     Validity alone would accept a reader that dropped the held bytes and never
//     resumed them, and a stored half-rune renders as U+FFFD, which is valid UTF-8
//     and is NOT a prefix of the source. Together they pin both failure directions.
//
// TotalBytes is asserted at the end of every split: held bytes are counted on the
// write that brought them, so the count must be independent of the chunking.
func TestScrollbackRendersValidTextWholeAtEveryChunkBoundary(t *testing.T) {
	// No terminator and no '\r' in any of these: both would legitimately change
	// what the last line holds, and this test is about the UTF-8 hold alone.
	for _, s := range []string{
		"abc",                  // all one-byte: no hold is ever taken
		"aé中\U0001F600b",       // one rune of every length, in one line
		"中中中",                  // three-byte runes back to back
		"\U0001F600\U0001F600", // four-byte runes, where a split can land three ways
		"héllo wörld",
		"\U0001F600",               // a single rune that is nothing but a split candidate
		"aa\U0001F600\xe4\xb8\x96", // a lead late in the string
	} {
		t.Run(strings.ToValidUTF8(s, "?"), func(t *testing.T) {
			if !utf8.ValidString(s) {
				t.Fatalf("test input %q is not valid UTF-8; the property does not apply", s)
			}
			// Every split into one, two and three non-empty chunks.
			for _, chunks := range splits(s) {
				b := NewScrollback(10)
				for n, c := range chunks {
					b.Write([]byte(c))
					got := b.LastN(1)
					if !utf8.ValidString(got) {
						t.Fatalf("split %q, after chunk %d of %d: read %q is not valid UTF-8\n"+
							"a character still arriving was stored before it was complete",
							chunks, n+1, len(chunks), got)
					}
					if strings.ContainsRune(got, utf8.RuneError) {
						t.Fatalf("split %q, after chunk %d of %d: read %q contains U+FFFD\n"+
							"the input has no invalid byte, only an incomplete sequence",
							chunks, n+1, len(chunks), got)
					}
					if !strings.HasPrefix(s, got) {
						t.Fatalf("split %q, after chunk %d of %d: read %q is not a prefix of %q\n"+
							"held bytes were dropped, reordered, or rendered as damage",
							chunks, n+1, len(chunks), got, s)
					}
				}
				if got := b.LastN(1); got != s {
					t.Fatalf("split %q: final read %q, want %q: held bytes were never resumed",
						chunks, got, s)
				}
				if got := b.TotalBytes(); got != uint64(len(s)) {
					t.Fatalf("split %q: TotalBytes is %d, want %d: the count must not depend\n"+
						"on where the chunks fell", chunks, got, len(s))
				}
			}
		})
	}
}

// splits enumerates every division of s into one, two or three non-empty chunks at
// byte boundaries -- deliberately byte boundaries and not rune boundaries, since
// splitting inside a rune is the whole point.
func splits(s string) [][]string {
	out := [][]string{{s}}
	for i := 1; i < len(s); i++ {
		out = append(out, []string{s[:i], s[i:]})
		for j := i + 1; j < len(s); j++ {
			out = append(out, []string{s[:i], s[i:j], s[j:]})
		}
	}
	return out
}
