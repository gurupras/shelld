//go:build linux || darwin

package daemon

import (
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// ATTACK 10: the real client path. A browser bridge that
// relays an attach to a web terminal does this before it opens the
// WebSocket, for EVERY viewer:
//
//	backend.resizeWindow(session, window, initialCols, initialRows)   // POST /resize
//	openUnixSocketWebSocket(ep.socketPath, ep.path)                   // then attach
//
// POST /resize is a PIN in shelld (Window.Resize sets pinned/pinCols/pinRows)
// and there is no unpin route. So a watcher going through the bridge sizes the
// shared grid with a mechanism ?observer=1 does not touch, and the change
// outlives the watcher.
func TestAttackObserverBridgePreResizePin(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	agent := newScreen(t, agentCh, 100, 30)

	_ = agentConn.WriteMessage(ws.OpBinary, marker(90))
	agent.await("agent marker", 5*time.Second, func() bool {
		for _, n := range agent.runs('0') {
			if n >= 20 {
				return true
			}
		}
		return false
	})

	// A watcher opens the browser terminal at 200x50. This is what the bridge
	// does, verbatim: pre-resize over REST, then attach.
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":200,"rows":50}`); st != 200 {
		t.Fatalf("bridge pre-resize: %d %s", st, raw)
	}
	obsConn, _ := attachObserverAt(t, sock, "s", 200, 50)
	defer obsConn.Close()
	time.Sleep(400 * time.Millisecond)

	got := effectiveSize(t, sock, "s")
	t.Logf("after a read-only watcher opened the browser terminal: effective size %v (agent declared 100x30)", got)
	if got != [2]int{200, 50} {
		t.Fatalf("expected the watcher's REST pre-resize to pin the grid to [200 50], got %v", got)
	}

	// The agent's program is now laid out for 200 columns while the agent's own
	// client renders only 100 of them.
	_ = agentConn.WriteMessage(ws.OpBinary, marker(190))
	agent.await("wide marker", 5*time.Second, func() bool {
		for _, n := range agent.runs('0') {
			if n >= 100 {
				return true
			}
		}
		return false
	})
	agent.drain(300 * time.Millisecond)
	t.Logf("agent zero-runs after the watcher arrived: %v (100 = its whole width, so the 190-column line is truncated)", agent.runs('0'))

	// And it does not go back: the watcher closes the tab, the pin stays.
	obsConn.Close()
	time.Sleep(400 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{200, 50} {
		t.Fatalf("pin cleared itself: %v", got)
	}
	t.Log("watcher gone, grid still [200 50]: there is no unpin route")
}
