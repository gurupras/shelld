//go:build linux || darwin

package daemon

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"
)

// head sends a request and returns the response, which the request helper cannot:
// what is under test here is a header, and that helper reports only the status and
// the body.
func rawRequest(t *testing.T, sock, method, path string) *http.Response {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n", method, path)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Two defects in one header, on a route that has more than one method registered.
//
// It was built by ranging a map, so its value had no order: two identical requests
// answered with "GET,DELETE" and "DELETE,GET", and any assertion on it -- a
// client's or a test's -- was right half the time by coin toss rather than wrong
// outright, which is the shape that survives a suite run and fails in the field.
//
// And it listed only the methods registered, while the router serves HEAD wherever
// GET is registered. So the header said the resource does not accept a method that
// it does accept.
//
// Both are asserted on the same route, because both are properties of the same
// list; the HEAD half is paired with a HEAD request that succeeds, so the header is
// held to what the router does rather than to what this test claims it does.
func TestTheAllowHeaderIsOrderedAndNamesHEAD(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	for _, tc := range []struct {
		path string
		want string
	}{
		// GET and DELETE registered, so a map's order is visible in the answer.
		{"/sessions/s", "DELETE,GET,HEAD"},
		{"/sessions", "GET,HEAD,POST"},
		// One method registered and it is a GET, so HEAD is the whole of what the
		// old list was missing.
		{"/sessions/s/windows/w/capture", "GET,HEAD"},
		// No GET, so nothing is added: a list that always named HEAD would be as
		// wrong as one that never did.
		{"/sessions/s/windows/w/resize", "POST"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// Repeated, because one reading of a map-ordered header is not evidence
			// about its order.
			for range 8 {
				resp := rawRequest(t, sock, "PUT", tc.path)
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("PUT %s: status %d, want 405", tc.path, resp.StatusCode)
				}
				if got := resp.Header.Get("Allow"); got != tc.want {
					t.Fatalf("PUT %s: Allow %q, want %q", tc.path, got, tc.want)
				}
			}
		})
	}

	// The claim the header makes about HEAD, against the router.
	if resp := rawRequest(t, sock, "HEAD", "/sessions/s"); resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /sessions/s: status %d, so naming HEAD in Allow is a claim the router does not honour", resp.StatusCode)
	}
}
