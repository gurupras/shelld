package ws

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// oldHandshakeOutcome is what Client made of a handshake response when it read
// it with net/http, kept here as the reference the hand-written reader is held
// to. It returns "ok" or the error Client returned.
func oldHandshakeOutcome(raw, key string) string {
	br := bufio.NewReader(strings.NewReader(raw))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return "error: " + err.Error()
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Sprintf("error: websocket: handshake failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return "error: websocket: bad Sec-WebSocket-Accept"
	}
	return "ok"
}

// newHandshakeOutcome runs Client against a peer that answers with raw, in which
// {{A}} stands for the accept value of the key Client sent, and then closes.
func newHandshakeOutcome(t *testing.T, raw string) (outcome, key string) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	keyc := make(chan string, 1)
	go func() {
		defer b.Close()
		br := bufio.NewReader(b)
		k := ""
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				keyc <- ""
				return
			}
			if v, ok := strings.CutPrefix(l, "Sec-WebSocket-Key: "); ok {
				k = strings.TrimSpace(v)
			}
			if l == "\r\n" {
				break
			}
		}
		keyc <- k
		_, _ = b.Write([]byte(strings.ReplaceAll(raw, "{{A}}", acceptKey(k))))
	}()
	c, err := Client(a, "shelld", "/x")
	key = <-keyc
	if err != nil {
		return "error: " + err.Error(), key
	}
	c.Close()
	return "ok", key
}

// Client's handshake reader replaced net/http's ReadResponse so that shellctl
// does not link net/http. Every response shape below is read by both and must
// come out the same: the same success, or the same error text where the old
// reader's text was reachable (a refusal's status and body, a bad accept value).
// Malformed heads must fail under both; their wording is the reader's own.
func TestHandshakeReaderMatchesNetHTTP(t *testing.T) {
	const ok101 = "HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: websocket\r\n"
	cases := []struct {
		name  string
		raw   string
		exact bool // the error text must match too, not only failure
	}{
		{"plain 101", ok101 + "Sec-WebSocket-Accept: {{A}}\r\n\r\n", true},
		{"lower-case header name", ok101 + "sec-websocket-accept: {{A}}\r\n\r\n", true},
		{"padded value", ok101 + "Sec-WebSocket-Accept:   {{A}}  \r\n\r\n", true},
		{"repeated, first wrong", ok101 + "Sec-WebSocket-Accept: nope\r\nSec-WebSocket-Accept: {{A}}\r\n\r\n", true},
		{"repeated, first right", ok101 + "Sec-WebSocket-Accept: {{A}}\r\nSec-WebSocket-Accept: nope\r\n\r\n", true},
		{"missing accept", ok101 + "\r\n", true},
		{"bare LF line ends", "HTTP/1.1 101 Switching Protocols\nUpgrade: websocket\nSec-WebSocket-Accept: {{A}}\n\n", true},
		{"HTTP/1.0 101", "HTTP/1.0 101 Switching Protocols\r\nSec-WebSocket-Accept: {{A}}\r\n\r\n", true},
		{"101 without a reason", "HTTP/1.1 101\r\nSec-WebSocket-Accept: {{A}}\r\n\r\n", true},
		{"404 with Content-Length", "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 30\r\n\r\n{\"error\":\"window not found\"}\n", true},
		{"404 delimited by close", "HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\n\r\nno such window\n", true},
		{"404 without a reason", "HTTP/1.1 404\r\nContent-Length: 4\r\n\r\ngone", true},
		{"400 with an empty body", "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n", true},
		{"200 instead of 101", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi", true},
		{"body longer than the 4096 kept", "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 5000\r\n\r\n" + strings.Repeat("e", 4095) + "|" + strings.Repeat("f", 904), true},
		{"nothing at all", "", false},
		{"not HTTP", "garbage\r\n\r\n", false},
		{"two-digit status", "HTTP/1.1 10 Short\r\n\r\n", false},
		{"header line without a colon", ok101 + "Bogus\r\n\r\n", false},
		{"head cut off", ok101 + "Sec-WebSocket-Accept: {{A}}\r\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, key := newHandshakeOutcome(t, c.raw)
			want := oldHandshakeOutcome(strings.ReplaceAll(c.raw, "{{A}}", acceptKey(key)), key)
			if c.exact {
				if got != want {
					t.Fatalf("hand-written reader: %q\nnet/http reader:     %q", got, want)
				}
				return
			}
			if got == "ok" || want == "ok" {
				t.Fatalf("a malformed head must fail under both readers: new %q, old %q", got, want)
			}
		})
	}
}
