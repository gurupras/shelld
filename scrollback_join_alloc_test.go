package shelld

import (
	"bytes"
	"runtime"
	"testing"
)

// joinFixture is lines lines of width bytes each. The invalid variant uses 0xFF,
// which is never valid UTF-8 and never a continuation byte, so every byte is its own
// maximal invalid subpart and the line renders to three times its raw length. That is
// the widest gap there is between what joinLocked's sum of len(line) predicts and what
// join actually writes, which is the direction that has to be checked: see join on why
// the hint may only err low.
func joinFixture(t *testing.T, lines, width int, invalid bool) *Scrollback {
	t.Helper()
	fill := byte('a')
	if invalid {
		fill = 0xFF
	}
	var buf []byte
	for range lines {
		buf = append(append(buf, bytes.Repeat([]byte{fill}, width)...), '\n')
	}
	s := NewScrollback(lines * 2)
	s.Write(buf)
	return s
}

// The size handed to join bounds what join then writes, exactly on the bounded path
// and from below on the unbounded one.
//
// This is the invariant join's comment rests on, and it is the reason size may be
// passed in unchecked. It is asserted here rather than left to the callers' agreement
// because size reaches nothing but Grow: a wrong one leaves the returned text
// byte-for-byte correct, so no test of the text can see it. Returning 0, or 64 times
// the real size, is invisible to every other test in this package.
//
// The two relations differ on purpose. fitLines accumulates LossyLen, which is the
// rendered length, so its size is exact. joinLocked accumulates len, which is the
// rendered length only when every line is valid UTF-8 and below it otherwise, because
// LossyLen(b) >= len(b) for every b.
//
// Note what each half of this test is worth. fitLines is called here, so its size is
// observed. joinLocked's is not: it returns only the joined string, and size reaches
// nothing a caller can read, so the unbounded arm recomputes the same arithmetic and
// checks the arithmetic rather than the call. That joinLocked actually performs it is
// pinned by TestScrollbackReadAllocatesOnceAndNoLargerThanItsResult, whose unbounded
// arms are the only thing that fails when the sum is dropped.
func TestScrollbackJoinSizeBoundsTheRenderedLength(t *testing.T) {
	const lines, width = 200, 300

	for _, invalid := range []bool{false, true} {
		name := "valid"
		if invalid {
			name = "invalid"
		}
		t.Run(name, func(t *testing.T) {
			s := joinFixture(t, lines, width, invalid)

			s.mu.RLock()
			defer s.mu.RUnlock()

			// The bounded path, with a bound too large to cut anything, so keep is
			// every line and size is the whole rendered payload.
			keep, valid, size := s.fitLines(s.n-lines, lines, 1<<30, true)
			if keep != lines {
				t.Fatalf("fitLines kept %d of %d lines under a bound nothing can reach; "+
					"the arms below would then be measuring a different payload", keep, lines)
			}
			if valid == invalid {
				t.Fatalf("fitLines reports valid=%v for a payload built to be invalid=%v; "+
					"the fixture is not exercising the arm it names", valid, invalid)
			}
			if text := s.join(s.n-keep, keep, valid, size); len(text) != size {
				t.Errorf("fitLines sized the join at %d and join wrote %d bytes; it "+
					"accumulates LossyLen, which is the rendered length exactly, and "+
					"join's buffer is the string it returns, so an oversize hint is "+
					"memory the consumer keeps", size, len(text))
			}

			// The unbounded path, which has no measurement to reuse and so sizes from
			// the raw lengths.
			unboundedSize := lines - 1
			for i := range lines {
				unboundedSize += len(s.line(s.n - lines + i))
			}
			rendered := len(s.join(s.n-lines, lines, false, unboundedSize))
			switch {
			case unboundedSize > rendered:
				t.Errorf("joinLocked would size the join at %d for a payload that "+
					"renders to %d: above the rendered length the Grow'n slack is "+
					"retained, because Builder.String hands over its buffer",
					unboundedSize, rendered)
			case !invalid && unboundedSize != rendered:
				t.Errorf("joinLocked would size the join at %d for an all-valid payload "+
					"that renders to %d; with nothing to render lossily the raw lengths "+
					"are the rendered length and the read should not grow at all",
					unboundedSize, rendered)
			case invalid && unboundedSize == rendered:
				t.Errorf("joinLocked's size equals the rendered length %d for a payload "+
					"of bytes that all render to U+FFFD; the fixture is not producing "+
					"the undershoot this arm exists to bound", rendered)
			}
		})
	}
}

// A read allocates once, and allocates no more than the text it returns.
//
// Neither half implies the other, and the change these pin is the point of assembling
// the join in a strings.Builder: the read used to allocate the payload twice, once to
// build it and once to convert it to a string.
//
// The byte bound is what makes an unchecked size hint safe. Builder.String returns its
// buffer rather than a copy, so the returned string's backing array is the whole Grow'n
// allocation and the collector keeps all of it live for as long as the consumer holds
// the text. A hint several times too large keeps the allocation count at 1 and the text
// correct, and shows up only here.
//
// Allocation counts and allocated bytes are counters the runtime maintains exactly, so
// unlike a duration they mean the same thing on a loaded box as on an idle one.
func TestScrollbackReadAllocatesOnceAndNoLargerThanItsResult(t *testing.T) {
	const lines, width = 500, 800

	valid, invalid := joinFixture(t, lines, width, false), joinFixture(t, lines, width, true)

	// TotalAlloc is cumulative and monotonic, so the delta over a known number of
	// calls is exact for whatever ran in between. Nothing else in this test allocates
	// while it runs.
	perCall := func(read func() int) (bytesPerCall, textLen int) {
		const reps = 20
		textLen = read() // Let any first-call growth land outside the measurement.
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range reps {
			read()
		}
		runtime.ReadMemStats(&after)
		return int(after.TotalAlloc-before.TotalAlloc) / reps, textLen
	}

	for _, arm := range []struct {
		name string
		read func() int
		// maxAllocs is 1 wherever the hint is exact. It is above 1 only for the
		// unbounded read of invalid bytes, the one case where joinLocked deliberately
		// errs low: that payload renders to three times the raw length its sum of
		// len(line) predicts, and Builder closes the gap by geometric regrowth, since
		// each grow asks for twice the current capacity. The ceiling bounds that while
		// staying well under the twenty-odd a read with no hint at all would take.
		maxAllocs float64
	}{
		{"bounded/valid", func() int { text, _, _, _ := valid.LastNBytes(lines, 1<<30); return len(text) }, 1},
		{"bounded/invalid", func() int { text, _, _, _ := invalid.LastNBytes(lines, 1<<30); return len(text) }, 1},
		{"unbounded/valid", func() int { return len(valid.LastN(lines)) }, 1},
		{"unbounded/invalid", func() int { return len(invalid.LastN(lines)) }, 8},
	} {
		t.Run(arm.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(50, func() { arm.read() }); got > arm.maxAllocs {
				t.Errorf("%.1f allocations per read, want at most %v: the join is meant "+
					"to assemble the payload once, into a buffer sized before it is filled",
					got, arm.maxAllocs)
			}
			bytesPerCall, textLen := perCall(arm.read)
			// The read allocates the rendered length, plus size-class rounding, plus
			// whatever regrowth an undershooting hint costs. Four times it depends on
			// none of those and still names a hint that is multiples too large.
			if limit := 4 * textLen; bytesPerCall > limit {
				t.Errorf("%d bytes allocated per read returning %d bytes, limit %d: a "+
					"size hint above the rendered length is retained for the life of "+
					"the returned text, because Builder.String hands over the buffer "+
					"rather than a copy of it", bytesPerCall, textLen, limit)
			}
		})
	}
}
