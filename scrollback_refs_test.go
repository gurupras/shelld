package shelld

import "testing"

// checkRefs asserts the internal invariants of the line ring and the chunk
// list. They are not observable through the public reads until they are already
// broken: a line referencing a released chunk reads other lines' bytes, or
// bytes outside any chunk, so a differential test reports a content mismatch
// far from the write that caused it — or panics on a slice bound.
//
// The ring wrapping before refs reached capacity is what broke: appending one
// slot changes the modulus, which moves every retained line to a different
// slot, and the oldest line then no longer holds the lowest chunk number that
// release relies on.
func checkRefs(t *testing.T, tag string, s *Scrollback) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.n > s.capacity {
		t.Fatalf("%s: retains %d lines over a capacity of %d", tag, s.n, s.capacity)
	}
	if len(s.refs) > s.capacity {
		t.Fatalf("%s: the ring has %d slots, over a capacity of %d", tag, len(s.refs), s.capacity)
	}
	if s.n > len(s.refs) {
		t.Fatalf("%s: retains %d lines in %d slots", tag, s.n, len(s.refs))
	}
	if s.n == 0 {
		return
	}
	if s.start >= len(s.refs) {
		t.Fatalf("%s: oldest line is slot %d of %d", tag, s.start, len(s.refs))
	}
	// Until refs reaches capacity it grows, so the retained range must not have
	// wrapped: growing it would renumber the slots.
	if len(s.refs) < s.capacity && s.start+s.n > len(s.refs) {
		t.Fatalf("%s: the ring wrapped (start %d + %d lines) with %d of %d slots allocated",
			tag, s.start, s.n, len(s.refs), s.capacity)
	}

	held := 0
	for _, c := range s.chunks {
		held += cap(c)
	}
	if held != s.held {
		t.Fatalf("%s: holds %d chunk bytes, counted %d", tag, s.held, held)
	}

	var prev lineRef
	for i := range s.n {
		r := s.refs[(s.start+i)%len(s.refs)]
		if r.chunk < s.base || int(r.chunk-s.base) >= len(s.chunks) {
			t.Fatalf("%s: line %d of %d is in chunk %d, outside the %d chunks from %d",
				tag, i, s.n, r.chunk, len(s.chunks), s.base)
		}
		if int(r.off)+int(r.n) > len(s.chunks[r.chunk-s.base]) {
			t.Fatalf("%s: line %d of %d is %d bytes at %d in a chunk of %d",
				tag, i, s.n, r.n, r.off, len(s.chunks[r.chunk-s.base]))
		}
		if int(r.n) > sbMaxLine {
			t.Fatalf("%s: line %d of %d is %d bytes, over the %d cap", tag, i, s.n, r.n, sbMaxLine)
		}
		// Lines are appended in arrival order, which is what lets release drop
		// every chunk before the oldest retained line's.
		if i > 0 {
			if r.chunk < prev.chunk {
				t.Fatalf("%s: line %d of %d is in chunk %d, before the previous line's %d",
					tag, i, s.n, r.chunk, prev.chunk)
			}
			if r.chunk == prev.chunk && int(r.off) < int(prev.off)+int(prev.n) {
				t.Fatalf("%s: line %d of %d starts at %d in chunk %d, inside the previous line's %d..%d",
					tag, i, s.n, r.off, r.chunk, prev.off, int(prev.off)+int(prev.n))
			}
		}
		prev = r
	}
}
