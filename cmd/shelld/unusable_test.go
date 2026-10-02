package main

// What a socket says and what the CLI concludes from it. sessions and windows
// are the two commands that read a JSON body, and before this they collapsed
// every way that could fail into exit 4 with "shelld not reachable" -- including
// a socket that had completed a round trip and answered 200. A caller that
// treats 4 as "no daemon is running, start one" would start a second daemon
// against the one that had just replied.
//
// The daemon here is a raw byte server rather than an httptest.Server, for two
// reasons. net/http will not write most of these replies: a body that stops
// mid-chunk, a Content-Length longer than the body, a status line that is not
// one. And httptest.Server speaks TCP, while shelld's transport is a unix socket
// or a named pipe -- daemon.Listen and daemon.Dial are the pair under test on
// both, so the fake binds with the product's own listener and the cases below
// run as written on Windows too -- executed there on 2026-09-25, windows/amd64,
// every case passing over a named pipe, owner check included.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// sessionsBody is a complete, valid answer to GET /sessions, used both as the
// control and as the part of a truncated reply that did arrive.
const sessionsBody = `{"sessions":[{"name":"only-chunk-one"}]}`

// fakeAddr is an address of the shape this platform's transport uses. The unix
// half does not use t.TempDir because that names the directory after the test,
// and a socket path is limited to about 108 bytes: the names in this file's
// table would be most of the budget on their own.
func fakeAddr(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\shelld-unusable-` + daemon.NewNonce()
	}
	dir, err := os.MkdirTemp("", "shelld-u")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// fakeDaemon binds an address and answers every request with reply, byte for
// byte, then hangs up. An empty reply means it accepts the connection and closes
// without writing anything, which is a case of its own: something holds the
// socket, and it is not a daemon.
//
// It reads the request head before replying. The client sends `Connection:
// close` and no body, so the blank line ends it -- but replying and closing
// without reading can reset the connection on some transports before the
// client's own write has landed, which would turn every case here into the same
// broken-pipe error.
func fakeDaemon(t *testing.T, reply string) string {
	t.Helper()
	addr := fakeAddr(t)
	ln, err := daemon.Listen(addr, daemon.ListenOptions{})
	if err != nil {
		t.Fatalf("bind the fake daemon at %s: %v", addr, err)
	}
	var handlers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the cleanup below closed the listener
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				serveRaw(conn, reply)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-accepted
		handlers.Wait()
	})
	return addr
}

func serveRaw(conn net.Conn, reply string) {
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}
	}
	if reply != "" {
		_, _ = io.WriteString(conn, reply)
	}
}

// TestSessionsAndWindowsTellNoDaemonFromAnUnusableAnswer is the whole exit-code
// contract of the two JSON commands. 4 means nothing is there; 6 means something
// is, and its answer could not be used -- which is a different thing to do about
// it, so the two cannot share a number.
func TestSessionsAndWindowsTellNoDaemonFromAnUnusableAnswer(t *testing.T) {
	// A second chunk header promising 60 bytes that never arrive. The first chunk
	// is complete and valid on its own, so a client that ignored the error would
	// print a one-session list and exit 0 -- a short answer presented as the whole
	// answer.
	truncatedChunked := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		fmt.Sprintf("%x\r\n%s\r\n3c\r\n", len(sessionsBody), sessionsBody)

	notFound := "HTTP/1.1 404 Not Found\r\nContent-Length: 2\r\n\r\n{}"

	windowsBody := `{"windows":[{"name":"w0","pid":42,"alive":true}]}`
	ok := func(body string) string {
		return "HTTP/1.1 200 OK\r\nContent-Length: " + fmt.Sprint(len(body)) + "\r\n\r\n" + body
	}

	for _, tc := range []struct {
		name    string
		reply   string // what the socket writes back; "" closes without writing
		nothing bool   // nothing is bound to the address at all
		args    []string
		code    int
		wantOut string
		wantErr string
		// A phrase the message must NOT contain. With truncation and a body that
		// will never parse sharing exit 6, the sentence is the only thing that
		// separates "ask again, it may work" from "these bytes are not JSON", so a
		// message that said "ended before it was complete" for both would pass
		// every wantErr here while telling the operator to retry for ever.
		wantNotErr string
	}{
		{
			name:    "nothing is listening",
			nothing: true,
			args:    []string{"sessions"},
			code:    4,
			wantErr: "shelld not reachable",
		}, {
			name:    "a complete answer",
			reply:   ok(sessionsBody),
			args:    []string{"sessions"},
			code:    0,
			wantOut: "only-chunk-one\n",
		}, {
			// A status line may end after the code: RFC 9112 section 4 makes the
			// reason phrase optional, and an intermediary is allowed to drop it. It
			// is a 200 and must be read as one. A client that looked for the text
			// " 200 " rather than parsing the code would call this not-200 and,
			// now, exit 6 on a session list that arrived intact.
			name:    "a 200 with no reason phrase",
			reply:   "HTTP/1.1 200\r\nContent-Length: " + fmt.Sprint(len(sessionsBody)) + "\r\n\r\n" + sessionsBody,
			args:    []string{"sessions"},
			code:    0,
			wantOut: "only-chunk-one\n",
		}, {
			name:    "the body stops mid-chunk",
			reply:   truncatedChunked,
			args:    []string{"sessions"},
			code:    exitUnusableAnswer,
			wantErr: "the response body ended before it was complete",
		}, {
			name:    "the body is shorter than its Content-Length",
			reply:   "HTTP/1.1 200 OK\r\nContent-Length: 200\r\n\r\n" + sessionsBody,
			args:    []string{"sessions"},
			code:    exitUnusableAnswer,
			wantErr: "the response body ended before it was complete",
		}, {
			name:       "a complete body that is not JSON",
			reply:      "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nnot me",
			args:       []string{"sessions"},
			code:       exitUnusableAnswer,
			wantErr:    "could not use its answer",
			wantNotErr: "ended before it was complete",
		}, {
			name:    "a status the command does not expect",
			reply:   "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n",
			args:    []string{"sessions"},
			code:    exitUnusableAnswer,
			wantErr: "status 500",
		}, {
			// Someone else's server on the socket path or the pipe name. The status
			// stays 0 here, exactly as it does when nothing is listening, which is
			// why the two are told apart by whether the dial succeeded.
			name:    "a reply that is not HTTP",
			reply:   "hello, i am not a web server\r\n",
			args:    []string{"sessions"},
			code:    exitUnusableAnswer,
			wantErr: "could not use its answer",
		}, {
			name:    "the socket accepts and hangs up",
			reply:   "",
			args:    []string{"sessions"},
			code:    exitUnusableAnswer,
			wantErr: "could not use its answer",
		}, {
			// windows has one status of its own, and it has to stay ahead of the
			// unusable-answer branch: a session that does not exist is an answer.
			name:    "windows on a session the daemon does not have",
			reply:   notFound,
			args:    []string{"windows", "nope"},
			code:    1,
			wantErr: `session "nope" not found`,
		}, {
			// The same status with a body that did not all arrive is not that
			// answer: nothing established that the 404 was about the session rather
			// than about a path this client got wrong.
			name:    "windows on a 404 whose body stops early",
			reply:   "HTTP/1.1 404 Not Found\r\nContent-Length: 200\r\n\r\n{}",
			args:    []string{"windows", "nope"},
			code:    exitUnusableAnswer,
			wantErr: "the response body ended before it was complete",
		}, {
			// The 404 branch has to stay a test for 404 and not for "anything that
			// is not 200". Widened, it reports a daemon that is failing as a session
			// that does not exist: exit 1 and "not found" for a 500, which sends
			// whoever reads it looking for a session name instead of at the daemon.
			// Every other windows case here survives that widening, so this is the
			// one that refuses it.
			name:       "windows on a status the command does not expect",
			reply:      "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n",
			args:       []string{"windows", "s"},
			code:       exitUnusableAnswer,
			wantErr:    "status 500",
			wantNotErr: "not found",
		}, {
			name:    "windows on a complete answer",
			reply:   ok(windowsBody),
			args:    []string{"windows", "s"},
			code:    0,
			wantOut: "w0\tpid=42\talive\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var addr string
			if tc.nothing {
				// An address of the right shape that nothing ever bound, so the dial
				// is what fails. On unix that is a path in a directory that exists.
				addr = fakeAddr(t)
			} else {
				addr = fakeDaemon(t, tc.reply)
			}
			args := append([]string{"--socket", addr}, tc.args...)
			stdout, stderr, code := runShelld(t, args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d\nstdout: %q\nstderr: %q", code, tc.code, stdout, stderr)
			}
			if stdout != tc.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.wantErr)
			}
			if tc.wantNotErr != "" && strings.Contains(stderr, tc.wantNotErr) {
				t.Errorf("stderr = %q, want it not to say %q", stderr, tc.wantNotErr)
			}
			if tc.wantErr == "" && stderr != "" {
				t.Errorf("stderr = %q, want nothing: this case succeeded", stderr)
			}
			// The defect itself, stated once for every case that is not case 1: a
			// socket that answered must not be reported as one that could not be
			// reached, whatever went wrong with the answer.
			if !tc.nothing && strings.Contains(stderr, "not reachable") {
				t.Errorf("stderr = %q: %s completed a round trip, so a caller reading this starts a second daemon against the one that just replied", stderr, addr)
			}
		})
	}
}

// TestNotReachableIsTheDialFailureAndNothingElse pins the signal the exit codes
// above are built on, at the level of the transport rather than the CLI. A status
// code cannot carry it: it is 0 for a reply whose header never parsed as well as
// for a socket nothing is listening on.
func TestNotReachableIsTheDialFailureAndNothingElse(t *testing.T) {
	var unreachable *daemon.NotReachableError

	gone := fakeAddr(t)
	status, _, err := daemon.HTTPRoundTrip(gone, "GET", "/sessions", time.Second)
	if err == nil {
		t.Fatalf("a round trip to %s, which nothing has ever bound, reported status %d and no error", gone, status)
	}
	if !errors.As(err, &unreachable) {
		t.Errorf("round trip to an unbound %s: err = %v (%T), want a *daemon.NotReachableError", gone, err, err)
	} else if unreachable.Addr != gone {
		t.Errorf("NotReachableError.Addr = %q, want %q: it is what the message names", unreachable.Addr, gone)
	}

	held := fakeDaemon(t, "hello, i am not a web server\r\n")
	status, _, err = daemon.HTTPRoundTrip(held, "GET", "/sessions", time.Second)
	if err == nil {
		t.Fatalf("a round trip that got %q back reported status %d and no error", "hello, i am not a web server", status)
	}
	if status != 0 {
		t.Errorf("status = %d, want 0: there was no status line to read", status)
	}
	if errors.As(err, &unreachable) {
		t.Errorf("a socket that accepted a connection and answered reported %v as not reachable, which is what sends the CLI to exit 4", err)
	}
}
