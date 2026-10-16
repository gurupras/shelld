package vt

import "slices"

// maxCacheSize is the number of rows kept allocated beyond the live buffer
// (alacritty's MAX_CACHE_SIZE); history grows in chunks of this many rows.
const maxCacheSize = 1000

// storage is a ring buffer of rows. Line 0 is the top of the viewport,
// negative lines are history. Rotation is a modular update of zero, so
// scrolling never moves row data.
type storage struct {
	inner        []Row
	zero         int // ring index of the bottommost line
	visibleLines int
	len          int // scrollback + visible lines currently in use

	// Damage stamps: stamps[line] is the epoch at which visible
	// line was last reached through row or swap, allStamp the epoch of the
	// last change that may have moved or rewritten every line. Reaching a row
	// is taken as changing it, which over-reports reads and never misses a
	// write: every write to a visible row goes through row or swap.
	epoch    *uint64
	stamps   []uint64
	allStamp uint64
}

func (s *storage) stampAll() {
	if s.epoch == nil {
		return
	}
	s.allStamp = *s.epoch
	if len(s.stamps) != s.visibleLines {
		s.stamps = make([]uint64, s.visibleLines)
	}
}

// changedSince reports whether visible line may have changed after epoch e.
func (s *storage) changedSince(line int, e uint64) bool {
	if s.allStamp > e || uint(line) >= uint(len(s.stamps)) {
		return true
	}
	return s.stamps[line] > e
}

func newStorage(visibleLines, columns int) storage {
	s := storage{visibleLines: visibleLines, len: visibleLines}
	s.inner = allocRows(visibleLines, columns)
	return s
}

// allocRows allocates n full rows, each with cells of its own. Only the viewport
// is allocated this way; rows the ring grows by start empty (see initialize).
//
// A row must own its cells: no row's cells may live in an allocation another row also
// points into. That is the invariant, and it is not a style preference.
//
// One shared slab would be one allocation instead of n, and that is what this did.
// It leaks: a reflow keeps some of a chunk's rows and drops the rest, and a slice
// into a slab keeps the whole slab reachable however few of its rows survive. Since
// initialize allocates maxCacheSize rows to add one and replaceInner then discards
// the spare, a resize-heavy workload pins a fresh slab per resize. Alacritty's Row
// owns its own Vec<Cell>, so the port did not inherit this. Compaction keeps to
// it: a compacted row gets an array of its own or one an evicted row gave up,
// never a slice of a shared one.
func allocRows(n, columns int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i].inner = make([]Cell, columns)
	}
	return rows
}

func (s *storage) computeIndex(line int) int {
	zeroed := s.zero + (s.visibleLines - line - 1)
	if zeroed >= len(s.inner) {
		return zeroed - len(s.inner)
	}
	return zeroed
}

func (s *storage) row(line int) *Row {
	if uint(line) < uint(len(s.stamps)) {
		s.stamps[line] = *s.epoch
	}
	return &s.inner[s.computeIndex(line)]
}

// peek is row without the damage stamp, for readers outside the package: the
// exported Grid.Row and Grid.Cell, which nothing in this package writes through.
func (s *storage) peek(line int) *Row { return &s.inner[s.computeIndex(line)] }

func (s *storage) swap(a, b int) {
	if uint(a) < uint(len(s.stamps)) && uint(b) < uint(len(s.stamps)) {
		e := *s.epoch
		s.stamps[a], s.stamps[b] = e, e
	} else {
		s.stampAll()
	}
	ia, ib := s.computeIndex(a), s.computeIndex(b)
	s.inner[ia], s.inner[ib] = s.inner[ib], s.inner[ia]
}

func (s *storage) growVisibleLines(next int) {
	defer s.stampAll()
	s.initialize(next - s.visibleLines)
	s.visibleLines = next
}

func (s *storage) shrinkVisibleLines(next int) {
	defer s.stampAll()
	s.shrinkLines(s.visibleLines - next)
	s.visibleLines = next
}

func (s *storage) shrinkLines(n int) {
	defer s.stampAll()
	s.len -= n
	if len(s.inner) > s.len+maxCacheSize {
		s.truncate()
	}
}

func (s *storage) truncate() {
	defer s.stampAll()
	s.rezero()
	// The clear is what drops the trimmed rows, not the reslice: a reslice leaves their
	// Rows in [len, cap) with their cell pointers intact, so the cells would stay
	// reachable through an array nothing reads any more. Same class as the one
	// allocRows documents, on the other side of the ring.
	clear(s.inner[s.len:])
	s.inner = s.inner[:s.len]
}

// initialize brings additional rows into use, growing the ring by at least
// maxCacheSize rows when it has too few. The rows it adds store no cells: a row
// coming into the viewport is given its cells by whatever brings it there (the
// grid's blankRow, or the normalisation that ends a resize), and one that becomes
// history is compacted anyway, so allocating a full row here would pay for 1000
// rows a window may never use and that stay allocated as the ring's slack once
// the history is full.
func (s *storage) initialize(additional int) {
	defer s.stampAll()
	if s.len+additional > len(s.inner) {
		s.rezero()
		grow := max(additional, maxCacheSize)
		s.inner = append(s.inner, make([]Row, grow)...)
	}
	s.len += additional
}

// rotate moves every line count positions (negative = up into history).
func (s *storage) rotate(count int) {
	defer s.stampAll()
	n := len(s.inner)
	s.zero = ((s.zero+count)%n + n) % n
}

func (s *storage) rotateDown(count int) {
	defer s.stampAll()
	s.zero = (s.zero + count) % len(s.inner)
}

func (s *storage) replaceInner(rows []Row) {
	defer s.stampAll()
	s.len = len(rows)
	s.inner = rows
	s.zero = 0
}

func (s *storage) takeAll() []Row {
	defer s.stampAll()
	s.truncate()
	rows := s.inner
	s.inner = nil
	s.len = 0
	return rows
}

// rezero rotates the ring in place so zero is 0, by the three-reversal identity:
// reverse each of the two runs the split point leaves, then reverse the whole,
// and the two runs come back in the other order with their own order restored.
// That is a rotation in place with no scratch buffer and no modular index
// arithmetic on every element, which matters because a Row is 32 bytes and the
// ring can hold the whole of a large scrollback.
func (s *storage) rezero() {
	defer s.stampAll()
	if s.zero == 0 {
		return
	}
	slices.Reverse(s.inner[:s.zero])
	slices.Reverse(s.inner[s.zero:])
	slices.Reverse(s.inner)
	s.zero = 0
}
