package shelld

import (
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// glyphRow reads row r of term as a client would show it: each cell's
// character followed by the zero-width characters it carries, trailing
// spaces trimmed. It reads the vt grid directly, so it is independent of the
// snapshot and emitter code under test.
func glyphRow(term *vt.Term, r int) string {
	var sb strings.Builder
	row := term.Grid().Row(r)
	for c := 0; c < term.Columns(); c++ {
		cell := row.Cell(c)
		if cell.Flags()&vt.FlagWideCharSpacer != 0 {
			continue
		}
		ch := cell.Rune()
		if ch == 0 {
			ch = ' '
		}
		sb.WriteRune(ch)
		for _, m := range term.Marks(cell.MarkID()) {
			sb.WriteRune(m)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

const acute = "́"

// TestCombiningMarksReachEveryFrame writes "e" and then, in a later output,
// the combining acute that makes it "é": a change to a cell's marks alone. A
// client fed each path's frames must hold "é", which it does only if the
// snapshot sees the change and the emitter draws the marks behind the base.
func TestCombiningMarksReachEveryFrame(t *testing.T) {
	const cols, rows = 10, 2
	want := "xe" + acute + "y"

	t.Run("full", func(t *testing.T) {
		g := gridOf(cols, rows, "xe"+acute+"y")
		if got := glyphRow(replayFrame(t, g.RenderFrame(), cols, rows), 0); got != want {
			t.Fatalf("client holds %+q, want %+q", got, want)
		}
	})

	t.Run("diff", func(t *testing.T) {
		g := gridOf(cols, rows, "xey")
		first := g.RenderFrame()
		prev := g.Snapshot()
		g.Advance([]byte("\x1b[1;3H" + acute))
		diff, _ := g.RenderDiff(prev)
		if len(diff) == 0 {
			t.Fatal("a change to a cell's marks alone produced no diff")
		}
		if got := glyphRow(replayFrame(t, append(first, diff...), cols, rows), 0); got != want {
			t.Fatalf("client holds %+q, want %+q (diff %q)", got, want, diff)
		}
	})

	t.Run("live", func(t *testing.T) {
		g := gridOf(cols, rows, "xey")
		var f LiveFramer
		first, _, full := f.Frame(nil, g, cols, rows, true)
		if !full {
			t.Fatal("the first frame was not a repaint")
		}
		g.Advance([]byte("\x1b[1;3H" + acute))
		diff, rendered, full := f.Frame(nil, g, cols, rows, true)
		if !rendered || full || len(diff) == 0 {
			t.Fatalf("mark-only change: rendered=%v full=%v len=%d, want a non-empty diff", rendered, full, len(diff))
		}
		if got := glyphRow(replayFrame(t, append(first, diff...), cols, rows), 0); got != want {
			t.Fatalf("client holds %+q, want %+q (diff %q)", got, want, diff)
		}
	})

	t.Run("frozen", func(t *testing.T) {
		g := gridOf(cols, rows, "xe"+acute+"y")
		f := freezeHistory(g.Term())
		snap := f.SnapshotInto(nil, 0, cols, rows)
		if got := glyphRow(replayFrame(t, renderFullFrozen(nil, snap), cols, rows), 0); got != want {
			t.Fatalf("repaint: client holds %+q, want %+q", got, want)
		}
		frame := f.RenderDiff(nil, EmptySnapshot(), snap)
		if got := glyphRow(replayFrame(t, frame, cols, rows), 0); got != want {
			t.Fatalf("diff from empty: client holds %+q, want %+q", got, want)
		}
	})
}

// TestALiveBridgeNeverRedrawsAMarkedCell: between two changed cells one
// column apart, the live diff redraws the unchanged ASCII cell between them
// rather than moving. Redrawn from its slot's base character, a marked cell
// would lose its mark in the client.
func TestALiveBridgeNeverRedrawsAMarkedCell(t *testing.T) {
	const cols, rows = 10, 2
	g := gridOf(cols, rows, "Xe"+acute+"Y")
	var f LiveFramer
	first, _, _ := f.Frame(nil, g, cols, rows, true)
	g.Advance([]byte("\x1b[1;1HA\x1b[1;3HB"))
	diff, _, full := f.Frame(nil, g, cols, rows, true)
	if full {
		t.Fatal("the change was sent as a repaint, which bridges nothing")
	}
	want := "Ae" + acute + "B"
	if got := glyphRow(replayFrame(t, append(first, diff...), cols, rows), 0); got != want {
		t.Fatalf("client holds %+q, want %+q (diff %q)", got, want, diff)
	}
}

// TestAMarkOnTrailingSpaceIsDrawn: a space carrying a mark draws something,
// so it ends the row's text in a repaint rather than being trimmed with the
// blanks after it.
func TestAMarkOnTrailingSpaceIsDrawn(t *testing.T) {
	const cols, rows = 10, 2
	g := gridOf(cols, rows, "a "+acute)
	want := "a " + acute
	if got := glyphRow(replayFrame(t, g.RenderFrame(), cols, rows), 0); got != want {
		t.Fatalf("live repaint: client holds %+q, want %+q", got, want)
	}
	snap := freezeHistory(g.Term()).SnapshotInto(nil, 0, cols, rows)
	if got := glyphRow(replayFrame(t, renderFullFrozen(nil, snap), cols, rows), 0); got != want {
		t.Fatalf("frozen repaint: client holds %+q, want %+q", got, want)
	}
}

// TestAWideCharacterCutByTheCropLosesItsMarks: a wide character whose spacer
// lies outside the client's width is drawn as a blank, and its marks, which
// combined with the glyph that is not drawn, go with it.
func TestAWideCharacterCutByTheCropLosesItsMarks(t *testing.T) {
	g := gridOf(10, 2, "a中"+acute)
	snap := g.SnapshotCropped(2, 2)
	if m := snap.Marks(snap.Cells[1]); m != nil {
		t.Fatalf("the blanked wide half kept marks %+q", m)
	}
	frozen := freezeHistory(g.Term()).SnapshotInto(nil, 0, 2, 2)
	if m := frozen.Marks(frozen.Cells[1]); m != nil {
		t.Fatalf("the frozen view's blanked wide half kept marks %+q", m)
	}
	for name, frame := range map[string][]byte{
		"full": g.RenderFrameCropped(2, 2),
		"diff": func() []byte { d, _ := g.RenderDiffCropped(EmptySnapshot(), 2, 2); return d }(),
	} {
		if got := glyphRow(replayFrame(t, frame, 2, 2), 0); got != "a" {
			t.Errorf("%s: client holds %+q, want %+q", name, got, "a")
		}
	}
}

// TestCaptureVisibleReadsWholeGlyphs: the text capture reads a cell as its
// character and the marks that combine with it, as shelld-rs's does.
func TestCaptureVisibleReadsWholeGlyphs(t *testing.T) {
	w := &Window{vt: gridOf(10, 2, "ne"+acute)}
	if got, want := w.CaptureVisible(0), "ne"+acute; got != want {
		t.Fatalf("CaptureVisible = %+q, want %+q", got, want)
	}
}
