package shelld

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/utf8x"
)

// sbWith writes each string into a fresh scrollback and returns it.
func sbWith(capacity int, writes ...string) *Scrollback {
	s := NewScrollback(capacity)
	for _, w := range writes {
		s.Write([]byte(w))
	}
	return s
}

// sbOfLines is a store holding exactly these lines, checked, so a fixture that
// stops holding what it claims loses its premise loudly instead of testing
// something else.
func sbOfLines(t *testing.T, lines ...string) *Scrollback {
	t.Helper()
	s := NewScrollback(1000)
	for _, l := range lines {
		s.Write([]byte(l + "\n"))
	}
	if s.Len() != len(lines) {
		t.Fatalf("fixture holds %d lines, want %d", s.Len(), len(lines))
	}
	return s
}

// A line runs to 64 KiB, so a read of "the last n lines" is not bounded in
// bytes by n. Tail bounds it, and reports the one thing a caller cannot see
// from the text: that the text begins where the bound fell rather than where
// the program began a line.
func TestScrollbackTailBytes(t *testing.T) {
	// Three lines, "aaa\nbb\nc" joined: 8 bytes, so every drop is visible in
	// the bound that causes it.
	s := sbWith(10, "aaa\nbb\nc\n")
	if got := s.Len(); got != 3 {
		t.Fatalf("history holds %d lines, want 3: %q", got, s.LastN(9))
	}
	for _, c := range []struct {
		n, maxBytes int
		text        string
		cut         bool
	}{
		{3, 1 << 20, "aaa\nbb\nc", false},
		{3, 8, "aaa\nbb\nc", false}, // exactly fits
		{3, 7, "bb\nc", false},      // one byte short: the oldest line goes whole
		{3, 4, "bb\nc", false},
		{3, 3, "c", false},
		{3, 1, "c", false},
		{3, 0, "", false},  // no bytes allowed, and none begins mid-line
		{3, -1, "", false}, // ditto, and no panic
		{2, 1 << 20, "bb\nc", false},
		{1, 1 << 20, "c", false},
		{0, 1 << 20, "", false},
		{-1, 1 << 20, "", false},
	} {
		text, cut := s.Tail(c.n, c.maxBytes)
		if text != c.text || cut != c.cut {
			t.Errorf("Tail(%d, %d) = %q, %v; want %q, %v", c.n, c.maxBytes, text, cut, c.text, c.cut)
		}
	}

	// A line longer than the bound: its end is what a tail is for, and cut says
	// the text starts at a position the program never wrote a line at.
	long := sbWith(10, strings.Repeat("z", 100)+"\n")
	text, cut := long.Tail(1, 10)
	if text != strings.Repeat("z", 10) || !cut {
		t.Errorf("Tail over one long line = %q (%d bytes), cut %v; want 10 bytes cut", clip(text), len(text), cut)
	}
	// And the lines before it are not a way around the bound.
	mixed := sbWith(10, "short\n"+strings.Repeat("z", 100)+"\n")
	if text, cut = mixed.Tail(2, 10); text != strings.Repeat("z", 10) || !cut {
		t.Errorf("Tail past a long newest line = %q, cut %v", clip(text), cut)
	}

	// A whole line that fits is kept whole even when an older one would have to
	// be cut to use the rest of the bound: the caller gets line starts it can
	// trust, and cut stays false.
	twoLong := sbWith(10, strings.Repeat("y", 60)+"\n"+strings.Repeat("z", 60)+"\n")
	if text, cut = twoLong.Tail(2, 100); text != strings.Repeat("z", 60) || cut {
		t.Errorf("Tail(2, 100) over two 60-byte lines = %d bytes, cut %v; want the newest whole", len(text), cut)
	}
}

// Cutting into a line must not cut into a rune: the text is handed to a regexp
// and to JSON, and half a rune is neither. Where a whole rune does not fit at all
// the rune wins and the bound gives way by up to three bytes, so the bound asserted
// here is max(maxBytes, utf8.UTFMax) — see
// TestScrollbackByteBoundedReadsAlwaysMakeProgress for why that is the trade.
func TestScrollbackByteReadsKeepRunesWhole(t *testing.T) {
	// A 3-byte rune, so a bound that is not a multiple of three lands inside
	// one, and a 4-byte one after it so the tail and the head disagree about
	// where the boundaries are.
	line := strings.Repeat("→", 20) + strings.Repeat("😀", 5)
	s := sbWith(10, line+"\n")
	for maxBytes := 1; maxBytes <= len(line)+2; maxBytes++ {
		text, cut := s.Tail(1, maxBytes)
		if len(text) > max(maxBytes, utf8.UTFMax) {
			t.Fatalf("Tail(1, %d) returned %d bytes", maxBytes, len(text))
		}
		// The overshoot is only ever the one rune that does not fit: the last
		// rune here is four bytes, so it is the bounds below four that give way,
		// and every other bound holds exactly.
		if maxBytes >= utf8.UTFMax && len(text) > maxBytes {
			t.Fatalf("Tail(1, %d) returned %d bytes with room for a whole rune", maxBytes, len(text))
		}
		if !utf8.ValidString(text) {
			t.Fatalf("Tail(1, %d) split a rune: %q", maxBytes, text)
		}
		if want := maxBytes < len(line); cut != want {
			t.Fatalf("Tail(1, %d) cut = %v, want %v (line is %d bytes)", maxBytes, cut, want, len(line))
		}
		if !strings.HasSuffix(line, text) {
			t.Fatalf("Tail(1, %d) = %q, not a suffix of the line", maxBytes, text)
		}
		head, _, _, _ := s.RangeBytes(0, 1, maxBytes)
		if len(head) > max(maxBytes, utf8.UTFMax) || !utf8.ValidString(head) {
			t.Fatalf("RangeBytes(0, 1, %d) = %d bytes, valid %v", maxBytes, len(head), utf8.ValidString(head))
		}
		if !strings.HasPrefix(line, head) {
			t.Fatalf("RangeBytes(0, 1, %d) = %q, not a prefix of the line", maxBytes, head)
		}
	}
}

// LastNBytes and RangeBytes drop from opposite ends, because one is reading the
// newest output and the other is paging forwards through history.
func TestScrollbackByteBoundedReadsDropFromTheRightEnd(t *testing.T) {
	s := sbWith(10, "aaa\nbb\nc\n")
	// The line counts are here rather than in their own test because they are a
	// property of each of these reads, and a count that is right in general but
	// wrong at the edges of the table is the failure a client sees.
	for _, c := range []struct {
		maxBytes  int
		last      string
		lastLines int
		lastTrunc bool
		rng       string
		rngLines  int
		rngTrunc  bool
	}{
		{1 << 20, "aaa\nbb\nc", 3, false, "aaa\nbb\nc", 3, false},
		{8, "aaa\nbb\nc", 3, false, "aaa\nbb\nc", 3, false},
		{7, "bb\nc", 2, true, "aaa\nbb", 2, true},
		{6, "bb\nc", 2, true, "aaa\nbb", 2, true},
		{5, "bb\nc", 2, true, "aaa", 1, true},
		{3, "c", 1, true, "aaa", 1, true},
		{2, "c", 1, true, "aa", 1, true}, // the first line alone is over: its start
		{0, "", 0, true, "", 0, true},    // nothing came back, so it stands for no lines
	} {
		if text, lines, _, trunc := s.LastNBytes(3, c.maxBytes); text != c.last || lines != c.lastLines || trunc != c.lastTrunc {
			t.Errorf("LastNBytes(3, %d) = %q, %d lines, %v; want %q, %d, %v",
				c.maxBytes, text, lines, trunc, c.last, c.lastLines, c.lastTrunc)
		}
		if text, lines, _, trunc := s.RangeBytes(0, 3, c.maxBytes); text != c.rng || lines != c.rngLines || trunc != c.rngTrunc {
			t.Errorf("RangeBytes(0, 3, %d) = %q, %d lines, %v; want %q, %d, %v",
				c.maxBytes, text, lines, trunc, c.rng, c.rngLines, c.rngTrunc)
		}
	}
	// Out of range reads are empty, stand for no lines, and are not truncated:
	// nothing was left out.
	for _, c := range []struct{ offset, count int }{{3, 1}, {4, 1}, {-1, 1}, {0, 0}, {0, -1}} {
		if text, lines, _, trunc := s.RangeBytes(c.offset, c.count, 1<<20); text != "" || lines != 0 || trunc {
			t.Errorf("RangeBytes(%d, %d, 1M) = %q, %d lines, %v; want empty, 0 and not truncated",
				c.offset, c.count, text, lines, trunc)
		}
	}
	if text, lines, _, trunc := s.LastNBytes(0, 1<<20); text != "" || lines != 0 || trunc {
		t.Errorf("LastNBytes(0, 1M) = %q, %d lines, %v; want empty, 0 and not truncated", text, lines, trunc)
	}
}

// A client pages through history by adding the lines it received to its offset,
// so the line count a byte-bounded page reports has to be the count the text
// stands for — otherwise paging either skips lines or repeats them. The pager
// here advances by the reported count, which is the rule the doc comments give,
// and checks the text against it.
func TestScrollbackRangeBytesPagesEveryLineOnce(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	var want []string
	s := NewScrollback(500)
	longest := 0
	for i := range 300 {
		// Lines from empty to 3 KiB, so pages hold anywhere from one line to
		// hundreds and some boundaries fall between a line and its separator.
		line := strings.Repeat(string(rune('a'+i%26)), rng.IntN(3000))
		want = append(want, line)
		longest = max(longest, len(line))
		s.Write([]byte(line + "\n"))
	}
	if s.Len() != len(want) {
		t.Fatalf("history holds %d lines, want %d", s.Len(), len(want))
	}
	// A bound below the longest line cannot page through that line, which the
	// doc comment says; at or above it, paging sees every line exactly once.
	for _, maxBytes := range []int{longest, longest + 1, 8 << 10, 64 << 10} {
		var got []string
		for offset := 0; offset < s.Len(); {
			text, returned, _, _ := s.RangeBytes(offset, 1000, maxBytes)
			if len(text) > maxBytes {
				t.Fatalf("max_bytes %d: page at %d is %d bytes", maxBytes, offset, len(text))
			}
			if returned < 1 {
				t.Fatalf("max_bytes %d: page at %d of %d reports %d lines, so the offset "+
					"cannot advance", maxBytes, offset, s.Len(), returned)
			}
			lines := strings.Split(text, "\n")
			if len(lines) != returned {
				t.Fatalf("max_bytes %d: page at %d reports %d lines and holds %d",
					maxBytes, offset, returned, len(lines))
			}
			got = append(got, lines...)
			offset += returned
		}
		if len(got) != len(want) {
			t.Fatalf("max_bytes %d: paging returned %d lines, want %d", maxBytes, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("max_bytes %d: line %d is %q, want %q", maxBytes, i, clip(got[i]), clip(want[i]))
			}
		}
	}
	// Below the longest line the page is that line's start and the rest of it
	// is unreachable. This is the documented cost of a byte bound; it is here
	// so the behaviour is a decision and not a surprise.
	i := 0
	for len(want[i]) < 100 {
		i++
	}
	text, returned, _, trunc := s.RangeBytes(i, 1000, 50)
	if len(text) != 50 || !trunc || !strings.HasPrefix(want[i], text) {
		t.Errorf("a page smaller than its first line = %d bytes, truncated %v; want the line's first 50", len(text), trunc)
	}
	if returned != 1 {
		t.Errorf("a page holding part of one line reports %d lines, want 1: the pager has to "+
			"move on, and truncated is what tells it the rest was left behind", returned)
	}
}

// The bounds must hold over the byte streams that gave the store its shape —
// terminators split from their '\r', lone '\r's, lines at the cap — and must
// agree with the unbounded reads whenever the bound is not reached.
func TestScrollbackByteBoundsHoldOverStreams(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	streams := append(joinStreams(rng), randomStream(rng), randomStream(rng))
	// A line of bytes that are not valid UTF-8: a read replaces each maximal invalid
	// subpart with one three-byte replacement character, so a line chosen by its
	// stored size can still be over the bound as text, and the bound has to be the
	// text's. The 0xc3 run below does expand 3x, because no 0xc3 in it is followed by
	// a continuation byte and so each is its own one-byte subpart — per byte and per
	// subpart coincide for this fixture and not in general.
	streams = append(streams, []byte(strings.Repeat("q", sbMaxLine+7)),
		[]byte("é"+strings.Repeat("\xc3", 400)+"\n"))
	for si, stream := range streams {
		for _, capacity := range []int{1, 7, 1000} {
			s := sbWith(capacity, string(stream))
			for _, n := range []int{1, 3, 5, 50, 100000} {
				for _, maxBytes := range []int{1, 2, 7, 64, 1000, 1 << 20} {
					tag := fmt.Sprintf("stream %d capacity %d n %d maxBytes %d", si, capacity, n, maxBytes)
					text, cut := s.Tail(n, maxBytes)
					checkBounded(t, tag+" Tail", text, maxBytes)
					if !strings.HasSuffix(s.LastN(n), text) {
						t.Fatalf("%s: Tail is not a suffix of LastN: %q", tag, clip(text))
					}
					lastN, lastLines, _, trunc := s.LastNBytes(n, maxBytes)
					checkBounded(t, tag+" LastNBytes", lastN, maxBytes)
					if lastN != text {
						t.Fatalf("%s: LastNBytes %q and Tail %q disagree", tag, clip(lastN), clip(text))
					}
					if want := lastN != s.LastN(n); trunc != want {
						t.Fatalf("%s: LastNBytes truncated %v, but the text %s the unbounded read",
							tag, trunc, map[bool]string{true: "differs from", false: "equals"}[want])
					}
					if !trunc && cut {
						t.Fatalf("%s: Tail reports a cut with nothing dropped", tag)
					}
					rangeText, rangeLines, _, trunc := s.RangeBytes(0, n, maxBytes)
					checkBounded(t, tag+" RangeBytes", rangeText, maxBytes)
					// A read that returned text reports at least one line, and no
					// read reports more lines than the store holds or more than
					// the text's own '\n' count admits: those are the ways a
					// reported count breaks a pager outright, by stalling it, by
					// running it off the end, or by skipping lines.
					//
					// The converse — no text, so no lines — does not hold: an
					// empty line is a line, and returning it returns no bytes.
					for _, r := range []struct {
						what  string
						text  string
						lines int
					}{{"LastNBytes", lastN, lastLines}, {"RangeBytes", rangeText, rangeLines}} {
						if r.text != "" && r.lines < 1 {
							t.Fatalf("%s: %s returned %d bytes and reports %d lines",
								tag, r.what, len(r.text), r.lines)
						}
						if r.lines > s.Len() {
							t.Fatalf("%s: %s reports %d lines from a store of %d",
								tag, r.what, r.lines, s.Len())
						}
						if k := strings.Count(r.text, "\n") + 1; r.lines > k {
							t.Fatalf("%s: %s reports %d lines in text holding at most %d",
								tag, r.what, r.lines, k)
						}
					}
					if !strings.HasPrefix(s.Range(0, n), rangeText) {
						t.Fatalf("%s: RangeBytes is not a prefix of Range: %q", tag, clip(rangeText))
					}
					if want := rangeText != s.Range(0, n); trunc != want {
						t.Fatalf("%s: RangeBytes truncated %v, want %v", tag, trunc, want)
					}
				}
				// A bound past what the store can hold changes nothing, which
				// is what lets a caller pass a bound and forget about it.
				if text, cut := s.Tail(n, 1<<30); text != s.LastN(n) || cut {
					t.Fatalf("stream %d capacity %d n %d: a bound of 1 GiB changed the tail", si, capacity, n)
				}
				if text, lines, _, trunc := s.RangeBytes(0, n, 1<<30); text != s.Range(0, n) || trunc {
					t.Fatalf("stream %d capacity %d n %d: a bound of 1 GiB changed the range", si, capacity, n)
				} else if want := min(n, s.Len()); lines != want {
					t.Fatalf("stream %d capacity %d n %d: an unbounded range reports %d lines, want %d",
						si, capacity, n, lines, want)
				}
			}
		}
	}
}

// The bound is measured on the bytes the caller receives, not on the bytes stored.
// A read returns utf8x.Lossy of the history, which expands each maximal invalid
// subpart, however long, into one three-byte replacement character, so a line
// that fits the bound in stored bytes can arrive over it. The line count was taken
// on the stored bytes, and the read then trimmed the joined text to hold the bound —
// which cost the caller the very thing a line-counted read promises.
//
// Every fixture here uses 0xff, a one-byte subpart and so the widest expansion. A
// truncated four-byte sequence is three stored bytes and one three-byte
// replacement, which does not expand at all, which is why "some invalid bytes" is
// not a fixture for this.
func TestScrollbackByteBoundIsMeasuredOnWhatTheCallerReceives(t *testing.T) {
	if n := utf8x.LossyLen([]byte{0xff, 0xff}); n != 6 {
		t.Fatalf("premise: two one-byte invalid subparts measure %d, want 6", n)
	}
	if n := utf8x.LossyLen([]byte{0xf0, 0x90, 0x80}); n != 3 {
		t.Fatalf("premise: a truncated four-byte sequence measures %d, want 3 — no expansion", n)
	}

	// The case that made a whole line unreachable. Stored, the first two lines and
	// their separator are 4 bytes and fit a bound of 7; as text they are 8, and the
	// trim landed on the '\n', so the page held one line while its own '\n' count
	// said two — and the caller, following that count, never asked for "b" again.
	s := sbOfLines(t, "\xff\xff", "b", "ccc")
	text, _, _, truncated := s.RangeBytes(0, 3, 7)
	if want := "��"; text != want || !truncated {
		t.Errorf("RangeBytes(0, 3, 7) = %q, %v; want %q and truncated: a line only fits if it "+
			"fits as the caller will receive it", text, truncated, want)
	}
	// Paging this fixture to exhaustion, at every bound rather than at the 7 the
	// defect was found at: the walk has to finish and see every line exactly once at
	// any bound, and it reproduces the history byte for byte from the reconstruction
	// edge upwards. Sweeping it is what puts the edge itself inside the test — a
	// single bound above the edge cannot tell "exact from 6" from "exact from 1", and
	// a measurement taken on stored bytes moves the edge without breaking the walk.
	want := lossyLines([]string{"\xff\xff", "b", "ccc"})
	exactFrom := firstExactBound([]string{"\xff\xff", "b", "ccc"})
	if exactFrom != 6 {
		t.Fatalf("premise: this fixture's reconstruction edge is %d, not the 6 bytes \"\\xff\\xff\" "+
			"renders to, so the sweep below straddles the wrong bound", exactFrom)
	}
	for maxBytes := 1; maxBytes <= exactFrom+2; maxBytes++ {
		got := pageForward(t, s, maxBytes)
		if len(got) != s.Len() {
			t.Fatalf("max_bytes %d: paging saw %d lines over %d in the store",
				maxBytes, len(got), s.Len())
		}
		if exact := slices.Equal(got, want); exact != (maxBytes >= exactFrom) {
			t.Errorf("max_bytes %d reproduces the history: %v; want %v, since this fixture's "+
				"longest rendered line of two or more runes is %d bytes\n got %+q\nwant %+q",
				maxBytes, exact, maxBytes >= exactFrom, exactFrom, got, want)
		}
	}
}

// The property behind the case above, swept: for k the number of lines a read says
// it returned, the text is exactly those k whole lines, in both directions and at
// every bound. The one exception the doc comments admit is the line at the near end
// alone being over the bound, which is returned partial by design.
//
// k is the count the read reports, not a count recovered from the text, because the
// reported count is what a pager acts on: a text that is the right lines under a
// count nobody sends is no use to a caller.
//
// The excuse has to be the code's own test, on the length the code measures. An
// excuse on the stored length would be stricter than the code and invent failures;
// one on a length that is never smaller would be more generous and hide them —
// which is how the first version of this sweep, written against the stored length
// while the code measured Lossy bytes, under-counted by six pairs.
//
// And the excused case is asserted rather than skipped, because an excuse nothing
// checks is a hole with a comment over it: a text that is not the lines it claims
// has to be one line, non-empty, truncated, and a prefix or a suffix of that line's
// rendered image depending on which end the read is anchored to. Both classes are
// counted per read and over fixtures, since the two reads scan from opposite ends
// and a class present in one says nothing about the other.
func TestScrollbackByteBoundedPagesAreWholeLines(t *testing.T) {
	const forward, backward = 0, 1
	readName := [2]string{"RangeBytes", "LastNBytes"}
	// Indexed by read. wholeSeen is the rule, fragSeen the exception; a sweep that
	// met only one of them would be asserting nothing about the other.
	var wholeSeen, fragSeen, concessionSeen [2]int
	fragFrom := [2]map[string]bool{{}, {}}

	check := func(t *testing.T, fixture string, lines []string) {
		s := sbOfLines(t, lines...)
		whole := s.Range(0, s.Len())
		// The rendered image of a stored line: what a read gives back for it, and so
		// what a fragment of it must be a prefix or a suffix of.
		rendered := func(i int) string { return string(utf8x.Lossy(s.line(i))) }
		// at is the oldest stored line the text claims to cover, and edge the one line
		// the bound can end inside — the offset going forwards, the newest line going
		// backwards. k is the count the read reported: the oracle takes that and the
		// fixed end from the caller's request, so a wrong count is a text that is not
		// the range it names rather than a range that moves to match it. asked and full
		// are the request itself — the lines it covers once clamped to the history, and
		// what an unbounded read of them returns.
		type call struct {
			read         int
			tag          string
			text         string
			k, lineBytes int
			trunc        bool
			at, edge     int
			maxBytes     int
			asked        int
			full         string
		}
		verify := func(t *testing.T, c call) {
			if c.k < 1 {
				t.Fatalf("%s reports %d lines: every read here asks for at least one line "+
					"of a non-empty history, so a zero count is not a possible answer", c.tag, c.k)
			}
			// lineBytes names the line the bound can end inside, at every bound and on
			// both reads, whether or not this one ended inside it. Asserted here rather
			// than in its own sweep because this is the loop that already knows which
			// line that is.
			if c.lineBytes != len(rendered(c.edge)) {
				t.Fatalf("%s reports lineBytes %d; the line its bound can end inside is "+
					"line %d, which renders to %d bytes", c.tag, c.lineBytes, c.edge, len(rendered(c.edge)))
			}
			// truncated says one thing: you did not get the whole of what you asked for.
			// One direction holds without exception.
			if !c.trunc && c.text != c.full {
				t.Fatalf("%s reports nothing left out and is not the %d lines asked for\n got %+q\nwant %+q",
					c.tag, c.asked, clip(c.text), clip(c.full))
			}
			// And so does the other, now that truncated reports what was dropped rather
			// than which arm ran. It used to have one exception (L7): a request for one
			// line whose rendering is over the bound comes back whole when that line is a
			// single rune, and the flag was true there because the over-the-bound arm
			// returned true unconditionally — sending a caller to raise a bound that was
			// already enough. Nothing is exempt here any more.
			if c.trunc && c.text == c.full {
				t.Fatalf("%s returned the whole of the %d lines asked for, %+q, and reports "+
					"truncated: nothing was left out, and line %d renders to %d bytes against a "+
					"bound of %d", c.tag, c.asked, clip(c.text), c.edge, len(rendered(c.edge)), c.maxBytes)
			}
			// The class that exception lived in still has to occur, or the sweep says
			// nothing about the flag there: the whole of what was asked for, returned
			// while the bound was exceeded. It is the KEEP-ONE-RUNE concession and only
			// that, so the shape is asserted as a premise before it is counted.
			if c.text == c.full && len(rendered(c.edge)) > c.maxBytes {
				if c.asked != 1 || utf8.RuneCountInString(c.text) != 1 {
					t.Fatalf("%s returned the whole of the %d lines asked for, %+q, while line %d "+
						"renders to %d bytes against a bound of %d: only a single-rune line asked "+
						"for on its own comes back over the bound",
						c.tag, c.asked, clip(c.text), c.edge, len(rendered(c.edge)), c.maxBytes)
				}
				concessionSeen[c.read]++
			}
			if c.text == s.Range(c.at, c.k) {
				wholeSeen[c.read]++
				return
			}
			// Not the lines it claims. The one case the doc comments admit is a single
			// line that does not fit the bound on its own, tested as the code tests it —
			// on the rendered length.
			if len(rendered(c.edge)) <= c.maxBytes {
				t.Fatalf("%s reports %d lines and is not those lines, while line %d renders "+
					"to %d bytes and fits the bound on its own\n got %+q\nwant %+q",
					c.tag, c.k, c.edge, len(rendered(c.edge)), clip(c.text), clip(s.Range(c.at, c.k)))
			}
			fragSeen[c.read]++
			fragFrom[c.read][fixture] = true
			if c.k != 1 || c.text == "" || !c.trunc {
				t.Fatalf("%s returned %+q as %d lines, truncated %v; a text that is not the "+
					"lines it claims is one line, is not empty, and says it left something out",
					c.tag, clip(c.text), c.k, c.trunc)
			}
			if c.read == forward && !strings.HasPrefix(rendered(c.edge), c.text) {
				t.Fatalf("%s returned %+q, which is not the start of line %d, %+q: a forward "+
					"read keeps the beginning of the line it stops inside",
					c.tag, clip(c.text), c.edge, clip(rendered(c.edge)))
			}
			if c.read == backward && !strings.HasSuffix(rendered(c.edge), c.text) {
				t.Fatalf("%s returned %+q, which is not the end of line %d, %+q: a tail keeps "+
					"the newest bytes of the line it stops inside",
					c.tag, clip(c.text), c.edge, clip(rendered(c.edge)))
			}
			// The fragment bit a caller derives has to be true exactly here, which is
			// what makes lineBytes usable without a second flag beside it.
			if c.lineBytes <= len(c.text) {
				t.Fatalf("%s returned %d bytes of line %d and reports its length as %d, so "+
					"lineBytes > len(text) does not tell a caller this is part of a line",
					c.tag, len(c.text), c.edge, c.lineBytes)
			}
		}
		for maxBytes := 1; maxBytes <= len(whole)+2; maxBytes++ {
			// count is swept as well as offset: a page asked for fewer lines than the
			// bound would allow is truncated for a different reason, and a count reported
			// as min(count, keep) rather than as keep would show up only where the two
			// differ.
			for offset := 0; offset < s.Len(); offset++ {
				for count := 1; count <= s.Len()-offset; count++ {
					text, k, lineBytes, trunc := s.RangeBytes(offset, count, maxBytes)
					verify(t, call{
						read: forward, text: text, k: k, lineBytes: lineBytes, trunc: trunc,
						at: offset, edge: offset, maxBytes: maxBytes,
						asked: count, full: s.Range(offset, count),
						tag: fmt.Sprintf("%s: RangeBytes(%d, %d, %d)", fixture, offset, count, maxBytes),
					})
				}
			}
			// Past the end of the history as well, where n saturates: the text is then
			// the whole history under the bound and still has to be the lines it claims,
			// and nothing is left out for having asked for more lines than exist.
			for n := 1; n <= s.Len()+2; n++ {
				text, k, lineBytes, trunc := s.LastNBytes(n, maxBytes)
				asked := min(n, s.Len())
				verify(t, call{
					read: backward, text: text, k: k, lineBytes: lineBytes, trunc: trunc,
					at: s.Len() - k, edge: s.Len() - 1, maxBytes: maxBytes,
					asked: asked, full: s.Range(s.Len()-asked, asked),
					tag: fmt.Sprintf("%s: LastNBytes(%d, %d)", fixture, n, maxBytes),
				})
			}
		}
	}

	// The fixture the two counterexamples behind this property were built on: its
	// stored bytes expand, so a measurement of the store and a measurement of the
	// text disagree here, which is what the trim reconciling them was reaching into
	// whole lines to do.
	expanding := []string{"plain", "\xff\xffmid", "another line", "\xff", "end"}
	if s := sbOfLines(t, expanding...); len(s.Range(0, s.Len())) == len(strings.Join(expanding, "\n")) {
		t.Fatal("premise: the fixture's bytes do not expand, so nothing here separates a " +
			"stored measurement from a measurement of the text")
	}
	t.Run("expanding lines", func(t *testing.T) { check(t, "expanding lines", expanding) })
	for _, f := range cutFixtures {
		t.Run(f.name, func(t *testing.T) { check(t, f.name, f.lines) })
	}

	for read, name := range readName {
		if wholeSeen[read] == 0 {
			t.Fatalf("%s never returned the lines it claims: this sweep is measuring only the "+
				"exception, and the rule it is named for is untested", name)
		}
		if fragSeen[read] == 0 {
			t.Fatalf("%s never returned a text that is not the lines it claims, so the excuse "+
				"below it is unexercised and a wrong fragment would pass here", name)
		}
		// The concession is the class where truncated used to be wrong, so it has to
		// occur on both reads or "truncated means something was dropped" is untested
		// exactly where it was untrue.
		if concessionSeen[read] == 0 {
			t.Fatalf("%s never returned the whole of what was asked for while the bound was "+
				"exceeded, so the class truncated used to over-report is unexercised on this read",
				name)
		}
		if len(fragFrom[read]) < 2 {
			t.Fatalf("%s returned a partial line only in %v, so what this sweep says about "+
				"fragments is a property of that history rather than of the bound",
				name, fragFrom[read])
		}
	}
	t.Logf("whole-line pages: forward=%d backward=%d; partial: forward=%d from %d fixtures, "+
		"backward=%d from %d fixtures; whole line over the bound: forward=%d backward=%d",
		wholeSeen[forward], wholeSeen[backward],
		fragSeen[forward], len(fragFrom[forward]), fragSeen[backward], len(fragFrom[backward]),
		concessionSeen[forward], concessionSeen[backward])
}

// cut says one thing: the text begins at a position the program never began a line
// at, so a '^' in the caller's pattern could match a column the bound invented. It
// was computed from whether the trim dropped anything, which was also true when the
// drop landed exactly on a '\n' — and the idle scan, reading cut, then refused to
// treat a real prompt at a real line start as one.
func TestScrollbackTailCutOnlyWhenTheTextBeginsInsideALine(t *testing.T) {
	newest := strings.Repeat("\xff", 8) + "aaaaaa" // 14 bytes stored, 30 as text
	s := sbOfLines(t, "0123456789", newest)
	if got := utf8x.LossyLen(s.line(1)); got != 30 {
		t.Fatalf("premise: the newest line measures %d bytes as text, want 30", got)
	}

	// A bound of exactly the newest line: the older line does not fit, so the text
	// begins where the program began a line and nothing was cut into.
	text, cut := s.Tail(2, 30)
	if want := s.Range(1, 1); text != want {
		t.Fatalf("premise: Tail(2, 30) = %q, want exactly the newest line", clip(text))
	}
	if cut {
		t.Error("Tail reported cut for text beginning exactly where the program began a line")
	}
	// And a bound that does fall inside the newest line still reports it.
	if _, cut := s.Tail(2, 20); !cut {
		t.Error("Tail did not report a cut for a bound falling inside the newest line")
	}

	// The premise lineBytes depends on, asserted rather than trusted. A tail's
	// lineBytes names the newest line because that is the only line the bound can cut
	// into: the keep >= 1 path's trim cannot fire, since fitLines measured those lines
	// through the same Lossy rendering, so cut originates only where the newest line
	// alone did not fit. If a measurement drift ever made that trim fire, the text
	// would be a suffix of the *oldest* line in a multi-line page while lineBytes
	// still named the newest — and because such a text is at least as long as the
	// newest line, lineBytes > len(text), the fragment bit callers derive, would come
	// out false for a genuine fragment. The field and the bit would both look healthy,
	// which is why the state behind them is what gets asserted.
	newestText := string(utf8x.Lossy(s.line(1)))
	whole := len(s.Range(0, s.Len()))
	// Counted per band because the concession in maxBytes of 1..3 is the only place a
	// cut text can be a whole line, so a cut seen only above the band would leave the
	// claim untested exactly where the two differ. The complement is not counted per
	// band: cut is false only when a whole line fits, and this fixture's newest line
	// is 30 bytes, so no bound below 4 can produce one — a property of the fixture,
	// stated instead of asserted.
	var cutSeen, uncutSeen [2]int
	for n := 1; n <= s.Len(); n++ {
		for maxBytes := 1; maxBytes <= whole+2; maxBytes++ {
			band := 0
			if maxBytes >= utf8.UTFMax {
				band = 1
			}
			text, lines, lineBytes, _, cut := s.tailLocked(n, maxBytes)
			if !cut {
				uncutSeen[band]++
				continue
			}
			cutSeen[band]++
			if lines != 1 || lineBytes != len(newestText) || !strings.HasSuffix(newestText, text) {
				t.Fatalf("tailLocked(%d, %d) reports cut with %d lines and lineBytes %d for %q; a cut "+
					"tail is a suffix of the newest line alone, so it covers 1 line and reports that "+
					"line's %d bytes", n, maxBytes, lines, lineBytes, clip(text), len(newestText))
			}
		}
	}
	for band, name := range map[int]string{0: "maxBytes 1..3", 1: "maxBytes >= utf8.UTFMax"} {
		if cutSeen[band] == 0 {
			t.Fatalf("no cut tail in %s, so the claim is vacuous in the band where a cut text can be "+
				"a whole line and where it cannot, respectively", name)
		}
	}
	if uncutSeen[1] == 0 {
		t.Fatalf("every tail at maxBytes >= utf8.UTFMax was cut, so cut is not being distinguished " +
			"from a bound that merely dropped whole lines")
	}
}

// cutFixtures are the histories the cut sweep runs over. Each one puts something
// different at the newest end, since that is the only end a tail's bound reaches:
// a plain line, an expansion, a line that is one rune and so comes back whole at
// any bound, a blank line, and a line far longer than the rest.
var cutFixtures = []struct {
	name  string
	lines []string
}{
	{"plain lines", []string{"alpha", "beta", "gamma"}},
	{"an expansion at the newest end", []string{"first", "\xff\xffnewest"}},
	// A newest line of exactly one rune is the only shape that can make cut
	// over-report, since that is the only way the concession returns a whole line, so
	// there are three of them: a three-byte rune, a four-byte one, and an expansion
	// that is one rune of three bytes from a single stored byte. Over-reporting is
	// then shown to be about the rune's length rather than about any one of these.
	{"the newest line is one rune", []string{"older", "→"}},
	{"the newest line is one four-byte rune", []string{"older", "𝒳"}},
	{"the newest line is one invalid byte", []string{"before", "\xff"}},
	{"a blank newest line", []string{"before", "after", ""}},
	{"one line much longer than the rest", []string{"x", "y", strings.Repeat("ab", 40)}},
	{"invalid bytes beside whole runes", []string{"a\xffb", "é\xff", "ok"}},
	// §3a's shape: a stored measurement admits both lines while the rendered join is
	// over the bound, and the trim then lands exactly on the '\n'. That is the one
	// arrangement where cut is true for a text beginning at a line start above the
	// band, so without it a stored measurement passes this sweep — measured, not
	// assumed: it does.
	{"a trim that can land on a line boundary", []string{"0123456789", strings.Repeat("\xff", 8) + "aaaaaa"}},
}

// cut is sound and complete for "the text begins inside a line" at every n and every
// maxBytes >= utf8.UTFMax, and inside the 1..3 band it can over-report but never
// under-report. Both halves are asserted, because the band is a documented weakening
// of one direction and not of the other, and a test that swept the whole range under
// the weaker claim would stop noticing if the exact half broke above the band.
//
// The oracle is computed from the text and the rendered history alone: every text a
// bounded tail returns is a suffix of the rendered history, so "begins inside a line"
// is "the byte before where this suffix starts is not a '\n'". It never consults keep,
// fitLines or fitTail — an oracle that asks which branch ran can only confirm that the
// branch sets the flag it sets, which is true of a wrong flag too.
func TestScrollbackTailCutIsExactAboveTheBandAndOnlyOverReportsInside(t *testing.T) {
	// Indexed by band: 0 is maxBytes 1..3, 1 is maxBytes >= utf8.UTFMax. Counted per
	// band because the claim differs per band, and a class that is dense in one band
	// must not satisfy a guard on the other band's behalf.
	var cutSeen, uncutSeen, insideSeen, atStartSeen, overReported [2]int
	// Keyed by fixture, so a class arising entirely within one history is caught as a
	// property of that fixture rather than counted as breadth.
	cutFrom, overFrom := map[string]bool{}, map[string]bool{}
	for _, f := range cutFixtures {
		t.Run(f.name, func(t *testing.T) {
			s := sbOfLines(t, f.lines...)
			whole := s.Range(0, s.Len())
			for n := 1; n <= s.Len()+2; n++ {
				for maxBytes := 1; maxBytes <= len(whole)+2; maxBytes++ {
					band := 0
					if maxBytes >= utf8.UTFMax {
						band = 1
					}
					text, cut := s.Tail(n, maxBytes)
					if !strings.HasSuffix(whole, text) {
						t.Fatalf("premise: Tail(%d, %d) = %+q is not a suffix of the rendered history "+
							"%+q, so the oracle below cannot locate where it begins",
							n, maxBytes, clip(text), clip(whole))
					}
					at := len(whole) - len(text)
					beginsInside := at > 0 && whole[at-1] != '\n'
					if cut {
						cutSeen[band]++
						cutFrom[f.name] = true
					} else {
						uncutSeen[band]++
					}
					if beginsInside {
						insideSeen[band]++
					} else {
						atStartSeen[band]++
					}
					// The direction that holds everywhere: a text beginning inside a
					// line is always reported. Under-reporting is what fed a '^' in
					// the idle scan's pattern a column the bound invented.
					if beginsInside && !cut {
						t.Fatalf("Tail(%d, %d) = %+q begins inside a line at offset %d of %+q and "+
							"reports cut false", n, maxBytes, clip(text), at, clip(whole))
					}
					if band == 1 && cut && !beginsInside {
						t.Fatalf("Tail(%d, %d) = %+q begins where the program began a line and reports "+
							"cut true; above the band cut is exact, not conservative",
							n, maxBytes, clip(text))
					}
					if band == 0 && cut && !beginsInside {
						overReported[0]++
						overFrom[f.name] = true
					}
				}
			}
		})
	}
	for band, name := range map[int]string{0: "maxBytes 1..3", 1: "maxBytes >= utf8.UTFMax"} {
		for class, count := range map[string]int{
			"cut": cutSeen[band], "uncut": uncutSeen[band],
			"beginning inside a line": insideSeen[band], "beginning at a line start": atStartSeen[band],
		} {
			if count == 0 {
				t.Fatalf("no case %s in %s: the claim for that band is vacuous, and this sweep would "+
					"pass with the class it is about absent", class, name)
			}
		}
	}
	if len(cutFrom) < 2 {
		t.Fatalf("every cut in this sweep came from %v, so the result is a property of one history "+
			"rather than of the bound", cutFrom)
	}
	// The band's exception asserted to exist, not just admitted. If no bound in 1..3
	// ever over-reports, the weaker claim there is the exact one and the doc comments
	// promising less than the code delivers are what should change.
	if overReported[0] == 0 {
		t.Fatalf("no tail in maxBytes 1..3 reported cut for a text beginning at a line start, so the " +
			"exception the band is documented for does not occur and cut is exact everywhere")
	}
	if len(overFrom) < 2 {
		t.Fatalf("the band's over-reporting occurs only in %v, so it is a property of that history "+
			"rather than of the one-rune concession", overFrom)
	}
	t.Logf("above the band: cut=%d uncut=%d beginsInside=%d beginsAtStart=%d, cuts from %d fixtures",
		cutSeen[1], uncutSeen[1], insideSeen[1], atStartSeen[1], len(cutFrom))
	t.Logf("inside the band: cut=%d uncut=%d over-reported=%d from %d fixtures",
		cutSeen[0], uncutSeen[0], overReported[0], len(overFrom))
}

// n saturates on the tail: once maxBytes is the binding constraint, raising n changes
// nothing, and the lines outside the bound are not reachable at that maxBytes by any n.
// That is a documented property with a sharp edge — it means a caller cannot page
// backwards by growing n, and a caller that tries reads the same end forever.
//
// Asserted as reachability rather than as an equality between two calls: for a bound
// that binds, the set of stored lines that appear in *any* response over every n is a
// strict subset of the history, and the lines outside it appear in none. Presence is
// tested against the text — each fixture line is a distinct string — rather than
// against the reported line count, so the claim does not rest on the count being right.
func TestScrollbackTailSaturatesAndLeavesOlderLinesUnreachable(t *testing.T) {
	lines := []string{"oldest-aaa", "second-bbb", "third-cccc", "fourth-dddd", "newest-eeee"}
	s := sbOfLines(t, lines...)
	whole := s.Range(0, s.Len())
	// A bound that holds the newest two lines and their join, and cannot hold the
	// third: the saturation point is then inside the history rather than at its edge.
	maxBytes := len(lines[4]) + 1 + len(lines[3])
	if maxBytes+1+len(lines[2]) <= maxBytes {
		t.Fatal("premise: the fixture's lines do not bound the sweep")
	}
	seen := map[string]bool{}
	var texts []string
	for n := 1; n <= s.Len()+5; n++ {
		text, _, _, _ := s.LastNBytes(n, maxBytes)
		checkBounded(t, fmt.Sprintf("LastNBytes(%d, %d)", n, maxBytes), text, maxBytes)
		texts = append(texts, text)
		for _, l := range lines {
			if strings.Contains(text, l) {
				seen[l] = true
			}
		}
	}
	var missing []string
	for _, l := range lines {
		if !seen[l] {
			missing = append(missing, l)
		}
	}
	// The guard that makes this a test rather than a tautology: if the bound stopped
	// binding — a larger default, a floor on maxBytes, a fitLines that measures
	// something smaller — every line would be reachable and the property being
	// asserted would no longer be observable here.
	if len(missing) == 0 {
		t.Fatalf("every one of the %d stored lines is reachable at maxBytes %d: either the bound stopped "+
			"binding, or n became a movable anchor and this read can now page backwards — which would be "+
			"a change to the contract, not a fixture that needs adjusting", len(lines), maxBytes)
	}
	if len(missing) == len(lines) {
		t.Fatalf("no stored line is reachable at maxBytes %d, so the bound is not binding but starving",
			maxBytes)
	}
	// Saturation itself: past the point where the bound binds, every larger n gives
	// the same text. Compared against the last response rather than a computed one,
	// so a change in which lines fit is a failure here and not a silently new baseline.
	last := texts[len(texts)-1]
	for n, text := range texts {
		if text == last {
			continue
		}
		if n+1 >= s.Len() {
			t.Errorf("LastNBytes(%d, %d) differs from the saturated response: %+q against %+q",
				n+1, maxBytes, clip(text), clip(last))
		}
	}
	// And the unreachable lines are unreachable at this bound only — they are in the
	// store, which is what makes "not reachable by any n" a statement about the bound.
	for _, l := range missing {
		if !strings.Contains(whole, l) {
			t.Fatalf("premise: %q is not in the history at all, so its absence says nothing about n", l)
		}
	}
	if text, _, _, _ := s.LastNBytes(s.Len(), len(whole)); text != whole {
		t.Errorf("an unbounded tail returned %+q, not the whole history: the lines missing at maxBytes "+
			"%d are then missing for a reason other than the bound", clip(text), maxBytes)
	}
	t.Logf("maxBytes %d over %d lines: %d reachable, unreachable at every n: %v",
		maxBytes, len(lines), len(seen), missing)
}

// A bounded read has to make progress: whenever a line remains it returns at least
// one whole rune, so a caller that advances by what it received always advances.
//
// It did not. When maxBytes was below the rune at the near end — the first rune of
// the line at offset for RangeBytes, the last rune of the newest line for
// LastNBytes — no line fit, and the trim then walked to a rune boundary that was
// the whole way back and returned "". A caller then had nothing to advance by, or,
// following the rule the doc comments gave at the time, advanced by the one line an
// empty text's '\n' count claims and landed on a page empty for the same reason:
// not a hung daemon, an unbounded client loop. It needs no invalid UTF-8 and no
// crafted output, only max_bytes <= 3, which the HTTP layer accepts because it
// clamps the top of the range and not the bottom.
//
// The price is that the bound may be exceeded, by at most utf8.UTFMax-1 bytes and
// only here: max_bytes is a resource guard and nothing downstream sizes a buffer to
// it exactly, and a guard that can return nothing is a livelock rather than a
// guard. Both halves are pinned, because "returns something" is satisfied by
// returning the first four bytes of whatever is there.
func TestScrollbackByteBoundedReadsAlwaysMakeProgress(t *testing.T) {
	for _, tc := range []struct {
		line     string
		maxBytes int
	}{
		{"é", 1}, {"→", 1}, {"→", 2}, {"𝄞", 1}, {"𝄞", 3}, {"\xff", 1}, {"\xff", 2},
	} {
		// The two reads scan from opposite ends, so they need their own fixtures.
		// One fixture with the boundary rune oldest leaves LastNBytes trimming
		// "ccc" and coming back non-empty at every bound — a green that tests
		// nothing, which is how this defect stayed hidden on the tail side once
		// already.
		forward := sbOfLines(t, tc.line, "b", "ccc")
		backward := sbOfLines(t, "aaa", "b", tc.line)

		// The premise, asserted rather than described: the rune the read will reach
		// first is longer than the bound, and it is at the end the read starts from.
		if r, size := utf8.DecodeRuneInString(forward.Range(0, 1)); size <= tc.maxBytes {
			t.Fatalf("premise: the first rune %q of the oldest line is %d bytes, not over "+
				"the bound of %d", r, size, tc.maxBytes)
		}
		if r, size := utf8.DecodeLastRuneInString(backward.Range(backward.Len()-1, 1)); size <= tc.maxBytes {
			t.Fatalf("premise: the last rune %q of the newest line is %d bytes, not over "+
				"the bound of %d", r, size, tc.maxBytes)
		}

		rangeText, rangeLines, _, rangeTrunc := forward.RangeBytes(0, 3, tc.maxBytes)
		tailText, tailLines, _, tailTrunc := backward.LastNBytes(3, tc.maxBytes)
		for _, c := range []struct {
			what    string
			text    string
			lines   int
			trunc   bool
			atStart bool // the text must be the start of its line, not its end
			line    string
		}{
			{"RangeBytes", rangeText, rangeLines, rangeTrunc, true, forward.Range(0, 1)},
			{"LastNBytes", tailText, tailLines, tailTrunc, false, backward.Range(backward.Len()-1, 1)},
		} {
			tag := fmt.Sprintf("%s at max_bytes %d over %+q", c.what, tc.maxBytes, c.line)
			if c.text == "" {
				t.Errorf("%s returned nothing with three lines in the store (truncated %v): "+
					"a caller advancing by the lines it received cannot advance", tag, c.trunc)
				continue
			}
			// And it says so: the partial line still counts as one, so the offset
			// moves on rather than asking for the same line at the same bound.
			if c.lines != 1 {
				t.Errorf("%s returned part of one line but reports %d lines", tag, c.lines)
			}
			if !c.trunc {
				t.Errorf("%s returned %+q without reporting truncation", tag, c.text)
			}
			if n := utf8.RuneCountInString(c.text); n != 1 || !utf8.ValidString(c.text) {
				t.Errorf("%s returned %+q: %d runes, valid %v; want exactly one whole rune",
					tag, c.text, n, utf8.ValidString(c.text))
			}
			if len(c.text) > max(tc.maxBytes, utf8.UTFMax) {
				t.Errorf("%s returned %d bytes, over max(bound, %d): the overshoot is bounded "+
					"by one rune and nothing more", tag, len(c.text), utf8.UTFMax)
			}
			if c.atStart && !strings.HasPrefix(c.line, c.text) {
				t.Errorf("%s returned %+q, not the start of its line %+q", tag, c.text, c.line)
			}
			if !c.atStart && !strings.HasSuffix(c.line, c.text) {
				t.Errorf("%s returned %+q, not the end of its line %+q", tag, c.text, c.line)
			}
		}
	}

	// Progress is about lines remaining, so the empty reads that mean "there is
	// nothing there" must stay empty, report no lines, and keep saying nothing was
	// left out. Reporting one line here is the other half of the livelock: a pager
	// that has run off the end would be told to keep going.
	s := sbOfLines(t, "é", "b", "ccc")
	if raw, text := len(s.line(0)), len(s.Range(0, 1)); raw != text {
		t.Fatalf("premise: the fixture is not valid UTF-8 (%d stored, %d as text), so this "+
			"would be about the Lossy expansion rather than about the bound", raw, text)
	}
	for _, c := range []struct{ offset, count int }{{3, 1}, {9, 1}, {-1, 1}, {0, 0}} {
		if text, lines, _, trunc := s.RangeBytes(c.offset, c.count, 8192); text != "" || lines != 0 || trunc {
			t.Errorf("RangeBytes(%d, %d, 8192) = %q, %d lines, truncated %v; a read past the end "+
				"is empty, stands for no lines and is not truncated", c.offset, c.count, text, lines, trunc)
		}
	}
}

// "you asked for no lines" and "the bound left you nothing" are different answers,
// and truncated is what tells them apart: false for the first, true for the second.
// Which one a caller gets for a zero-line read depends on the order of two guards
// inside RangeBytes, and that order is load-bearing rather than incidental, because
// a zero bound does reach it from outside: readByteCap has only an upper clamp, so
// ScrollRange(offset, 0) — and /scroll?lines=0 through it — passes maxBytes 0 down.
//
// Asserted from the outside, where a client sees it, so that adding a floor to
// readByteCap or swapping the guards changes a test rather than only a reply.
//
// The same table pins lineBytes for every read that returns no text, because that
// is the field's whole hazard: a caller reads "this page is a fragment" off
// lineBytes > len(text), and an empty text makes that true for any non-zero
// lineBytes. Each row is zero for a different reason today — two of them because a
// range check rejected the request before a line was looked at, one because the
// bound did, one because the line really is empty — and rows that agree by
// accident are the ones a tidy-up separates, so all of them are asserted rather
// than only the interesting one.
func TestScrollbackZeroCountIsAnsweredBeforeTheBound(t *testing.T) {
	// The last line is multi-byte so that a lineBytes of zero is a statement about
	// the read rather than the only value this fixture can produce.
	s := sbOfLines(t, "a", "", "𝒳bc")
	if got := readByteCap(0); got != 0 {
		t.Fatalf("premise: readByteCap(0) is %d, so a zero bound no longer reaches RangeBytes "+
			"and this test is about a path that cannot be taken", got)
	}
	if text, lines, lineBytes, _ := s.RangeBytes(2, 1, 8192); lines != 1 || lineBytes != len(text) || lineBytes != 6 {
		t.Fatalf("premise: RangeBytes(2, 1, 8192) = %q, %d lines, lineBytes %d; a whole read of the "+
			"multi-byte line should report its 6 rendered bytes, so the zeros below mean something", text, lines, lineBytes)
	}
	for _, c := range []struct {
		offset, count, maxBytes int
		lines                   int
		truncated               bool
		why                     string
	}{
		// A request for no lines is answered as such, before the bound is consulted.
		{offset: 0, count: 0, maxBytes: 0, why: "no lines asked for, and that answers before a zero bound"},
		{offset: 0, count: 0, maxBytes: 8192, why: "no lines asked for, with a bound that would have fit"},
		// The other side of the same coin, which is what makes the ordering observable
		// at all: with a line actually asked for, the zero bound is what answers.
		{offset: 0, count: 1, maxBytes: 0, truncated: true, why: "a zero bound with a line requested left something behind"},
		{offset: 0, count: 1, maxBytes: -1, truncated: true, why: "a negative bound is the zero bound, not an absent one"},
		// Off the end in either direction: no line was reached, so there is no
		// rendered length to report and no fragment to infer.
		{offset: 3, count: 1, maxBytes: 8192, why: "an offset past the last line reaches no line"},
		{offset: -1, count: 1, maxBytes: 8192, why: "a negative offset reaches no line"},
		// The one row where the emptiness is the history's rather than the request's:
		// a line is returned, it is empty, and lineBytes says the text is all of it.
		{offset: 1, count: 1, maxBytes: 8192, lines: 1, why: "the stored line is empty, so one line of no bytes is the whole of it"},
	} {
		text, lines, lineBytes, trunc := s.RangeBytes(c.offset, c.count, c.maxBytes)
		if text != "" || lines != c.lines || lineBytes != 0 || trunc != c.truncated {
			t.Errorf("RangeBytes(%d, %d, %d) = %q, %d lines, lineBytes %d, truncated %v; want %q, %d lines, "+
				"lineBytes 0, truncated %v: %s", c.offset, c.count, c.maxBytes, text, lines, lineBytes, trunc,
				"", c.lines, c.truncated, c.why)
		}
	}
}

// lossyLines is the text a read gives back for each of these stored lines. The
// bound is measured on the Lossy expansion, so that — not the stored bytes — is
// what a pager can reconstruct.
func lossyLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(utf8x.Lossy([]byte(l)))
	}
	return out
}

// firstExactBound is the smallest max_bytes at which paging forward reproduces
// these lines byte for byte, derived from the two reads' own rules rather than
// guessed at.
//
// A page starting at offset j holds whole lines when line j fits the bound, and
// fitLines measures exactly what the join costs, so nothing is trimmed after that.
// When line j does *not* fit, the page is that line's first max_bytes — except that
// KEEP-ONE-RUNE returns one whole rune regardless, so a line of a single rune comes
// back whole at any bound and can never be the line that breaks reconstruction.
// So the bound has to hold the longest rendered line *of two or more runes*, and
// only those lines count towards it.
func firstExactBound(lines []string) int {
	bound := 1
	for _, rendered := range lossyLines(lines) {
		if utf8.RuneCountInString(rendered) >= 2 {
			bound = max(bound, len(rendered))
		}
	}
	return bound
}

// pageForward walks the whole history at one bound, the way a client does: read,
// advance by the number of lines the read says it covered, stop at the end. It
// returns the lines it saw, and fails rather than spinning if a read reports
// nothing to advance by.
func pageForward(t *testing.T, s *Scrollback, maxBytes int) []string {
	t.Helper()
	var got []string
	for offset, reads := 0, 0; offset < s.Len(); reads++ {
		if reads > s.Len() {
			t.Fatalf("max_bytes %d: %d reads to cover %d lines, so a read is not advancing",
				maxBytes, reads, s.Len())
		}
		text, returned, _, _ := s.RangeBytes(offset, s.Len(), maxBytes)
		checkBounded(t, fmt.Sprintf("the page at %d with max_bytes %d", offset, maxBytes),
			text, maxBytes)
		if returned < 1 {
			t.Fatalf("max_bytes %d: the page at %d of %d reports %d lines, so the offset cannot "+
				"advance", maxBytes, offset, s.Len(), returned)
		}
		page := strings.Split(text, "\n")
		if len(page) != returned {
			t.Fatalf("max_bytes %d: the page at %d reports %d lines and holds %d",
				maxBytes, offset, returned, len(page))
		}
		got = append(got, page...)
		offset += returned
	}
	return got
}

// A pager reads to exhaustion, and the properties that matter are properties of the
// whole walk rather than of one call: it has to finish, it has to see every line
// exactly once, and there is a bound above which it reproduces history byte for
// byte. Below that bound it cannot — a line wider than the bound comes back in part,
// by design, since the alternative was returning nothing and never advancing.
//
// What each assertion here can actually see, measured by breaking the
// implementation rather than assumed. Three changes that keep a working pager are
// all caught: measuring stored bytes instead of the Lossy expansion in fitLines
// ("the page at 0 reports 2 lines and holds 1"), KEEP-ONE-RUNE keeping two runes,
// and a floor on max_bytes — the shape that was rejected during the ruling. The last
// two are caught by the bound assertion, not by the boundary equivalence below, so
// that equivalence is documentation of the contract more than an independent check:
// at any bound at or above the edge every page holds whole lines, so exactness there
// is not a separate fact. One change it does *not* catch, stated so the next reader
// does not assume otherwise: an off-by-one in fitLines' fit test (`>=` for `>`),
// which drops a line that exactly fills the bound and then returns that same line
// whole through the fragment path — identical text, only `truncated` differs.
func TestScrollbackRangeBytesPagesToExhaustionAtEveryBound(t *testing.T) {
	for _, f := range []struct {
		name  string
		lines []string
	}{
		{"plain lines", []string{"alpha", "beta", "gamma"}},
		// Every line is one rune, so KEEP-ONE-RUNE returns each of them whole at
		// any bound and reconstruction is exact from 1 — even though each renders
		// to three bytes. This is the fixture that refutes "the bound has to hold
		// the longest rendered line".
		{"single-rune lines", []string{"\xff", "\xff", "\xff"}},
		// The same edge without an invalid byte anywhere: the four-byte rune is a
		// line of its own, so the edge is set by "bb", at 2.
		{"a wide rune alone on its line", []string{"a", "𝄞", "bb"}},
		// And the two refinements at once, in the case that discriminates hardest:
		// the longest *rendered* line is the single invalid byte at three bytes,
		// but the edge is "ab" at two, so a bound of 2 reconstructs a history whose
		// longest rendered line does not fit in it.
		{"the longest rendered line is one rune", []string{"\xff", "ab"}},
		{"empty lines among text", []string{"", "hello", "", "", "x"}},
		{"invalid bytes beside whole runes", []string{"a\xffb", "\xc3", "é\xff", "ok"}},
		{"one line much longer than the rest", []string{"x", strings.Repeat("ab", 40), "y"}},
		{"combining marks and wide glyphs", []string{"é́", "漢字", "→→→"}},
	} {
		t.Run(f.name, func(t *testing.T) {
			s := sbOfLines(t, f.lines...)
			want := lossyLines(f.lines)
			exactFrom := firstExactBound(f.lines)
			// The sweep starts at 1, not 0: max_bytes <= 0 is documented as
			// returning nothing, so it is outside a pager's contract. Asking there
			// produces a verdict that depends only on the frame it is asked in —
			// a per-offset comparison calls it exact, since the empty string is
			// what a zero-line read should give, while a loop calls it a stall.
			for maxBytes := 1; maxBytes <= exactFrom+2; maxBytes++ {
				got := pageForward(t, s, maxBytes)
				if len(got) != s.Len() {
					t.Fatalf("max_bytes %d: paging saw %d lines over %d in the store",
						maxBytes, len(got), s.Len())
				}
				exact := slices.Equal(got, want)
				if exact != (maxBytes >= exactFrom) {
					which, mismatch := -1, ""
					for i := range want {
						if got[i] != want[i] {
							which, mismatch = i, fmt.Sprintf("%+q against %+q", got[i], want[i])
							break
						}
					}
					if exact {
						t.Errorf("max_bytes %d reproduces every line, below the %d this fixture's "+
							"longest multi-rune line needs: the bound where paging becomes exact "+
							"has moved", maxBytes, exactFrom)
					} else {
						t.Errorf("max_bytes %d does not reproduce line %d (%s), at or above the %d "+
							"needed by the longest multi-rune line", maxBytes, which, mismatch, exactFrom)
					}
				}
			}
		})
	}
}

// checkBounded is the post-condition every bounded read shares: whole runes, and
// within the bound except for the one rune the bound cannot hold, which is returned
// rather than leaving the caller nothing to advance by.
func checkBounded(t *testing.T, tag, text string, maxBytes int) {
	t.Helper()
	if len(text) > max(maxBytes, utf8.UTFMax) {
		t.Fatalf("%s: %d bytes, over the bound", tag, len(text))
	}
	if len(text) > maxBytes && utf8.RuneCountInString(text) != 1 {
		t.Fatalf("%s: %d bytes over a bound of %d, and %d runes: the bound gives way to one "+
			"whole rune and nothing else", tag, len(text), maxBytes, utf8.RuneCountInString(text))
	}
	if !utf8.ValidString(text) {
		t.Fatalf("%s: not valid UTF-8: %q", tag, clip(text))
	}
}
