package vt

import (
	"strings"
	"testing"
)

// BenchmarkEDPaths separates the two halves of what a full-screen erase costs, since
// an aggregate over repeated erases is not either of them. A benchmark that replays
// ESC[H ESC[2J many times over a 200x50 screen spends about 90% of its time in
// usedLines: after the first erase the viewport is empty, so every later one walks
// all 10,000 cells to find nothing and then blanks rows whose occupancy hint is
// already zero. Attributing such a benchmark's movement to the fill is wrong twice —
// the fill barely runs, and the walk it does run is not what a change to the fill
// touches.
//
// used-lines-empty is that walk alone, on an empty viewport with history below it,
// which is its worst case: it reaches the bottom having tested every cell.
//
// reset-default and reset-colored are the blanking alone, with the occupancy hints
// written back each iteration so that every row fills its whole width. The default
// arm is the memclr that recorded sessions take — the 91 full-screen erases the
// three codex casts issue all carry a default template — and the colored arm is the
// one fillBlank's doubling is for. Both are needed: they move in opposite directions
// under changes to the fill, and a single arm would report either half of that as the
// whole.
func BenchmarkEDPaths(b *testing.B) {
	const (
		lines   = 50
		columns = 200
	)
	// Full lines, so that the walk and the fill both cover every cell of every row.
	full := strings.Repeat(strings.Repeat("x", columns-1)+"\r\n", lines-1) + strings.Repeat("x", columns-1)

	b.Run("used-lines-empty", func(b *testing.B) {
		term := NewTerm(Config{ScrollingHistory: 1000}, columns, lines, nil)
		p := NewProcessor()
		// The erase scrolls the filled lines into history, which is what leaves the
		// viewport empty with somewhere below it for the walk to stop.
		p.Advance(term, []byte(full+"\x1b[H\x1b[2J"))
		g := term.grid
		n := 0
		for b.Loop() {
			n += g.usedLines()
		}
		if n != 0 {
			b.Fatalf("usedLines summed to %d over the run, so the viewport was not empty: this arm is timing a walk that stops early", n)
		}
	})

	for _, arm := range []struct {
		name string
		// pre paints the screen before the run; the SGR in it is what decides which of
		// fillBlank's two bodies the arm times.
		pre string
		// wantColored is that decision, stated separately from the bytes that are meant
		// to produce it, so that an SGR which stops taking effect -- or one edited out --
		// reds here instead of quietly turning this arm into a second copy of the other.
		wantColored bool
	}{
		{"reset-default", "", false},
		{"reset-colored", "\x1b[41m", true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			term := NewTerm(Config{}, columns, lines, nil)
			p := NewProcessor()
			p.Advance(term, []byte(arm.pre+full))
			g := term.grid
			for b.Loop() {
				// reset leaves occ at 0, and a row whose hint says nothing was written
				// fills nothing, so without this the second iteration onwards would time
				// an empty span. MarkOccupied is the same hint a caller writing through
				// Cell has to set, and costs one compare per row.
				for line := 0; line < lines; line++ {
					g.Row(line).MarkOccupied(columns)
				}
				g.resetRegion(0, lines)
			}
			// The read is of what the fill wrote, after the run, and not of what the
			// paint wrote before it.
			if colored := g.Row(0).Cell(columns-1).bg != 0; colored != arm.wantColored {
				b.Fatalf("%s ran against a background-colored grid = %v, want %v: it is timing the wrong half of fillBlank", arm.name, colored, arm.wantColored)
			}
		})
	}
}
