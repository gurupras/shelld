package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// A LiveFramer that copies only damaged rows must send exactly
// the bytes, and hold exactly the snapshot, of one that copies the whole
// grid, through every kind of change a program can make: printing (wide and
// combining characters included), cursor moves, erases, insert/delete of
// lines and characters, scroll regions, scrolling into history, the
// alternate screen, a full reset, and resizes of the grid and of the client.
func TestLiveFramerDamageMatchesFull(t *testing.T) {
	old := damageFrames
	t.Cleanup(func() { damageFrames = old })
	pieces := []string{
		"a", "bc", "hello", "中", "é", "\r", "\n", "\r\n", "\b", "\t",
		"\x1b[H", "\x1b[5;7H", "\x1b[2A", "\x1b[3B", "\x1b[4C", "\x1b[2D",
		"\x1b[K", "\x1b[1K", "\x1b[2K", "\x1b[J", "\x1b[1J", "\x1b[2J", "\x1b[3J",
		"\x1b[2L", "\x1b[3M", "\x1b[4@", "\x1b[2P", "\x1b[5X",
		"\x1b[3;8r", "\x1b[r", "\x1b[2S", "\x1b[3T", "\x1bM", "\x1bD", "\x1bE",
		"\x1b[?1049h", "\x1b[?1049l", "\x1b[?25l", "\x1b[?25h",
		"\x1b[31m", "\x1b[1;4m", "\x1b[0m", "\x1b[7m", "\x1b7", "\x1b8",
		"\x1b[?7l", "\x1b[?7h", "\x1b[?6h", "\x1b[?6l", "\x1bc",
	}
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cols, rows := 20+rng.Intn(40), 5+rng.Intn(20)
		g := NewVtGrid(cols, rows)
		var full, dmg LiveFramer
		ccols, crows := cols, rows
		for step := 0; step < 400; step++ {
			var b bytes.Buffer
			for k := rng.Intn(6) + 1; k > 0; k-- {
				p := pieces[rng.Intn(len(pieces))]
				if p == "\x1bc" && rng.Intn(8) != 0 {
					p = "x"
				}
				b.WriteString(p)
			}
			// Long runs to wrap lines and scroll into history.
			if rng.Intn(10) == 0 {
				b.WriteString(fmt.Sprintf("%0*d\n", rng.Intn(3*cols), step))
			}
			g.Advance(b.Bytes())
			switch rng.Intn(40) {
			case 0:
				cols, rows = 20+rng.Intn(40), 5+rng.Intn(20)
				g.Resize(cols, rows)
			case 1:
				ccols, crows = 20+rng.Intn(60), 5+rng.Intn(30)
			case 2:
				ccols, crows = 0, 0
			case 3:
				full.Invalidate()
				dmg.Invalidate()
			}
			dirty := rng.Intn(4) != 0
			damageFrames = false
			want, wr, wf := full.Frame(nil, g, ccols, crows, dirty)
			damageFrames = true
			got, gr, gf := dmg.Frame(nil, g, ccols, crows, dirty)
			if !bytes.Equal(got, want) || gr != wr || gf != wf {
				t.Fatalf("seed %d step %d: damage frame %q (%v %v), full frame %q (%v %v)",
					seed, step, got, gr, gf, want, wr, wf)
			}
			if wr && !snapshotsEqual(dmg.last, full.last) {
				t.Fatalf("seed %d step %d: damage snapshot differs from full", seed, step)
			}
			if rng.Intn(16) == 0 {
				damageFrames = false
				want = full.Settle(nil, g, ccols, crows)
				damageFrames = true
				got = dmg.Settle(nil, g, ccols, crows)
				if !bytes.Equal(got, want) {
					t.Fatalf("seed %d step %d: settle differs", seed, step)
				}
			}
		}
	}
}

func snapshotsEqual(a, b *GridSnapshot) bool {
	if a.Cols != b.Cols || a.Rows != b.Rows || len(a.Cells) != len(b.Cells) ||
		a.CursorRow != b.CursorRow || a.CursorCol != b.CursorCol || a.CursorVisible != b.CursorVisible {
		return false
	}
	for i := range a.Cells {
		if a.Cells[i] != b.Cells[i] {
			return false
		}
	}
	return true
}
