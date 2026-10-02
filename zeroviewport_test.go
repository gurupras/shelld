package shelld

import (
	"bytes"
	"testing"
)

// A client's declared viewport is its own claim, and Attacher.SetSize stores it
// almost verbatim: packSize clamps it down to MaxCols/MaxRows and applies no
// floor, so both 0x0 and 1024x1024 are declarations the daemon accepts and
// hands to the framer. Frame resolves both against the grid.
//
// The two ends fail differently without that, which is why both are here:
//
//   - 0x0 sizes the snapshot at zero cells, and an empty Cells slice is also the
//     "no prior state" sentinel in renderDiff and in Frame's full term, so every
//     frame is a repaint and the client never receives a diff.
//   - above the grid, snapshotInto fills everything past it with the constant
//     padSlot, which is written and then compared against itself every frame and
//     reaches no client.
//
// The 20x5 leg is the control that makes this a resolution rather than a
// surrender: a declaration BELOW the grid is a real crop and must still be
// cropped. A fix that simply always used the grid's own size would satisfy
// every other leg here and fail that one.
func TestDeclaredViewportIsResolvedAgainstTheGrid(t *testing.T) {
	const termCols, termRows = 80, 24

	frameAt := func(t *testing.T, cols, rows int) []byte {
		t.Helper()
		g := grid(termCols, termRows, "\x1b[31mhello\r\nworld")
		f := &LiveFramer{}
		out, rendered, full := f.Frame(nil, g, cols, rows, false)
		if !rendered || !full {
			t.Fatalf("first frame at %dx%d: rendered=%v full=%v, want both true", cols, rows, rendered, full)
		}
		// A second frame on an untouched grid must be quiet at every declared
		// size. At 0x0 before the fix this was the one case that WAS quiet, for
		// the wrong reason: f.last.Cols == 0 == cols satisfied the generation
		// guard. It is kept because it must not regress, not as evidence.
		second, rendered, full := f.Frame(nil, g, cols, rows, false)
		if rendered || full || len(second) != 0 {
			t.Errorf("second frame at %dx%d on an unchanged grid: rendered=%v full=%v %d bytes %q",
				cols, rows, rendered, full, len(second), second)
		}
		return out
	}

	exact := frameAt(t, termCols, termRows)
	t.Logf("declared 80x24 (exact): %d bytes", len(exact))

	for _, tc := range []struct {
		name       string
		cols, rows int
		wantSame   bool
	}{
		{"zero resolves to the grid", 0, 0, true},
		{"above the grid resolves down to it", MaxCols, MaxRows, true},
		{"one axis zero", 0, termRows, true},
		{"one axis above", termCols, MaxRows, true},
		{"below the grid is a real crop and is kept", 20, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := frameAt(t, tc.cols, tc.rows)
			same := bytes.Equal(got, exact)
			t.Logf("declared %dx%d: %d bytes, identical to 80x24: %v", tc.cols, tc.rows, len(got), same)
			if same != tc.wantSame {
				t.Errorf("declared %dx%d produced %d bytes (identical to the exact-size frame: %v), want identical=%v\n got %q\nwant %q",
					tc.cols, tc.rows, len(got), same, tc.wantSame, got, exact)
			}
		})
	}
}

// Every declared size must produce diffs on a changing grid, which is the half
// the generation guard cannot mask and the half a 0x0 client never got: an empty
// snapshot tripped the "no prior state" term on every frame, so it received a
// 37-byte clear-and-repaint per tick and never a diff, more than a correctly
// sized client and never incremental.
func TestDeclaredViewportDiffsOnAChangingGrid(t *testing.T) {
	const termCols, termRows = 80, 24
	for _, tc := range []struct {
		name       string
		cols, rows int
	}{
		{"zero", 0, 0},
		{"exact", termCols, termRows},
		{"above the grid", MaxCols, MaxRows},
		{"cropped", 20, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := grid(termCols, termRows, "hello")
			f := &LiveFramer{}
			_, _, _ = f.Frame(nil, g, tc.cols, tc.rows, false)

			g.Advance([]byte("\r\nworld"))
			out2, rendered2, full2 := f.Frame(nil, g, tc.cols, tc.rows, true)
			g.Advance([]byte("\r\nagain"))
			out3, rendered3, full3 := f.Frame(nil, g, tc.cols, tc.rows, true)

			if !rendered2 || !rendered3 {
				t.Fatalf("a changed grid must render: %v %v", rendered2, rendered3)
			}
			if full2 || full3 {
				t.Errorf("full=%v,%v want false,false: a client holding a prior "+
					"frame must receive a diff, not a repaint", full2, full3)
			}
			t.Logf("%s: tick2 %d bytes %q; tick3 %d bytes %q", tc.name, len(out2), out2, len(out3), out3)
		})
	}
}

// A declaration does not size the framer's cell buffers. This is the cost
// assertion behind the upper end, and the one that fails loudly rather than
// subtly: at 1024x1024 against an 80x24 grid each of the framer's two snapshots
// held 1,048,576 cells against 1,920 -- 32.0 MiB against 0.1 MiB, and 1.564 ms
// per frame against 3.887 us -- to produce byte-identical output, all of it
// padSlot written and then compared against itself.
//
// Asserting the geometry rather than a duration keeps this meaningful on a
// loaded machine. It reads the framer's own snapshots rather than calling
// SnapshotCropped, because that is where the resolution is: SnapshotCropped is
// the general cropping API and a caller asking it for a 0x0 crop still gets
// one. Frame is what reconciles a client's claim before reaching it.
func TestDeclaredViewportDoesNotSizeTheFramersBuffers(t *testing.T) {
	const termCols, termRows = 80, 24

	// Expected geometry is written out per case rather than computed by calling
	// resolveDeclared: an expectation derived from the function under test is
	// satisfied by any mutation of it, including removing the guard entirely.
	for _, tc := range []struct {
		cols, rows         int
		wantCols, wantRows int
	}{
		{0, 0, termCols, termRows},
		{MaxCols, MaxRows, termCols, termRows},
		{termCols, termRows, termCols, termRows},
		{20, 5, 20, 5},
		{0, MaxRows, termCols, termRows},
		{MaxCols, 5, termCols, 5},
	} {
		g := grid(termCols, termRows, "hello")
		f := &LiveFramer{}
		// Twice, so both f.last and f.scratch have been filled at the resolved
		// size: they are swapped each frame, and the second is the other half of
		// the two snapshots per attached client that size.go accounts for.
		f.Frame(nil, g, tc.cols, tc.rows, false)
		g.Advance([]byte("\r\nworld"))
		f.Frame(nil, g, tc.cols, tc.rows, true)

		wantCols, wantRows := tc.wantCols, tc.wantRows
		want := wantCols * wantRows
		for name, snap := range map[string]*GridSnapshot{"last": f.last, "scratch": f.scratch} {
			if snap.Cols != wantCols || snap.Rows != wantRows {
				t.Errorf("declared %dx%d: %s geometry %dx%d, want %dx%d",
					tc.cols, tc.rows, name, snap.Cols, snap.Rows, wantCols, wantRows)
			}
			if len(snap.Cells) != want {
				t.Errorf("declared %dx%d: %s holds %d cells, want %d (%dx%d)",
					tc.cols, tc.rows, name, len(snap.Cells), want, wantCols, wantRows)
			}
		}
	}
}
