package shelld

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// A grid that shrinks below a client's declared size. resolveDeclared caps the
// client's size at the grid's, so the framer sees a new geometry and copies in
// full; this pins that, since a damaged copy at a size larger than the grid
// would leave the rows and columns the grid no longer covers unpadded.
func TestLiveFramerDamageGridShrinkUnderClient(t *testing.T) {
	old := damageFrames
	t.Cleanup(func() { damageFrames = old })
	for _, tc := range []struct {
		name           string
		c0, r0, c1, r1 int
	}{
		{"rows", 40, 20, 40, 10},
		{"cols", 60, 10, 30, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewVtGrid(tc.c0, tc.r0)
			g.Advance([]byte("\x1b[?7l"))
			for i := 0; i < tc.r0; i++ {
				g.Advance([]byte("\x1b[" + strconv.Itoa(i+1) + ";1H" + strings.Repeat("x", tc.c0)))
			}
			var full, dmg LiveFramer
			cc, cr := 70, 30
			step := func(i int) {
				damageFrames = false
				want, _, _ := full.Frame(nil, g, cc, cr, true)
				damageFrames = true
				got, _, _ := dmg.Frame(nil, g, cc, cr, true)
				if !bytes.Equal(got, want) || !snapshotsEqual(dmg.last, full.last) {
					t.Fatalf("step %d: damage %q\nfull %q", i, got, want)
				}
			}
			step(0)
			g.Advance([]byte("y"))
			step(1)
			g.Advance([]byte("z"))
			step(2)
			g.Resize(tc.c1, tc.r1)
			step(3)
			g.Advance([]byte("w"))
			step(4)
		})
	}
}
