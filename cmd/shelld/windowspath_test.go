//go:build linux || darwin

package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// postJSON creates something on the daemon over its socket. The CLI has no command
// that makes a session, and the names this test needs are exactly the ones a
// command line would be the wrong place to prove anything about, since the point is
// what the client does with a name it already holds.
func postJSON(t *testing.T, sock, path, body string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return daemon.Dial(ctx, sock, 5*time.Second)
		},
	}}
	resp, err := client.Post("http://shelld"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", path, resp.StatusCode)
	}
}

// `shelld windows <SESSION>` interpolates the name straight into a request path,
// and a session name is not a path segment: shelld does not restrict it, and a
// client may name sessions after things like working directories. Unescaped, a
// name holding a slash asked a different route, one holding '?' truncated at the
// query and asked about the prefix, and both answered confidently about the wrong
// session rather than failing — which is worse than an error, because the operator
// reads the answer as being about the session they named.
//
// This runs the real client against the real daemon because that pairing is the
// claim: the escape is only a fix if the router's EscapedPath-plus-per-segment
// unescape puts the name back together. internal/daemon has the same round trip at
// the handler level; this is the half of it that executes cmd/shelld's own code.
func TestWindowsFindsASessionWhoseNameIsNotAPathSegment(t *testing.T) {
	p := newPaths(t)
	p.startRun(t)

	for _, name := range []string{
		"my project",
		"a/b",
		"a?b",
		"a#b",
		"a%b",
		"~/src/shelld",
	} {
		t.Run(name, func(t *testing.T) {
			// The window name is fixed and ordinary: this is about the session
			// segment, and a window that failed to spawn would look like the
			// failure being tested.
			postJSON(t, p.socket, "/sessions", `{"name":`+strconv.Quote(name)+`}`)
			postJSON(t, p.socket, "/sessions/"+url.PathEscape(name)+"/windows",
				`{"name":"w","cols":80,"rows":24,"shell":"/bin/cat"}`)

			code, out := p.shelld(t, nil, "windows", name)
			if code != 0 {
				t.Fatalf("shelld windows %q exited %d: %s", name, code, out)
			}
			if !strings.Contains(out, "w") {
				t.Errorf("shelld windows %q printed %q, want the window listed", name, out)
			}
			// The specific wrong answer the unescaped path produced: the route
			// resolved, so the client reported a session that exists as missing,
			// or listed some other session's windows.
			if strings.Contains(out, "not found") {
				t.Errorf("shelld windows %q: %q", name, out)
			}
		})
	}
}
