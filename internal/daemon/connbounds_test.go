//go:build linux || darwin

package daemon

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// withBounds shortens the connection bounds for one test. The server and its listener
// read them when they are built, so this must run before boot.
func withBounds(t *testing.T, conns int, permit, body time.Duration) {
	t.Helper()
	oldConns, oldPermit, oldBody := maxConnections, permitTimeout, bodyTimeout
	maxConnections, permitTimeout, bodyTimeout = conns, permit, body
	t.Cleanup(func() { maxConnections, permitTimeout, bodyTimeout = oldConns, oldPermit, oldBody })
}

// heldRequest opens a connection and completes one request on it, leaving it open and
// idle: a keep-alive connection, which holds its connection slot like any other.
func heldRequest(t *testing.T, sock string) net.Conn {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	fmt.Fprint(nc, "GET /health HTTP/1.1\r\nHost: unix\r\n\r\n")
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
	if err != nil {
		t.Fatalf("setup: a request below the cap got no answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	_ = nc.SetReadDeadline(time.Time{})
	return nc
}

// health sends GET /health on a fresh connection and returns the raw response.
func health(t *testing.T, sock string, within time.Duration) (*http.Response, string, error) {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprint(nc, "GET /health HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n")
	_ = nc.SetReadDeadline(time.Now().Add(within))
	resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

// The connection cap, as shelld-rs has it (serve.rs MAX_CONNECTIONS, PERMIT_TIMEOUT,
// refuse_saturated): a connection over the cap waits for a slot, and one that waits
// out the permit timeout is told so in rs's exact bytes, with a Retry-After of one
// permit timeout in whole seconds rounded up. A slot that frees serves the next client.
func TestAConnectionOverTheCapWaitsThenIsRefused(t *testing.T) {
	withBounds(t, 2, 300*time.Millisecond, bodyTimeout)
	sock := boot(t)
	a := heldRequest(t, sock)
	heldRequest(t, sock)

	start := time.Now()
	resp, body, err := health(t, sock, 5*time.Second)
	if err != nil {
		t.Fatalf("a connection over the cap got no answer: %v", err)
	}
	waited := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable ||
		resp.Header.Get("Content-Type") != "application/json" ||
		resp.Header.Get("Retry-After") != "1" ||
		body != `{"error":"daemon is at its connection limit, retry in a moment"}` {
		t.Errorf("a connection over the cap was answered %d %v %q, want 503 application/json Retry-After 1 and rs's body", resp.StatusCode, resp.Header, body)
	}
	if waited < 250*time.Millisecond {
		t.Errorf("refused after %v, without waiting out the %v permit timeout for a slot", waited, permitTimeout)
	}

	a.Close()
	resp, _, err = health(t, sock, 5*time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("after a held connection closed, a new one got %v %v, want 200", resp, err)
	}
}

// A connection that arrives while the daemon is full and is given a slot within the
// permit timeout is served, not refused: the wait is a wait.
func TestAConnectionOverTheCapIsServedWhenASlotFrees(t *testing.T) {
	withBounds(t, 1, 3*time.Second, bodyTimeout)
	sock := boot(t)
	a := heldRequest(t, sock)
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.Close()
	}()
	resp, _, err := health(t, sock, 5*time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a connection that a slot freed for got %v %v, want 200", resp, err)
	}
}

// Upgraded attaches give their connection slot back at the 101, so they are bounded by
// the attach cap and not this one: with room for two connections, three attaches all
// upgrade and a plain request is still served beside them. rs measured 300 concurrent
// attaches all 101 against its cap of 256.
func TestUpgradedAttachesDoNotHoldConnectionSlots(t *testing.T) {
	withBounds(t, 2, 300*time.Millisecond, bodyTimeout)
	sock := boot(t)
	openWindow(t, sock, "s")
	for i := range 3 {
		resp, body, _ := upgradeRequest(t, sock, "/sessions/s/windows/w/attach")
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("attach %d answered %d %q under a connection cap of 2, want 101", i+1, resp.StatusCode, body)
		}
	}
	resp, body, err := health(t, sock, 5*time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health beside three attaches got %v %q %v, want 200", resp, body, err)
	}
}

// A request body that goes silent is given up on after the body timeout and answered
// as the route answers a body it could not read, rather than holding the handler for
// ever (rs serve.rs BODY_TIMEOUT). The bound is on silence, not on duration: a body
// that keeps arriving, however slowly, is read to the end.
func TestABodyThatGoesSilentIsGivenUpOn(t *testing.T) {
	withBounds(t, maxConnections, permitTimeout, 300*time.Millisecond)
	sock := boot(t)
	const body = `{"name":"slow"}`
	send := func(t *testing.T, gap time.Duration, stopAfter int) (*http.Response, error) {
		nc, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		fmt.Fprintf(nc, "POST /sessions HTTP/1.1\r\nHost: unix\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
		for i := 0; i < len(body) && i < stopAfter; i++ {
			time.Sleep(gap)
			if _, err := nc.Write([]byte{body[i]}); err != nil {
				return nil, err
			}
		}
		_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		return http.ReadResponse(bufio.NewReader(nc), nil)
	}

	t.Run("a stalled body gets the route's answer", func(t *testing.T) {
		resp, err := send(t, 0, 3)
		if err != nil {
			t.Fatalf("a body that stalled got no answer: the handler is still waiting on it (%v)", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("a body that stalled was answered %d, want the route's 400", resp.StatusCode)
		}
	})
	t.Run("a body that dribbles steadily is read to the end", func(t *testing.T) {
		// 15 bytes at 100 ms is 1.5 s in total, five times the bound, with no gap
		// reaching it.
		resp, err := send(t, 100*time.Millisecond, len(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("a body that kept arriving was answered %d, want 200: the bound is on silence, not on how long a body takes", resp.StatusCode)
		}
	})
}

// A saturation verdict goes stale: once the permit timeout has run out with no one
// arriving, the accept loop is parked in Accept with the verdict in hand, and the
// daemon can empty before the next client comes. That client is served, because the
// slot is tested again after the accept -- answering it 503 would be a lie about an
// idle daemon.
func TestAClientArrivingAfterTheDaemonEmptiedIsServed(t *testing.T) {
	withBounds(t, 1, 200*time.Millisecond, bodyTimeout)
	sock := boot(t)
	a := heldRequest(t, sock)
	// Past the permit timeout, so the accept loop has stopped waiting for a slot.
	time.Sleep(600 * time.Millisecond)
	a.Close()
	// The slot is given back when the server sees the close.
	time.Sleep(200 * time.Millisecond)
	resp, body, err := health(t, sock, 5*time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a client arriving at an emptied daemon got %v %q %v, want 200", resp, body, err)
	}
}
