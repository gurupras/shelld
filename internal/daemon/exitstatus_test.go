//go:build linux || darwin

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptWindow writes an executable script and creates a window running it, so
// a body can hold both quote forms and newlines, which the creation route's
// "shell" command line cannot carry.
func scriptWindow(t *testing.T, sock, session, name, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name+".sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	st, _, raw := request(t, sock, "POST", "/sessions/"+session+"/windows",
		`{"name":"`+name+`","cols":80,"rows":24,"shell":"`+p+`"}`)
	if st != 200 {
		t.Fatalf("create %s: %d %s", name, st, raw)
	}
}

// windowsByName is the window listing keyed by name, with the raw body for
// assertions about the JSON itself.
func windowsByName(t *testing.T, sock, session string) (map[string]map[string]any, string) {
	t.Helper()
	st, v, raw := request(t, sock, "GET", "/sessions/"+session+"/windows", "")
	if st != 200 {
		t.Fatalf("list windows: %d %s", st, raw)
	}
	out := map[string]map[string]any{}
	for _, w := range v["windows"].([]any) {
		m := w.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out, raw
}

// A program can end three ways a supervisor has to tell apart — a clean exit, a
// non-zero exit and a signal — and alive=false is the same answer for all
// three. exitStatus carries the difference, as tmux's pane_dead_status and
// pane_dead_signal do.
//
// The presence of the field is asserted separately from its value, on the raw
// JSON: a running window has to report null rather than omit the key, because a
// client that has to work against older daemons detects support by looking the
// key up, and an omitted key reads as an unsupported daemon.
func TestWindowExitStatusAPI(t *testing.T) {
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	scriptWindow(t, sock, "s", "code", "echo bye\nexit 3\n")
	scriptWindow(t, sock, "s", "signal", "echo bye\nkill -TERM $$\nsleep 5\n")
	scriptWindow(t, sock, "s", "alive", "exec sleep 600\n")

	var wins map[string]map[string]any
	var raw string
	deadline := time.Now().Add(10 * time.Second)
	for {
		wins, raw = windowsByName(t, sock, "s")
		// No cross-field invariant is asserted here. alive=true beside a recorded
		// exitStatus looks like a contradiction and is not one — a background
		// process can hold the terminal open after the shell is reaped — and these
		// three windows have no holder, so a check like that would pass here for a
		// reason that does not generalise. See
		// TestListingReportsAHeldTerminalAsAlive, which constructs the holder.
		if wins["code"]["exitStatus"] != nil && wins["signal"]["exitStatus"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("windows never reported an exit status: %s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, c := range []struct {
		win  string
		want map[string]any
	}{
		{"code", map[string]any{"code": 3.0}},
		{"signal", map[string]any{"code": -1.0, "signal": "SIGTERM"}},
	} {
		got := wins[c.win]["exitStatus"]
		if !equalJSON(got, c.want) {
			t.Errorf("window %s exitStatus = %v, want %v", c.win, got, c.want)
		}
		if wins[c.win]["alive"] != false {
			t.Errorf("window %s is still alive with an exit status", c.win)
		}
	}

	// The running window: the key is there and its value is null. Both halves
	// matter, and only the raw body can tell an explicit null from an absent
	// key — the decoded map holds nil either way.
	if v, ok := wins["alive"]["exitStatus"]; !ok || v != nil {
		t.Errorf("running window exitStatus = %v, present %v; want null and present", v, ok)
	}
	if !strings.Contains(raw, `"exitStatus":null`) {
		t.Errorf("running window omits the exitStatus key: %s", raw)
	}
}

func equalJSON(got, want any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}
