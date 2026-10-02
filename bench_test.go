package shelld

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// fixtureCorpus is coloured ASCII text scrolled through a 200×50 screen, so the
// render benchmarks have a full screen of varied attributes to draw.
func fixtureCorpus(testing.TB) []byte {
	var buf bytes.Buffer
	for i := range 400 {
		fmt.Fprintf(&buf, "\x1b[3%dm%4d\x1b[0m \x1b[1mbuild\x1b[0m step %d: compiled %d files in %d.%02ds\r\n",
			i%8, i, i%17, i*7%300, i%9, i*13%100)
	}
	return buf.Bytes()
}

func BenchmarkTickUnchanged(b *testing.B) {
	g := NewVtGrid(200, 50)
	g.Advance(fixtureCorpus(b))
	snap := g.Snapshot()
	scratch := &GridSnapshot{}
	for b.Loop() {
		cur := g.SnapshotInto(scratch, 0, 200, 50)
		_ = g.RenderDiffSnapshots(nil, snap, cur, 0)
	}
}

func BenchmarkFullFrame(b *testing.B) {
	g := NewVtGrid(200, 50)
	g.Advance(fixtureCorpus(b))
	var out []byte
	for b.Loop() {
		out = g.RenderFullInto(out[:0], 0, 200, 50)
	}
}

// wideScreen is a 200×50 grid with CJK and emoji on every row, so render
// benchmarks take the wide-character paths fixtureCorpus never reaches.
func wideScreen() *VtGrid {
	g := NewVtGrid(200, 50)
	g.Advance([]byte("\x1b[?7l"))
	for r := range 50 {
		g.Advance(fmt.Appendf(nil, "\x1b[%d;1H\x1b[3%dm", r+1, r%8))
		for range 12 {
			g.Advance([]byte("中文字 ok 🙂 build ▶ "))
		}
	}
	return g
}

func BenchmarkTickUnchangedWide(b *testing.B) {
	g := wideScreen()
	snap := g.Snapshot()
	scratch := &GridSnapshot{}
	for b.Loop() {
		cur := g.SnapshotInto(scratch, 0, 200, 50)
		_ = g.RenderDiffSnapshots(nil, snap, cur, 0)
	}
}

func BenchmarkFullFrameWide(b *testing.B) {
	g := wideScreen()
	var out []byte
	for b.Loop() {
		out = g.RenderFullInto(out[:0], 0, 200, 50)
	}
}

// TestBenchReport prints the metrics the Rust comparison harness prints, on
// the same corpus and protocol:
//
//	SHELLD_BENCH_CORPUS=corpus.raw go test . -run TestBenchReport -v
func TestBenchReport(t *testing.T) {
	path := os.Getenv("SHELLD_BENCH_CORPUS")
	if path == "" {
		t.Skip("set SHELLD_BENCH_CORPUS")
	}
	corpus, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := 200, 50

	best := time.Duration(1<<63 - 1)
	for range 5 {
		g := NewVtGrid(cols, rows)
		start := time.Now()
		for c := range slices.Chunk(corpus, 4096) {
			g.Advance(c)
		}
		best = min(best, time.Since(start))
	}
	fmt.Printf("advance_mb_per_s %.1f\n", float64(len(corpus))/1e6/best.Seconds())

	g := NewVtGrid(cols, rows)
	g.Advance(corpus)
	const iters = 2000
	snap := g.Snapshot()

	// The daemon's tick path reuses snapshot buffers; the Rust harness calls
	// render_diff_cropped, which allocates. Report both for Go.
	start := time.Now()
	for range iters {
		out, _ := g.RenderDiffCropped(snap, cols, rows)
		_ = out
	}
	fmt.Printf("tick_unchanged_us %.1f\n", float64(time.Since(start).Microseconds())/iters)

	scratch := &GridSnapshot{}
	start = time.Now()
	for range iters {
		cur := g.SnapshotInto(scratch, 0, cols, rows)
		_ = g.RenderDiffSnapshots(nil, snap, cur, 0)
	}
	fmt.Printf("tick_unchanged_reuse_us %.1f\n", float64(time.Since(start).Microseconds())/iters)

	start = time.Now()
	for i := range iters {
		g.Advance(fmt.Appendf(nil, "\x1b[%d;1Hx%d", i%rows+1, i))
		out, _ := g.RenderDiffCropped(snap, cols, rows)
		_ = out
	}
	fmt.Printf("tick_small_change_us %.1f\n", float64(time.Since(start).Microseconds())/iters)

	start = time.Now()
	for range iters {
		_ = g.RenderFrameCropped(cols, rows)
	}
	fmt.Printf("full_frame_us %.1f\n", float64(time.Since(start).Microseconds())/iters)
}
