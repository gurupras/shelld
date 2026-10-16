package shelld

import (
	"testing"
)

// BenchmarkLiveFrameKeystroke is the idle-keystroke tick: one echoed key
// changes one cell, and the live framer builds the client's next frame. The
// arms are the full snapshot and the damaged-rows snapshot.
func BenchmarkLiveFrameKeystroke(b *testing.B) {
	screens := []struct {
		name       string
		cols, rows int
		grid       func(testing.TB) *VtGrid
	}{
		{"fixture", 200, 50, func(tb testing.TB) *VtGrid {
			g := NewVtGrid(200, 50)
			g.Advance(fixtureCorpus(tb))
			return g
		}},
		{"wide", 200, 50, func(testing.TB) *VtGrid { return wideScreen() }},
		// The echo:none probe's window (cat, nearly blank) at a 200x50 client.
		{"blank200x50", 200, 50, func(testing.TB) *VtGrid {
			g := NewVtGrid(200, 50)
			g.Advance([]byte("$ cat\r\n"))
			return g
		}},
		// The echo:none probe's window: cat at 80x24.
		{"80x24", 80, 24, func(tb testing.TB) *VtGrid {
			g := NewVtGrid(80, 24)
			g.Advance(fixtureCorpus(tb))
			return g
		}},
	}
	keys := [][]byte{[]byte("x\b"), []byte("y\b")}
	for _, s := range screens {
		for _, arm := range []struct {
			name   string
			damage bool
		}{{"full", false}, {"damaged", true}} {
			b.Run(s.name+"/"+arm.name, func(b *testing.B) {
				defer func(v bool) { damageFrames = v }(damageFrames)
				damageFrames = arm.damage
				g := s.grid(b)
				var f LiveFramer
				var out []byte
				out, _, _ = f.Frame(out[:0], g, s.cols, s.rows, true)
				out, _, _ = f.Frame(out[:0], g, s.cols, s.rows, true)
				i := 0
				for b.Loop() {
					g.Advance(keys[i&1])
					i++
					out, _, _ = f.Frame(out[:0], g, s.cols, s.rows, true)
				}
				if len(out) == 0 {
					b.Fatal("keystroke frame is empty")
				}
			})
		}
	}
}
