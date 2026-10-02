package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gurupras/shelld"
)

// H16 over HTTP, with the default caps: a payload that can never fit the
// window's inject queue is a 413 naming the limit, and one that does not fit
// beside what is already queued is the 429 a full queue of messages gets. The
// figures are the ones measured against shelld-rs (DEVIATIONS §3c): 16 MiB + 1,
// and a second 9 MiB behind a first. A fake window runs no delivery loop, so
// nothing drains underneath.
func TestInjectRefusesBytesTheQueueCannotHold(t *testing.T) {
	st, h := api(t, map[string][]string{"s": {"w"}})
	// The queues share a process-wide budget, so give back what this one holds
	// rather than leave 9 MiB of it to every later test.
	t.Cleanup(func() { st.Mgr.Session("s").Window("w").InjectQueue().Close() })
	const path = "/sessions/s/windows/w/inject"
	body := func(n int) string { return `{"payload":"` + strings.Repeat("a", n) + `"}` }
	over := shelld.DefaultInputQueueLimit + 1
	want413 := fmt.Sprintf(`{"error":"payload of %d bytes exceeds the %d-byte inject queue"}`, over, shelld.DefaultInputQueueLimit)
	if st, got := call(t, h, "POST", path, body(over)); st != 413 || got != want413 {
		t.Errorf("a payload one byte past the cap: %d %.200s, want 413 %s", st, got, want413)
	}
	const nine = 9 << 20
	if st, got := call(t, h, "POST", path, body(nine)); st != 202 {
		t.Fatalf("a first 9 MiB payload: %d %.200s, want 202", st, got)
	}
	if st, got := call(t, h, "POST", path, body(nine)); st != 429 || got != `{"error":"queue full"}` {
		t.Errorf("a second 9 MiB payload: %d %.200s, want 429 queue full", st, got)
	}
}
