//go:build linux || darwin

package daemon

import (
	"bufio"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestNewServerBoundsBothWaysAConnectionCanStall runs a server built exactly as
// serveListener builds it, with only the two durations shortened to something a
// test can wait for, and holds each way a connection can stop making progress to
// being released.
//
// The three states are distinct in the standard library and one bound does not
// cover the other two: ReadHeaderTimeout governs a connection whose request
// headers have not finished arriving, including one that has sent nothing at all,
// while a connection that has already completed a request and gone quiet is
// governed by IdleTimeout alone. Setting only the first leaves the third held
// forever, which is why both are set and both are asserted here.
//
// Released is read as "the server let go", not as a particular status: a header
// timeout answers 408 and closes, an idle timeout just closes, and either way the
// client's next read returns rather than blocking. A still-held connection is the
// one where that read times out with nothing on it.
//
// Because it overwrites both fields, this test says nothing about whether
// newServer sets them: dropping either one from newServer leaves every case here
// passing. What it establishes is that the two bounds between them cover all three
// states, which is the reason both are set. That they are set on the server the
// daemon actually runs is a separate claim, asserted in
// TestServerSetsBothConnectionBounds.
func TestNewServerBoundsBothWaysAConnectionCanStall(t *testing.T) {
	const bound = 250 * time.Millisecond

	// stall opens a connection, puts it in the named state, and reports whether
	// the server was still holding it well after the bound should have fired.
	stall := func(t *testing.T, sock string, prime func(net.Conn, *bufio.Reader)) bool {
		t.Helper()
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		br := bufio.NewReader(c)
		prime(c, br)
		// Four times the bound: long enough that a fired timeout has certainly
		// landed, short enough that a whole table of these stays quick.
		time.Sleep(4 * bound)
		if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, err := br.Read(make([]byte, 64))
		ne, isNet := err.(net.Error)
		return n == 0 && isNet && ne.Timeout()
	}

	for _, tc := range []struct {
		name  string
		prime func(net.Conn, *bufio.Reader)
	}{
		{
			// Connected and silent. Nothing identifies this peer or what it
			// wants, so nothing but a timer can end it.
			name:  "sends nothing at all",
			prime: func(net.Conn, *bufio.Reader) {},
		},
		{
			// A well-formed request line and a header, but never the blank line
			// that ends them, so the handler is never reached and the read
			// inside the standard library's header parse does not return.
			name: "sends headers that never end",
			prime: func(c net.Conn, _ *bufio.Reader) {
				if _, err := c.Write([]byte("GET /sessions HTTP/1.1\r\nHost: shelld\r\n")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// One complete request, answered, then silence on a keep-alive
			// connection. This is the state ReadHeaderTimeout does not reach.
			name: "goes quiet after a completed request",
			prime: func(c net.Conn, br *bufio.Reader) {
				if _, err := c.Write([]byte("GET /sessions HTTP/1.1\r\nHost: shelld\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodGet, "/sessions", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(br, req)
				if err != nil {
					t.Fatal(err)
				}
				// Drained and closed, so the connection returns to idle rather
				// than staying mid-response, which would be a different state.
				resp.Body.Close()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := api(t, map[string][]string{"s": {"w"}})
			srv := newServer(st)
			srv.ReadHeaderTimeout = bound
			srv.IdleTimeout = bound

			ln, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go srv.Serve(ln)
			defer srv.Close()

			if stall(t, ln.Addr().String(), tc.prime) {
				t.Errorf("a connection that %s was still held %v after a %v bound: the server has no way to reclaim the goroutine and descriptor it costs, and nothing else in the daemon ever lets go of it", tc.name, 4*bound, bound)
			}
		})
	}
}

// TestIdleTimeoutDoesNotReachAHijackedConnection is the control for the bound
// above: every WebSocket the daemon serves is a connection the attach handler
// hijacked, and an observer is entitled to hold one open and silent for as long
// as it likes. If IdleTimeout reached a hijacked connection, setting it would
// disconnect every idle observer after two minutes.
//
// No other test in this package would show that. The observer tests all finish
// well inside idleTimeout, so the failure would only appear in a session left
// running longer than any test runs -- which is to say, only in use.
//
// The handler here hijacks and then reads, rather than being the real attach
// route, because what is under test is whose deadline governs a hijacked
// connection. That answer comes from net/http and is the same for any hijacker.
func TestIdleTimeoutDoesNotReachAHijackedConnection(t *testing.T) {
	const bound = 250 * time.Millisecond

	read := make(chan error, 1)
	srv := &http.Server{
		ReadHeaderTimeout: bound,
		IdleTimeout:       bound,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				read <- err
				return
			}
			defer conn.Close()
			if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n"); err != nil {
				read <- err
				return
			}
			if err := brw.Flush(); err != nil {
				read <- err
				return
			}
			// The client stays silent past the bound before sending this.
			_, err = brw.ReadString('\n')
			read <- err
		}),
	}
	ln, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Close()

	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /ws HTTP/1.1\r\nHost: shelld\r\nUpgrade: websocket\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	if status, err := br.ReadString('\n'); err != nil {
		t.Fatalf("upgrade: %v", err)
	} else if status != "HTTP/1.1 101 Switching Protocols\r\n" {
		t.Fatalf("upgrade: %q", status)
	}

	time.Sleep(4 * bound)
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatalf("writing after %v idle: %v -- the server closed a hijacked connection, so setting idleTimeout disconnects idle observers", 4*bound, err)
	}
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("server read after %v idle: %v -- the server's idle bound reached a hijacked connection, so setting idleTimeout disconnects idle observers", 4*bound, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hijacking handler never returned a read result")
	}
}
