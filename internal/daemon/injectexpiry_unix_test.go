//go:build linux || darwin

package daemon

// The real-daemon half of injectexpiry_test.go, split out because it is the only
// part that needs one: bootWith serves over a unix socket and the window runs a
// real program, where the case next to it drives the handler in process and holds
// on any platform.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The empty case, in injectexpiry_test.go, holds for a handler that returns a hardwired empty array,
// which is the whole of what this route adds. So this one drives a real expiry
// through a real daemon: a one-millisecond deadline and a window whose program
// never draws a prompt, so the scan never reports idle and the payload is never
// deliverable. What the client polling this route receives is the point, so the
// assertions are on the body rather than on Window.InjectExpiries.
//
// cat rather than a shell: the idle pattern is derived from the program being run,
// so a shell's own prompt matches it and the payload would be typed. cat writes
// nothing unprompted, which leaves the scan with no line to match and makes the
// refusal a property of the fixture rather than of a race.
func TestInjectQueueRouteReportsARealExpiry(t *testing.T) {
	stall := uint64(1)
	sock := bootWith(t, func(st *State) { st.StallTimeoutMS = &stall })
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatal(raw)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"w","cols":80,"rows":24,"shell":"/bin/cat"}`); st != 200 {
		t.Fatal(raw)
	}

	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/inject",
		`{"payload":"echo never-42"}`); st != 202 {
		t.Fatalf("inject: %d %s", st, raw)
	}

	// Polled rather than slept: the drain loop notices the payload within one
	// 200ms poll, and the deadline has passed by the time it looks.
	var body string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, _, raw := request(t, sock, "GET", "/sessions/s/windows/w/inject/queue", "")
		if st != 200 {
			t.Fatalf("queue: %d %s", st, raw)
		}
		body = raw
		if strings.Contains(raw, `"expired":[{`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var v struct {
		Expired []struct {
			ExpiredAt int64 `json:"expiredAt"`
			Message   struct {
				ID      string `json:"id"`
				Payload string `json:"payload"`
			} `json:"message"`
			Reason string `json:"reason"`
		} `json:"expired"`
		Length  int               `json:"length"`
		Pending []json.RawMessage `json:"pending"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Expired) != 1 {
		t.Fatalf("the route reported %d expiries after the deadline passed: %s", len(v.Expired), body)
	}
	e := v.Expired[0]
	// The payload is what makes this an expiry report rather than a placeholder:
	// a client has to be able to tell which of the things it sent was dropped.
	if e.Message.Payload != "echo never-42" {
		t.Errorf("the expiry names payload %q: %s", e.Message.Payload, body)
	}
	if e.Message.ID == "" || e.ExpiredAt == 0 {
		t.Errorf("the expiry carries id %q and expiredAt %d: %s", e.Message.ID, e.ExpiredAt, body)
	}
	// not-idle rather than scan-refused: the scan read the screen and it was not
	// the prompt it was configured to recognise.
	if e.Reason != "not-idle" {
		t.Errorf("the expiry reason is %q, want %q: %s", e.Reason, "not-idle", body)
	}
	// And it left the queue: a payload reported as both pending and expired would
	// tell a polling client to keep waiting for something already dropped.
	if v.Length != 0 || len(v.Pending) != 0 {
		t.Errorf("the expired payload is still pending: %s", body)
	}
}
