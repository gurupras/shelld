package shelld

// Property test for the snapshot and render code: arbitrary cell contents
// are written straight into grid rows, so every flag bit, color kind, odd
// rune and malformed wide pair is reached whether or not the parser can
// produce it, and snapshots, changed-cell counts, full frames and diffs
// are checked byte for byte against the frozen reference in
// render_ref_test.go. Adapted from a pre-review of the render work rather than
// written alongside it. It writes cells through unsafe, because vt has no
// setter for the underline color; TestRenderCellLayout checks the layout
// that relies on.

import (
	"bytes"
	"math/rand"
	"testing"
	"unsafe"

	"github.com/gurupras/shelld/vt"
)

type rawCell struct {
	ch    rune
	fg    vt.Color
	bg    vt.Color
	ul    vt.Color
	flags vt.Flags
	zw    uint16
}

func TestRenderCellLayout(t *testing.T) {
	var c vt.Cell
	if unsafe.Sizeof(c) != unsafe.Sizeof(rawCell{}) {
		t.Fatalf("vt.Cell size %d, rawCell %d", unsafe.Sizeof(c), unsafe.Sizeof(rawCell{}))
	}
	r := (*rawCell)(unsafe.Pointer(&c))
	r.ch, r.fg, r.bg, r.ul, r.flags = 'a'^' ', 1, 2, 3, 4
	if c.Rune() != 'a' || c.UnderlineColor() != 3 || c.Flags() != 4 {
		t.Fatalf("layout mismatch")
	}
	var s CellSlot
	t.Logf("CellSlot size %d align %d offsets ch=%d fg=%d bg=%d attrs=%d; vt.Cell size %d",
		unsafe.Sizeof(s), unsafe.Alignof(s), unsafe.Offsetof(s.Ch), unsafe.Offsetof(s.Fg),
		unsafe.Offsetof(s.Bg), unsafe.Offsetof(s.Attrs), unsafe.Sizeof(c))
	if unsafe.Sizeof(s) != 16 || unsafe.Offsetof(s.Attrs) != 12 {
		t.Fatalf("CellSlot has padding")
	}
}

func propColor(rng *rand.Rand) vt.Color {
	switch rng.Intn(8) {
	case 0, 1, 2:
		return vt.NewNamedColor([]vt.NamedColor{vt.Foreground, vt.Background}[rng.Intn(2)])
	case 3:
		return vt.NewNamedColor(vt.NamedColor(rng.Intn(16)))
	case 4:
		return vt.NewNamedColor(vt.Foreground + vt.NamedColor(rng.Intn(13)))
	case 5:
		return vt.NewIndexedColor(uint8(rng.Intn(256)))
	case 6:
		return vt.NewRGBColor(uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)))
	}
	return vt.Color(rng.Uint32() & 0x03ffffff) // any kind incl. none, odd payloads
}

func propCell(rng *rand.Rand) rawCell {
	var c vt.Cell
	c.SetRune([]rune{' ', 0, 'a', 'Z', '中', '🙂', '\t', 0x20 | 0x100, 0xd800, -1, 0x110000}[rng.Intn(11)])
	if rng.Intn(3) == 0 {
		c.SetFg(propColor(rng))
	}
	if rng.Intn(3) == 0 {
		c.SetBg(propColor(rng))
	}
	if rng.Intn(4) == 0 {
		c.SetFlags(vt.Flags(1) << rng.Intn(16))
	} else if rng.Intn(6) == 0 {
		c.SetFlags(vt.Flags(rng.Uint32()))
	}
	r := *(*rawCell)(unsafe.Pointer(&c))
	if rng.Intn(8) == 0 {
		r.ul = propColor(rng)
	}
	return r
}

func propScribble(rng *rand.Rand, g *VtGrid) {
	grid := g.Term().Grid()
	lines, cols, hist := grid.ScreenLines(), grid.Columns(), g.ScrollbackLen()
	pick := func() int { return rng.Intn(lines+min(hist, 60)) - min(hist, 60) }
	for k := rng.Intn(12); k >= 0; k-- {
		row := grid.Row(pick())
		// The cases below fabricate cells by writing through the pointer Cell
		// returns, which bypasses the occupancy hint every writer inside vt
		// maintains. A row whose occupancy is understated loses those cells at its
		// next reset and at the next narrowing, so declare the whole row occupied.
		//
		// It reads like coverage given up, since a fully occupied row declines
		// the shrink fast path, and it is the opposite. Without it these rows
		// understate occupancy, a narrowing drops the fabricated cells, and this
		// property still passes: the reference is built from the same terminal,
		// so the loss lands on both sides and cancels. The assertion that can see
		// that class is TestMarkOccupiedKeepsFabricatedCells in vt.
		row.MarkOccupied(row.Len())
		switch rng.Intn(7) {
		case 0: // one cell
			*(*rawCell)(unsafe.Pointer(row.Cell(rng.Intn(cols)))) = propCell(rng)
		case 1: // last column only
			*(*rawCell)(unsafe.Pointer(row.Cell(cols - 1))) = propCell(rng)
		case 2: // first column only
			*(*rawCell)(unsafe.Pointer(row.Cell(0))) = propCell(rng)
		case 3: // wide pair, possibly malformed
			c := rng.Intn(cols)
			w := propCell(rng)
			w.flags |= vt.FlagWideChar
			*(*rawCell)(unsafe.Pointer(row.Cell(c))) = w
			if c+1 < cols && rng.Intn(4) != 0 {
				s := propCell(rng)
				s.flags |= vt.FlagWideCharSpacer
				*(*rawCell)(unsafe.Pointer(row.Cell(c + 1))) = s
			}
		case 4: // blank the row
			for c := 0; c < cols; c++ {
				*(*rawCell)(unsafe.Pointer(row.Cell(c))) = rawCell{}
			}
		case 5: // trailing run of near-blank cells
			for c := rng.Intn(cols); c < cols; c++ {
				r := rawCell{}
				if rng.Intn(cols) == 0 {
					r = propCell(rng)
				}
				*(*rawCell)(unsafe.Pointer(row.Cell(c))) = r
			}
		case 6: // copy another row (equal rows at different positions)
			src := grid.Row(pick())
			for c := 0; c < cols; c++ {
				*row.Cell(c) = *src.Cell(c)
			}
		}
	}
	if rng.Intn(5) == 0 {
		g.Advance([]byte([]string{"\x1b[?25l", "\x1b[?25h", "\x1b[3;5H", "\x1b[H", "\x1b[999;999H"}[rng.Intn(5)]))
	}
}

type propView struct {
	cols, rows, offset int
	prev, refPrev      *GridSnapshot
	scratch            *GridSnapshot
}

func (v *propView) step(t *testing.T, g *VtGrid, it int) {
	t.Helper()
	off := g.clampOffset(v.offset)
	cur := g.SnapshotInto(v.scratch, off, v.cols, v.rows)
	ref := refSnapshotInto(nil, g.term, off, v.cols, v.rows)
	if cur.CursorRow != ref.CursorRow || cur.CursorCol != ref.CursorCol || cur.CursorVisible != ref.CursorVisible {
		t.Fatalf("it %d view %+v: cursor", it, *v)
	}
	full := g.RenderFullInto(nil, off, v.cols, v.rows)
	refFull := refRenderFull(nil, g.term, off, v.cols, v.rows)
	if !bytes.Equal(full, refFull) {
		t.Fatalf("it %d view %dx%d@%d: full frame differs\n got %q\nwant %q", it, v.cols, v.rows, off, trunc(string(full)), trunc(string(refFull)))
	}
	if v.prev != nil {
		if v.prev.Cols == cur.Cols && v.prev.Rows == cur.Rows && len(v.prev.Cells) == len(cur.Cells) {
			n := CountChanged(v.prev, cur)
			refN := 0
			for i := range ref.Cells {
				if v.refPrev.Cells[i] != ref.Cells[i] {
					refN++
				}
			}
			if n != refN {
				t.Fatalf("it %d view %dx%d@%d: CountChanged %d, reference %d", it, v.cols, v.rows, off, n, refN)
			}
		}
		d := g.RenderDiffSnapshots(nil, v.prev, cur, off)
		refD := refRenderDiff(nil, v.refPrev, ref, g.term, off)
		if !bytes.Equal(d, refD) {
			t.Fatalf("it %d view %dx%d@%d: diff differs\n got %q\nwant %q", it, v.cols, v.rows, off, trunc(string(d)), trunc(string(refD)))
		}
	}
	v.scratch, v.prev, v.refPrev = v.prev, cur, ref
}

func TestRenderArbitraryCellsMatchReference(t *testing.T) {
	iters := 3000
	if testing.Short() {
		iters = 300
	}
	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cols, rows := 1+rng.Intn(90), 1+rng.Intn(30)
		g := NewVtGrid(cols, rows)
		for i := 0; i < rows+80; i++ {
			g.Advance([]byte("history\r\n"))
		}
		views := []*propView{
			{cols: cols, rows: rows},
			{cols: max(cols-3, 1), rows: max(rows-2, 1)},
			{cols: cols + 4, rows: rows + 3},
			{cols: 1, rows: 1},
			{cols: cols, rows: rows, offset: 5},
			{cols: max(cols-3, 1), rows: rows + 2, offset: 17}, // paused with frozen crop/pad
			{cols: cols + 6, rows: max(rows-1, 1), offset: 60},
		}
		for it := 0; it < iters; it++ {
			propScribble(rng, g)
			if rng.Intn(200) == 0 {
				g.Resize(max(1, cols+rng.Intn(11)-5), max(1, rows+rng.Intn(5)-2))
			}
			for _, v := range views {
				v.step(t, g, it)
			}
		}
	}
}
