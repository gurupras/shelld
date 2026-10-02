package utf8x

import (
	"math/rand/v2"
	"testing"
	"unicode/utf8"
)

// randomBytes draws over the lead bytes, continuations and boundary values the
// error model turns on, so runs where a maximal invalid subpart is one, two or
// three bytes all occur.
func randomBytes(rng *rand.Rand) []byte {
	pool := []byte{
		'a', ' ', '\n', 0x7f,
		0x80, 0xbf, 0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xed, 0xef, 0xf0, 0xf4, 0xf5, 0xff,
	}
	b := make([]byte, rng.IntN(12))
	for i := range b {
		b[i] = pool[rng.IntN(len(pool))]
	}
	return b
}

// Lossy and LossyLenValid answer "is this valid?" with unicode/utf8 rather than
// with Check, because the standard library scans a word at a time where Check
// walks bytes. That is only sound while the two agree on validity — they are
// meant to differ on where an invalid sequence ends, never on whether one is
// there. This is the assertion the fast path rests on; if it fails, a capture
// gains or loses replacement characters.
func TestValidAgreesWithStdlib(t *testing.T) {
	check := func(b []byte) {
		t.Helper()
		_, _, ok := Check(b)
		if want := utf8.Valid(b); ok != want {
			t.Errorf("Check(%q) ok = %v, utf8.Valid = %v", b, ok, want)
		}
	}
	for _, s := range []string{
		"hello", "", "a\xffb", "\xe2\x82A", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\xc3\xa9\x80\x80z", "\x80", "\x80\x80\x80", "\xff\xff",
		"é→𝄞", "\xf4\x90\x80\x80", "\xc0\xaf", "\xe0\x80\xaf", "\xed\x9f\xbf",
		"\xee\x80\x80", "\xf4\x8f\xbf\xbf", "\xc2\x80", "\xdf\xbf", "\xe0\xa0\x80",
	} {
		check([]byte(s))
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 50_000 {
		check(randomBytes(rng))
	}
}

// The second return of LossyLenValid is what lets a bounded read join lines it
// has already measured without validating them again, so it has to mean exactly
// "Lossy would return these bytes unchanged". The length cannot stand in for it:
// a truncated 4-byte sequence is three bytes and renders as a three-byte U+FFFD.
func TestLossyLenValidReportsValidity(t *testing.T) {
	check := func(b []byte) {
		t.Helper()
		n, valid := LossyLenValid(b)
		if want := len(Lossy(b)); n != want {
			t.Errorf("LossyLenValid(%q) length = %d, want %d", b, n, want)
		}
		if want := Lossy(b) == string(b); valid != want {
			t.Errorf("LossyLenValid(%q) valid = %v, want %v", b, valid, want)
		}
	}
	for _, s := range []string{"hello", "", "é→𝄞", "a\xffb", "\xf0\x9f\x98", "\xed\xa0\x80"} {
		check([]byte(s))
	}
	// The case that rules out len(b) == LossyLen(b) as a validity test.
	if n, valid := LossyLenValid([]byte("\xf0\x9f\x98")); n != 3 || valid {
		t.Errorf(`LossyLenValid("\xf0\x9f\x98") = %d, %v; want 3, false`, n, valid)
	}
	rng := rand.New(rand.NewPCG(5, 6))
	for range 20_000 {
		check(randomBytes(rng))
	}
}

// The two properties of validUpTo that Check's doc promises on a !ok return: it
// indexes inside s, and the byte there is >= 0x80. Neither is visible from a call
// site, and one caller acts on both — vt.Processor.advanceGround indexes s[validUpTo]
// and dispatches it as a C1 control having tested only `<= 0x9f`. Without the lower
// bound it would hand an ASCII control to the C1 path; without the index bound it
// would panic. Both come from the same one line in the loop, `if b < 0x80`, which is
// exactly the kind of thing a later rewrite drops as an ASCII fast path.
//
// errLen is tallied so the assertions cannot pass vacuously: the interesting return
// is errLen == 0, where the position names the LEAD byte of a sequence the buffer
// ended inside rather than a byte that is wrong on its own.
func TestCheckReportsANonASCIIPosition(t *testing.T) {
	seen := map[int]int{}
	check := func(b []byte) {
		t.Helper()
		validUpTo, errLen, ok := Check(b)
		if ok {
			if validUpTo != len(b) {
				t.Errorf("Check(%q) ok, validUpTo = %d, want %d", b, validUpTo, len(b))
			}
			return
		}
		seen[errLen]++
		if validUpTo < 0 || validUpTo >= len(b) {
			t.Fatalf("Check(%q) = %d, %d, false; validUpTo not inside a %d-byte slice",
				b, validUpTo, errLen, len(b))
		}
		if b[validUpTo] < 0x80 {
			t.Errorf("Check(%q) = %d, %d, false; byte there is %#x, want >= 0x80",
				b, validUpTo, errLen, b[validUpTo])
		}
	}
	for _, s := range []string{
		"hello", "", "a\xffb", "\xe2\x82A", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\xc3\xa9\x80\x80z", "\x80", "\x80\x80\x80", "\xff\xff", "é→𝄞",
		"\xf4\x90\x80\x80", "\xc0\xaf", "\xe0\x80\xaf", "\xc2", "\xe0\xa0",
		"\xf0\x9f\x98\x00", "\x7f\x80", "abc\x1b", "\x00\x1f\x80",
	} {
		check([]byte(s))
	}
	rng := rand.New(rand.NewPCG(7, 8))
	for range 50_000 {
		check(randomBytes(rng))
	}
	for _, errLen := range []int{0, 1, 2, 3} {
		if seen[errLen] == 0 {
			t.Errorf("no !ok return with errLen = %d; the assertions above did not cover it", errLen)
		}
	}
}

func TestLossy(t *testing.T) {
	cases := map[string]string{
		"hello":             "hello",
		"a\xffb":            "a�b",
		"\xe2\x82A":         "�A",  // one replacement for a truncated 3-byte sequence
		"\xf0\x9f\x98":      "�",   // incomplete at end
		"\xed\xa0\x80":      "���", // surrogate: E0-EF rule rejects second byte
		"\xc3\xa9\x80\x80z": "é��z",
		"":                  "",
	}
	for in, want := range cases {
		if got := Lossy([]byte(in)); got != want {
			t.Errorf("Lossy(%q) = %q, want %q", in, got, want)
		}
	}
}

// LossyLen is what the byte-bounded reads measure their budget against, so it
// has to be len(Lossy) exactly and not an estimate: one byte out and a read
// either breaks its own bound or drops a line that fit.
//
// The random arm is what makes this more than a restatement of the table. Its
// bytes are drawn to land on the boundaries the error model turns on — lead
// bytes with too few continuations, continuations with no lead, the surrogate
// and over-long ranges — so runs where a maximal invalid subpart is one, two or
// three bytes all occur, and each is a different arithmetic in the loop.
func TestLossyLenMatchesLossy(t *testing.T) {
	check := func(b []byte) {
		t.Helper()
		if got, want := LossyLen(b), len(Lossy(b)); got != want {
			t.Errorf("LossyLen(%q) = %d, want %d (= len(Lossy))", b, got, want)
		}
	}
	for _, s := range []string{
		"hello", "", "a\xffb", "\xe2\x82A", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\xc3\xa9\x80\x80z", "\x80", "\x80\x80\x80", "\xff\xff",
		"é→𝄞", "\xf4\x90\x80\x80", "\xc0\xaf", "\xe0\x80\xaf",
	} {
		check([]byte(s))
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for range 20_000 {
		check(randomBytes(rng))
	}

	// The reason it exists: it is called once per candidate line on the /scroll
	// read path, where len(Lossy(...)) allocated a string that was thrown away.
	b := []byte("a\xffb\xe2\x82ccc\xf0\x9f\x98")
	if n := testing.AllocsPerRun(100, func() { _ = LossyLen(b) }); n > 0 {
		t.Errorf("LossyLen allocates %.1f times per call; it is on the read path "+
			"to avoid exactly that", n)
	}
}
