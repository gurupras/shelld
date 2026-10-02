//go:build linux || darwin

package daemon

import "testing"

// H13's queued-gap cap end to end: each Enter carries a 60 s gap the writer is
// still waiting out (the text ahead of it is what starts the gap: a gap is
// measured from the end of the previous write), so a third would take the window's queued pacing past
// shelld-rs's 120 s MAX_QUEUED_GAP (shelld-core input.rs:81, :344-347). It gets
// the same 429 as a full queue: retryable once the gaps ahead are waited.
func TestKeysRefusesPacingPastTheQueuedGapCap(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	const path = "/sessions/s/windows/w/keys"
	const body = `{"keys":"x","enter":true,"enter_delay_ms":60000}`
	for i := 1; i <= 2; i++ {
		if st, _, raw := request(t, sock, "POST", path, body); st != 200 {
			t.Fatalf("Enter %d with a 60 s gap: %d %s, want 200", i, st, raw)
		}
	}
	if st, _, raw := request(t, sock, "POST", path, body); st != 429 || raw != `{"error":"input queue full"}` {
		t.Errorf("a third 60 s gap: %d %s, want 429 {\"error\":\"input queue full\"}", st, raw)
	}
}
