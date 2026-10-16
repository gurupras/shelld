package vt

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"
)

// Packing must not cost the rows it does not pack anything: a row that stays
// plain is the row 4661351 stored, header included.
func TestRowSizeUnchanged(t *testing.T) {
	if n := unsafe.Sizeof(Row{}); n != 32 {
		t.Fatalf("Row is %d bytes, want 32", n)
	}
}

// cellOf is a cell as the parser writes it; NoColor stands for the default
// colour, which the parser stores as 0.
func cellOf(r rune, fg, bg Color, f Flags) Cell {
	var c Cell
	c.SetRune(r)
	if fg != NoColor {
		c.SetFg(fg)
	}
	if bg != NoColor {
		c.SetBg(bg)
	}
	c.SetFlags(f)
	return c
}

// The cells a shell prints have to stay inline, or packing silently stores
// them extended at 25 bytes: correct, and five times the memory the change is
// for. Nothing else checks this, since an extended cell decodes correctly.
func TestCommonCellsStayInline(t *testing.T) {
	var cells []Cell
	colors := []Color{NoColor}
	for n := Black; n <= BrightWhite; n++ {
		colors = append(colors, NewNamedColor(n))
	}
	for i := range 256 {
		colors = append(colors, NewIndexedColor(uint8(i)), NewNamedColor(NamedColor(i)))
	}
	for _, fg := range colors {
		cells = append(cells, cellOf('x', fg, NoColor, 0))
	}
	for _, bg := range colors {
		cells = append(cells, cellOf('x', NoColor, bg, 0))
	}
	// The default colours by role: SGR 39/49 store them as 0, OSC-less code
	// paths as the role itself.
	cells = append(cells, cellOf('x', NewNamedColor(Foreground), NewNamedColor(Background), 0))
	for r := rune(0); r < 256; r++ {
		cells = append(cells, cellOf(r, NoColor, NoColor, 0))
	}
	for b := range 11 {
		cells = append(cells, cellOf('x', NewNamedColor(Red), NewIndexedColor(4), Flags(1)<<b))
	}
	cells = append(cells, cellOf('x', NewNamedColor(Red), NewIndexedColor(4), packedFlags), Cell{})
	var e [packedEntry]byte
	for i := range cells {
		if !encodeInline(e[:], &cells[i]) {
			t.Errorf("cell %d (%q fg %v bg %v flags %v) is extended", i, cells[i].Rune(), cells[i].Fg(), cells[i].Bg(), cells[i].Flags())
		}
	}
	// What does not fit, and must not be squeezed in.
	for _, c := range []Cell{
		cellOf('中', NoColor, NoColor, 0),
		cellOf('x', NewRGBColor(1, 2, 3), NoColor, 0),
		cellOf('x', NoColor, NewRGBColor(0, 0, 0), 0),
		cellOf('x', NewNamedColor(DimRed), NoColor, 0),
		cellOf('x', NoColor, NoColor, FlagDoubleUnderline),
		{ul: NewRGBColor(9, 9, 9)},
		{zw: 1},
	} {
		if encodeInline(e[:], &c) {
			t.Errorf("%+v encodes inline", c)
		}
	}
}

// randCell is any cell the fields can hold, weighted towards the boundaries
// of every class an entry distinguishes.
func randCell(rng *rand.Rand) Cell {
	color := func(def Color) Color {
		switch rng.IntN(9) {
		case 0:
			return 0 // stored default
		case 1:
			return NewNamedColor(NamedColor(rng.IntN(16))) ^ def
		case 2:
			return NewNamedColor(NamedColor(rng.IntN(512))) ^ def
		case 3:
			return NewNamedColor(NamedColor(rng.Uint32())) ^ def
		case 4:
			return NewIndexedColor(uint8(rng.IntN(256))) ^ def
		case 5:
			return NewRGBColor(uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256))) ^ def
		case 6:
			return Color(rng.Uint32()) // anything the field holds
		case 7:
			return def // stored value of colour 0 of that kind
		}
		return NewNamedColor(Foreground) ^ def
	}
	var c Cell
	switch rng.IntN(6) {
	case 0:
		c.ch = rune(rng.IntN(256))
	case 1:
		c.ch = rune(256 + rng.IntN(2))
	case 2:
		c.ch = rune(rng.Uint32())
	default:
		c.SetRune(rune(' ' + rng.IntN(95)))
	}
	c.fg, c.bg = color(defaultFg), color(defaultBg)
	switch rng.IntN(4) {
	case 0:
		c.flags = Flags(rng.Uint32())
	case 1:
		c.flags = Flags(rng.IntN(1 << 11))
	}
	if rng.IntN(6) == 0 {
		c.ul = Color(rng.Uint32())
	}
	if rng.IntN(6) == 0 {
		c.zw = uint16(rng.Uint32())
	}
	return c
}

// checkPackedRow packs a row of cells and checks that every reader returns
// what the row compact makes of the same cells returns: Len, CellAt, Cell,
// Cells, View, isClear, and the row unpack gives back.
func checkPackedRow(t *testing.T, cells []Cell, columns int) *Row {
	t.Helper()
	want := Row{inner: append([]Cell(nil), cells...), occ: int32(len(cells))}
	want.compact(nil)
	r := Row{inner: append([]Cell(nil), cells...), occ: int32(len(cells))}
	r.packOrCompact(nil, columns, false)
	if r.Len() != want.Len() {
		t.Fatalf("packed=%v Len %d, compact keeps %d", r.packed, r.Len(), want.Len())
	}
	var scratch []Cell
	view := r.View(&scratch)
	all := r.Cells()
	if len(view) != want.Len() || len(all) != want.Len() {
		t.Fatalf("View %d, Cells %d cells, want %d", len(view), len(all), want.Len())
	}
	for c := range want.Len() + 3 {
		w := want.CellAt(c)
		if got := r.CellAt(c); got != w {
			t.Fatalf("packed=%v CellAt(%d) = %+v, want %+v", r.packed, c, got, w)
		}
		if got := *r.Cell(c); got != w {
			t.Fatalf("Cell(%d) = %+v, want %+v", c, got, w)
		}
		if c < want.Len() && (view[c] != w || all[c] != w) {
			t.Fatalf("cell %d: View %+v Cells %+v, want %+v", c, view[c], all[c], w)
		}
	}
	if c := want.Len(); c > 0 && r.CellAt(c+5) != (Cell{}) {
		t.Fatalf("CellAt past %d is not the default cell", c)
	}
	if r.isClear() != want.isClear() {
		t.Fatalf("isClear %v, compact row says %v", r.isClear(), want.isClear())
	}
	u := r
	u.unpack()
	if u.packed || u.Len() != want.Len() || int(u.occ) != want.Len() || u.dirty {
		t.Fatalf("unpack gives packed=%v len %d occ %d dirty %v, want %d cells at occ %d", u.packed, u.Len(), u.occ, u.dirty, want.Len(), want.Len())
	}
	for c := range want.Len() {
		if u.inner[c] != want.inner[c] {
			t.Fatalf("unpacked cell %d = %+v, want %+v", c, u.inner[c], want.inner[c])
		}
	}
	return &r
}

func TestPackRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 3000 {
		n := rng.IntN(300)
		cells := make([]Cell, n)
		mode := rng.IntN(3)
		for c := range cells {
			switch {
			case mode == 0 || rng.IntN(4) == 0:
				cells[c] = randCell(rng)
			default:
				cells[c] = cellOf(rune('a'+rng.IntN(26)), NewNamedColor(NamedColor(rng.IntN(16))), NewIndexedColor(uint8(rng.IntN(256))), Flags(rng.IntN(1<<11)))
			}
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) { checkPackedRow(t, cells, max(n, 1)) })
	}
	// A row of blanks that are empty without being default cells is kept by
	// trimming and is clear; random cells almost never make one.
	for _, f := range []Flags{FlagBold, FlagDim, FlagItalic} {
		cells := make([]Cell, 40)
		for c := range cells {
			cells[c] = cellOf(' ', NoColor, NoColor, f)
		}
		cells[7].SetRune('\t')
		if r := checkPackedRow(t, cells, 40); !r.packed || !r.isClear() {
			t.Fatalf("blanks with %v: packed %v, isClear %v; want a clear packed row", f, r.packed, r.isClear())
		}
	}
	// Neighbouring entries whose fg, bg and flags bytes are equal and whose meta
	// is not: a decoder that reused the cell before for them would be wrong.
	for _, pair := range [][2]Cell{
		{cellOf('a', NewNamedColor(1), NoColor, 0), cellOf('b', NewIndexedColor(1), NoColor, 0)},
		{cellOf('a', NoColor, NoColor, 0), cellOf('b', NewNamedColor(0), NoColor, 0)},
		{cellOf('a', NoColor, NewNamedColor(2), 0), cellOf('b', NoColor, NewIndexedColor(2), 0)},
		{cellOf('a', NoColor, NoColor, FlagBold), cellOf('b', NoColor, NoColor, FlagBold|1<<8)},
	} {
		cells := make([]Cell, 40)
		for c := range cells {
			cells[c] = pair[c/3%2]
		}
		if r := checkPackedRow(t, cells, 40); !r.packed {
			t.Fatalf("%+v: not packed", pair)
		}
	}
}

// A packed row longer than 256 extended cells indexes them with both bytes of
// its entry: one byte would read cell 256 as cell 0.
func TestPackRoundTripLong(t *testing.T) {
	var s PackStats
	defer SetPackStats(SetPackStats(&s))
	rng := rand.New(rand.NewPCG(3, 4))
	cells := make([]Cell, 1000)
	for c := range cells {
		cells[c] = cellOf(rune('a'+c%26), NoColor, NoColor, 0)
		if c%5 < 2 {
			cells[c].SetFg(NewRGBColor(uint8(c), uint8(c>>8), uint8(rng.IntN(256))))
		}
	}
	if r := checkPackedRow(t, cells, len(cells)); !r.packed {
		t.Fatal("a row with 40% extended cells was stored plain")
	}
	if s.MaxExt < 256 {
		t.Fatalf("largest row held %d extended cells, want past 256", s.MaxExt)
	}
}

// An entry indexes at most packedMaxExt extended cells; a row that needs more
// is stored plain, and one that needs exactly that many is packed.
// TestPackOnlyWhenSmaller checks the rule for rows with no extended cells,
// which the encoder never stops early on: a row is packed only when its packed
// array is smaller than its plain one, so a single cell stays plain.
func TestPackOnlyWhenSmaller(t *testing.T) {
	for n := 1; n <= 8; n++ {
		r := Row{inner: make([]Cell, n)}
		for i := range r.inner {
			r.inner[i].SetRune('a')
		}
		r.occ = int32(n)
		r.packOrCompact(nil, n, false)
		if want := packedCells(packedEntry*n) < n; r.packed != want {
			t.Errorf("%d cells: packed %v, want %v", n, r.packed, want)
		}
	}
}

func TestPackExtIndexLimit(t *testing.T) {
	var s PackStats
	defer SetPackStats(SetPackStats(&s))
	const n = 90000
	for _, ext := range []int{packedMaxExt, packedMaxExt + 1} {
		cells := make([]Cell, n)
		for c := range cells {
			cells[c] = cellOf('a', NoColor, NoColor, 0)
			if c < ext {
				cells[c].SetFg(NewRGBColor(uint8(c), uint8(c>>8), uint8(c>>16)))
			}
		}
		r := checkPackedRow(t, cells, n)
		if want := ext <= packedMaxExt; r.packed != want {
			t.Fatalf("%d extended cells: packed %v, want %v", ext, r.packed, want)
		}
	}
	if s.PlainIndex != 1 {
		t.Fatalf("PlainIndex %d, want 1", s.PlainIndex)
	}
}

func FuzzPackRow(f *testing.F) {
	f.Add([]byte("hello"), uint16(5))
	f.Add(make([]byte, 400), uint16(80))
	f.Fuzz(func(t *testing.T, b []byte, columns uint16) {
		cells := make([]Cell, len(b)/20)
		for i := range cells {
			x := b[20*i:]
			cells[i] = Cell{
				ch:    rune(binary.LittleEndian.Uint32(x)),
				fg:    Color(binary.LittleEndian.Uint32(x[4:])),
				bg:    Color(binary.LittleEndian.Uint32(x[8:])),
				ul:    Color(binary.LittleEndian.Uint32(x[12:])),
				flags: Flags(binary.LittleEndian.Uint16(x[16:])),
				zw:    binary.LittleEndian.Uint16(x[18:]),
			}
		}
		checkPackedRow(t, cells, max(len(cells), int(columns)))
	})
}

// Any inline entry decodes to a cell that encodes back to the same entry, so
// the two directions agree on every byte an entry can hold.
func FuzzDecodeEntry(f *testing.F) {
	f.Add([]byte{'a' ^ ' ', 1, 4, 0x10, 1 | 2<<2 | 5<<4})
	f.Fuzz(func(t *testing.T, e []byte) {
		if len(e) < packedEntry {
			return
		}
		e = e[:packedEntry]
		m := e[4]
		if m&packedExtBit != 0 || m&3 == 3 || m>>2&3 == 3 {
			return // extended, or a class no encoder writes
		}
		c := decodeEntry(e, nil)
		var back [packedEntry]byte
		if !encodeInline(back[:], &c) {
			t.Fatalf("%x decodes to %+v, which is not inline", e, c)
		}
		// A class-0 colour stores no byte, and its byte in e is not read.
		want := append([]byte(nil), e...)
		if m&3 == 0 {
			want[1] = 0
		}
		if m>>2&3 == 0 {
			want[2] = 0
		}
		if string(back[:]) != string(want) {
			t.Fatalf("%x decodes to %+v, which encodes to %x", e, c, back)
		}
		if again := decodeEntry(back[:], nil); again != c {
			t.Fatalf("%x: second decode %+v, first %+v", e, again, c)
		}
	})
}

// historyCells is the cell capacity a grid's history rows hold.
func historyCells(g *Grid) int {
	n := 0
	for l := -g.HistorySize(); l < 0; l++ {
		n += cap(g.Row(l).inner)
	}
	return n
}

// Packing exists to make history smaller and must never make it larger: a
// truecolor history, which packs worse than plain storage, is stored plain.
func TestPackedHistoryNoLarger(t *testing.T) {
	for _, tc := range []struct {
		name string
		line func(i int) string
		max  float64 // packed history cells over plain ones
	}{
		{"named", func(i int) string { return fmt.Sprintf("\x1b[3%dm%s\x1b[0m", i%8, strings.Repeat("word ", 10)) }, 0.35},
		{"truecolor", func(i int) string {
			var b strings.Builder
			for c := range 50 {
				fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dmx", i%256, c, 7)
			}
			return b.String()
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := func(noPack bool) int {
				term, p := NewTerm(Config{ScrollingHistory: 2000}, 200, 24, nil), NewProcessor()
				term.Grid().SetNoPack(noPack)
				for i := range 2500 {
					feed(term, p, tc.line(i)+"\r\n")
				}
				return historyCells(term.Grid())
			}
			packed, plain := size(false), size(true)
			t.Logf("history holds %d cells packed, %d plain", packed, plain)
			if float64(packed) > tc.max*float64(plain) {
				t.Fatalf("packed history holds %d cells, larger than %.2f of plain's %d", packed, tc.max, plain)
			}
		})
	}
}

// A grid keeps its pack scratch between rows, but not one sized by a single
// very long line: after a line of 4000 cells reaches history, the scratch
// holds no more than maxScratch.
func TestPackScratchReleasedAfterLongRow(t *testing.T) {
	term, p := NewTerm(Config{ScrollingHistory: 100}, 4000, 2, nil), NewProcessor()
	feed(term, p, "\x1b[31m"+strings.Repeat("x", 4000)+"\x1b[0m\r\n\r\n\r\n")
	if term.Grid().HistorySize() == 0 {
		t.Fatal("the long line never reached history")
	}
	sc := &term.Grid().spare.scratch
	if got := cap(sc.ent) + 4*cap(sc.x); got > maxScratch {
		t.Fatalf("scratch keeps %d bytes after a long row, want at most %d", got, maxScratch)
	}
}

// C1: a packed row can store a full row's cells (a wrapped line, a line erased
// to a colour), and when a full history evicts it into the viewport it has to
// be replaced, not reset in place: reset sees no cells in a packed row and
// would leave the oldest history line on the screen.
func TestEvictedPackedRowLeavesNoGhost(t *testing.T) {
	var s PackStats
	defer SetPackStats(SetPackStats(&s))
	term, p := NewTerm(Config{ScrollingHistory: 3}, 10, 4, nil), NewProcessor()
	for i := range 12 {
		if i%2 == 0 {
			feed(term, p, strings.Repeat(string(rune('A'+i)), 15)+"\r\n") // wraps: a full first row
		} else {
			feed(term, p, "\x1b[41m"+string(rune('A'+i))+"\x1b[K\x1b[0m\r\n") // erased to red: full width
		}
	}
	// The ring keeps maxCacheSize slots of slack past the history, so eviction
	// reaches packed rows only after that many more lines.
	for i := range maxCacheSize + 50 {
		if i%2 == 0 {
			feed(term, p, strings.Repeat("W", 15)+"\r\n")
		} else {
			feed(term, p, "\x1b[42mE\x1b[K\x1b[0m\r\n")
		}
	}
	feed(term, p, strings.Repeat("\n", 8))
	g := term.Grid()
	for l := 0; l < g.ScreenLines(); l++ {
		r := g.Row(l)
		for c := range g.Columns() {
			if cell := r.CellAt(c); cell != (Cell{}) {
				t.Fatalf("viewport line %d column %d holds a ghost %+v after only line feeds", l, c, cell)
			}
		}
	}
	if s.Packed == 0 || s.Replaced == 0 {
		t.Fatalf("counters %+v: the test needs packed rows replaced", s)
	}
}

// term.go:288: writing over a wide character that wrapped clears the flag the
// row above keeps for it, and that row can be packed history.
func TestPackedRewriteAbove(t *testing.T) {
	var s PackStats
	defer SetPackStats(SetPackStats(&s))
	run := func(noPack bool) *Term {
		term, p := NewTerm(Config{ScrollingHistory: 100}, 10, 1, nil), NewProcessor()
		term.Grid().SetNoPack(noPack)
		feed(term, p, "aaaaaaaaa中")
		if f := term.Grid().Row(-1).CellAt(9).flags; f&FlagLeadingWideCharSpacer == 0 {
			t.Fatalf("setup: line -1 column 9 has flags %v, want the leading spacer", f)
		}
		feed(term, p, "\rx")
		return term
	}
	plain := run(true)
	s = PackStats{}
	packed := run(false)
	if f := packed.Grid().Row(-1).CellAt(9).flags; f&FlagLeadingWideCharSpacer != 0 {
		t.Fatalf("line -1 column 9 still has the leading spacer: %v", f)
	}
	if d := diffTerms(packed, plain); d != "" {
		t.Fatal(d)
	}
	if s.Rewrites == 0 || s.Packed == 0 {
		t.Fatalf("counters %+v: the test needs a packed row rewritten", s)
	}
}

// diffTerms compares everything a caller can read from two terminals: both
// grids' geometry, cursor and every line's cells, history included, a few
// columns past what each row stores.
func diffTerms(a, b *Term) string {
	for gi, gs := range [][2]*Grid{{a.grid, b.grid}, {a.inactiveGrid, b.inactiveGrid}} {
		ga, gb := gs[0], gs[1]
		if ga.columns != gb.columns || ga.lines != gb.lines || ga.HistorySize() != gb.HistorySize() ||
			ga.Cursor.Point != gb.Cursor.Point || ga.Cursor.InputNeedsWrap != gb.Cursor.InputNeedsWrap ||
			ga.SavedCursor.Point != gb.SavedCursor.Point {
			return fmt.Sprintf("grid %d: %dx%d hist %d cursor %v/%v vs %dx%d hist %d cursor %v/%v", gi,
				ga.columns, ga.lines, ga.HistorySize(), ga.Cursor.Point, ga.Cursor.InputNeedsWrap,
				gb.columns, gb.lines, gb.HistorySize(), gb.Cursor.Point, gb.Cursor.InputNeedsWrap)
		}
		for l := -ga.HistorySize(); l < ga.lines; l++ {
			ra, rb := ga.Row(l), gb.Row(l)
			if ra.Len() != rb.Len() {
				return fmt.Sprintf("grid %d line %d stores %d cells, plain %d", gi, l, ra.Len(), rb.Len())
			}
			for c := range ra.Len() + 2 {
				if x, y := ra.CellAt(c), rb.CellAt(c); x != y {
					return fmt.Sprintf("grid %d line %d cell %d differs: %+v vs plain %+v", gi, l, c, x, y)
				}
			}
		}
	}
	return ""
}

// packOp is one step of TestPackedMatchesPlain: output for both terminals, a
// resize, or raw cells written into the viewport.
type packOp struct {
	out        string
	cols, rows int
	scribble   int64 // seed for raw cells, 0 for none
}

// genPackOps is output that sends every kind of cell into history and through
// every resize path: Latin-1 and wide text, combining marks, every colour
// kind and style, coloured erases, wrapped lines, bare CRs, scroll regions,
// widening twice in a row, and raw cells SGR cannot produce.
func genPackOps(rng *rand.Rand, n, minCols, maxCols int) []packOp {
	sgr := func() string {
		switch rng.IntN(12) {
		case 0:
			return fmt.Sprintf("\x1b[%dm", 30+rng.IntN(8))
		case 1:
			return fmt.Sprintf("\x1b[%dm", 90+rng.IntN(8))
		case 2:
			return fmt.Sprintf("\x1b[48;5;%dm", rng.IntN(256))
		case 3:
			return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", rng.IntN(256), rng.IntN(256), rng.IntN(256))
		case 4:
			return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", rng.IntN(256), rng.IntN(256), rng.IntN(256))
		case 5:
			return fmt.Sprintf("\x1b[58;2;%d;%d;%dm", rng.IntN(256), rng.IntN(256), rng.IntN(256))
		case 6:
			return fmt.Sprintf("\x1b[%dm", []int{1, 2, 3, 4, 7, 8, 9, 21}[rng.IntN(8)])
		case 7:
			return fmt.Sprintf("\x1b[4:%dm", 2+rng.IntN(4))
		case 8:
			return fmt.Sprintf("\x1b[38;5;%dm", rng.IntN(256))
		}
		return "\x1b[0m"
	}
	word := func() string {
		var b strings.Builder
		for range 1 + rng.IntN(12) {
			switch rng.IntN(20) {
			case 0:
				b.WriteString("中")
			case 1:
				b.WriteString("é")
			case 2:
				b.WriteString("é")
			case 3:
				b.WriteString("ÿ")
			case 4:
				b.WriteString("⏺")
			default:
				b.WriteByte(byte('a' + rng.IntN(26)))
			}
		}
		return b.String()
	}
	var ops []packOp
	for range n {
		switch k := rng.IntN(20); {
		case k < 11:
			var b strings.Builder
			for range 1 + rng.IntN(6) {
				if rng.IntN(3) == 0 {
					b.WriteString(sgr())
				}
				b.WriteString(word())
				b.WriteByte(' ')
			}
			switch rng.IntN(6) {
			case 0:
				b.WriteString(sgr() + "\x1b[K")
			case 1:
				b.WriteString(strings.Repeat("=", maxCols+rng.IntN(maxCols)))
			case 2:
				b.WriteString("\r" + word())
			}
			b.WriteString("\x1b[0m\r\n")
			ops = append(ops, packOp{out: b.String()})
		case k < 13:
			ops = append(ops, packOp{out: []string{"\n\n\n", "\x1b[2J", "\x1b[3J", "\x1b[H\x1b[2L", "\x1b[1;3r\n\n\n\x1b[r", "\x1b[S", "\x1b[?1049h" + word() + "\x1b[?1049l", "\x1b[J", "\x1b[" + fmt.Sprint(1+rng.IntN(5)) + ";1H"}[rng.IntN(9)]})
		case k < 16:
			ops = append(ops, packOp{cols: minCols + rng.IntN(maxCols-minCols+1), rows: 2 + rng.IntN(8)})
		case k < 17:
			// Widening twice: the second sees what the first left wrapped.
			c := minCols + rng.IntN(max(1, (maxCols-minCols)/2))
			ops = append(ops, packOp{cols: c, rows: 3}, packOp{cols: c + 1 + rng.IntN(maxCols-c+1), rows: 3})
		default:
			ops = append(ops, packOp{scribble: 1 + rng.Int64N(1<<40)})
		}
	}
	return ops
}

func applyPackOp(t *Term, p *Processor, op packOp) {
	switch {
	case op.out != "":
		feed(t, p, op.out)
	case op.cols > 0:
		t.Resize(op.cols, op.rows)
	case op.scribble != 0:
		// Raw cells, any field, written as the parser would write them: into the
		// viewport, from where scrolling takes them into history.
		rng := rand.New(rand.NewPCG(uint64(op.scribble), 9))
		g := t.grid
		for range 1 + rng.IntN(3*g.columns) {
			c := randCell(rng)
			c.flags &^= FlagWideChar | FlagWideCharSpacer | FlagLeadingWideCharSpacer | FlagZerowidth
			c.zw = 0 // a mark id has to name a stored mark
			*g.Row(rng.IntN(g.lines)).at(rng.IntN(g.columns)) = c
		}
	}
}

// packedRun replays ops through a terminal that packs and one that does not,
// compares them after every op and returns the counters the packing one
// updated; noPackBoth turns packing off in both, for the negative control.
func packedRun(t *testing.T, ops []packOp, history, cols, rows int, noPackBoth bool) PackStats {
	t.Helper()
	var s PackStats
	defer SetPackStats(SetPackStats(nil))
	a, pa := NewTerm(Config{ScrollingHistory: history}, cols, rows, nil), NewProcessor()
	b, pb := NewTerm(Config{ScrollingHistory: history}, cols, rows, nil), NewProcessor()
	a.Grid().SetNoPack(noPackBoth)
	b.Grid().SetNoPack(true)
	for i, op := range ops {
		SetPackStats(&s)
		applyPackOp(a, pa, op)
		SetPackStats(nil)
		applyPackOp(b, pb, op)
		// A resize builds a new grid; the setting goes with the terminal.
		a.Grid().SetNoPack(noPackBoth)
		b.Grid().SetNoPack(true)
		if d := diffTerms(a, b); d != "" {
			t.Fatalf("after op %d %+v: %s", i, op, d)
		}
		checkOccupancy(t, a.grid, fmt.Sprintf("op %d", i))
		checkForm(t, a.grid, fmt.Sprintf("op %d", i))
		checkForm(t, b.grid, fmt.Sprintf("op %d, unpacked", i))
	}
	return s
}

// checkForm fails unless every row of g is in the form the grid keeps it in
// between operations: a viewport row plain and a full row wide, a history row
// packed or stored up to its last non-default cell. A row in the wrong form
// reads the same, so diffTerms cannot see it; it costs memory, and a packed
// viewport row is one a raw write would index past.
func checkForm(t *testing.T, g *Grid, label string) {
	t.Helper()
	for line := g.topmostLine(); line < g.lines; line++ {
		r := g.raw.row(line)
		switch {
		case line >= 0 && !r.full(g.columns):
			t.Fatalf("%s: viewport line %d: packed %v, %d cells; want plain, %d", label, line, r.packed, len(r.inner), g.columns)
		case line < 0 && !r.packed && len(r.inner) != r.trimmedLen():
			t.Fatalf("%s: history line %d stores %d cells, content %d", label, line, len(r.inner), r.trimmedLen())
		}
	}
}

// missingCounters names the counters TestPackedMatchesPlain requires and s
// does not have, "" when it has them all. WrapGrows is not one: a widening
// joins every wrapped row it meets before its final loop, a joined row ends a
// full new row wide, and the grid that resizes without reflow, the alternate
// screen, has no history, so the final loop does not meet a packed wrapped
// history row shorter than the new width. It is counted to show that.
func missingCounters(s PackStats, long bool) string {
	var miss []string
	for name, v := range map[string]int{
		"Packed": s.Packed, "Plain": s.Plain, "Ext": s.Ext, "Unpacks": s.Unpacks,
		"Replaced": s.Replaced, "PoolPut": s.PoolPut, "PoolTake": s.PoolTake,
		"Settles": s.Settles,
	} {
		if v == 0 {
			miss = append(miss, name)
		}
	}
	if long && s.MaxExt < 256 {
		miss = append(miss, fmt.Sprintf("MaxExt>=256 (%d)", s.MaxExt))
	}
	return strings.Join(miss, " ")
}

// TestPackedMatchesPlain is the in-tree differential: the same output, resizes
// and raw cells through a grid that packs history and one that does not must
// read the same after every step. Its oracle shares every call site with the
// code under test, so the independent one is the out-of-tree comparison with
// ce43d7d; this one runs on every go test and asserts that each lifecycle path
// it exists for was taken.
func TestPackedMatchesPlain(t *testing.T) {
	var total PackStats
	for seed := range 60 {
		rng := rand.New(rand.NewPCG(uint64(seed), 77))
		history := []int{0, 3, 7, 50, 2000}[seed%5]
		ops := genPackOps(rng, 150, 2, 40)
		s := packedRun(t, ops, history, 10+rng.IntN(20), 2+rng.IntN(6), false)
		addStats(&total, s)
	}
	t.Logf("counters %+v", total)
	if m := missingCounters(total, false); m != "" {
		t.Fatalf("no run took these paths: %s", m)
	}
}

// The long-row arm: rows wide enough to hold more than 256 extended cells and
// still pack, which only both bytes of an entry's index can address.
func TestPackedMatchesPlainLong(t *testing.T) {
	var total PackStats
	for seed := range 6 {
		rng := rand.New(rand.NewPCG(uint64(seed), 78))
		var ops []packOp
		for range 40 {
			var b strings.Builder
			for c := range 400 + rng.IntN(600) {
				if rng.IntN(2) == 0 {
					fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", c%256, rng.IntN(256), 3)
				} else {
					b.WriteString("\x1b[39m")
				}
				b.WriteByte(byte('a' + rng.IntN(26)))
			}
			ops = append(ops, packOp{out: b.String() + "\x1b[0m\r\n"})
			if rng.IntN(8) == 0 {
				ops = append(ops, packOp{cols: 400 + rng.IntN(625), rows: 4})
			}
		}
		addStats(&total, packedRun(t, ops, []int{5, 100}[seed%2], 1024, 4, false))
	}
	t.Logf("counters %+v", total)
	if total.MaxExt < 256 {
		t.Fatalf("largest packed row held %d extended cells, want past 256", total.MaxExt)
	}
}

// The negative control: with packing off in both terminals, the counter check
// TestPackedMatchesPlain relies on must fail, or it would pass for a run that
// never packed.
func TestPackCountersNeedPacking(t *testing.T) {
	var total PackStats
	for seed := range 10 {
		rng := rand.New(rand.NewPCG(uint64(seed), 77))
		addStats(&total, packedRun(t, genPackOps(rng, 150, 2, 40), []int{0, 3, 7, 50, 2000}[seed%5], 20, 4, true))
	}
	if m := missingCounters(total, false); !strings.Contains(m, "Packed") {
		t.Fatalf("with packing off the counter check reports only %q missing: %+v", m, total)
	}
}

func addStats(t *PackStats, s PackStats) {
	t.Packed += s.Packed
	t.Plain += s.Plain
	t.PlainIndex += s.PlainIndex
	t.Ext += s.Ext
	t.MaxExt = max(t.MaxExt, s.MaxExt)
	t.Unpacks += s.Unpacks
	t.Replaced += s.Replaced
	t.PoolPut += s.PoolPut
	t.PoolTake += s.PoolTake
	t.Settles += s.Settles
	t.Rewrites += s.Rewrites
	t.WrapGrows += s.WrapGrows
}
