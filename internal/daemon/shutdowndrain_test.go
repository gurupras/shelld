//go:build linux || darwin

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// serveCancellable serves the API like bootWith, but hands back the cancel and a
// channel closed when ServeListener has returned, so a test can shut the daemon down
// and watch what that does.
func serveCancellable(t *testing.T) (sock string, cancel context.CancelFunc, returned <-chan struct{}) {
	t.Helper()
	dir, err := os.MkdirTemp("", "shelld")
	if err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "s.sock")
	ln, err := listen(sock, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(sock)
	st.Exit = func(int) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = ServeListener(ctx, ln, st)
		close(done)
	}()
	t.Cleanup(func() {
		st.Mgr.KillAll()
		cancel()
		<-done
		os.RemoveAll(dir)
	})
	return sock, cancel, done
}

// readClose reads c until its close frame, and reports the close the daemon sent.
func readClose(c *ws.Conn, within time.Duration) (*ws.CloseError, error) {
	_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(within))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *ws.CloseError
		if errors.As(err, &ce) {
			return ce, nil
		}
		return nil, err
	}
}

// Every attach is told the daemon is going away, with shelld-rs's 1001 "daemon
// shutting down" (attach.rs going_away), including one that has not sent its opening
// frame yet: that client is parked in the handshake's first read, where nothing else
// would ever reach it.
func TestShutdownTellsEveryAttachTheDaemonIsGoingAway(t *testing.T) {
	sock, cancel, returned := serveCancellable(t)
	openWindow(t, sock, "s")
	silent := openWS(t, sock, "s")
	attached := openWS(t, sock, "s")
	if err := attached.WriteMessage(ws.OpText, resizeCtl(80, 24)); err != nil {
		t.Fatal(err)
	}
	// Its first frame, so this client is past the handshake and in the read loop.
	if _, _, err := attached.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	cancel()
	for name, c := range map[string]*ws.Conn{"an attached client": attached, "a client that never sent a frame": silent} {
		ce, err := readClose(c, 5*time.Second)
		if err != nil {
			t.Errorf("%s got no close frame on shutdown: %v", name, err)
			continue
		}
		if ce.Code != ws.CloseGoingAway || ce.Reason != "daemon shutting down" {
			t.Errorf("%s was closed with %d %q, want 1001 \"daemon shutting down\"", name, ce.Code, ce.Reason)
		}
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListener did not return after shutdown")
	}
	t.Logf("shutdown returned after %v", time.Since(start))
}

// An HTTP request in flight when the daemon is told to stop is finished, not cut: its
// body is still arriving, and the daemon waits for the response before it returns, as
// shelld-rs's drain does (serve.rs DRAIN_TIMEOUT). The wait is bounded: a request that
// never finishes is cut when the 2 s drain runs out.
func TestShutdownDrainsAnInFlightRequestWithinTheDrain(t *testing.T) {
	const body = `{"name":"x"}`
	sendHead := func(t *testing.T, sock string) net.Conn {
		t.Helper()
		nc, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { nc.Close() })
		fmt.Fprintf(nc, "POST /sessions HTTP/1.1\r\nHost: unix\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:5])
		return nc
	}

	t.Run("a request that finishes inside the drain is answered", func(t *testing.T) {
		sock, cancel, returned := serveCancellable(t)
		nc := sendHead(t, sock)
		// Let the handler start reading the body before the shutdown.
		time.Sleep(200 * time.Millisecond)
		cancel()
		time.Sleep(300 * time.Millisecond)
		select {
		case <-returned:
			t.Fatal("ServeListener returned with a request still in flight, so the daemon would exit before answering it")
		default:
		}
		if _, err := io.WriteString(nc, body[5:]); err != nil {
			t.Fatal(err)
		}
		_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
		if err != nil {
			t.Fatalf("the in-flight request got no response: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the in-flight request was answered %d, want 200", resp.StatusCode)
		}
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("ServeListener did not return once the in-flight request was answered")
		}
	})

	t.Run("a request that never finishes is cut at the drain", func(t *testing.T) {
		sock, cancel, returned := serveCancellable(t)
		nc := sendHead(t, sock)
		time.Sleep(200 * time.Millisecond)
		start := time.Now()
		cancel()
		select {
		case <-returned:
		case <-time.After(drainTimeout + 3*time.Second):
			t.Fatalf("ServeListener had not returned %v after shutdown, past the %v drain", time.Since(start), drainTimeout)
		}
		took := time.Since(start)
		if took < drainTimeout-100*time.Millisecond {
			t.Errorf("ServeListener returned after %v, before the %v drain was up", took, drainTimeout)
		}
		_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := nc.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the stalled connection is still open after the drain (read: %v)", err)
		}
	})
}
