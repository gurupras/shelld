package daemon

import "testing"

func TestPipeSDDL(t *testing.T) {
	const sid = "S-1-5-21-1-2-3-1001"
	for _, c := range []struct {
		elevated, shared bool
		want             string
	}{
		{false, false, "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;S-1-5-21-1-2-3-1001)(A;;GA;;;SY)"},
		{true, false, "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;S-1-5-21-1-2-3-1001)(A;;GA;;;SY)S:(ML;;NW;;;HI)"},
		{false, true, "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;S-1-5-21-1-2-3-1001)(A;;GA;;;SY)(A;;0x12019b;;;AU)"},
		{true, true, "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;S-1-5-21-1-2-3-1001)(A;;GA;;;SY)(A;;0x12019b;;;AU)"},
	} {
		if got := pipeSDDL(sid, c.elevated, c.shared); got != c.want {
			t.Errorf("elevated=%v shared=%v: %s", c.elevated, c.shared, got)
		}
	}
}
