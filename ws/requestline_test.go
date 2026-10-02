package ws

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

// Client assembles the request line by concatenating method, path and version, so
// path is the one field in it that comes from outside. Nothing downstream would
// catch a space or a CR LF in it: a space ends the request target and the rest of
// the path is read as the HTTP version, and a CR LF ends the line so everything
// after it is read as headers, or as a second pipelined request. The callers build
// path by interpolating session and window names, which shelld does not restrict.
func TestClientRefusesAPathItCannotPutInARequestLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"a space", "/sessions/my project/windows/w/attach"},
		{"a CR LF", "/sessions/a\r\nX-Injected: 1\r\n\r\n/windows/w/attach"},
		{"a bare CR", "/sessions/a\rb/windows/w/attach"},
		{"a bare LF", "/sessions/a\nb/windows/w/attach"},
		{"a NUL", "/sessions/a\x00b/windows/w/attach"},
		{"a tab", "/sessions/a\tb/windows/w/attach"},
		{"DEL", "/sessions/a\x7fb/windows/w/attach"},
		{"a C1 control", "/sessions/a\u0085b/windows/w/attach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A closed pipe: if the guard did not fire, the failure would be an
			// I/O error on the connection instead, which reads differently.
			a, b := net.Pipe()
			a.Close()
			b.Close()
			c, err := Client(a, "shelld", tc.path)
			if c != nil {
				c.Close()
			}
			if err == nil {
				t.Fatal("Client accepted the path")
			}
			if !strings.Contains(err.Error(), "must be URL-escaped") {
				t.Errorf("Client: %v, want the escaping error rather than an I/O "+
					"failure: the path was written into the request line", err)
			}
		})
	}

	// WHICH character the message names, not merely that a message exists. The arms
	// above all assert the error's category, and the category was never in doubt --
	// the rejection is right either way -- so none of them can see the one part of
	// this error an operator can act on. IndexFunc returns a BYTE offset while the
	// predicate judges a RUNE, so indexing the string yields a C1's 0xC2 lead byte,
	// and %q renders that byte as a rune: 'Â', which is not a character the path
	// contains. The C1 arm is the one this bites, which is the arm the 0x80..0x9f
	// range exists for.
	t.Run("the message names the offending rune and not its lead byte", func(t *testing.T) {
		const path = "/sessions/a\u0085b/windows/w/attach"
		const at = 11 // the byte offset of the C1 in path, counted above
		if path[at] != 0xc2 {
			t.Fatalf("premise: path[%d] = %#x, want the 0xc2 lead byte of the C1; the "+
				"needles below are built from that assumption", at, path[at])
		}
		_, err := Client(nil, "shelld", path)
		if err == nil {
			t.Fatal("Client accepted the path")
		}
		msg := err.Error()
		if want := fmt.Sprintf("%q", '\u0085'); !strings.Contains(msg, want) {
			t.Errorf("Client: %v, want the message to name %s, the rune the guard rejected", err, want)
		}
		if lead := fmt.Sprintf("%q", rune(path[at])); strings.Contains(msg, lead) {
			t.Errorf("Client: %v, names %s: that is the UTF-8 lead byte at the offset, "+
				"rendered as a rune, and the path does not contain that character", err, lead)
		}
		// The offset itself is a byte offset and the message says so; it was never
		// the wrong half.
		if want := fmt.Sprintf("at byte %d", at); !strings.Contains(msg, want) {
			t.Errorf("Client: %v, want %q", err, want)
		}
	})

	// The check has to come before anything is written, because a request line
	// already on the wire cannot be recalled. A nil connection is the assertion:
	// the guard returns without touching it.
	t.Run("nothing is written before the check", func(t *testing.T) {
		if _, err := Client(nil, "shelld", "/a b"); err == nil {
			t.Error("Client accepted the path")
		}
	})

	// The control, and the reason the guard is a rejection rather than a scrub: a
	// correctly escaped path holds percent triplets for exactly these bytes, and it
	// must go through and reach a real handshake.
	t.Run("an escaped path still handshakes", func(t *testing.T) {
		srv := echoServer(t)
		defer srv.Close()
		nc, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		c, err := Client(nc, "shelld", "/sessions/my%20project%0D/windows/w%2Fx/attach")
		if err != nil {
			t.Fatalf("Client rejected an escaped path: %v", err)
		}
		c.Close()
	})
}
