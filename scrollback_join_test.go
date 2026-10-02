package shelld

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/utf8x"
)

// lineModel is the reference for where lines begin and end. It processes one
// byte at a time and keeps a window of recent lines, where Scrollback appends
// whole segments to a line held open across writes and stores them in shared
// chunks. The two share only the cut rule at the cap.
//
// It bounds nothing but the window: Scrollback's retained lines must be a
// suffix of what it holds, which pins the line semantics without also
// modelling eviction, and lets the streams below run long lines that the byte
// budget legitimately evicts.
type lineModel struct {
	lines   [][]byte
	count   uint64
	keep    int
	open    bool
	pendCR  bool   // a '\r' seen but not yet placed: it may be a CRLF's
	pendCut []byte // a chunk ended inside a UTF-8 sequence: its bytes wait here
}

func newLineModel(keep int) *lineModel { return &lineModel{keep: keep} }

// lastEndsLine is the offset of the last LF, VT or FF in d, or -1. The bytes
// after it are the run an incomplete sequence can be held out of.
func lastEndsLine(d []byte) int {
	for i := len(d) - 1; i >= 0; i-- {
		if endsLine(d[i]) {
			return i
		}
	}
	return -1
}

func (m *lineModel) push(line []byte) {
	m.lines = append(m.lines, line)
	m.count++
	if len(m.lines) > m.keep {
		m.lines = m.lines[1:]
	}
}

// Write models the two holds by chunk rather than by byte, because both are
// properties of where a chunk ended: bytes held from the last chunk lead this
// one, and a sequence this one leaves incomplete is held out of it.
//
// The hold is taken off the chunk before any byte is stored, not off the line
// afterwards. Storing a byte can cut the line at the cap, and a byte that is
// being held back must not be able to cause a cut it is not part of — deciding
// it after the fact counts one line too many on a write that fills a line.
func (m *lineModel) Write(d []byte) {
	if len(m.pendCut) > 0 {
		d = append(append([]byte(nil), m.pendCut...), d...)
		m.pendCut = m.pendCut[:0]
	}
	// Only the run after the last terminator can be held; a run ending in '\r'
	// cannot, because a '\r' is a complete character, so a sequence before it is
	// already damage however the '\r' itself resolves.
	heldWholeRun := false
	if tail := d[lastEndsLine(d)+1:]; len(tail) > 0 && tail[len(tail)-1] != '\r' {
		if k := partialTailLen(tail); k > 0 {
			m.pendCut = append(m.pendCut[:0], d[len(d)-k:]...)
			d, heldWholeRun = d[:len(d)-k], k == len(tail)
		}
	}
	// Scrollback appends the run even when the hold emptied it, so a line is
	// open either way; with nothing left to store, the byte loop below would
	// not open one.
	defer func() {
		if heldWholeRun && !m.open {
			m.push(nil)
			m.open = true
		}
	}()
	for _, b := range d {
		switch {
		case b == '\r':
			// A '\r' is content unless the next byte ends the line, which is
			// not known yet. It still begins a line: output has arrived.
			if m.pendCR {
				m.add('\r')
			} else if !m.open {
				m.push(nil)
				m.open = true
			}
			m.pendCR = true
		case endsLine(b):
			m.pendCR = false // a CRLF's '\r' is dropped with its terminator
			if !m.open {
				m.push(nil)
			}
			m.open = false
		default:
			if m.pendCR {
				m.pendCR = false
				m.add('\r')
			}
			m.add(b)
		}
	}
}

// add puts one byte on the open line, opening one when none is, and cuts the
// line at the cap. Only the cut rule is shared with Scrollback.
func (m *lineModel) add(b byte) {
	if !m.open {
		m.push(nil)
		m.open = true
	}
	i := len(m.lines) - 1
	m.lines[i] = append(m.lines[i], b)
	if len(m.lines[i]) <= sbMaxLine {
		return
	}
	l := m.lines[i]
	at := sbMaxLine - cutBackoff(nil, l[:sbMaxLine])
	m.lines[i] = l[:at]
	m.push(append([]byte(nil), l[at:]...))
}

// checkAgainstModel asserts that b started exactly the lines the model did and
// that what it retains is the newest of them, byte for byte.
func checkAgainstModel(t *testing.T, tag string, b *Scrollback, m *lineModel) {
	t.Helper()
	checkRefs(t, tag, b)
	if b.TotalLines() != m.count {
		t.Fatalf("%s: started %d lines, want %d", tag, b.TotalLines(), m.count)
	}
	if b.Len() > b.Capacity() {
		t.Fatalf("%s: retains %d lines over a capacity of %d", tag, b.Len(), b.Capacity())
	}
	if b.Len() > len(m.lines) {
		t.Fatalf("%s: retains %d lines, more than the model's window of %d", tag, b.Len(), len(m.lines))
	}
	want := m.lines[len(m.lines)-b.Len():]
	for i, w := range want {
		// Raw bytes, because Range replaces invalid UTF-8 and a split rune
		// would then compare equal to the replacement character.
		if got := rawLine(b, i); !bytes.Equal(got, w) {
			t.Fatalf("%s: retained line %d of %d: %d bytes, want %d\n got %q\nwant %q",
				tag, i, b.Len(), len(got), len(w), clip(string(got)), clip(string(w)))
		}
		if got, want := b.Range(i, 1), utf8x.Lossy(w); got != want {
			t.Fatalf("%s: Range of line %d is not the lossy form of its bytes\n got %q\nwant %q",
				tag, i, clip(got), clip(want))
		}
	}
}

// checkJoinedReads asserts that the multi-line reads are the retained lines
// joined by '\n', in order. The per-line comparison cannot see the order or
// the separators, and it is those that a reader splitting the result back into
// lines depends on.
//
// It copies the whole history, so callers keep it off the per-write path.
func checkJoinedReads(t *testing.T, tag string, b *Scrollback, m *lineModel) {
	t.Helper()
	n := b.Len()
	// Lossy replaces invalid sequences in place and cannot absorb the ASCII
	// separator, so joining then converting is the same as the reverse.
	want := utf8x.Lossy(bytes.Join(m.lines[len(m.lines)-n:], []byte("\n")))
	if got := b.LastN(n); got != want {
		t.Fatalf("%s: LastN(%d) is not the %d retained lines joined\n got %q\nwant %q",
			tag, n, n, clip(got), clip(want))
	}
	if got := b.Range(0, n); got != want {
		t.Fatalf("%s: Range(0, %d) differs from LastN(%d)\n got %q\nwant %q",
			tag, n, n, clip(got), clip(want))
	}
	// Reads outside the retained range are empty rather than clamped to a line.
	if got := b.LastN(0); got != "" {
		t.Fatalf("%s: LastN(0) = %q", tag, clip(got))
	}
	if got := b.Range(n, 1); got != "" {
		t.Fatalf("%s: Range past the newest line = %q", tag, clip(got))
	}
	if got := b.Range(-1, 1); got != "" {
		t.Fatalf("%s: Range at a negative offset = %q", tag, clip(got))
	}
}

// rawLine is the stored bytes of the i'th retained line, before the lossy
// conversion the public reads apply.
func rawLine(b *Scrollback, i int) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]byte(nil), b.line(i)...)
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:60] + "..." + s[len(s)-60:]
	}
	return s
}

// joinStreams returns byte streams that stress every line boundary the store
// has to get right: terminators split from the '\r' before them, lone and
// repeated CRs, empty lines, VT and FF, lines at and either side of the cap,
// escape sequences and multi-byte runes straddling the cap, and invalid UTF-8.
func joinStreams(rng *rand.Rand) [][]byte {
	esc := []byte("\x1b[38;5;120m")
	return [][]byte{
		[]byte("abc\r\ndef\r\n"),
		[]byte("\r"),
		[]byte("\r\n"),
		[]byte("\r\r\n"),
		[]byte("a\rb"),
		[]byte("a\r\r\rb\nc"),
		[]byte("no newline at all"),
		[]byte("x\vy\fz\n"),
		[]byte("x\r\vy\r\fz\r\n"),
		[]byte("\n\n\n\r\n\r\r\n"),
		append(bytes.Repeat([]byte("q"), sbMaxLine), '\n'),
		bytes.Repeat([]byte("q"), sbMaxLine-1),
		bytes.Repeat([]byte("q"), sbMaxLine+1),
		bytes.Repeat([]byte("q"), 3*sbMaxLine+7),
		// A rune and an escape sequence sitting exactly on the cap.
		append(bytes.Repeat([]byte("q"), sbMaxLine-2), "étail"...),
		append(bytes.Repeat([]byte("q"), sbMaxLine-3), "\U0001F600tail"...),
		append(bytes.Repeat([]byte("q"), sbMaxLine-len(esc)+2), append(esc, "tail"...)...),
		append(bytes.Repeat([]byte("q"), sbMaxLine-1), append(esc, "tail"...)...),
		append(bytes.Repeat([]byte("q"), sbMaxLine), '\r'),
		append(append(bytes.Repeat([]byte("q"), sbMaxLine), '\r'), 'X'),
		{0xff, 0xe2, 0x82, '\n', 0x80, 0xc1, '\r', '\n'},
		randomStream(rng),
		randomStream(rng),
	}
}

// randomStream is ordinary-looking output with the awkward bytes mixed in at a
// rate that makes long runs of them likely somewhere.
func randomStream(rng *rand.Rand) []byte {
	var d []byte
	for range 400 {
		switch rng.IntN(12) {
		case 0:
			d = append(d, '\n')
		case 1:
			d = append(d, '\r')
		case 2:
			d = append(d, "\r\n"...)
		case 3:
			d = append(d, byte("\v\f"[rng.IntN(2)]))
		case 4:
			d = append(d, "\x1b[K"...)
		case 5:
			d = append(d, " ́─\U0001F600"...)
		case 6:
			d = append(d, bytes.Repeat([]byte("L"), rng.IntN(2*sbMaxLine))...)
		case 7:
			d = append(d, byte(rng.IntN(256)))
		default:
			for range rng.IntN(200) {
				d = append(d, byte(32+rng.IntN(90)))
			}
		}
	}
	return d
}

// splitters returns the ways a stream is broken into writes. The PTY hands the
// daemon whatever the kernel had ready, so every one of these is a shape real
// output arrives in, and all of them must give the same history.
func splitters() []struct {
	name  string
	split func(*rand.Rand, []byte) [][]byte
} {
	fixed := func(n int) func(*rand.Rand, []byte) [][]byte {
		return func(_ *rand.Rand, d []byte) [][]byte {
			var out [][]byte
			for len(d) > 0 {
				k := min(n, len(d))
				out = append(out, d[:k])
				d = d[k:]
			}
			return out
		}
	}
	return []struct {
		name  string
		split func(*rand.Rand, []byte) [][]byte
	}{
		{"whole", func(_ *rand.Rand, d []byte) [][]byte { return [][]byte{d} }},
		{"1", fixed(1)},
		{"2", fixed(2)},
		{"3", fixed(3)},
		{"4096", fixed(4096)},
		{"4095", fixed(4095)},
		{"65536", fixed(65536)},
		{"random", func(rng *rand.Rand, d []byte) [][]byte {
			var out [][]byte
			for len(d) > 0 {
				k := min(1+rng.IntN(6000), len(d))
				out = append(out, d[:k])
				d = d[k:]
			}
			return out
		}},
	}
}

// The history must be a function of the bytes written, not of where the writes
// happened to end. This is the property that a PTY read boundary must not be a
// line boundary: the kernel decides where a read ends, so anything that
// depends on it is not the program's output.
func TestScrollbackSplitWritesMatchOneWrite(t *testing.T) {
	for _, capacity := range []int{1, 2, 5, 64, 1000} {
		t.Run(fmt.Sprint("cap", capacity), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(uint64(capacity), 7))
			for si, stream := range joinStreams(rng) {
				var want []string
				for _, sp := range splitters() {
					b := NewScrollback(capacity)
					for _, w := range sp.split(rand.New(rand.NewPCG(9, 9)), stream) {
						b.Write(w)
					}
					got := []string{
						fmt.Sprint(b.Len()), fmt.Sprint(b.TotalLines()), fmt.Sprint(b.TotalBytes()),
						b.LastN(capacity + 2), b.Range(0, capacity+2), b.LastN(1),
					}
					if want == nil {
						want = got
						continue
					}
					for k := range got {
						if got[k] != want[k] {
							t.Fatalf("stream %d in %s-byte writes: read %d differs\n got %q\nwant %q",
								si, sp.name, k, clip(got[k]), clip(want[k]))
						}
					}
				}
			}
		})
	}
}

// Where lines begin and end, against a model that scans the stream a byte at a
// time instead of appending segments to an open line.
func TestScrollbackMatchesLineModel(t *testing.T) {
	for _, capacity := range []int{1, 2, 5, 64, 1000} {
		t.Run(fmt.Sprint("cap", capacity), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(uint64(capacity), 11))
			for si, stream := range joinStreams(rng) {
				for _, sp := range splitters() {
					b, m := NewScrollback(capacity), newLineModel(capacity+4)
					for _, w := range sp.split(rand.New(rand.NewPCG(5, 5)), stream) {
						b.Write(w)
						m.Write(w)
					}
					checkAgainstModel(t, fmt.Sprintf("stream %d in %s-byte writes", si, sp.name), b, m)
				}
			}
		})
	}
}

// A stream of writes, each checked, so a divergence is caught at the write
// that caused it rather than at the end of the run.
func TestScrollbackMatchesLineModelIncrementally(t *testing.T) {
	for _, capacity := range []int{1, 3, 17} {
		t.Run(fmt.Sprint("cap", capacity), func(t *testing.T) {
			t.Parallel()
			for seed := range uint64(4) {
				rng := rand.New(rand.NewPCG(seed, uint64(capacity)))
				b, m := NewScrollback(capacity), newLineModel(capacity+4)
				for i := range scrollbackIters(600) {
					d := awkwardWrite(rng)
					b.Write(d)
					m.Write(d)
					tag := fmt.Sprintf("seed %d write %d", seed, i)
					checkAgainstModel(t, tag, b, m)
					checkJoinedReads(t, tag, b, m)
				}
			}
		})
	}
}

// The deterministic cases behind the property tests, so a failure names the
// rule that broke rather than a seed.
func TestScrollbackLineEnds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		lines  []string
		total  uint64
	}{
		// The CRLF that D4 turned into an empty line: the '\r' ends one write
		// and the '\n' starts the next.
		{"crlf split across writes", []string{"abc\r", "\ndef\r\n"}, []string{"abc", "def"}, 2},
		{"crlf in one write", []string{"abc\r\ndef\r\n"}, []string{"abc", "def"}, 2},
		{"lf alone across writes", []string{"abc", "\ndef\n"}, []string{"abc", "def"}, 2},
		// A '\r' that is not a CRLF's is content: it is what a progress bar
		// overwrites with, and raw history keeps it.
		{"lone cr mid stream", []string{"a\r", "b"}, []string{"a\rb"}, 1},
		{"lone cr at the end", []string{"a\r"}, []string{"a"}, 1},
		{"cr alone", []string{"\r"}, []string{""}, 1},
		{"cr then lf", []string{"\r", "\n"}, []string{""}, 1},
		{"two crs then lf", []string{"abc\r", "\r\n"}, []string{"abc\r"}, 1},
		{"progress bar frames", []string{" 10%\r", " 50%\r", "100%\n"}, []string{" 10%\r 50%\r100%"}, 1},
		// VT and FF feed a line as LF does, and strip a '\r' before them.
		{"vt ends a line", []string{"a\vb"}, []string{"a", "b"}, 2},
		{"ff ends a line", []string{"a\fb"}, []string{"a", "b"}, 2},
		{"cr vt across writes", []string{"a\r", "\vb"}, []string{"a", "b"}, 2},
		{"cr ff across writes", []string{"a\r", "\fb"}, []string{"a", "b"}, 2},
		// A line still open is readable, which is what idle detection needs.
		{"open line is visible", []string{"$ "}, []string{"$ "}, 1},
		{"open line extends", []string{"$ ", "ls"}, []string{"$ ls"}, 1},
		{"empty writes", []string{"", "a", "", "\n"}, []string{"a"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewScrollback(100)
			for _, w := range tc.writes {
				b.Write([]byte(w))
			}
			if got := b.Len(); got != len(tc.lines) {
				t.Fatalf("retains %d lines, want %d: %q", got, len(tc.lines), b.LastN(100))
			}
			if got, want := b.LastN(100), strings.Join(tc.lines, "\n"); got != want {
				t.Errorf("history %q, want %q", got, want)
			}
			if got := b.TotalLines(); got != tc.total {
				t.Errorf("started %d lines, want %d", got, tc.total)
			}
		})
	}
}

// A cut at the cap must not split a rune or a short escape sequence, because a
// reader of one line would then see bytes that are not a sequence and the next
// line would begin with the rest of one.
func TestScrollbackLineCapCutsCleanly(t *testing.T) {
	esc := "\x1b[38;5;120m"
	for _, tc := range []struct{ name, fill, at string }{
		{"two-byte rune", "q", "é"},
		{"three-byte rune", "q", "─"},
		{"four-byte rune", "q", "\U0001F600"},
		{"escape sequence", "q", esc},
		{"combining mark", "q", "é"},
	} {
		for off := -4; off <= 4; off++ {
			t.Run(fmt.Sprintf("%s/%+d", tc.name, off), func(t *testing.T) {
				head := sbMaxLine - len(tc.at) - off
				if head < 0 {
					t.Skip("offset puts the sequence before the start of the line")
				}
				stream := strings.Repeat(tc.fill, head) + tc.at + strings.Repeat("z", 40)
				b := NewScrollback(100)
				for i := 0; i < len(stream); i += 3 {
					b.Write([]byte(stream[i:min(i+3, len(stream))]))
				}
				if b.Len() < 2 {
					t.Fatalf("a %d-byte line was not cut: %d lines", len(stream), b.Len())
				}
				var rejoined []byte
				for i := range b.Len() {
					line := rawLine(b, i)
					rejoined = append(rejoined, line...)
					if len(line) > sbMaxLine {
						t.Errorf("line %d is %d bytes, over the %d cap", i, len(line), sbMaxLine)
					}
					if !utf8.Valid(line) {
						t.Errorf("line %d is not valid UTF-8: the cut split a rune", i)
					}
					if strings.Contains(tc.at, "\x1b") && i > 0 && strings.HasPrefix(string(line), esc[1:]) {
						t.Errorf("line %d begins with the tail of an escape sequence", i)
					}
				}
				// The cut moves bytes between lines but never loses or
				// duplicates one.
				if string(rejoined) != stream {
					t.Errorf("the cut lines do not rejoin to the stream: %d bytes, want %d", len(rejoined), len(stream))
				}
			})
		}
	}
}

// The chunk bytes held must stay within the budget, whatever the shape of the
// output: without it, lines of up to the cap would let a capacity-line history
// hold sixteen times what it could when a line could not outlast one read.
//
// The budget is set per case rather than left at the default, so that eviction by
// bytes is what the assertion sees. At the default a thousand lines of this shape
// never reach 8 MB and the line count would be doing the bounding, which is the
// path the other tests cover; the smallest budget the store allows is one maximum
// line plus a chunk, and it has to hold even there.
func TestScrollbackHeldBytesBound(t *testing.T) {
	for _, capacity := range []int{1, 10, 1000} {
		for _, budget := range []int64{sbMaxLine + sbMaxChunk, 1 << 20, sbDefaultBudget} {
			t.Run(fmt.Sprint("cap", capacity, "budget", budget), func(t *testing.T) {
				t.Parallel()
				for _, piece := range []int{1, 4096} {
					for seed := range uint64(3) {
						rng := rand.New(rand.NewPCG(seed, uint64(piece)))
						b := NewScrollbackWithBudget(capacity, budget)
						for range scrollbackIters(300) {
							d := budgetStream(rng)
							for len(d) > 0 {
								k := min(piece, len(d))
								b.Write(d[:k])
								d = d[k:]
								checkRefs(t, fmt.Sprintf("cap %d piece %d seed %d", capacity, piece, seed), b)
								held, spares := b.heldAndSpares()
								if int64(held) > budget+sbMaxChunk {
									t.Fatalf("cap %d piece %d seed %d: holds %d bytes, over budget %d + one chunk",
										capacity, piece, seed, held, budget)
								}
								if spares > sbSpares*sbMaxChunk {
									t.Fatalf("cap %d piece %d seed %d: %d bytes of spares", capacity, piece, seed, spares)
								}
							}
						}
					}
				}
			})
		}
	}
}

// budgetStream is output whose lines are long enough for the byte budget to
// bind before the line count does: a redraw that never writes '\n', and lines
// on either side of the cap.
func budgetStream(rng *rand.Rand) []byte {
	switch rng.IntN(4) {
	case 0:
		return bytes.Repeat([]byte("a"), 33<<10)
	case 1:
		return bytes.Repeat([]byte("b"), sbMaxLine-1)
	case 2:
		return append(bytes.Repeat([]byte("c"), sbMaxLine+1), '\n')
	default:
		return append(bytes.Repeat([]byte("d"), rng.IntN(8<<10)), '\n')
	}
}

// Output whose lines fit one PTY read and end in '\n' is the whole of what the
// store could hold before lines were joined, so the budget must not evict any
// of it: retention for that class is unchanged.
func TestScrollbackBudgetKeepsShortLineHistory(t *testing.T) {
	for _, capacity := range []int{1, 10, 500} {
		b := NewScrollback(capacity)
		want := 0
		for i := range 4 * capacity {
			line := bytes.Repeat([]byte("x"), sbMinChunk-1)
			b.Write(append(line, '\n'))
			want = min(i+1, capacity)
			if b.Len() != want {
				t.Fatalf("cap %d after %d lines of %d bytes: retains %d, want %d",
					capacity, i+1, sbMinChunk, b.Len(), want)
			}
		}
	}
}

// A PTY read can end anywhere, including part-way through a character. History
// is stored as raw bytes and decoded when it is read, so bytes that are only the
// start of a character would read as U+FFFD for a character that is merely still
// arriving; Write holds them instead. The read between the two writes is the
// whole point, so this asserts on it rather than on the settled result.
func TestScrollbackHoldsARuneThatIsStillArriving(t *testing.T) {
	for _, tc := range []struct{ name, first, rest, mid, whole string }{
		{"two-byte", "a\xc3", "\xa9", "a", "aé"},
		{"three-byte", "a\xe4", "\xb8\xad", "a", "a中"},
		{"four-byte", "a\xf0", "\x9f\x98\x80", "a", "a\U0001F600"},
		{"lead and one continuation", "a\xe4\xb8", "\xad", "a", "a中"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewScrollback(10)
			b.Write([]byte(tc.first))
			if got := b.LastN(1); got != tc.mid {
				t.Errorf("between the writes the line reads %q, want %q: a character still\n"+
					"arriving must not be rendered as damage", got, tc.mid)
			}
			if got := b.LastN(1); strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("between the writes the line reads %q, which contains U+FFFD", got)
			}
			b.Write([]byte(tc.rest))
			if got := b.LastN(1); got != tc.whole {
				t.Errorf("after the sequence completed the line reads %q, want %q", got, tc.whole)
			}
			if b.TotalBytes() != uint64(len(tc.first)+len(tc.rest)) {
				t.Errorf("TotalBytes is %d, want %d: held bytes are counted on the write\n"+
					"that brought them, once", b.TotalBytes(), len(tc.first)+len(tc.rest))
			}
		})
	}
}

// The hold has to survive a sequence spread over more writes than it has bytes.
// Held bytes are prepended to the next chunk rather than put back on the line
// for exactly this case: the middle write carries only continuation bytes, so a
// hold decided on that chunk alone would find no sequence start in it and store
// a sequence that can still complete.
func TestScrollbackHoldsARuneSplitAcrossThreeWrites(t *testing.T) {
	b := NewScrollback(10)
	for i, w := range []string{"a\xe4", "\xb8", "\xad"} {
		b.Write([]byte(w))
		if got, want := b.LastN(1), "a"; i < 2 && got != want {
			t.Fatalf("after write %d (%q) the line reads %q, want %q", i, w, got, want)
		}
	}
	if got, want := b.LastN(1), "a中"; got != want {
		t.Errorf("the completed line reads %q, want %q", got, want)
	}
}

// Bytes that can never become a character are stored at once. Holding them would
// delay a U+FFFD that is not going to change, and a hold withholds bytes from
// every read, so it has to be reserved for bytes that waiting can improve.
func TestScrollbackStoresBytesThatCanNeverComplete(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"bare continuation", "a\x80"},
		{"overlong lead c0", "a\xc0"},
		{"overlong lead c1", "a\xc1"},
		{"surrogate", "a\xed\xa0"},
		{"lead above the range", "a\xf5"},
		{"beyond U+10FFFF", "a\xf4\x90"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewScrollback(10)
			b.Write([]byte(tc.in))
			if got := b.LastN(1); !strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("%q reads %q, with no U+FFFD: damage that cannot be repaired by\n"+
					"the next write must be rendered now, not held", tc.in, got)
			}
			if k := partialTailLen([]byte(tc.in)); k != 0 {
				t.Errorf("partialTailLen(%q) = %d, want 0", tc.in, k)
			}
		})
	}
}

// partialTailLen and cutBackoff answer different questions about the same bytes,
// and \xc0 is where the answers differ: it claims a second byte, so cutBackoff
// keeps it with what follows, while it can never begin a character, so a hold
// must not wait for one. Asserted side by side so the two cannot be collapsed
// into one helper later: cutBackoff moves bytes to the next line, where being
// wrong costs a line boundary, but a hold keeps bytes from every read, where
// being wrong hides a byte the reader is entitled to see.
func TestPartialTailLenIsNotCutBackoff(t *testing.T) {
	if got := partialTailLen([]byte("a\xc0")); got != 0 {
		t.Errorf("partialTailLen(%q) = %d, want 0: \\xc0 begins no character", "a\xc0", got)
	}
	if got := cutBackoff([]byte("a"), []byte("\xc0")); got != 1 {
		t.Errorf("cutBackoff(%q, %q) = %d, want 1: \\xc0 claims a second byte", "a", "\xc0", got)
	}
	if got := runeLen(0xc0); got != 2 {
		t.Errorf("runeLen(0xc0) = %d, want 2", got)
	}
	// \xe4 is the control: both helpers hold it back, so the case above is the
	// disagreement and not simply one helper being inert.
	if got := partialTailLen([]byte("a\xe4")); got != 1 {
		t.Errorf("partialTailLen(%q) = %d, want 1", "a\xe4", got)
	}
	if got := cutBackoff([]byte("a"), []byte("\xe4")); got != 1 {
		t.Errorf("cutBackoff(%q, %q) = %d, want 1", "a", "\xe4", got)
	}
}

// The predicate's own table, holding both directions of its question: how much of
// a trailing sequence can still become a character, and that a sequence which
// cannot is not held at all.
//
// Both directions are here deliberately. A table of completable prefixes alone
// answers the same under a claimed-length rule -- runeLen of the last sequence
// start, minus what has arrived -- as under this one, because the two rules only
// part company on a byte that claims more bytes than it can ever legally deliver.
// Such a table pins the shape of the answer while leaving the question free to
// change underneath it, which is the one thing a predicate's table is for.
func TestPartialTailLenHoldsOnlyACompletablePrefix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 0},
		{"a\xe4\xb8\xad", 0}, // complete
		{"a\xc2", 1},
		{"a\xe4", 1},
		{"a\xe4\xb8", 2},
		{"a\xf0\x9f\x98", 3},
		{"a\xed", 1},    // a completable prefix, unlike \xed\xa0 below
		{"\xb8\xad", 0}, // continuations with no start in reach
		{"a\xe4\xb8\xad\xe4", 1},
		// Bytes that claim more than they can deliver: held by claimed length,
		// not held here. These are the rows that make the table discriminating.
		{"a\x80", 0},     // a lone continuation, claiming nothing
		{"a\xc0", 0},     // overlong: claims 2
		{"a\xc1", 0},     // overlong: claims 2
		{"a\xed\xa0", 0}, // a surrogate's second byte: claims a third
		{"a\xf5", 0},     // above U+10FFFF: claims 4
		{"a\xf4\x90", 0}, // above U+10FFFF at the second byte: claims two more
	} {
		if got := partialTailLen([]byte(tc.in)); got != tc.want {
			t.Errorf("partialTailLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
		// Whatever is held must be exactly what unicode/utf8 cannot yet decode,
		// and what is kept must decode without loss.
		kept := tc.in[:len(tc.in)-partialTailLen([]byte(tc.in))]
		if !utf8.ValidString(kept) && !strings.ContainsRune(string(utf8x.Lossy([]byte(kept))), utf8.RuneError) {
			t.Errorf("partialTailLen(%q) kept %q, which is neither valid nor damage", tc.in, kept)
		}
	}
}

// The one write where both holds are in play. A '\r' arriving as the whole next
// chunk resolves the held tail to damage -- nothing after a '\r' can continue a
// sequence -- and is itself held. This is where "mutually exclusive" has to be
// true rather than merely commented: an implementation that carries a tail past a
// terminator decision fails here and nowhere else.
func TestScrollbackHoldsMeetOnOneWrite(t *testing.T) {
	b := NewScrollback(10)
	const dmg = string(utf8.RuneError)
	for _, step := range []struct{ write, want string }{
		{"a\xe4", "a"},             // the tail is held, so the line reads short
		{"\r", "a" + dmg},          // the '\r' ends the sequence: damage now
		{"X\n", "a" + dmg + "\rX"}, // the '\r' was content, not a CRLF's
	} {
		b.Write([]byte(step.write))
		if got := b.LastN(1); got != step.want {
			t.Fatalf("after Write(%q) the line reads %q, want %q", step.write, got, step.want)
		}
		if b.heldCR && len(b.heldTail) > 0 {
			t.Fatalf("after Write(%q) both holds are set: heldCR=%v heldTail=%q",
				step.write, b.heldCR, b.heldTail)
		}
	}
}

// Neither hold is flushed when output stops: a held tail stays held, exactly as
// a held '\r' does. Asserted as a pair because a flush added for one and not the
// other would be the real inconsistency, and because a truncated sequence that
// never completes is damage whether it is rendered now or at the next write.
func TestScrollbackDoesNotFlushEitherHold(t *testing.T) {
	for _, tc := range []struct{ name, write, want string }{
		{"held CR", "a\r", "a"},
		{"held tail", "a\xe4", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewScrollback(10)
			b.Write([]byte(tc.write))
			for range 3 {
				if got := b.LastN(1); got != tc.want {
					t.Fatalf("the line reads %q, want %q: reading does not flush a hold", got, tc.want)
				}
			}
			if b.TotalBytes() != uint64(len(tc.write)) {
				t.Errorf("TotalBytes is %d, want %d", b.TotalBytes(), len(tc.write))
			}
			if b.Len() != 1 || b.TotalLines() != 1 {
				t.Errorf("Len=%d TotalLines=%d, want 1 and 1: a line is open either way,\n"+
					"so a reader sees the line the program started", b.Len(), b.TotalLines())
			}
		})
	}
}

// A write that is held in its entirety still advances TotalBytes, so the "has
// anything happened" signal does not stall on a chunk that happens to be one
// partial sequence, and the bytes are not counted again when they are stored.
func TestScrollbackCountsAWhollyHeldWriteOnce(t *testing.T) {
	b := NewScrollback(10)
	b.Write([]byte("\xe4"))
	if got := b.TotalBytes(); got != 1 {
		t.Errorf("TotalBytes after a wholly held write is %d, want 1", got)
	}
	if got := b.TotalLines(); got != 1 {
		t.Errorf("TotalLines is %d, want 1: the write opened a line even though it\n"+
			"stored no byte", got)
	}
	b.Write([]byte("\xb8\xad"))
	if got := b.TotalBytes(); got != 3 {
		t.Errorf("TotalBytes is %d, want 3: held bytes counted twice", got)
	}
	if got, want := b.LastN(1), "中"; got != want {
		t.Errorf("the line reads %q, want %q", got, want)
	}
}
