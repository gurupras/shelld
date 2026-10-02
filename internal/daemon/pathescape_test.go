//go:build linux || darwin

package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A session or window name is whatever the caller named it — shelld restricts
// neither — so the clients that build "/sessions/" + name + "/windows" are turning
// data into path structure. The server half was already right: router.go reads
// EscapedPath and unescapes each segment after splitting it. This is the pair of
// them, because an escape on one side is only a fix if the other side undoes it,
// and either half alone is worse than neither.
//
// Both server-side halves were measured by removing them. Dropping the
// PathUnescape reds all eight names below; reading r.URL.Path instead of
// EscapedPath reds four — the slash and the percent, which are the two bytes whose
// meaning the decode changes, while a space, '?', '#' and non-ASCII survive it.
// So the unescape is the half this test constrains tightly and the EscapedPath
// read is the half it constrains only through those four.
func TestAnEscapedNameRoundTripsToTheNameTheClientMeant(t *testing.T) {
	// Each of these breaks a different part of an unescaped path. The slash asks a
	// different route, '?' truncates at the query so the route answers about the
	// prefix, '%' is the one that makes a double-escape visible, and the space
	// reaches the hand-built request line.
	//
	// '#' is here as a name a client can hold, not as one of those: measured
	// against the mutant, it round-trips unescaped too, because a request line is
	// parsed by url.ParseRequestURI, which keeps '#' in the path rather than
	// splitting a fragment off it. It is a case, not a detector.
	for _, name := range []string{
		"my project",
		"a/b",
		"a?b",
		"a#b",
		"a%b",
		"a%2Fb",
		"日本語",
		"a b/c?d#e%f",
	} {
		t.Run(name, func(t *testing.T) {
			_, h := api(t, map[string][]string{name: {name}})
			path := "/sessions/" + url.PathEscape(name) +
				"/windows/" + url.PathEscape(name) + "/capture"
			st, body := call(t, h, "GET", path, "")
			if st != http.StatusOK {
				t.Errorf("GET %s = %d %q, want 200: the escaped name did not "+
					"round-trip to %q", path, st, body, name)
			}
		})
	}

	// The other half, which is what makes the above a measurement of the escaping
	// rather than of the router being lenient: unescaped, these same names name
	// something else. Only the ones that still parse as a URL are here — a raw
	// space would fail httptest.NewRequest's own parse.
	for _, tc := range []struct {
		name, path string
	}{
		{"a/b", "/sessions/a/b/windows/a/b/capture"},
		{"a?b", "/sessions/a?b/windows/a?b/capture"},
	} {
		_, h := api(t, map[string][]string{tc.name: {tc.name}})
		if st, _ := call(t, h, "GET", tc.path, ""); st == http.StatusOK {
			t.Errorf("GET %s (unescaped) = 200, so this test cannot tell the "+
				"escaping apart from a router that matches anything", tc.path)
		}
	}
}

// HTTPRoundTrip concatenates its request line the same way ws.Client does and has
// the same hazard, so it carries the same guard. The distinction that matters is
// that the check comes before the dial: a caller cannot tell a malformed path from
// an unreachable daemon if both arrive as a connection failure, and one of those is
// a bug in the caller.
func TestHTTPRoundTripRefusesAPathItCannotPutInARequestLine(t *testing.T) {
	const unreachable = "/nonexistent/shelld-pathescape-test.sock"
	for _, tc := range []struct {
		name string
		path string
	}{
		{"a space", "/sessions/my project/windows"},
		{"a CR LF", "/sessions/a\r\nX-Injected: 1\r\n\r\n/windows"},
		{"a bare LF", "/sessions/a\nb/windows"},
		{"a NUL", "/sessions/a\x00b/windows"},
		{"DEL", "/sessions/a\x7fb/windows"},
		{"a C1 control", "/sessions/a\u0085b/windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, body, err := HTTPRoundTrip(unreachable, "GET", tc.path, time.Second)
			if err == nil {
				t.Fatalf("HTTPRoundTrip = %d %q, nil: the path was accepted", st, body)
			}
			if !strings.Contains(err.Error(), "must be URL-escaped") {
				t.Errorf("HTTPRoundTrip: %v, want the escaping error", err)
			}
			// Before the dial, so the caller learns which of the two things is
			// wrong. This socket does not exist, so a guard that ran after Dial
			// would report it as unreachable instead.
			var nr *NotReachableError
			if errors.As(err, &nr) {
				t.Errorf("HTTPRoundTrip: %v, want the check to precede the dial", err)
			}
		})
	}

	// WHICH character the message names. The arms above assert the error's category,
	// which was never in doubt, so none of them sees the part an operator acts on:
	// IndexFunc returns a byte offset while the predicate judges a rune, so indexing
	// the string yields a C1's 0xC2 lead byte and %q renders it as 'Â'. ws.Client
	// carries the same guard and requestline_test.go pins the same property there.
	t.Run("the message names the offending rune and not its lead byte", func(t *testing.T) {
		const path = "/sessions/a\u0085b/windows"
		const at = 11 // the byte offset of the C1 in path
		if path[at] != 0xc2 {
			t.Fatalf("premise: path[%d] = %#x, want the 0xc2 lead byte of the C1", at, path[at])
		}
		_, _, err := HTTPRoundTrip(unreachable, "GET", path, time.Second)
		if err == nil {
			t.Fatal("HTTPRoundTrip accepted the path")
		}
		msg := err.Error()
		if want := fmt.Sprintf("%q", '\u0085'); !strings.Contains(msg, want) {
			t.Errorf("HTTPRoundTrip: %v, want the message to name %s", err, want)
		}
		if lead := fmt.Sprintf("%q", rune(path[at])); strings.Contains(msg, lead) {
			t.Errorf("HTTPRoundTrip: %v, names %s: the UTF-8 lead byte at that offset "+
				"rendered as a rune, which the path does not contain", err, lead)
		}
	})

	// The control: a properly escaped path gets past the guard and fails for the
	// real reason, which is the socket.
	t.Run("an escaped path reaches the dial", func(t *testing.T) {
		_, _, err := HTTPRoundTrip(unreachable, "GET", "/sessions/my%20project/windows", time.Second)
		var nr *NotReachableError
		if !errors.As(err, &nr) {
			t.Errorf("HTTPRoundTrip: %v, want a NotReachableError: the guard "+
				"rejected a correctly escaped path", err)
		}
	})
}
