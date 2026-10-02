//go:build linux || darwin

package daemon

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// upgradeRequest sends a WebSocket upgrade for path and returns the response, with its
// body read, and the connection, which stays open when the answer was a 101.
func upgradeRequest(t *testing.T, sock, path string) (*http.Response, string, net.Conn) {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	_, err = nc.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: shelld\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(nc), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ = io.ReadAll(resp.Body)
	}
	_ = nc.SetReadDeadline(time.Time{})
	return resp, string(body), nc
}

// The attach cap, at a size a test can reach: two attaches are held, observers counted
// with the rest, and the third is told so with an ordinary 503 before any 101, in
// exactly shelld-rs's bytes (routes.rs:1647-1648). A window that does not exist and a
// query that does not parse are still answered for what they are, because the slot is
// taken after both; and an attach that ends gives its slot back.
func TestTheAttachCapAnswers503BeforeTheUpgrade(t *testing.T) {
	limit := uint64(2)
	sock := bootWith(t, func(s *State) { s.MaxAttaches = &limit })
	openWindow(t, sock, "s")
	const path = "/sessions/s/windows/w/attach"

	resp, _, first := upgradeRequest(t, sock, path)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("setup: first attach answered %d", resp.StatusCode)
	}
	if resp, _, _ := upgradeRequest(t, sock, path+"?observer=1"); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("setup: an observer below the cap answered %d", resp.StatusCode)
	}

	resp, body, _ := upgradeRequest(t, sock, path)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Content-Type") != "application/json" || body != `{"error":"too many attaches"}` {
		t.Errorf("an attach over the cap answered %d %q %q, want 503 application/json {\"error\":\"too many attaches\"}", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _, _ := upgradeRequest(t, sock, path+"?observer=1"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an observer over the cap answered %d, want 503: observers hold a socket like any attach", resp.StatusCode)
	}
	if resp, _, _ := upgradeRequest(t, sock, "/sessions/s/windows/nope/attach"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing window at the cap answered %d, want 404: the window is resolved before the slot", resp.StatusCode)
	}
	if resp, _, _ := upgradeRequest(t, sock, path+"?observer=maybe"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad query at the cap answered %d, want 400: the query is parsed before the slot", resp.StatusCode)
	}

	first.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, _, _ := upgradeRequest(t, sock, path)
		if resp.StatusCode == http.StatusSwitchingProtocols {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an attach that ended never gave its slot back: still %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SHELLD_MAX_ATTACHES, read once by NewState: unset or empty is the default of 256,
// 0 is no cap, and anything that is not a count is ignored for the default rather than
// for no cap, so a typo cannot remove the bound.
func TestMaxAttachesFromTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value  string
		set    bool
		limit  uint64
		capped bool
	}{
		{set: false, limit: 256, capped: true},
		{value: "", set: true, limit: 256, capped: true},
		{value: "0", set: true, capped: false},
		{value: "3", set: true, limit: 3, capped: true},
		{value: "-1", set: true, limit: 256, capped: true},
		{value: "1O", set: true, limit: 256, capped: true},
	} {
		// Setenv first in both arms, so the variable is restored after the test.
		t.Setenv("SHELLD_MAX_ATTACHES", tc.value)
		if !tc.set {
			os.Unsetenv("SHELLD_MAX_ATTACHES")
		}
		limit, capped := NewState("/tmp/does-not-need-to-exist.sock").maxAttaches()
		if capped != tc.capped || (capped && limit != tc.limit) {
			t.Errorf("SHELLD_MAX_ATTACHES=%q (set %v): limit %d capped %v, want %d capped %v", tc.value, tc.set, limit, capped, tc.limit, tc.capped)
		}
	}
}

// The boundary itself, which the cap test above reaches only at one size.
func TestAttachSlotAdmitsExactlyTheLimit(t *testing.T) {
	limit := uint64(3)
	s := &State{MaxAttaches: &limit}
	for i := range 3 {
		if !s.acquireAttach() {
			t.Fatalf("slot %d of 3 refused", i+1)
		}
	}
	if s.acquireAttach() {
		t.Fatal("a fourth slot was admitted under a cap of 3")
	}
	s.releaseAttach()
	if !s.acquireAttach() {
		t.Fatal("a released slot was not admitted again")
	}
	zero := uint64(0)
	u := &State{MaxAttaches: &zero}
	for range 1000 {
		if !u.acquireAttach() {
			t.Fatal("a cap of 0 refused an attach; 0 means uncapped")
		}
	}
}
