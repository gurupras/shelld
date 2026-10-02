package shelld

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/utf8x"
)

const defaultScrollbackCapacity = 10_000

// Line bytes live in shared chunks that are only appended to. Lines leave in
// the order they arrived, so a chunk is released once its last line has been
// evicted, and a few released full-size chunks are kept for reuse: once
// history is full, storing a line allocates nothing unless the bytes the
// history holds grow by more than the spares. A buffer per line instead
// turned every evicted line into garbage — a flooding window's whole output
// rate, which piles up until the next collection, and with a large live
// history that is a long way off.
const (
	sbMinChunk = 4 << 10
	sbMaxChunk = 64 << 10
	// sbSpares bounds the released chunks kept per scrollback (256 KiB).
	// Two already absorb the churn of steady build output; recorded agent
	// sessions swing by more (long redraws between short lines), and the
	// remainder is ordinary allocation.
	sbSpares = 4
	// sbSpareShare bounds spares further to 1/sbSpareShare of the chunk
	// bytes in use, so a window whose history went from wide lines to short
	// ones gives the spares back instead of keeping 256 KiB for a few KiB of
	// lines. A quarter raised the garbage of recorded agent sessions by 60 %
	// over no bound; a half, by a tenth.
	sbSpareShare = 2
	// sbMaxLine caps one line, so it still fits a single chunk and a
	// line-counted read stays bounded in bytes. Without a cap, a program that
	// never writes '\n' — a full-screen application redrawing with cursor
	// addressing, or a '\r' progress bar — would grow one line without end.
	sbMaxLine = sbMaxChunk
	// sbCutLook is how far back a cut at the cap looks for an ESC, so a CSI
	// or OSC sequence shorter than this is not split between two lines.
	sbCutLook = 32
)

// sbDefaultBudget bounds the chunk bytes one scrollback holds by default.
//
// It is a byte figure and not a function of the line capacity on purpose. A
// capacity is chosen to hold a useful amount of *shell* history — 10 000 lines
// of a few tens of bytes each — and deriving a byte ceiling from it authorises
// that many lines of the longest line the store allows, which for a full-screen
// program is 43.6 MB per window. What such a window costs is a property of the
// bytes, so it is set in bytes.
//
// 8 MB, from what recorded agent output measures at:
//
//   - A saturated window of recorded agent output holds 28.2 MB under a
//     capacity-derived ceiling, so the old figure was reachable, not notional.
//   - The mean retained line of a real TUI is ~2.3 KB (they do emit '\n' when
//     they scroll), so 8 MB is ~32 000 frames or ~1 340 full repaints — orders
//     more than a lines=50 capture or the 16 KiB idle scan ever read, and more
//     than a person scrolls back through.
//   - 10 000 lines of ordinary shell output still fit with ~17x to spare; the
//     budget only starts binding for that shape below ~2 MB.
//   - 100 windows cost ~800 MB rather than ~2.8 GB.
//
// A window can raise or lower it: see WindowOpts.ScrollbackBytes. The figure
// comes from a corpus of recordings replayed to saturation, so windows log what
// they hold and it can be revisited from live sessions.
const sbDefaultBudget = 8 << 20

// lineRef locates a line: n bytes at off in the chunk with sequence number
// chunk. off and n are 32-bit; sbMaxLine keeps a line far below that.
//
// Chunk storage costs about as much as the lines only while lines are short
// next to a chunk. Lines of 32–64 KiB leave up to half of each 64 KiB chunk
// unused, about twice their bytes against roughly 1.25× for a buffer per
// line, and a line that outgrew its chunk leaves a dead copy behind. The
// budget bounds the total either way; it is retention, not memory, that a
// stream of long lines gives up.
type lineRef struct {
	chunk uint64
	off   uint32
	n     uint32
}

// Scrollback is a fixed-capacity ring of raw PTY output lines (ANSI
// preserved).
//
// A line ends at '\n', '\v' or '\f', which a terminal treats alike; the
// terminator is not stored and one '\r' before it is stripped. Output that has
// not ended its line yet is the last line, and a later write extends it rather
// than starting a line — so what the history holds is a function of the bytes
// written and not of how they were split into writes, which is what lets a
// line longer than one PTY read come back whole.
//
// A '\r' does not end a line: raw history is byte-preserving, and keeps the
// frames a progress bar or a redraw overwrote. Window.Capture's visible mode
// is the rendered screen, which is where those overwrites are resolved.
//
// Two bounds keep it finite. A line is cut at sbMaxLine bytes, and the oldest
// lines are evicted once the chunk bytes held pass sbBudget.
//
// The last retained line's text can change between reads as output extends it;
// every other retained line is immutable. TotalLines counts lines started, so
// it does not advance while the last line grows — TotalBytes does.
type Scrollback struct {
	mu       sync.RWMutex
	refs     []lineRef // the ring; grows to capacity as lines arrive
	capacity int
	start    int    // index in refs of the oldest retained line
	n        int    // retained lines
	count    uint64 // lines ever started
	bytes    uint64 // bytes ever written
	open     bool   // the newest line has not ended: a write extends it
	heldCR   bool   // ...and ended with a '\r' held out of its stored bytes
	// ...or ended inside a UTF-8 sequence, whose bytes are held out of it.
	// Mutually exclusive with heldCR by construction rather than by agreement:
	// a chunk's last byte is either a '\r', which is a complete character, or
	// inside a sequence, and it cannot be both. Never more than utf8.UTFMax-1
	// bytes, and its backing array is reused across writes.
	heldTail []byte
	budget   int64
	// chunks[i] has sequence number base+i; lines are appended to the last.
	chunks [][]byte
	base   uint64
	held   int // capacity of chunks, in bytes
	spares [sbSpares][]byte
	nspare int
}

// NewScrollback returns a store holding up to capacity lines, or
// defaultScrollbackCapacity lines when capacity is 0, under the default byte
// budget. The line count is not the binding limit for a full-screen program,
// whose lines can each be tens of kilobytes; see NewScrollbackWithBudget.
func NewScrollback(capacity int) *Scrollback {
	return NewScrollbackWithBudget(capacity, sbDefaultBudget)
}

// NewScrollbackWithBudget is NewScrollback with an explicit ceiling on the chunk
// bytes held; 0 means sbDefaultBudget. The ceiling is not allowed below one
// maximum line, since a store that cannot hold the line it just cut would evict
// it as it arrived.
func NewScrollbackWithBudget(capacity int, budget int64) *Scrollback {
	if capacity == 0 {
		capacity = defaultScrollbackCapacity
	}
	if budget == 0 {
		budget = sbDefaultBudget
	}
	budget = max(budget, sbMaxLine+sbMaxChunk)
	return &Scrollback{capacity: capacity, budget: budget}
}

// HeldBytes is the chunk bytes the store holds, which is what the budget bounds.
func (s *Scrollback) HeldBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.held
}

// Budget is the ceiling on HeldBytes.
func (s *Scrollback) Budget() int64 { return s.budget }

// Capacity is the ceiling on Len, in lines. Either it or Budget can be the one
// that evicts, whichever binds first for the traffic the window is carrying.
func (s *Scrollback) Capacity() int { return s.capacity }

// endsLine reports the C0 controls that feed a line: LF, VT and FF, which
// tmux's input_c0_dispatch also treats alike. They are 0x0a to 0x0c, so one
// comparison covers all three.
func endsLine(b byte) bool { return b-'\n' <= 2 }

// indexEndsLine is the offset of the first LF, VT or FF in data, or -1.
//
// bytes.IndexByte is vectorised where a byte loop is not, and this runs over
// every byte a window produces. LF is by far the most common of the three, so
// the other two are only looked for in the bytes before it: real output has
// none, and the search then covers one line rather than the whole read.
func indexEndsLine(data []byte) int {
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		i = len(data)
	}
	if j := bytes.IndexByte(data[:i], '\v'); j >= 0 {
		i = j
	}
	if j := bytes.IndexByte(data[:i], '\f'); j >= 0 {
		i = j
	}
	if i == len(data) {
		return -1
	}
	return i
}

// Write adds a chunk of the program's raw output, splitting it into lines on LF,
// VT and FF and dropping the CR of a CRLF. A line that begins and ends inside one
// chunk is stored as it stands; a line spanning chunks is joined. Lines are
// evicted from the oldest end once either Capacity or Budget binds.
//
// Two things a caller reading history back should know.
//
// A chunk ending in '\r' does not store that byte yet. Whether it is the CR of a
// CRLF that the next chunk completes, or content the program meant to overwrite
// with, is not decidable until the next chunk arrives — so it is held, and the
// open line reads one byte short until then. A caller that writes a chunk ending
// in '\r' and immediately reads the tail sees the line without it. Nothing is
// lost: the next Write resolves it either way.
//
// A chunk ending part-way through a UTF-8 sequence holds those bytes for the same
// reason. History is stored as raw bytes and decoded when it is read, so bytes
// that are only a prefix of a character would read as U+FFFD for a character that
// is merely still arriving — a PTY read can end anywhere, including mid-rune. The
// hold is what keeps that out of the read; the next Write completes the sequence
// and it renders whole. Only a sequence that can still become a character is
// held: bytes that are already certainly damage, such as \xc0 or a surrogate, are
// stored at once rather than delaying a U+FFFD that is not going to change. See
// partialTailLen, which is not cutBackoff and must not become it.
//
// A single line is cut at sbMaxLine. A full-screen program that never emits a
// newline produces one unbounded line, and cutting it is what keeps a redrawing
// program from consuming the whole budget in one entry. The cut backs off so it
// does not land inside a UTF-8 sequence or inside a short escape sequence; see
// cutBackoff.
func (s *Scrollback) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes += uint64(len(data))

	// Bytes held back mid-sequence lead the chunk that continues them, so that
	// the rest of Write sees one sequence rather than the halves two reads
	// delivered. They are prepended rather than put back through appendOpen --
	// the cheaper thing -- because a held byte must not enter the line until its
	// sequence is complete: a rune split across three writes would otherwise be
	// stored by the middle one, whose own bytes are continuations with no
	// sequence start among them, and partialTailLen would find nothing to hold.
	// Keeping the hold whole is what makes the answer depend on the stream
	// rather than on where a read happened to end.
	//
	// They are not counted again -- s.bytes above counted them on the write that
	// brought them, which is why the count comes first. This runs before the
	// heldCR branch, which indexes data[0]; the two holds are mutually
	// exclusive, so neither can be looking at the other's bytes.
	if len(s.heldTail) > 0 {
		joined := make([]byte, 0, len(s.heldTail)+len(data))
		joined = append(joined, s.heldTail...)
		data = append(joined, data...)
		s.heldTail = s.heldTail[:0]
	}

	// A '\r' held back from the open line is the CR of a CRLF only if the next
	// byte ends the line. Otherwise it is a byte the program overwrote with,
	// and belongs in the line.
	if s.heldCR {
		s.heldCR = false
		if endsLine(data[0]) {
			s.open = false
			data = data[1:]
		} else {
			s.appendOpen([]byte{'\r'})
		}
	}
	for len(data) > 0 {
		i := indexEndsLine(data)
		if i < 0 {
			line := data
			if line[len(line)-1] == '\r' {
				// Held rather than stored: whether it is a CRLF's CR or
				// content is not known until the next write.
				line, s.heldCR = line[:len(line)-1], true
			} else if k := partialTailLen(line); k > 0 {
				// Held rather than stored: these bytes are the start of a
				// character whose remaining bytes have not arrived.
				s.heldTail = append(s.heldTail[:0], line[len(line)-k:]...)
				line = line[:len(line)-k]
			}
			// Appended even when the hold took the whole chunk, so that a line
			// is open either way: a reader between the two writes should see
			// the line the program started.
			s.appendOpen(line)
			break
		}
		line := data[:i]
		if k := len(line); k > 0 && line[k-1] == '\r' {
			line = line[:k-1]
		}
		// A line that both begins and ends in this write is stored as it is:
		// one copy, packed against the line before it, and nothing left open.
		// Joining across writes costs only the writes that need it.
		if !s.open && len(line) <= sbMaxLine {
			s.push(line)
		} else {
			s.appendOpen(line)
			s.open = false
		}
		data = data[i+1:]
	}
	s.evictToBudget()
}

// appendOpen adds seg to the open line, starting a line when none is open and
// cutting the line at sbMaxLine when it would grow past it.
func (s *Scrollback) appendOpen(seg []byte) {
	for {
		if !s.open {
			s.push(nil)
			s.open = true
		}
		room := sbMaxLine - int(s.refs[s.newest()].n)
		if len(seg) <= room {
			s.extend(seg)
			return
		}
		back := cutBackoff(s.newestBytes(), seg[:room])
		if back > room {
			// The back-off reaches past the bytes being added into the ones
			// already stored: give those back to the next line.
			moved := s.shrinkOpen(back - room)
			s.open = false
			s.appendOpen(moved)
			continue
		}
		s.extend(seg[:room-back])
		s.open = false
		seg = seg[room-back:]
		if len(seg) == 0 {
			// Unreachable, and deliberately kept: this arm was entered because
			// len(seg) > room and back <= room, so what is left is at least
			// len(seg)-room bytes. Its being dead is what makes appendOpen
			// always return through the extend above, i.e. with a line open —
			// which is what lets Write treat a held CR as implying one. A
			// change that can reach this return has broken that.
			return
		}
	}
}

// cutBackoff reports how many of the bytes before a cut at the cap belong with
// the next line instead: enough to leave a UTF-8 sequence whole, and, when an
// ESC falls within sbCutLook of the cut, everything from that ESC, so a short
// escape sequence is not split.
//
// The cut is at the end of add, which the caller has already limited to the
// bytes that fit; line is what precedes it in the same line. Looking at both
// is what makes the answer a property of the stream rather than of where a
// write happened to end.
func cutBackoff(line, add []byte) int {
	var buf [sbCutLook + utf8.UTFMax - 1]byte
	w := buf[:0]
	if k := len(buf) - len(add); k > 0 {
		w = append(w, line[max(len(line)-k, 0):]...)
	}
	w = append(w, add[max(len(add)-len(buf), 0):]...)

	// An ESC opens a sequence, so every byte after it belongs to that
	// sequence.
	for i := len(w) - 1; i >= 0 && len(w)-i <= sbCutLook; i-- {
		if w[i] == 0x1b {
			return len(w) - i
		}
	}
	// Otherwise keep the rune at the cut whole. The bytes after the cut have
	// not arrived, so the length has to come from the leading byte rather than
	// from decoding: a sequence that is short only because it is cut is exactly
	// what utf8.DecodeRune reports as one invalid byte.
	for i := len(w) - 1; i >= 0 && len(w)-i < utf8.UTFMax; i-- {
		if !utf8.RuneStart(w[i]) {
			continue // a continuation byte: the sequence starts earlier
		}
		if runeLen(w[i]) > len(w)-i {
			return len(w) - i
		}
		return 0
	}
	return 0
}

// partialTailLen is how many bytes at the end of data begin a UTF-8 sequence
// that is incomplete but can still be completed by bytes that have not arrived,
// or 0 when there is nothing worth waiting for. Write holds exactly these bytes
// back so a read cannot render a character that is still arriving as U+FFFD.
//
// This is not cutBackoff and the two must not be merged, however similar they
// look. cutBackoff backs off over any byte that *claims* more bytes, \xc0
// included; this holds only bytes that can still *become* a character, so \xc0 is
// not held. The answers differ because the consequences do: cutBackoff moves
// bytes to the next line, where being wrong costs a line boundary, while a hold
// withholds bytes from every read until the next write, where being wrong hides a
// byte the reader is entitled to see. Bytes that are certainly damage are not
// worth hiding, because waiting cannot improve them.
//
// The test is Rust's str::from_utf8 error model, which internal/utf8x.Check
// implements: an errLen of 0 with nothing valid before it means the string ends
// inside an otherwise valid sequence, as opposed to a definite error length.
//
// The validUpTo == 0 half of that test is implied by where the scan starts, not a
// case it catches: starting at the LAST sequence start in reach means Check never
// sees a complete character before the incomplete one, so validUpTo is already 0
// whenever errLen is. Checked exhaustively over every byte string up to length 3
// and a lead-byte-class alphabet at length 4 -- 16,908,544 cases, no disagreement
// with the clause dropped. It is kept because it is what makes the condition mean
// "ends inside a sequence" on its own terms rather than by agreement with the
// loop above it: widen the bound, or walk forward instead of back, and it starts
// doing work.
func partialTailLen(data []byte) int {
	for i := len(data) - 1; i >= 0 && len(data)-i < utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue // a continuation byte: the sequence starts earlier
		}
		if validUpTo, errLen, ok := utf8x.Check(data[i:]); !ok && validUpTo == 0 && errLen == 0 {
			return len(data) - i
		}
		// The last sequence start in reach either completes a character or is
		// already damage; either way nothing before it is worth holding.
		return 0
	}
	return 0
}

// runeLen is the length the UTF-8 sequence starting with b claims. A byte that
// starts no sequence counts as one, so invalid bytes stay where they are
// instead of moving a byte to the next line at every cut.
func runeLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b < 0xc0:
		return 1 // a continuation byte, reached only for invalid input
	case b < 0xe0:
		return 2
	case b < 0xf0:
		return 3
	case b < 0xf8:
		return 4
	}
	return 1
}

// wrap reduces a slot index that may have run one lap past the end of refs.
// The retained range is at most the whole ring, so i is under twice its length
// and a conditional subtraction does what '%' would — which is a division,
// since the ring's length is not a constant, on a path that runs per line.
func (s *Scrollback) wrap(i int) int {
	if i >= len(s.refs) {
		return i - len(s.refs)
	}
	return i
}

// newest is the index in refs of the newest retained line.
func (s *Scrollback) newest() int { return s.wrap(s.start + s.n - 1) }

func (s *Scrollback) newestBytes() []byte {
	r := s.refs[s.newest()]
	return s.chunks[r.chunk-s.base][r.off : r.off+r.n]
}

// extend appends seg to the open line. Nothing is stored after a line while it
// is open, so the open line is the last bytes of the last chunk and this is an
// append in place — unless it outgrows that chunk, when the line moves once
// into a chunk of its own.
func (s *Scrollback) extend(seg []byte) {
	if len(seg) == 0 {
		return
	}
	i := s.newest()
	r := s.refs[i]
	last := len(s.chunks) - 1
	c := s.chunks[last]
	if cap(c)-len(c) >= len(seg) {
		s.chunks[last] = append(c, seg...)
		s.refs[i].n = r.n + uint32(len(seg))
		return
	}
	line := c[r.off : r.off+r.n]
	nc := s.newChunk(len(line) + len(seg))
	nc = append(nc, line...)
	nc = append(nc, seg...)
	s.refs[i] = lineRef{off: 0, n: uint32(len(nc))}
	if r.off == 0 {
		// The line was alone in its chunk, so the copy it leaves behind is
		// garbage rather than dead space in a chunk that is still held.
		s.held += cap(nc) - cap(c)
		s.chunks[last] = nc
		s.refs[i].chunk = r.chunk
		return
	}
	s.chunks = append(s.chunks, nc)
	s.held += cap(nc)
	s.refs[i].chunk = s.base + uint64(len(s.chunks)-1)
}

// shrinkOpen removes the last k bytes of the open line and returns them.
func (s *Scrollback) shrinkOpen(k int) []byte {
	i := s.newest()
	r := s.refs[i]
	last := len(s.chunks) - 1
	c := s.chunks[last]
	out := make([]byte, k)
	copy(out, c[int(r.off)+int(r.n)-k:])
	s.chunks[last] = c[:len(c)-k]
	s.refs[i].n = r.n - uint32(k)
	return out
}

// newChunk returns an empty chunk with room for size bytes, reusing a spare
// when one will do. A capped line always fits sbMaxChunk.
func (s *Scrollback) newChunk(size int) []byte {
	if size <= sbMaxChunk && s.nspare > 0 {
		s.nspare--
		c := s.spares[s.nspare]
		s.spares[s.nspare] = nil
		return c
	}
	return make([]byte, 0, max(size, sbMaxChunk))
}

// push stores line as a new line, evicting the oldest when the ring is full.
func (s *Scrollback) push(line []byte) {
	if s.n == s.capacity {
		s.start, s.n = s.wrap(s.start+1), s.n-1
	}
	ref := s.store(line)
	// The ring only wraps once refs has reached capacity: growing it while the
	// retained range already wrapped would change the modulus under it and
	// move every line to a different slot.
	switch i := s.start + s.n; {
	case i < len(s.refs):
		// Either room left in the slice, or a slot a front eviction freed.
		s.refs[i] = ref
	case len(s.refs) < s.capacity:
		s.refs = append(s.refs, ref)
	default:
		s.refs[s.wrap(i)] = ref
	}
	s.n++
	s.count++
	s.release(s.refs[s.start].chunk)
}

// evictToBudget drops the oldest lines while the chunk bytes held exceed the
// budget, always keeping the newest line.
func (s *Scrollback) evictToBudget() {
	for int64(s.held) > s.budget && s.n > 1 {
		s.start, s.n = s.wrap(s.start+1), s.n-1
		s.release(s.refs[s.start].chunk)
	}
}

// store copies line into the last chunk, starting a new chunk when it does
// not fit. Chunks double from sbMinChunk so a quiet window stays small; a
// line longer than sbMaxChunk gets a chunk of its own.
func (s *Scrollback) store(line []byte) lineRef {
	last := len(s.chunks) - 1
	if last < 0 || cap(s.chunks[last])-len(s.chunks[last]) < len(line) {
		size := sbMinChunk
		if last >= 0 {
			size = min(2*cap(s.chunks[last]), sbMaxChunk)
		}
		var c []byte
		if size == sbMaxChunk && s.nspare > 0 && len(line) <= sbMaxChunk {
			s.nspare--
			c, s.spares[s.nspare] = s.spares[s.nspare], nil
		} else {
			c = make([]byte, 0, max(size, len(line)))
		}
		s.chunks = append(s.chunks, c)
		s.held += cap(c)
		last++
	}
	c := s.chunks[last]
	s.chunks[last] = append(c, line...)
	return lineRef{chunk: s.base + uint64(last), off: uint32(len(c)), n: uint32(len(line))}
}

// release drops the chunks before sequence number oldest, which no retained
// line references, keeping full-size ones as spares while the spares stay
// within their share of the chunks still in use.
func (s *Scrollback) release(oldest uint64) {
	k := int(oldest - s.base)
	if k == 0 {
		return
	}
	for _, c := range s.chunks[:k] {
		s.held -= cap(c)
	}
	keep := min(sbSpares, s.held/sbSpareShare/sbMaxChunk)
	for s.nspare > keep {
		s.nspare--
		s.spares[s.nspare] = nil
	}
	for _, c := range s.chunks[:k] {
		if s.nspare < keep && cap(c) == sbMaxChunk {
			s.spares[s.nspare] = c[:0]
			s.nspare++
		}
	}
	n := copy(s.chunks, s.chunks[k:])
	clear(s.chunks[n:])
	s.chunks = s.chunks[:n]
	s.base = oldest
}

// line returns the bytes of the i'th retained line, oldest first.
func (s *Scrollback) line(i int) []byte {
	r := s.refs[s.wrap(s.start+i)]
	return s.chunks[r.chunk-s.base][r.off : r.off+r.n]
}

func (s *Scrollback) lenLocked() int { return s.n }

// Len is the number of retained lines.
func (s *Scrollback) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lenLocked()
}

// TotalLines counts every line ever started, including evicted ones. It does
// not advance while output extends the last line.
func (s *Scrollback) TotalLines() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

// TotalBytes counts every byte ever written, including evicted ones. Unlike
// TotalLines it advances whenever output arrives, so it is the signal a caller
// wants for "has anything happened".
func (s *Scrollback) TotalBytes() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes
}

// LastN joins the most recent n lines with '\n'. A line can be up to
// sbMaxLine, so this copies up to n × 64 KiB; LastNBytes bounds that.
//
// The last line's text can change between calls, because output extends it.
// Every other retained line is immutable, so a caller that has read a line
// other than the last one can keep it.
func (s *Scrollback) LastN(n int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n = min(n, s.lenLocked())
	if n <= 0 {
		return ""
	}
	return s.joinLocked(s.n-n, n)
}

// Range joins count lines starting offset lines after the oldest retained
// line. A line can be up to sbMaxLine, so this copies up to count × 64 KiB;
// RangeBytes bounds that.
//
// The last line's text can change between calls; see LastN.
func (s *Scrollback) Range(offset, count int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if offset < 0 || offset >= s.lenLocked() || count <= 0 {
		return ""
	}
	return s.joinLocked(offset, min(count, s.n-offset))
}

// joinLocked is join for the unbounded reads, which applied no byte bound and so
// never measured what the lines render to.
//
// The sum of the raw line lengths is that size exactly when every line is valid
// UTF-8, and below it otherwise, which is the direction the hint has to err in: see
// join on why an overshoot would be retained memory rather than a wasted buffer.
//
// Erring low costs reallocation, and more of it than one growth: Builder grows
// through append, geometrically, so a payload that renders to three times its raw
// length takes about five reallocations, each copying what is already there. That is
// paid only on a read that had invalid bytes in it, whereas measuring the rendered
// length exactly would cost a validation pass over every read that does not.
func (s *Scrollback) joinLocked(start, n int) string {
	size := n - 1
	for i := 0; i < n; i++ {
		size += len(s.line(start + i))
	}
	return s.join(start, n, false, size)
}

// join renders the n lines at start, joined with '\n', in one allocation.
//
// valid is fitLines' verdict on those same lines: when every one of them was
// already valid UTF-8, joining them with '\n' cannot produce anything Lossy would
// change, so they go in as they are and the second validation pass over the whole
// payload is pure repetition. Passing false validates line by line instead, which
// is what a read with no bound has to do.
//
// The join goes into a strings.Builder rather than a []byte that is then converted,
// because the conversion copies the payload a second time: a 1 MiB read allocated
// and copied 1 MiB twice to return one string. Builder.String hands over the buffer
// it already filled.
//
// size is what those lines render to, joined, which fitLines measured while applying
// the bound. It cannot change the text, but it is not free to be anything: it must
// not exceed the rendered length. Builder.String hands over the buffer instead of
// copying out of it, so the returned string's backing array is the whole Grow'n
// allocation, and the garbage collector keeps all of it live for as long as the
// consumer holds that text. An overshoot is retained memory, not slack in a buffer
// that goes away -- with the hint multiplied by 64, 200 MB of returned text held
// 12.8 GB of live heap. Undershooting only costs reallocation.
//
// Both callers stay under that bound by construction rather than by agreeing with
// each other, which is why size is passed in rather than recomputed here.
// LossyLen(b) >= len(b) for every b, because a maximal invalid subpart is one to
// three bytes in and exactly three out, so fitLines' sum of LossyLen is the rendered
// length exactly and joinLocked's sum of len is at or below it.
func (s *Scrollback) join(start, n int, valid bool, size int) string {
	var sb strings.Builder
	if size > 0 { // Grow panics on a negative size, and a hint is not worth that.
		sb.Grow(size)
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('\n')
		}
		line := s.line(start + i)
		if valid {
			sb.Write(line)
			continue
		}
		utf8x.WriteLossy(&sb, line)
	}
	return sb.String()
}

// A line runs to sbMaxLine bytes, so "the last n lines" is not a bound on
// bytes: a program that writes no '\n' — a full-screen redraw, a '\r' progress
// bar — puts 64 KiB in each of them. The byte-bounded reads below give every
// line-counted read a bound in bytes as well, and each reports whether the
// bound cost it anything so a caller can say so rather than silently showing
// part of the history as if it were all of it.
//
// fitLines counts the lines of [start, start+n) that fit maxBytes when joined
// with '\n', taking them from the newest end when fromEnd and from the oldest
// otherwise.
//
// A line is measured as utf8x.Lossy will render it, not as it is stored, because
// that is what the caller receives and the bound is a promise about that. The two
// differ whenever a maximal invalid subpart is one or two bytes: a line counted as
// fitting in raw bytes could arrive over the bound, and the read then had to trim
// the joined text mid-line to keep the bound exact — which cost the caller a line
// boundary, and, worse, could land on the '\n' and make a whole line unreachable at
// that bound. Measured this way the joined text is within the bound by
// construction, so the keep >= 1 paths never trim.
//
// valid reports whether every line it kept was already valid UTF-8, which the
// measurement establishes anyway. It is what lets the caller join those lines
// without validating them a second time. It says nothing about the lines the
// bound dropped, which are not in the text.
//
// size is what those kept lines render to, joined: the bound is applied by
// accumulating it, so returning it costs nothing and saves the caller from sizing
// the join by a second walk over the same lines.
func (s *Scrollback) fitLines(start, n, maxBytes int, fromEnd bool) (keep int, valid bool, size int) {
	valid = true
	for i := 0; i < n; i++ {
		j := start + i
		if fromEnd {
			j = start + n - 1 - i
		}
		add, lineValid := utf8x.LossyLenValid(s.line(j))
		if keep > 0 {
			add++ // the '\n' joining it to the lines already counted
		}
		if size+add > maxBytes {
			break
		}
		size += add
		keep++
		valid = valid && lineValid
	}
	return keep, valid, size
}

// tailLocked is LastNBytes and Tail: the newest lines that fit maxBytes,
// reporting how many lines the text covers, how long the newest line renders,
// whether the bound dropped anything, and whether the text begins inside a line.
func (s *Scrollback) tailLocked(n, maxBytes int) (text string, lines, lineBytes int, truncated, cut bool) {
	n = min(n, s.lenLocked())
	if n <= 0 {
		return "", 0, 0, false, false
	}
	if maxBytes <= 0 {
		return "", 0, 0, true, false
	}
	// The newest line is the one this read is anchored at and the only one the
	// bound can cut into, so its rendered length is the size a caller needs to
	// raise maxBytes to. Zero on the returns above, where no read was attempted.
	lineBytes = utf8x.LossyLen(s.line(s.n - 1))
	keep, valid, size := s.fitLines(s.n-n, n, maxBytes, true)
	if keep == 0 {
		// Not even the newest line fits. Its end is what a reader of a tail
		// wants, so that is what it gets, and the text then begins at a
		// position the program never started a line at.
		text, _ = fitTail(utf8x.Lossy(s.line(s.n-1)), maxBytes)
		// truncated is what was dropped, not which arm ran. n is already clamped
		// to the history, so n > 1 means whole lines went; len(text) < lineBytes
		// means bytes of the newest line went. Neither holds when the bound gave
		// way to a one-rune line and the caller asked for that one line — the
		// text is then the whole of what was requested, and reporting it as
		// truncated sends a caller to raise a bound that is already enough. cut
		// stays true regardless: the answer to "does the text begin where a line
		// did" is documented as inexact in the 1..3 band, for this same reason.
		return text, 1, lineBytes, n > 1 || len(text) < lineBytes, true
	}
	// fitLines measured these lines as Lossy renders them, so the joined text is
	// within the bound and this trim does not fire. It stays as the enforcement
	// of the bound rather than as an assumption about the measurement; when it
	// does nothing, cut is false, which is what makes cut mean "the text begins
	// where no line did" instead of "the bound was tight".
	text, cut = fitTail(s.join(s.n-keep, keep, valid, size), maxBytes)
	return text, keep, lineBytes, keep < n || cut, cut
}

// Tail returns the most recent n lines, keeping at most maxBytes bytes of the
// end, and reports whether the text begins inside a line rather than where the
// program began one.
//
// It is the read for scanning recent output for a pattern: bounded in bytes, and
// with cut telling the caller that a '^' in its pattern would otherwise match
// at a position that is an artefact of the bound.
//
// Outside maxBytes of 1..3, cut answers that question exactly: it is true if and
// only if the text begins inside a line. In that band it can be true for a text
// that is a whole line, because the bound gave way to one rune.
func (s *Scrollback) Tail(n, maxBytes int) (text string, cut bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, _, _, _, cut = s.tailLocked(n, maxBytes)
	return text, cut
}

// LastNBytes is LastN with a byte bound: whole lines are dropped from the
// oldest end until the text fits maxBytes, and truncated reports whether any
// were. When the newest line alone is over the bound, its end is kept, and the
// text then begins inside a line rather than where the program began one. Use
// Tail to learn that it happened.
//
// The bound is measured on the returned text, not on the bytes stored: a line
// counted whole in raw bytes can still be over it once utf8x.Lossy has expanded
// each maximal invalid subpart into one three-byte replacement character. Per
// subpart, not per byte, and the difference decides the worst case a caller has
// to size for: "\xe4\xb8", the truncated head of a three-byte sequence, is one
// subpart and comes back as three bytes rather than six. So 3x is the ceiling and
// it is reached only by a run of one-byte subparts ("\xff\xff" is 6), while a
// truncated four-byte head is three stored bytes and three text bytes and does
// not expand at all. utf8x_test.go pins "\xe2\x82A" to "�A" for this.
//
// The post-condition, in this order: never a partial rune; never empty while a
// line remains; otherwise len(text) <= max(maxBytes, utf8.UTFMax). The bound is
// a resource guard rather than a frame — nothing downstream sizes a buffer to it
// exactly — so it gives way by at most three bytes to the one rune that makes
// the difference between a bounded read and a caller that cannot advance.
//
// lines is how many stored lines the text covers. It is not derivable from the
// text: an empty text stands for no lines, while strings.Count("", "\n")+1 is 1, so
// a caller counting '\n' advances past a line it never received. It is 1 when the
// newest line alone was over the bound and only part of it came back.
//
// It is not, however, something a caller can page backwards by, and this read cannot
// walk history at all: n is a width measured from a fixed anchor — the newest line —
// not a position, so "read n, subtract what you got, read again" re-reads the same
// end. Only RangeBytes has an offset to spend a line count on, so a forward pager can
// exhaust history and a backward one has to be written in terms of RangeBytes
// offsets. That asymmetry is a property of these two reads rather than an oversight
// in this comment; giving Tail a movable anchor would be a new method with its own
// review.
//
// n saturates: once maxBytes is the binding constraint, raising n changes nothing,
// and the lines that fall outside the bound are not reachable at that maxBytes by
// any n. Only a larger maxBytes reaches them, and it re-sends the whole tail.
//
// lineBytes is how long the newest line renders — the line this read is anchored
// at, and the only one the bound can cut into. It is what a caller raises maxBytes
// to when it needs that line whole, and it is why this read carries a size rather
// than a "this is a fragment" flag: the flag detects the case without saying what
// to ask for next, leaving a caller to double its bound until the text stops
// growing. A caller wanting the flag computes it, so there is one source of the
// one truth: the text is part of a line if and only if lineBytes > len(text). It
// is 0 on every return where no read was attempted, which is exactly the returns
// where lines is 0, so that derivation cannot read a rejected request as a
// fragment.
//
// A caller that also wants TotalLines must use LastNBytesWithTotal rather than
// calling this and then TotalLines: that is two acquisitions of the lock, a line
// can be started between them, and the count then covers a line the text does not
// carry — which is a loss for any client that banks the count as delivered.
// Window.CaptureBytes is that caller, and is why the combined method exists.
func (s *Scrollback) LastNBytes(n, maxBytes int) (text string, lines, lineBytes int, truncated bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, lines, lineBytes, truncated, _ = s.tailLocked(n, maxBytes)
	return text, lines, lineBytes, truncated
}

// LastNBytesWithTotal is LastNBytes and TotalLines from one snapshot: both are
// read under the same acquisition of the lock, so no line can be started between
// them. A caller that reads the two separately gets a count that can cover a line
// its text does not contain, and a count is the thing a client banks — it polls,
// takes what the count says as delivered, and never comes back for the rest. Taken
// together the question does not arise, which is why this exists as one call rather
// than as an ordering rule for callers to keep.
func (s *Scrollback) LastNBytesWithTotal(n, maxBytes int) (text string, lines, lineBytes int, totalLines uint64, truncated bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, lines, lineBytes, truncated, _ = s.tailLocked(n, maxBytes)
	return text, lines, lineBytes, s.count, truncated
}

// RangeBytes is Range with a byte bound: whole lines are dropped from the
// newest end until the text fits maxBytes, and truncated reports whether any
// were. When the line at offset alone is over the bound, its start is kept, so
// the text ends inside a line — and the rest of that line is then unreachable at
// this maxBytes, since the next offset is a whole line further on. A caller that
// has to see such a line whole has to raise maxBytes for it; there is no offset
// that reaches its remainder.
//
// The bound is measured on the returned text, not on the bytes stored, and holds
// as max(maxBytes, utf8.UTFMax) with a whole first rune always returned, both for
// the reasons given on LastNBytes.
//
// A maxBytes of zero or less is a defined request rather than an error or a request
// for a default: no text, no lines, and truncated true, because nothing fits a bound
// of nothing and a requested line was left behind. It is not clamped up to a floor,
// and the floor is the tempting change: readByteCap clamps only the top, so
// ScrollRange(offset, 0) — /scroll?lines=0 through it — arrives here with maxBytes 0,
// and a floor would answer a caller that asked for nothing with a page of history. A
// count of zero or less is answered before the bound, as "you asked for no lines"
// with truncated false, so the flag is what tells the two empty answers apart.
//
// lines is how many stored lines the text covers, and is what a caller paging
// forwards adds to offset: doing that visits every line exactly once, and the
// loop terminates because lines is 0 only when the read returned nothing — the
// offset is past the newest line, or maxBytes was not positive — and is
// otherwise at least 1. Counting '\n' in the text does not give this, and it is
// worth naming where: wherever the text is non-empty the two agree, and provably
// so — the returns below either join keep lines with keep-1 separators or return
// one cut line with none — so the whole difference is in the returns whose text is
// empty — and specifically in the two that mean "nothing was read" and carry lines
// 0, where strings.Count("", "\n")+1 is 1 and a caller counting '\n' advances past
// a line it never received. (The third empty text is a blank line, which carries
// lines 1; there the count agrees, and the hazard is the neighbouring one below,
// that an empty page is not the end of history.) Those returns are what
// scrollback_bytes_test.go pins, because they are the whole content of this claim:
// substituting the count for lines in the keep >= 1 arm leaves the suite green, and
// should.
//
// lines is 1 when the line at offset alone was over the bound and only its start
// came back, so a pager visits every line but does not necessarily see every
// byte; truncated says so.
//
// Where that stops happening is a function of the history: a pager reproduces it
// byte for byte at every maxBytes at or above the longest rendered line that holds
// two or more runes, and only there. A line of a single rune is returned whole at
// any bound — that is the KEEP-ONE-RUNE post-condition — so however long that rune
// renders, it never sets this threshold.
//
// This meaning is the one that ships, and a read shape that needs a different one
// must add a field rather than redefine this. A movable anchor for the reverse
// direction would keep it: lines would be the advance in both directions. A byte
// cursor would not — a line straddling two pages is covered by both, so the sum over
// a pass exceeds Len() and the advance becomes a byte offset — so such a shape ships
// that offset as its own field and retires or renames this one. Keeping the name and
// the type while changing what the number means leaves every existing caller
// compiling and wrong.
//
// A blank line is the one case where text is empty and lines is 1: it always fits,
// so it never reaches the over-the-bound arm. A caller that reads empty output as
// "no more data" stops early on it; lines, not the text's length, says whether the
// read covered anything.
//
// lineBytes is how long the line at offset renders, which is the one line this read
// can end inside, and it answers the question the paragraph above leaves open: a
// caller that has to see that line whole raises maxBytes to lineBytes. A page is
// part of a line if and only if lineBytes > len(text), so the caller computes that
// rather than being told it — one source for one truth. It is 0 on every return
// where no read was attempted, which is exactly the returns where lines is 0, so
// the derivation does not read a rejected request as a fragment of the line at
// offset.
func (s *Scrollback) RangeBytes(offset, count, maxBytes int) (text string, lines, lineBytes int, truncated bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if offset < 0 || offset >= s.lenLocked() || count <= 0 {
		return "", 0, 0, false
	}
	count = min(count, s.n-offset)
	if maxBytes <= 0 {
		return "", 0, 0, true
	}
	// Zero on the two returns above, where no read was attempted: a caller
	// deriving "this page is a fragment" from lineBytes > len(text) would
	// otherwise read a rejected request as a fragment of the line at offset.
	lineBytes = utf8x.LossyLen(s.line(offset))
	keep, valid, size := s.fitLines(offset, count, maxBytes, false)
	if keep == 0 {
		text, _ = fitHead(utf8x.Lossy(s.line(offset)), maxBytes)
		// As in tailLocked: truncated is what was dropped, not which arm ran.
		// count is clamped to the history above, so count > 1 means whole lines
		// went and len(text) < lineBytes means bytes of the line at offset did.
		// A one-rune line the bound gave way to, asked for alone, dropped
		// nothing.
		return text, 1, lineBytes, count > 1 || len(text) < lineBytes
	}
	// over cannot be true here: fitLines counted these keep lines as Lossy renders
	// them, the joining '\n' included, so the joined text is within maxBytes by
	// construction and fitHead has nothing to trim. It stays as the enforcement of
	// the bound rather than as an assumption about the measurement — if the two ever
	// disagree the bound still holds and truncated still says so, which is the
	// failure mode worth having. So truncated on this path means keep < count:
	// whole lines were dropped, and the text is whole lines.
	text, over := fitHead(s.join(offset, keep, valid, size), maxBytes)
	return text, keep, lineBytes, keep < count || over
}

// fitTail keeps at most maxBytes bytes of the end of text, starting at a rune
// boundary, and reports whether it dropped anything. text is valid UTF-8, as
// everything joinLocked and utf8x.Lossy return is.
//
// When the last rune alone is over maxBytes it is returned anyway, so the result
// is never empty for a text that is not: a read that can return nothing leaves a
// caller advancing by what it received unable to advance at all. The bound is
// exceeded by at most utf8.UTFMax-1 bytes, and only here.
//
// len(text) <= maxBytes holds unconditionally at maxBytes >= utf8.UTFMax. Below
// that the bound gives way to one whole rune: the overshoot arm needs a first rune
// longer than maxBytes, and no rune exceeds utf8.UTFMax, so it exists only at
// maxBytes of 1, 2 or 3.
func fitTail(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	i := len(text) - maxBytes
	for i < len(text) && !utf8.RuneStart(text[i]) {
		i++
	}
	if i == len(text) && len(text) > 0 {
		_, size := utf8.DecodeLastRuneInString(text)
		i = len(text) - size
	}
	return text[i:], true
}

// fitHead keeps at most maxBytes bytes of the start of text, ending at a rune
// boundary, and reports whether it dropped anything. It keeps the first rune
// whole even when that rune is over maxBytes, for the reason given on fitTail.
func fitHead(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	if i == 0 && len(text) > 0 {
		_, size := utf8.DecodeRuneInString(text)
		i = size
	}
	return text[:i], true
}
