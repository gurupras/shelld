package daemon

import (
	"testing"
	"time"
)

// H35: a window ended by a signal the platform has no name for still reports
// the signal, as shelld-rs does, rather than an exitStatus without the key,
// which reads as a normal exit. 36 is a realtime signal, so Linux-only.
func TestExitStatusNamesAnUnnamedSignalByNumber(t *testing.T) {
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	scriptWindow(t, sock, "s", "rt", "kill -36 $$\nsleep 5\n")
	deadline := time.Now().Add(10 * time.Second)
	for {
		wins, raw := windowsByName(t, sock, "s")
		if got := wins["rt"]["exitStatus"]; got != nil {
			if want := map[string]any{"code": -1.0, "signal": "signal 36"}; !equalJSON(got, want) {
				t.Fatalf("exitStatus = %v, want %v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the window never reported an exit status: %s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
