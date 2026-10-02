//go:build linux || darwin

package daemon

import (
	"strings"
	"testing"
)

// TestInputTooLargeNamesBothNumbers pins H12's 413 text to shelld-rs's
// InputTooLarge (shelld-core/src/input.rs:131-135): the bytes the push asked for
// and the queue it can never fit, the latter following SHELLD_INPUT_QUEUE_LIMIT.
// The count is the push's, so the Enter after the text is in it: 2000 bytes of
// keys with enter are 2001, as rs counts them.
func TestInputTooLargeNamesBothNumbers(t *testing.T) {
	t.Setenv("SHELLD_INPUT_QUEUE_LIMIT", "1000")
	sock := boot(t)
	openWindow(t, sock, "s")
	const path = "/sessions/s/windows/w/keys"
	keys := strings.Repeat("a", 2000)
	for _, tc := range []struct {
		body, want string
	}{
		{`{"keys":"` + keys + `","enter":true}`, `{"error":"input of 2001 bytes exceeds the 1000-byte input queue"}`},
		{`{"keys":"` + keys + `","enter":false}`, `{"error":"input of 2000 bytes exceeds the 1000-byte input queue"}`},
	} {
		if st, _, raw := request(t, sock, "POST", path, tc.body); st != 413 || raw != tc.want {
			t.Errorf("POST /keys of %d bytes: %d %s, want 413 %s", len(tc.body), st, raw, tc.want)
		}
	}
	// Exactly the limit, Enter included, is admitted: the bound is not off by
	// one in the direction that refuses a write for ever.
	if st, _, raw := request(t, sock, "POST", path, `{"keys":"`+keys[:999]+`"}`); st != 200 {
		t.Errorf("POST /keys of exactly the limit: %d %s, want 200", st, raw)
	}
}
