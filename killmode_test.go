package shelld

import "testing"

func TestParseKillMode(t *testing.T) {
	for _, c := range []struct {
		in   string
		want KillMode
		ok   bool
	}{
		{"", KillTree, true},
		{"tree", KillTree, true},
		{"default", KillTree, true},
		{" Hangup ", KillHangup, true},
		{"tmux", KillHangup, true},
		{"hang-up", KillTree, false},
		{"sigkill", KillTree, false},
	} {
		got, err := ParseKillMode(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseKillMode(%q) = %v, %v; want %v, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

// The default is the stronger kill, with nothing set: a daemon that never reads
// SHELLD_KILL_MODE, and every embedder of this package, gets KillTree.
func TestTheZeroKillModeIsTree(t *testing.T) {
	var zero KillMode
	if zero != KillTree || CurrentKillMode() != KillTree {
		t.Fatalf("zero KillMode = %v, current = %v; want tree", zero, CurrentKillMode())
	}
}
