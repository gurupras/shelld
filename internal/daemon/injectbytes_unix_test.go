//go:build linux || darwin

package daemon

import (
	"strings"
	"testing"
)

// H16's inject cap follows SHELLD_INPUT_QUEUE_LIMIT, as the window's input queue
// does: an injection is typed into that queue, so a payload it can never hold
// would only be accepted to expire as too-large later.
func TestInjectCapFollowsTheInputQueueLimit(t *testing.T) {
	t.Setenv("SHELLD_INPUT_QUEUE_LIMIT", "1000")
	sock := boot(t)
	openWindow(t, sock, "s")
	const want = `{"error":"payload of 2000 bytes exceeds the 1000-byte inject queue"}`
	body := `{"payload":"` + strings.Repeat("a", 2000) + `"}`
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/inject", body); st != 413 || raw != want {
		t.Errorf("2000 bytes with SHELLD_INPUT_QUEUE_LIMIT=1000: %d %s, want 413 %s", st, raw, want)
	}
}
