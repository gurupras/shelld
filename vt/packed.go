package vt

import (
	"encoding/binary"
	"sync"
	"unsafe"
)

// History rows are stored packed, in the layout tmux stores every line in.
//
// A Cell is 20 bytes, and the lines a shell prints are almost all ASCII in the
// 16 ANSI or 256 indexed colours, which fit in 5. tmux (grid.c) keeps a line as
// an array of 5-byte grid_cell_entry — character, attributes, foreground and
// background a byte each, plus flags — and moves any cell that does not fit
// into a per-line array of full grid_extd_entry, the entry then holding that
// array's index. A packed row is the same thing in one allocation:
//
//	pk = n entries of packedEntry bytes, then e extended cells of packedExt bytes
//
// An entry is {ch, fg, bg, flags, meta}: ch is the stored character (Cell.ch,
// already XOR ' ') when it is below 256; flags is the low byte of Cell.flags;
// meta holds the fg class in bits 0-1, the bg class in bits 2-3 and flags bits
// 8-10 (Hidden, Strikeout, LeadingWideCharSpacer) in bits 4-6. A colour class is
//
//	0: the stored field is 0, the default colour
//	1: a NamedColor below 256, the byte is its number
//	2: an indexed colour, the byte is its index
//
// Anything else — a character from 256 up, an RGB colour, an underline colour,
// zero-width marks, the underline styles past the plain one — makes the cell
// extended: meta is packedExtBit and ch, fg hold the extended cell's index, u16
// little-endian. An extended cell is the Cell's six fields as they are stored,
// little-endian.
//
// Only history rows are packed: a scroll packs a line as it enters history (see
// compactPushed) and the rows a resize leaves in history are packed too. The
// viewport is never packed, so the parser's writes never meet a packed row.
// Every Row method that writes, and every one that reads inner directly,
// unpacks a packed row first or handles it explicitly (row.go).
//
// Where this differs from tmux: a row whose extended cells would make it at
// least as large as its plain cells — truecolor everywhere, or text that is
// mostly not Latin-1 — is stored plain (see packOrCompact), so no row is ever
// larger than it was before packing. tmux pays its extended entry regardless.
const (
	packedEntry  = 5
	packedExt    = 20
	packedExtBit = 0x80
	// packedMaxExt is the most extended cells one row can index.
	packedMaxExt = 1 << 16
	// packedFlags are the flags an entry stores: its own byte and meta's three bits.
	packedFlags = Flags(1<<11 - 1)
)

// colorClass is the class of a stored colour and the byte that goes with it, or
// ok false when the colour needs an extended cell. def is the colour's XOR key.
func colorClass(stored, def Color) (class, b byte, ok bool) {
	if stored == 0 {
		return 0, 0, true
	}
	c := stored ^ def
	switch c.Kind() {
	case KindNamed:
		if n := c.Named(); n < 256 && c == NewNamedColor(n) {
			return 1, byte(n), true
		}
	case KindIndexed:
		if c == NewIndexedColor(c.Index()) {
			return 2, c.Index(), true
		}
	}
	return 0, 0, false
}

func colorFromClass(class, b byte, def Color) Color {
	switch class {
	case 1:
		return NewNamedColor(NamedColor(b)) ^ def
	case 2:
		return NewIndexedColor(b) ^ def
	}
	return 0
}

// encodeInline writes c's entry into e and reports whether c fits in one.
func encodeInline(e []byte, c *Cell) bool {
	if uint32(c.ch) >= 256 || c.zw != 0 || !encodeAttrs(e[1:], c) {
		return false
	}
	e[0] = byte(c.ch)
	return true
}

// encodeAttrs writes the four bytes of an entry that follow the character —
// everything but ch and zw — into a, and reports whether c's attributes fit in
// them.
func encodeAttrs(a []byte, c *Cell) bool {
	if c.ul != 0 || c.flags&^packedFlags != 0 {
		return false
	}
	fc, fb, ok := colorClass(c.fg, defaultFg)
	if !ok {
		return false
	}
	bc, bb, ok := colorClass(c.bg, defaultBg)
	if !ok {
		return false
	}
	a[0], a[1], a[2] = fb, bb, byte(c.flags)
	a[3] = fc | bc<<2 | byte(c.flags>>8)<<4
	return true
}

// decodeEntry is the cell entry e holds; ext is the row's extended cells.
func decodeEntry(e, ext []byte) Cell {
	m := e[4]
	if m&packedExtBit != 0 {
		x := ext[packedExt*int(binary.LittleEndian.Uint16(e)):]
		x = x[:packedExt]
		return Cell{
			ch:    rune(binary.LittleEndian.Uint32(x)),
			fg:    Color(binary.LittleEndian.Uint32(x[4:])),
			bg:    Color(binary.LittleEndian.Uint32(x[8:])),
			ul:    Color(binary.LittleEndian.Uint32(x[12:])),
			flags: Flags(binary.LittleEndian.Uint16(x[16:])),
			zw:    binary.LittleEndian.Uint16(x[18:]),
		}
	}
	return Cell{
		ch:    rune(e[0]),
		fg:    colorFromClass(m&3, e[1], defaultFg),
		bg:    colorFromClass(m>>2&3, e[2], defaultBg),
		flags: Flags(e[3]) | Flags(m>>4&7)<<8,
	}
}

func encodeExt(x []byte, c *Cell) {
	binary.LittleEndian.PutUint32(x, uint32(c.ch))
	binary.LittleEndian.PutUint32(x[4:], uint32(c.fg))
	binary.LittleEndian.PutUint32(x[8:], uint32(c.bg))
	binary.LittleEndian.PutUint32(x[12:], uint32(c.ul))
	binary.LittleEndian.PutUint16(x[16:], uint16(c.flags))
	binary.LittleEndian.PutUint16(x[18:], c.zw)
}

// packScratch holds a row's encoding while packOrCompact finds out whether it
// is worth keeping: ent its entries, and x the columns of its extended cells,
// which are written only once it is.
type packScratch struct {
	ent []byte
	x   []int32
}

// spareScratch serves the callers that pack without a pool: a resize's settle
// and the rewrite of a row above the screen.
// maxScratch is the bytes a packScratch keeps between rows: a row of 2048
// cells, each with an entry and a column.
const maxScratch = 2048 * (packedEntry + 4)

var spareScratch = sync.Pool{New: func() any { return new(packScratch) }}

// encode writes cells' entries into s and returns the bytes they take packed,
// and whether packing is worth it: false when the row needs more extended
// cells than an entry can index (ext is then packedMaxExt+1), or when packed it
// would take at least as many cells' worth of array as it takes plain. It stops
// at the first extended cell that settles either.
//
// A line's cells come in runs of one set of attributes, so whether they fit
// and the bytes they take are worked out once per run.
func (s *packScratch) encode(cells []Cell) (size, ext int, ok bool) {
	n := len(cells)
	if n == 0 {
		return 0, 0, false
	}
	if cap(s.ent) < packedEntry*n {
		s.ent = make([]byte, packedEntry*n)
	}
	ent := s.ent[:packedEntry*n]
	s.x = s.x[:0]
	// maxExt is the most extended cells the row can hold before this one and
	// still have room for it: the index limit, and the e for which
	// packedCells(packedEntry*n+packedExt*(e+1)) < n, which is
	// packedExt*e <= (packedExt-packedEntry)*n - 2*packedExt.
	maxExt := packedMaxExt - 1
	if room := (packedExt-packedEntry)*n - 2*packedExt; room < 0 {
		maxExt = -1
	} else {
		maxExt = min(maxExt, room/packedExt)
	}
	var a [packedEntry - 1]byte
	// A run is a stretch of cells whose bytes past ch — fg, bg, ul, flags and
	// zw — are equal. Its attributes are encoded once, at its first cell, and
	// the loops below walk the run comparing each cell's tail with words held
	// in registers. The inline loop stores only characters; it stops at the
	// run's end or at a character of 256 or more, which takes an extended cell
	// without ending the run. A run whose attributes do not fit an entry is
	// extended cells throughout.
	for i := 0; i < n; {
		k0, k1 := tailWords(&cells[i])
		misfit, attrs := runAttrs(&a, &cells[i])
		if misfit == 0 {
			for {
				for ; i < n; i++ {
					c := &cells[i]
					t0, t1 := tailWords(c)
					if t0 != k0 || t1 != k1 || uint32(c.ch) >= 256 {
						break
					}
					e := ent[packedEntry*i:]
					e = e[:packedEntry]
					e[0] = byte(c.ch)
					binary.LittleEndian.PutUint32(e[1:], attrs)
				}
				if i == n {
					break
				}
				if t0, t1 := tailWords(&cells[i]); t0 != k0 || t1 != k1 {
					break
				}
				if !s.extAt(ent, i, &ext, maxExt) {
					return 0, ext + 1, false
				}
				i++
			}
			continue
		}
		for ; i < n; i++ {
			if t0, t1 := tailWords(&cells[i]); t0 != k0 || t1 != k1 {
				break
			}
			if !s.extAt(ent, i, &ext, maxExt) {
				return 0, ext + 1, false
			}
		}
	}
	size = packedEntry*n + packedExt*ext
	return size, ext, packedCells(size) < n
}

// extAt makes entry i of ent refer to extended cell *ext, unless the row already
// holds maxExt+1 of them, and records the column for copyTo.
func (s *packScratch) extAt(ent []byte, i int, ext *int, maxExt int) bool {
	if *ext > maxExt {
		return false
	}
	e := ent[packedEntry*i:]
	e = e[:packedEntry]
	binary.LittleEndian.PutUint16(e, uint16(*ext))
	e[2], e[3], e[4] = 0, 0, packedExtBit
	s.x = append(s.x, int32(i))
	*ext++
	return true
}

// runAttrs encodes c's attributes into a and returns them as a word, with
// misfit 1 when a cell of them does not fit an entry whatever its character —
// they do not fit, or c has zero-width marks — and 0 otherwise.
func runAttrs(a *[packedEntry - 1]byte, c *Cell) (misfit, attrs uint32) {
	if c.zw != 0 || !encodeAttrs(a[:], c) {
		misfit = 1
	}
	return misfit, binary.LittleEndian.Uint32(a[:])
}

// tailWords is the 16 bytes of c past ch — fg, bg, ul, flags and zw — as two
// words. The words are unaligned; checkptr allows that for a pointer-free type.
func tailWords(c *Cell) (uint64, uint64) {
	w := (*[2]uint64)(unsafe.Pointer(&c.fg))
	return w[0], w[1]
}

// tailWords' view holds if fg starts the 16 bytes that end the Cell.
var _ = [1]struct{}{}[unsafe.Offsetof(Cell{}.fg)+16-unsafe.Sizeof(Cell{})]

// copyTo writes the row encode was given, cells, into pk, which is the size
// encode returned.
func (s *packScratch) copyTo(pk []byte, cells []Cell) {
	n := copy(pk, s.ent[:packedEntry*len(cells)])
	for j, i := range s.x {
		encodeExt(pk[n+packedExt*j:], &cells[i])
	}
}

// packedCells is the number of cells whose array holds size packed bytes.
func packedCells(size int) int { return (size + packedExt - 1) / packedExt }

// pk is a packed row's bytes: its array, past the 0 length, viewed as bytes.
// Cell holds no pointers, so the garbage collector does not scan the array and
// any bytes are a valid Cell; the view stays inside the capacity.
func (r *Row) pk() []byte {
	c := r.inner[:cap(r.inner)]
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(c))), len(c)*int(unsafe.Sizeof(Cell{})))
}

// full reports whether the row is a plain row of at least columns cells, which
// is what a viewport row has to be. It is the shape test every site that takes a
// ring slot for the viewport makes: a packed row can store as many cells as the
// grid has columns — a wrapped line, a line ending in coloured blanks — so its
// Len says nothing about whether its cells can be written.
func (r *Row) full(columns int) bool { return !r.packed && len(r.inner) >= columns }

// CellAt returns the cell at col by value, a default cell past what the row
// stores. Unlike Cell it allocates nothing on a packed row.
func (r *Row) CellAt(col int) Cell {
	if !r.packed {
		if col >= len(r.inner) {
			return Cell{}
		}
		return r.inner[col]
	}
	if col >= int(r.occ) {
		return Cell{}
	}
	pk := r.pk()
	return decodeEntry(pk[packedEntry*col:], pk[packedEntry*int(r.occ):])
}

// View returns the row's stored cells for reading: inner itself for a plain row,
// and for a packed one the cells decoded into *scratch, which it grows as it
// needs. The slice is valid until the row or *scratch next changes.
func (r *Row) View(scratch *[]Cell) []Cell {
	if !r.packed {
		return r.inner[:len(r.inner):len(r.inner)]
	}
	n := int(r.occ)
	if cap(*scratch) < n {
		*scratch = make([]Cell, n)
	}
	cells := (*scratch)[:n]
	r.decodeTo(cells)
	return cells
}

// decodeTo decodes the row's cells into cells. Like encode it works in runs:
// an entry whose attribute bytes match the previous inline entry's is that cell
// with its own character.
func (r *Row) decodeTo(cells []Cell) { r.decodeRange(cells, 0) }

// decodeRange decodes the row's cells from column from on into cells.
func (r *Row) decodeRange(cells []Cell, from int) {
	pk := r.pk()
	ext := pk[packedEntry*int(r.occ):]
	pk = pk[packedEntry*from : packedEntry*(from+len(cells))]
	var run Cell
	key := uint32(packedExtBit) << 24 // meta with packedExtBit: no inline entry's
	for i := range cells {
		e := pk[packedEntry*i : packedEntry*i+packedEntry]
		if e[4]&packedExtBit != 0 {
			cells[i] = decodeEntry(e, ext)
			continue
		}
		if w := binary.LittleEndian.Uint32(e[1:]); w != key {
			key, run = w, decodeEntry(e, ext)
		}
		// run is copied whole and the character stored on its own: storing
		// it into run first would make every copy wait on that store.
		cells[i] = run
		cells[i].ch = rune(e[0])
	}
}

// unpack turns a packed row back into a plain one of the cells it stores, with
// occ at their number, which is the row compact makes of the same cells.
func (r *Row) unpack() { r.unpackWith(nil) }

// unpackWith is unpack onto buf's array when it holds the row's cells — buf is
// then zeroed past its length — and returns the packed array the row gave up,
// nil when it was not packed.
func (r *Row) unpackWith(buf []Cell) []Cell {
	if !r.packed {
		return nil
	}
	if packHook != nil {
		packHook.Unpacks++
	}
	n := int(r.occ)
	var cells []Cell
	if cap(buf) >= n {
		cells = buf[:n]
	} else {
		cells = make([]Cell, n)
	}
	r.decodeTo(cells)
	old := r.inner
	*r = Row{inner: cells, occ: int32(n)}
	return old
}

// packOrCompact stores a history row in the least memory it can: packed when
// that is smaller, and otherwise compacted plain as compact does. pool, when not
// nil, supplies the array the row moves onto and takes back one it does not use.
// It returns the cell array the row gave up, nil when it kept its own.
//
// A packed row is never written, so it carries no occ of its own to keep: occ
// is its number of cells, and dirty is false.
func (r *Row) packOrCompact(pool *rowPool, columns int, noPack bool) []Cell {
	if r.packed {
		return nil
	}
	r.kept = false
	n := r.trimmedLen()
	content := r.inner[:n]
	var size, ext int
	ok := false
	var sc *packScratch
	if pool != nil {
		sc = &pool.scratch
	} else {
		sc = spareScratch.Get().(*packScratch)
		defer spareScratch.Put(sc)
	}
	if !noPack {
		size, ext, ok = sc.encode(content)
		// A grid keeps its scratch, so one very long line is not allowed to
		// leave it holding that line's size for good.
		defer func() {
			if cap(sc.ent)+4*cap(sc.x) > maxScratch {
				*sc = packScratch{}
			}
		}()
	}
	if !ok {
		if packHook != nil && n > 0 && !noPack {
			if ext > packedMaxExt {
				packHook.PlainIndex++
			} else {
				packHook.Plain++
			}
		}
		var buf []Cell
		if pool != nil {
			buf = pool.takeShort(n)
		}
		old := r.compact(buf)
		if old == nil && buf != nil {
			pool.put(Row{inner: buf}, columns)
		}
		r.kept = !noPack
		return old
	}
	k := packedCells(size)
	var arr []Cell
	if pool != nil {
		arr = pool.takeShort(k)
	}
	if packHook != nil {
		packHook.Packed++
		packHook.Ext += ext
		packHook.MaxExt = max(packHook.MaxExt, ext)
		if arr != nil {
			packHook.PoolTake++
		}
	}
	if arr == nil {
		arr = make([]Cell, k)
	}
	old := r.inner
	*r = Row{inner: arr[:0:k], occ: int32(n), packed: true}
	sc.copyTo(r.pk()[:size], content)
	return old
}

// settle stores a row a resize leaves in history as packOrCompact would, unless
// it is packed already or packOrCompact has kept it plain since it last changed.
// It returns the plain array the row gave up, which nothing else references.
func (r *Row) settle(pool *rowPool, columns int, noPack bool) []Cell {
	if !r.packed && !r.kept {
		if packHook != nil {
			packHook.Settles++
		}
		return r.packOrCompact(pool, columns, noPack)
	}
	return nil
}

// PackStats counts what packing did, for tests that have to show packing
// happened before what they compare can mean anything: a differential that
// compares a packed grid with a plain one passes trivially if nothing packed.
// It is a test-only diagnostic, in the shape of fillBlankCopies: SetPackStats
// installs one, and production never does.
type PackStats struct {
	// Packed rows stored packed, Plain rows kept plain because packed they would
	// not be smaller, PlainIndex rows kept plain because they need more extended
	// cells than an entry indexes. Ext counts the extended cells packed rows hold.
	Packed, Plain, PlainIndex, Ext int
	// MaxExt is the most extended cells one packed row held: past 256, entries
	// held indexes a byte cannot.
	MaxExt int
	// Unpacks counts packed rows turned back into plain ones.
	Unpacks int
	// The lifecycle a packed row goes through. Replaced counts packed ring slots
	// a scroll or erase replaced with a full row rather than writing; PoolPut
	// packed arrays offered to the scroll pool, PoolTake arrays a row was packed
	// into that came from it; Settles rows a resize stored for history, Rewrites
	// packed rows a wide character's write above the cursor unpacked, and
	// WrapGrows wrapped history rows a widening grew to the new width.
	Replaced, PoolPut, PoolTake, Settles, Rewrites, WrapGrows int
}

// countPack adds one to the counter f selects, when counters are installed.
func countPack(f func(*PackStats) *int) {
	if packHook != nil {
		*f(packHook)++
	}
}

var packHook *PackStats

// SetPackStats installs s as the counters packing updates, nil to remove them,
// and returns the ones it replaces. Test-only, like CheckOcc.
func SetPackStats(s *PackStats) *PackStats {
	old := packHook
	packHook = s
	return old
}

// SetNoPack stops the grid packing history rows, so that a test can run the same
// bytes through a grid that packs and one that does not and compare them. It is
// a field of the grid rather than a package setting so that two grids in one
// process can differ. Test-only.
func (g *Grid) SetNoPack(off bool) { g.noPack = off }
