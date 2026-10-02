// Package utf8x reproduces Rust's UTF-8 validation and lossy decoding, whose
// error boundaries (maximal subparts) differ from Go's unicode/utf8. The Rust
// daemon's observable output depends on those boundaries: the terminal
// parser's replacement characters and the text of /capture responses.
package utf8x

import (
	"strings"
	"unicode/utf8"
)

// isValid is Check's ok, and only that. Validity is the one question this
// package and unicode/utf8 answer identically — the models differ on where an
// invalid sequence ends, not on whether one is there — so the whole-buffer
// question can use the standard library's word-at-a-time scan instead of
// Check's byte loop. TestValidAgreesWithStdlib holds the two to that agreement.
func isValid(b []byte) bool { return utf8.Valid(b) }

// Check reports how far s is valid UTF-8 using Rust's str::from_utf8
// error model: errLen is the length of the invalid sequence (1..3), or 0
// when s ends inside an otherwise valid sequence.
//
// Two guarantees about validUpTo hold on every !ok return, including errLen == 0,
// and callers outside this package rely on both. It indexes inside s, so s[validUpTo]
// is in range; and s[validUpTo] >= 0x80, because the loop skips an ASCII byte before
// validUpTo is ever assigned from the position. On the ok return neither applies:
// validUpTo == len(s) and indexing it is out of range. These are promises of this
// function, not accidents of the loop below — see TestCheckReportsANonASCIIPosition,
// and vt.Processor.advanceGround, which dispatches s[validUpTo] as a C1 control
// having tested only its upper bound.
func Check(s []byte) (validUpTo, errLen int, ok bool) {
	i := 0
	n := len(s)
	for i < n {
		b := s[i]
		if b < 0x80 {
			i++
			continue
		}
		start := i
		var width int
		switch {
		case b >= 0xc2 && b <= 0xdf:
			width = 2
		case b >= 0xe0 && b <= 0xef:
			width = 3
		case b >= 0xf0 && b <= 0xf4:
			width = 4
		default:
			return start, 1, false
		}
		if i+1 >= n {
			return start, 0, false
		}
		c1 := s[i+1]
		switch width {
		case 2:
			if c1&0xc0 != 0x80 {
				return start, 1, false
			}
		case 3:
			switch {
			case b == 0xe0 && c1 >= 0xa0 && c1 <= 0xbf,
				b >= 0xe1 && b <= 0xec && c1 >= 0x80 && c1 <= 0xbf,
				b == 0xed && c1 >= 0x80 && c1 <= 0x9f,
				b >= 0xee && b <= 0xef && c1 >= 0x80 && c1 <= 0xbf:
			default:
				return start, 1, false
			}
			if i+2 >= n {
				return start, 0, false
			}
			if s[i+2]&0xc0 != 0x80 {
				return start, 2, false
			}
		case 4:
			switch {
			case b == 0xf0 && c1 >= 0x90 && c1 <= 0xbf,
				b >= 0xf1 && b <= 0xf3 && c1 >= 0x80 && c1 <= 0xbf,
				b == 0xf4 && c1 >= 0x80 && c1 <= 0x8f:
			default:
				return start, 1, false
			}
			if i+2 >= n {
				return start, 0, false
			}
			if s[i+2]&0xc0 != 0x80 {
				return start, 2, false
			}
			if i+3 >= n {
				return start, 0, false
			}
			if s[i+3]&0xc0 != 0x80 {
				return start, 3, false
			}
		}
		i += width
	}
	return n, 0, true
}

// LossyLen is len(Lossy(b)) without building the string.
//
// It exists for measuring a byte budget: a caller deciding how much it can
// return has to measure what the caller will receive, which is the Lossy form,
// and Lossy allocates a string per candidate. LossyLen(b) >= len(b) always,
// strictly greater whenever a maximal invalid subpart is one or two bytes.
func LossyLen(b []byte) int {
	n, _ := LossyLenValid(b)
	return n
}

// LossyLenValid is LossyLen along with whether b was already valid UTF-8, which
// the measurement establishes anyway.
//
// It exists so that a caller measuring lines one at a time and then joining them
// does not validate the same bytes twice: valid lines joined with an ASCII
// separator are valid, so the join can skip Lossy. The second return cannot be
// derived from the first — a three-byte maximal invalid subpart renders as a
// three-byte U+FFFD, so LossyLen(b) == len(b) does not mean b was valid.
func LossyLenValid(b []byte) (int, bool) {
	if isValid(b) {
		return len(b), true
	}
	n := 0
	for {
		upTo, errLen, ok := Check(b)
		if ok {
			return n + len(b), false
		}
		n += upTo + len("�")
		if errLen == 0 {
			return n, false
		}
		b = b[upTo+errLen:]
	}
}

// Lossy is String::from_utf8_lossy: each maximal invalid subpart becomes one
// U+FFFD.
func Lossy(b []byte) string {
	if isValid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + len("\uFFFD"))
	writeLossyInvalid(&sb, b)
	return sb.String()
}

// WriteLossy writes to sb what Lossy would have returned, without building a
// string for b first.
//
// It exists for a caller joining many pieces into one result. Lossy allocates a
// string per piece and the caller then copies each of them into the join, so the
// payload is allocated twice and copied twice. Writing into the caller's builder
// does it once, and a caller that can size the builder up front \u2014 LossyLen, or
// the sum of it over the pieces \u2014 gets the whole join in one allocation.
//
// sb is a *strings.Builder and not an io.Writer because the point is to reach the
// caller's buffer with no intermediate: Builder's writes cannot fail, so there is
// no error to return and no error path for a caller to get wrong.
func WriteLossy(sb *strings.Builder, b []byte) {
	if isValid(b) {
		sb.Write(b)
		return
	}
	writeLossyInvalid(sb, b)
}

// writeLossyInvalid is the rendering loop, for a b its caller has already found
// invalid.
//
// It is split out so that neither entry point scans b twice. Lossy and WriteLossy
// each have their own fast path to choose -- a string of b, or a copy of b into the
// caller's builder -- so each must ask isValid itself; had Lossy reached the loop by
// calling WriteLossy, the buffer it had just found invalid would have been scanned
// again to be told so a second time. This function asks nothing, so the second scan
// cannot come back by a caller being added.
func writeLossyInvalid(sb *strings.Builder, b []byte) {
	// The loop runs while b starts with something invalid and writes one U+FFFD per
	// maximal invalid subpart; it falls out as soon as the rest of b is clean, and
	// that remainder is the last write. Entering with ok already true would mean
	// isValid and Check disagreed about whether b is valid, which they are held not
	// to -- but then the whole of b is written verbatim, rather than a U+FFFD being
	// invented for an error neither the length nor the offset describes.
	valid, errLen, ok := Check(b)
	for !ok {
		sb.Write(b[:valid])
		sb.WriteString("\uFFFD")
		if errLen == 0 {
			return
		}
		b = b[valid+errLen:]
		valid, errLen, ok = Check(b)
	}
	sb.Write(b)
}
