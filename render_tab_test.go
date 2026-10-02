package shelld

import (
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// vt keeps a tab in the blank cell where it started, as alacritty does. The
// renderer used to send that '\t' to clients, where it jumped to the next tab
// stop and shifted the rest of the run. Every frame kind is replayed into a
// client, which must show the snapshot.
func TestRenderTabCells(t *testing.T) {
	green := "\x1b[42m" + strings.Repeat(" ", 20) + "\x1b[0m\x1b[1;1H"
	cases := []struct {
		name          string
		cols, view    int // grid and client columns
		before, after string
	}{
		{"a tab b", 20, 20, "", "a\tb"},
		{"tab at the row start", 20, 20, "", "\tx"},
		{"tab at the row end", 10, 10, "", "12345678\t"},
		{"tab at the crop edge", 20, 6, "", "abcde\tX"},
		{"tabs inside a changed run", 20, 20, green, "\x1b[Ka\tbc\tde"},
	}
	const rows = 3
	for _, tc := range cases {
		for _, mode := range []string{"full", "diff", "paused full", "paused diff"} {
			g := grid(tc.cols, rows, tc.before)
			client := vt.NewTerm(vt.Config{}, tc.view, rows, nil)
			p := vt.NewProcessor()
			p.Advance(client, g.RenderFrameCropped(tc.view, rows))
			prev := g.SnapshotCropped(tc.view, rows)
			g.Advance([]byte(tc.after))
			var cur *GridSnapshot
			switch mode {
			case "full":
				cur = g.SnapshotCropped(tc.view, rows)
				p.Advance(client, g.RenderFullInto(nil, 0, tc.view, rows))
			case "diff":
				cur = g.SnapshotCropped(tc.view, rows)
				p.Advance(client, g.RenderDiffSnapshots(nil, prev, cur, 0))
			default:
				// Push the rows into history and view them paused.
				g.Advance([]byte("\x1b[3;1H\r\n\r\n\r\n"))
				blank := NewVtGrid(tc.view, rows)
				p.Advance(client, blank.RenderFrame())
				prev = blank.Snapshot()
				cur = g.SnapshotInto(nil, rows, tc.view, rows)
				if mode == "paused full" {
					p.Advance(client, g.RenderFullInto(nil, rows, tc.view, rows))
				} else {
					p.Advance(client, g.RenderDiffSnapshots(nil, prev, cur, rows))
				}
			}
			if !strings.Contains(snapshotText(cur), "\t") {
				t.Fatalf("%s (%s): the snapshot holds no tab cell; the case tests nothing", tc.name, mode)
			}
			if d := visibleDiff(clientView(client), cur); d != "" {
				t.Errorf("%s (%s): %s", tc.name, mode, d)
			}
		}
	}
}

func snapshotText(s *GridSnapshot) string {
	var b strings.Builder
	for _, c := range s.Cells {
		b.WriteRune(c.Ch)
	}
	return b.String()
}

// Visible capture returns a tab where tmux 3.7c's capture-pane does for the
// cases below, each of which is written out here because the capture it was taken
// from is not in this repo: a tab
// cell whose cells up to the next multiple of 8 are blank reads as a tab,
// also when the tab stops change after the tab was written; a tab over text
// reads as that text; a tab in the last column reads as nothing.
func TestCaptureVisibleTabs(t *testing.T) {
	lines := []struct{ in, want string }{
		{"a\tb", "a\tb"},
		{"\tx", "\tx"},
		{"a\t", "a\t"},
		{"     xyz\r\tQ", "     xyzQ"},
		{"\t\tz", "\t\tz"},
		{"12345678\r\tw", "12345678w"},
		{"ab\x1b[5Ccd", "ab     cd"},
		{"a\tb\x1b[5G\x1bH", "a\tb"}, // stop added after the tab
		{"a\tb\x1b[3g", "a\tb"},      // stops cleared after the tab
		{strings.Repeat("1", 79) + "\t", strings.Repeat("1", 79)}, // last column
	}
	for _, l := range lines {
		w := NewFakeWindow("w") // 80 columns
		advanceFake(w, l.in)
		if got := w.CaptureVisible(0); got != l.want {
			t.Errorf("%q: got %q, want %q", l.in, got, l.want)
		}
	}
}
