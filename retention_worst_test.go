package shelld

import (
	"bytes"
	"fmt"
	"testing"
)

// The corpus measures recorded agent output, whose lines are short, so it is
// reassuring about exactly the case that was never at risk. This measures the
// case that is: a window whose every line is tens of KiB, where the byte budget
// binds instead of the line count.
//
// The line lengths straddle the chunk-tiling boundary deliberately. A line is
// stored without its terminator, so a line of lineLen bytes occupies lineLen-1,
// and a chunk holds floor(sbMaxChunk / stored) of them — the rest of the chunk is
// slack. Slack is therefore sbMaxChunk / (stored × floor(sbMaxChunk / stored)),
// which is 1.00x whenever the stored line divides a chunk and jumps to its worst,
// 2.00x, one byte past half a chunk. Each pair below is a dividing length and the
// length one byte longer, so a change that moves the tiling shows as a change in
// this table rather than as one row quietly crossing its own floor.
//
// Measured at sbDefaultBudget, capacity 10 000, one window at steady state. The
// slack column is the arithmetic above and the run reproduces it to two places:
//
//	line     stored   per chunk   held/text   lines retained
//	  4 097    4 096          16       1.00x            2 045
//	  4 098    4 097          15       1.07x            1 918
//	 21 846   21 845           3       1.00x              383
//	 21 847   21 846           2       1.51x              255
//	 32 769   32 768           2       1.00x              255
//	 32 770   32 769           1       2.00x              128
//	 65 537   65 536           1       1.00x              128
//	131 073      cut           1       1.00x              128
//
// So a long-line program's history costs the budget and nothing more, and what it
// gives up is retention: 128 lines of a 64 KiB redraw against the 10 000 the line
// count alone would have allowed. Against 4119cbd, the store before lines were
// joined, that trade is what bounds a window at all — the old store evicted by
// line count only, so 10 000 lines of a 64 KiB redraw were 625 MiB per window, and
// a 50-window daemon of 32 KiB-line output held 15.3 GiB.
func TestScrollbackRetentionWorstCase(t *testing.T) {
	const capacity = 10_000
	budget := int64(sbDefaultBudget)

	// A window whose lines are long keeps far fewer of them, but it must keep
	// enough for a capture to be useful. These floors are the measured values
	// less a margin; they are here so that a change making the store hold, say,
	// fifty lines of a long-line program is a failure rather than a silent
	// degradation of what an agent can see of its own output.
	for _, c := range []struct {
		lineLen, minLines int
		wantSlack         float64
	}{
		{(4 << 10) + 1, 1_800, 1.00}, // 16 to a chunk
		{(4 << 10) + 2, 1_700, 1.07}, // 15, and a sixteenth wasted
		{(64<<10)/3 + 1, 340, 1.00},  // 3 to a chunk
		{(64<<10)/3 + 2, 230, 1.50},  // 2
		{(32 << 10) + 1, 230, 1.00},  // 2 to a chunk
		{(32 << 10) + 2, 110, 2.00},  // 1: the worst slack there is
		{sbMaxLine + 1, 110, 1.00},   // 1 to a chunk, filling it
		{2*sbMaxLine + 1, 110, 1.00}, // cut at the cap into whole chunks
	} {
		t.Run(fmt.Sprint(c.lineLen), func(t *testing.T) {
			line := append(bytes.Repeat([]byte("x"), c.lineLen-1), '\n')
			sb := NewScrollback(capacity)
			peak := 0
			// Four times the budget's worth, so eviction has reached a steady
			// state rather than being measured while history is still filling.
			for range 4 * int(budget/int64(c.lineLen)) {
				sb.Write(line)
				held, _ := sb.heldAndSpares()
				peak = max(peak, held)
			}
			held, spares := sb.heldAndSpares()
			retained := len(sb.LastN(sb.Len()))
			slack := float64(held) / float64(max(retained, 1))
			t.Logf("line %d B: %d lines retained holding %d B; held %d B (%.2fx text, "+
				"tiling predicts %.2fx), peak %d B (%.1f%% of budget %d), spares %d B",
				c.lineLen, sb.Len(), retained, held, slack, c.wantSlack,
				peak, 100*float64(peak)/float64(budget), budget, spares)

			if int64(peak) > budget+sbMaxChunk {
				t.Errorf("peak held %d B is over budget %d + one chunk: the bound that "+
					"replaced the old store's unbounded growth does not hold", peak, budget)
			}
			if sb.Len() < c.minLines {
				t.Errorf("only %d lines of %d B retained, want at least %d: the byte budget "+
					"is costing more history than it did when this was measured",
					sb.Len(), c.lineLen, c.minLines)
			}
			// The slack a line's length implies is arithmetic, so it is asserted
			// rather than logged: a store that stopped packing lines into chunks,
			// or started sizing a chunk to the line, would show up here.
			if slack < c.wantSlack-0.06 || slack > c.wantSlack+0.06 {
				t.Errorf("chunks hold %.2fx the retained text, and this line length tiles a "+
					"chunk to %.2fx: the chunk arithmetic has changed", slack, c.wantSlack)
			}
		})
	}
}
