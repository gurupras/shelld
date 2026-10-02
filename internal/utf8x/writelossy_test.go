package utf8x

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// A caller joining lines can render each line on its own instead of rendering the
// concatenation, and these tests are why that is allowed.
//
// The two are equal because a maximal invalid subpart cannot straddle the
// separator: '\n' is ASCII, so it is never a continuation byte, and an invalid
// sequence therefore always ends at or before it. Per line, a truncated sequence at
// the end of a line reports errLen == 0 ("ends inside an otherwise valid
// sequence"); in the concatenation the same bytes report an errLen that stops
// before the '\n'. Both render exactly one U+FFFD over the same byte range and
// both resume at the '\n', so the results agree.
//
// That is an argument, not evidence, and it is the argument the whole change rests
// on -- so it is tested against the concatenation, which is what the previous
// implementation actually did, rather than against a reimplementation of the new
// one.

// joinPerLine is what Scrollback.join does: render each line into one builder,
// separated by '\n'.
func joinPerLine(lines [][]byte) string {
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		WriteLossy(&sb, l)
	}
	return sb.String()
}

// joinWhole is what the previous implementation did: concatenate, then render.
func joinWhole(lines [][]byte) string { return Lossy(bytes.Join(lines, []byte("\n"))) }

// randLine builds a line out of the byte shapes that can disagree: ASCII, whole
// multi-byte runes, lone continuation bytes, lead bytes with no continuation, and
// surrogate and over-long encodings. Truncation is applied last so a line can end
// mid-sequence, which is the case where per-line and whole-payload rendering take
// different branches of Check.
func randLine(r *rand.Rand) []byte {
	var b []byte
	for n := r.Intn(12); n >= 0; n-- {
		switch r.Intn(8) {
		case 0:
			b = append(b, byte(r.Intn(0x80)))
		case 1:
			b = append(b, "é"...)
		case 2:
			b = append(b, "€"...)
		case 3:
			b = append(b, "😀"...)
		case 4:
			b = append(b, byte(0x80+r.Intn(0x40))) // lone continuation
		case 5:
			b = append(b, 0xE2, 0x82) // 3-byte lead, one continuation
		case 6:
			b = append(b, 0xED, 0xA0, 0x80) // surrogate half
		default:
			b = append(b, 0xC0, 0xAF) // over-long
		}
	}
	if len(b) > 0 && r.Intn(4) == 0 {
		b = b[:r.Intn(len(b))] // may end mid-sequence
	}
	return b
}

func TestWriteLossyPerLineJoinMatchesWholePayload(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		lines := make([][]byte, 1+r.Intn(5))
		for j := range lines {
			lines[j] = randLine(r)
		}
		got, want := joinPerLine(lines), joinWhole(lines)
		if got != want {
			t.Fatalf("case %d lines=%q\n per-line %q\n whole    %q", i, lines, got, want)
		}
	}
}

// The pair that motivated the argument above, pinned by hand so a generator change
// cannot quietly drop it: a line ending inside a 3-byte sequence, followed by one
// that begins with a byte which cannot continue it.
func TestWriteLossyTruncatedSequenceAtLineEnd(t *testing.T) {
	lines := [][]byte{append([]byte("a"), 0xE2, 0x82), []byte("b")}
	const want = "a�\nb"
	if got := joinPerLine(lines); got != want {
		t.Errorf("per-line = %q, want %q", got, want)
	}
	if got := joinWhole(lines); got != want {
		t.Errorf("whole = %q, want %q", got, want)
	}
}

// WriteLossy writes into a builder that already holds bytes, which is the whole
// reason it exists and the one thing Lossy never exercises: Lossy always starts
// from an empty builder, so a WriteLossy that reset or ignored what was there
// would still pass every Lossy test.
func TestWriteLossyAppendsToExistingContent(t *testing.T) {
	for _, in := range [][]byte{
		[]byte("plain"),
		{'x', 0xFF, 'y'},
		{0xE2, 0x82},
		{},
	} {
		var sb strings.Builder
		sb.WriteString("PRE")
		WriteLossy(&sb, in)
		sb.WriteString("POST")
		if got, want := sb.String(), "PRE"+Lossy(in)+"POST"; got != want {
			t.Errorf("in=%q: got %q, want %q", in, got, want)
		}
	}
}

// LossyLen is what sizes the join, so it has to agree with what WriteLossy writes.
// A size that disagrees costs only a growth today, but the two are meant to measure
// the same thing and a drift between them is worth failing on.
func TestLossyLenMatchesWriteLossyLength(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 20000; i++ {
		b := randLine(r)
		var sb strings.Builder
		WriteLossy(&sb, b)
		if got, want := sb.Len(), LossyLen(b); got != want {
			t.Fatalf("b=%q: wrote %d bytes, LossyLen says %d", b, got, want)
		}
	}
}

func FuzzWriteLossyPerLineJoin(f *testing.F) {
	f.Add([]byte("a"), []byte("b"))
	f.Add([]byte{'a', 0xE2, 0x82}, []byte("b"))
	f.Add([]byte{0xED, 0xA0}, []byte{0x80})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		lines := [][]byte{a, b}
		if got, want := joinPerLine(lines), joinWhole(lines); got != want {
			t.Fatalf("per-line %q != whole %q for %q", got, want, lines)
		}
	})
}
