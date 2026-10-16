package vt

import "math/bits"

// rowPool keeps a few of the arrays a scroll frees, so that the next scroll
// reuses them instead of allocating.
//
// Each line a scroll pushes into history trades arrays with the row that takes
// its place at the bottom of the screen: the pushed line moves onto an array
// the size of its content, and the incoming row, the oldest history line the
// scroll evicted, needs a full one. compactPushed pairs the two directly when it
// can. It cannot when either row is the other shape, and lines of every length
// pass through a full history in no particular order: a wrapped line fills its
// row and is pushed whole, the line it evicts can be short, and the next short
// line pushed can evict a full one. Unpooled, every such mismatch freed one
// array and allocated another, which a corpus of long lines measured at five
// times the bytes of the uncompacted grid. The pool bridges the mismatches
// across lines, and its bounds keep it to a few rows' worth of cells.
type rowPool struct {
	// full holds rows a full row wide, with the occ and dirty their cells
	// still need for the reset that reuses them.
	full []Row
	// short holds smaller arrays by size class: compacted rows' and packed
	// rows' alike, which store their bytes in a []Cell (packed.go).
	short sizeClasses[Cell]
	// scratch is where packOrCompact encodes a row (packed.go).
	scratch packScratch
}

// sizeClasses holds arrays by size class, and total counts the elements they
// hold.
type sizeClasses[T any] struct {
	c     [][][]T
	total int
}

const (
	// poolFull bounds full between scrolls; within one, a scroll of more lines
	// can hold more until trim.
	poolFull = 4
	// poolPerClass bounds each size class, and poolShortRows the cells all of
	// them hold, as a number of full rows.
	poolPerClass  = 8
	poolShortRows = 32
	// poolFitDivisor bounds the slack a reused array may bring, as a fraction
	// of the cells it is taken for: compact itself accepts twice the cells, but
	// a pool hands out arrays all day, and on a corpus of long lines a
	// reuse at up to twice the size held a tenth more history than one at an
	// eighth over.
	poolFitDivisor = 8
	// poolClassBits subdivides each power of two into 1<<poolClassBits classes,
	// so arrays in one class differ by less than a quarter of their size.
	poolClassBits = 2
)

// poolClass is the size class of an array of n > 0 cells.
func poolClass(n int) int {
	b := bits.Len(uint(n))
	if b <= poolClassBits+1 {
		return n
	}
	return b<<poolClassBits | (n>>(b-1-poolClassBits))&(1<<poolClassBits-1)
}

// put offers r's array, which no row references any more, for reuse.
func (p *rowPool) put(r Row, columns int) {
	// A packed row's array is a []Cell like any other (packed.go), and
	// never as many cells as the row has, so it joins the short classes.
	switch c := cap(r.inner); {
	case c == 0:
	case len(r.inner) == columns:
		p.full = append(p.full, r)
	case c < columns:
		if r.packed {
			countPack(func(s *PackStats) *int { return &s.PoolPut })
		}
		p.short.put(r.inner[:0:c], columns)
	}
}

// put keeps a, an array of less than max elements, unless the classes already
// hold poolShortRows times max.
func (s *sizeClasses[T]) put(a []T, max int) {
	c := cap(a)
	if c == 0 || s.total+c > poolShortRows*max {
		return
	}
	k := poolClass(c)
	if k >= len(s.c) {
		s.c = append(s.c, make([][][]T, k+1-len(s.c))...)
	}
	if len(s.c[k]) < poolPerClass {
		s.c[k] = append(s.c[k], a[:0:c])
		s.total += c
	}
}

// takeFull returns a full row from the pool, or a zero Row when it has none.
func (p *rowPool) takeFull() Row {
	if len(p.full) == 0 {
		return Row{}
	}
	r := p.full[len(p.full)-1]
	p.full[len(p.full)-1] = Row{}
	p.full = p.full[:len(p.full)-1]
	return r
}

// takeShort returns an array of n to n+n/poolFitDivisor cells, which compact
// reuses for n, or nil when the pool has none.
func (p *rowPool) takeShort(n int) []Cell { return p.short.take(n) }

func (s *sizeClasses[T]) take(n int) []T {
	if n == 0 {
		return nil
	}
	for k := poolClass(n); k < len(s.c) && k <= poolClass(n)+1; k++ {
		a := s.c[k]
		for i := len(a) - 1; i >= 0; i-- {
			if c := cap(a[i]); c >= n && c <= n+n/poolFitDivisor {
				got := a[i]
				a[i] = a[len(a)-1]
				a[len(a)-1] = nil
				s.c[k] = a[:len(a)-1]
				s.total -= c
				return got
			}
		}
	}
	return nil
}

// trim drops full rows past poolFull.
func (p *rowPool) trim() {
	for len(p.full) > poolFull {
		p.full[len(p.full)-1] = Row{}
		p.full = p.full[:len(p.full)-1]
	}
}

// reset empties the pool, as a resize does: its arrays are sized for the old
// width.
func (p *rowPool) reset() { *p = rowPool{} }
