package daemon

import (
	"testing"

	"github.com/gurupras/shelld/vt"
)

// H30: keys naming the same palette index ("3", "03", "+3") are resolved as
// shelld-rs resolves them (routes.rs:1588-1589): among the keys that parse, the
// shortest raw key wins and a tie goes to the lexicographically smaller. Ranging
// over the map picked whichever came last, so the answer changed run to run;
// the repetition is what lets that show.
func TestADuplicatePaletteIndexHasOneWinner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		palette map[string]string
		want    vt.RGB
	}{
		{"the shortest key", map[string]string{"03": "000001", "3": "000002", "003": "000003", "+3": "000004"}, vt.RGB{B: 2}},
		// "+3" and "03" are the same length; '+' sorts before '0'.
		{"a tie, lexicographically", map[string]string{"03": "000001", "+3": "000004", "003": "000003"}, vt.RGB{B: 4}},
		// A key whose colour does not parse is not a candidate, so it cannot win.
		{"only keys that parse", map[string]string{"3": "nope", "03": "000001", "003": "000003"}, vt.RGB{B: 1}},
	} {
		for range 200 {
			entries := map[int]vt.RGB{}
			addPalette(entries, tc.palette)
			if got := entries[3]; len(entries) != 1 || got != tc.want {
				t.Errorf("%s: %v gave %v, want index 3 = %v", tc.name, tc.palette, entries, tc.want)
				break
			}
		}
	}
}
