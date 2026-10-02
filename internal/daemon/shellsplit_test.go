//go:build linux || darwin

package daemon

import (
	"strings"
	"testing"
	"time"
)

// H18: the create route splits a shell line as shelld-rs does. A single-quoted
// -c body reaches sh whole, and an unterminated quote is a 400 in rs's words
// instead of a 200 for an argv of another shape.
func TestCreateWindowSplitsTheShellLineAsShelldRs(t *testing.T) {
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}

	st, _, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"bad","shell":"/bin/sh -c \"x"}`)
	if want := `{"error":"spawn shell /bin/sh -c \"x: unterminated \" quote, so no command line was run"}`; st != 400 || strings.TrimSpace(raw) != want {
		t.Errorf("unterminated quote: %d %s, want 400 %s", st, raw, want)
	}

	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"w","cols":80,"rows":24,"shell":"/bin/sh -c 'echo hi-$((6*7)); exec cat'"}`); st != 200 {
		t.Fatalf("single-quoted body: %d %s", st, raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, v, raw := request(t, sock, "GET", "/sessions/s/windows/w/capture?lines=10", "")
		out, _ := v["output"].(string)
		if strings.Contains(out, "hi-42") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the -c body never ran whole: %s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
