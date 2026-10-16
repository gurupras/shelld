//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDaemon listens on a unix socket and answers every connection with
// respond, after reading the whole request. It returns the socket path and a
// channel that carries each request as it arrived on the wire.
func fakeDaemon(t *testing.T, respond func(c net.Conn)) (string, <-chan string) {
	t.Helper()
	// A short directory keeps the socket path inside sun_path's 108 bytes.
	dir, err := os.MkdirTemp("", "sw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "f.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqs := make(chan string, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				var head strings.Builder
				length := 0
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						return
					}
					head.WriteString(l)
					if v, ok := strings.CutPrefix(l, "Content-Length: "); ok {
						length, _ = strconv.Atoi(strings.TrimSpace(v))
					}
					if l == "\r\n" {
						break
					}
				}
				body := make([]byte, length)
				if _, err := io.ReadFull(br, body); err != nil {
					return
				}
				reqs <- head.String() + string(body)
				respond(c)
			}()
		}
	}()
	return path, reqs
}

// answer writes raw and closes, as a daemon answering HTTP/1.0 does.
func answer(raw string) func(net.Conn) {
	return func(c net.Conn) { _, _ = c.Write([]byte(raw)) }
}

const okCapture = `{"output":"l1\nl2  ","truncated":false}`

// Every body framing a server may use for an answer to HTTP/1.0 is read whole:
// Content-Length, the connection's end, a body arriving a byte at a time, and a
// 1.1 server that keeps the connection open after a Content-Length body, which
// the client must not wait on.
func TestOneShotReadsEveryBodyFraming(t *testing.T) {
	big := strings.Repeat("x", 8<<20)
	bigJSON := `{"output":"` + big + `","truncated":false}`
	for _, c := range []struct {
		name    string
		respond func(net.Conn)
		want    string
	}{
		{"content-length", answer("HTTP/1.0 200 OK\r\nContent-Length: " + strconv.Itoa(len(okCapture)) + "\r\n\r\n" + okCapture), "l1\nl2  \n"},
		{"delimited by close", answer("HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + okCapture), "l1\nl2  \n"},
		{"lower-case header, bare LF", answer("HTTP/1.1 200 OK\ncontent-length: " + strconv.Itoa(len(okCapture)) + "\n\n" + okCapture), "l1\nl2  \n"},
		{"a byte at a time", func(c net.Conn) {
			for _, b := range []byte("HTTP/1.0 200 OK\r\nContent-Length: " + strconv.Itoa(len(okCapture)) + "\r\n\r\n" + okCapture) {
				_, _ = c.Write([]byte{b})
				time.Sleep(time.Millisecond)
			}
		}, "l1\nl2  \n"},
		{"kept open after Content-Length", func(c net.Conn) {
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(okCapture)) + "\r\n\r\n" + okCapture))
			_, _ = io.Copy(io.Discard, c) // until the client hangs up
		}, "l1\nl2  \n"},
		{"8 MiB, delimited by close", answer("HTTP/1.0 200 OK\r\n\r\n" + bigJSON), big + "\n"},
		{"8 MiB, content-length", answer("HTTP/1.0 200 OK\r\nContent-Length: " + strconv.Itoa(len(bigJSON)) + "\r\n\r\n" + bigJSON), big + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, _ := fakeDaemon(t, c.respond)
			out, errs, code := shellctl(t, "--socket", sock, "capture", "s", "w")
			if code != 0 || out != c.want || errs != "" {
				t.Fatalf("exit %d, stdout %d bytes %.80q, stderr %q; want 0 and %d bytes", code, len(out), out, errs, len(c.want))
			}
		})
	}
}

// A response the client cannot read exits 1 with nothing on stdout, never a
// panic (which would exit 2, the usage code) and never a guess at the body.
func TestOneShotRefusesUnreadableResponses(t *testing.T) {
	for _, c := range []struct{ name, raw, want string }{
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", "unsupported Transfer-Encoding"},
		{"body shorter than Content-Length", "HTTP/1.0 200 OK\r\nContent-Length: 100\r\n\r\n" + okCapture, "unexpected EOF"},
		{"Content-Length beyond memory", "HTTP/1.0 200 OK\r\nContent-Length: 9000000000000000000\r\n\r\n" + okCapture, "unexpected EOF"},
		{"negative Content-Length", "HTTP/1.0 200 OK\r\nContent-Length: -1\r\n\r\n" + okCapture, "bad Content-Length"},
		{"no response", "", "read response: EOF"},
		{"not HTTP", "SSH-2.0-OpenSSH\r\n\r\n", "malformed status line"},
		{"head cut off", "HTTP/1.0 200 OK\r\nContent-Length: 5\r\n", "read response headers"},
		{"200 without output", "HTTP/1.0 200 OK\r\n\r\n{\"status\":\"ok\"}", `response has no "output"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, _ := fakeDaemon(t, answer(c.raw))
			out, errs, code := shellctl(t, "--socket", sock, "capture", "s", "w")
			if code != 1 || out != "" || !strings.Contains(errs, c.want) || strings.Contains(errs, "panic") {
				t.Fatalf("exit %d, stdout %q, stderr %.300q; want 1, nothing, and %q", code, out, errs, c.want)
			}
		})
	}
}

// A refusal names the status and the daemon's error, from a JSON body or, when
// the body is not the daemon's JSON, from the body's text.
func TestOneShotNamesTheRefusal(t *testing.T) {
	for _, c := range []struct{ name, raw, want string }{
		{"JSON error", "HTTP/1.0 500 Internal Server Error\r\nContent-Type: application/json\r\n\r\n{\"error\":\"boom\"}\n", "GET /sessions/s/windows/w/capture: HTTP 500: boom\n"},
		{"text body", "HTTP/1.0 400 Bad Request\r\nContent-Type: text/plain\r\n\r\n  bad lines \n", "GET /sessions/s/windows/w/capture: HTTP 400: bad lines\n"},
		{"empty body", "HTTP/1.0 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n", "GET /sessions/s/windows/w/capture: HTTP 503: \n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, _ := fakeDaemon(t, answer(c.raw))
			out, errs, code := shellctl(t, "--socket", sock, "capture", "s", "w")
			if code != 1 || out != "" || errs != "Error: "+c.want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out, errs, "Error: "+c.want)
			}
		})
	}
}

// The request on the wire: an escaped path, a query of the route's own names,
// and for send-keys a JSON body that carries KEYS byte for byte.
func TestOneShotRequestOnTheWire(t *testing.T) {
	const ok = "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\n{}"
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"health", []string{"health"}, "GET /health HTTP/1.0\r\nHost: shelld\r\n\r\n"},
		{"capture options", []string{"capture", "--visible", "--lines", "7", "--max-bytes=9", "--json", "s", "w"},
			"GET /sessions/s/windows/w/capture?lines=7&max_bytes=9&mode=visible HTTP/1.0\r\nHost: shelld\r\n\r\n"},
		{"scroll options", []string{"scroll", "--offset", "3", "--lines", "0", "--json", "s", "w"},
			"GET /sessions/s/windows/w/scroll?lines=0&offset=3 HTTP/1.0\r\nHost: shelld\r\n\r\n"},
		{"names escaped", []string{"capture", "--json", "a b/%c", "x?y#é"},
			"GET /sessions/a%20b%2F%25c/windows/x%3Fy%23%C3%A9/capture HTTP/1.0\r\nHost: shelld\r\n\r\n"},
		{"send-keys", []string{"send-keys", "s", "w", "ls -l"},
			keysReq("{\"keys\":\"ls -l\",\"enter\":true}")},
		{"send-keys control bytes", []string{"send-keys", "--no-enter", "s", "w", "\x03\x1b[A\t\"\\é"},
			keysReq("{\"keys\":\"\\u0003\\u001b[A\\t\\\"\\\\é\",\"enter\":false}")},
		{"send-keys empty, with delay", []string{"send-keys", "--enter-delay-ms", "250", "s", "w", ""},
			keysReq("{\"keys\":\"\",\"enter\":true,\"enter_delay_ms\":250}")},
		{"send-keys after --", []string{"send-keys", "s", "w", "--", "-n"},
			keysReq("{\"keys\":\"-n\",\"enter\":true}")},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, reqs := fakeDaemon(t, answer(ok))
			_, errs, code := shellctl(t, append([]string{"--socket", sock}, c.args...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			select {
			case got := <-reqs:
				if got != c.want {
					t.Fatalf("request\n%q\nwant\n%q", got, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no request arrived")
			}
		})
	}
}

// keysReq is the request send-keys makes for a JSON body.
func keysReq(body string) string {
	return "POST /sessions/s/windows/w/keys HTTP/1.0\r\nHost: shelld\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// KEYS that are not UTF-8 cannot travel as JSON text without being changed, so
// they are refused before anything is sent rather than typed as U+FFFD.
func TestSendKeysRefusesInvalidUTF8(t *testing.T) {
	sock, reqs := fakeDaemon(t, answer("HTTP/1.0 200 OK\r\n\r\n"))
	_, errs, code := shellctl(t, "--socket", sock, "send-keys", "s", "w", "a\xffb")
	if code != 2 || !strings.Contains(errs, "not valid UTF-8") {
		t.Fatalf("exit %d, stderr %q; want a usage error (2) naming UTF-8", code, errs)
	}
	select {
	case got := <-reqs:
		t.Fatalf("a request was sent: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// A daemon that accepts and never answers ends the exchange at the bound, with
// an error naming it, instead of holding the caller forever.
func TestOneShotGivesUpOnAWedgedDaemon(t *testing.T) {
	sock, _ := fakeDaemon(t, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
	defer func(d time.Duration) { oneShotTimeout = d }(oneShotTimeout)
	oneShotTimeout = 300 * time.Millisecond
	start := time.Now()
	var out bytes.Buffer
	err := oneShot(&out, nil, sock, "health", nil, nil)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v against a bound of %v", took, oneShotTimeout)
	}
	var te interface{ Timeout() bool }
	if !errors.As(err, &te) || !te.Timeout() || !strings.Contains(err.Error(), "did not answer within 300ms") || out.Len() != 0 {
		t.Fatalf("err %v, output %q; want the deadline named and no output", err, out.String())
	}
}

// The bound is on the exchange, not on each read: a daemon slower than nothing
// but inside the bound is still read.
func TestOneShotWaitsForASlowDaemon(t *testing.T) {
	sock, _ := fakeDaemon(t, func(c net.Conn) {
		time.Sleep(500 * time.Millisecond)
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\n{\"status\":\"ok\"}"))
	})
	defer func(d time.Duration) { oneShotTimeout = d }(oneShotTimeout)
	oneShotTimeout = 3 * time.Second
	var out bytes.Buffer
	if err := oneShot(&out, nil, sock, "health", nil, nil); err != nil || out.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("err %v, output %q", err, out.String())
	}
}
