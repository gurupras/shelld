package ws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testUpgrade is a minimal copy of wsserver.Upgrade's handshake, because
// wsserver imports this package and an in-package test cannot import it back;
// wsserver's own tests run Client against the real Upgrade.
func testUpgrade(w http.ResponseWriter, r *http.Request) (*Conn, bool) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		http.Error(w, "not a websocket handshake", http.StatusBadRequest)
		return nil, false
	}
	nc, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return nil, false
	}
	if _, err := nc.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n")); err != nil {
		nc.Close()
		return nil, false
	}
	_ = nc.SetDeadline(time.Time{})
	return NewServerConn(nc, brw.Reader), true
}

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := testUpgrade(w, r)
		if !ok {
			return
		}
		defer c.Close()
		for {
			op, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(op, msg); err != nil {
				return
			}
		}
	}))
}

func dial(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	nc, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Client(nc, "shelld", "/echo")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEchoSizes(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	c := dial(t, srv)
	defer c.Close()
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 3 << 20} {
		data := bytes.Repeat([]byte{byte(n)}, n)
		if err := c.WriteMessage(OpBinary, data); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != OpBinary || !bytes.Equal(got, data) {
			t.Fatalf("size %d: op=%v len=%d err=%v", n, op, len(got), err)
		}
	}
	if err := c.WriteMessage(OpText, []byte("\x01{\"resize\":{}}")); err != nil {
		t.Fatal(err)
	}
	if op, got, _ := c.ReadMessage(); op != OpText || string(got) != "\x01{\"resize\":{}}" {
		t.Fatalf("text echo %v %q", op, got)
	}
}

// rawFrame builds a masked client frame.
func rawFrame(fin bool, op Opcode, payload []byte) []byte {
	return frame(true, fin, op, payload)
}

// frame builds a frame with the mask bit set either way, so a test can send the
// same frame masked and unmasked and differ in nothing else. The mask key is
// fixed rather than random: nothing here depends on it being unpredictable, and a
// fixed key keeps a failure reproducible.
func frame(mask, fin bool, op Opcode, payload []byte) []byte {
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	var maskBit byte
	if mask {
		maskBit = 0x80
	}
	out := []byte{b0}
	switch n := len(payload); {
	case n < 126:
		out = append(out, maskBit|byte(n))
	default:
		out = append(out, maskBit|126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	}
	if !mask {
		return append(out, payload...)
	}
	key := [4]byte{1, 2, 3, 4}
	out = append(out, key[:]...)
	p := append([]byte(nil), payload...)
	maskBytes(key, p)
	return append(out, p...)
}

func TestFragmentsPingAndClose(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	c := dial(t, srv)
	defer c.Close()
	nc := c.UnderlyingConn()
	// Fragmented text with a ping interleaved.
	var b []byte
	b = append(b, rawFrame(false, OpText, []byte("hel"))...)
	b = append(b, rawFrame(true, OpPing, []byte("p"))...)
	b = append(b, rawFrame(true, OpContinuation, []byte("lo"))...)
	if _, err := nc.Write(b); err != nil {
		t.Fatal(err)
	}
	// The server's reply to our ping is a pong, consumed transparently.
	op, got, err := c.ReadMessage()
	if err != nil || op != OpText || string(got) != "hello" {
		t.Fatalf("fragmented: %v %q %v", op, got, err)
	}
	if err := c.SendClose(); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseNormal {
		t.Fatalf("close echo: %v", err)
	}
}

func TestInvalidUTF8TextClosesWith1007(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	c := dial(t, srv)
	defer c.Close()
	if _, err := c.UnderlyingConn().Write(rawFrame(true, OpText, []byte{0xff, 0xfe})); err != nil {
		t.Fatal(err)
	}
	_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseInvalidData {
		t.Fatalf("want close 1007, got %v", err)
	}
}

// TestMaskBitMustMatchTheSenderDirection covers both halves of RFC 6455's masking
// rule, which is about who sent a frame rather than how it is encoded: a client
// masks every frame, a server masks none, and each side fails the connection on a
// frame masked the wrong way.
//
// Each half sends the same opcode and the same payload twice, differing in the
// mask bit alone, and requires the correctly-masked one to be delivered. Without
// that control the test would pass against a connection that rejects this frame
// for some other reason -- a bad length, an opcode it does not implement -- and
// would not show that the mask bit is what was refused.
//
// The two halves assert different errors, and the difference is the point. When
// the server refuses, our client learns about it by reading the server's close
// frame, so the error is a CloseError carrying 1002. When our client refuses, it
// detected the violation itself and never asked anyone, so the error is the local
// ErrProtocol. Asserting the specific error in each direction is what pins which
// side did the refusing; a test that accepted any error would pass if the
// connection merely broke.
func TestMaskBitMustMatchTheSenderDirection(t *testing.T) {
	t.Run("server refuses an unmasked client frame", func(t *testing.T) {
		srv := echoServer(t)
		defer srv.Close()
		c := dial(t, srv)
		defer c.Close()
		// The control first: masked, so it must round-trip. It also proves the
		// frame below is well formed in every respect except its mask bit.
		if _, err := c.UnderlyingConn().Write(frame(true, true, OpBinary, []byte("hi"))); err != nil {
			t.Fatal(err)
		}
		_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(5 * time.Second))
		if op, got, err := c.ReadMessage(); err != nil || op != OpBinary || string(got) != "hi" {
			t.Fatalf("masked frame: op=%v payload=%q err=%v, want a binary %q -- if a masked frame is not served, the refusal below is not about masking", op, got, err, "hi")
		}
		if _, err := c.UnderlyingConn().Write(frame(false, true, OpBinary, []byte("hi"))); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage()
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Code != CloseProtocolError {
			t.Fatalf("unmasked frame: payload=%q err=%v, want close %d -- an unmasked client frame must fail the connection, not be decoded as if the mask bit were advisory", got, err, CloseProtocolError)
		}
	})

	t.Run("client refuses a masked server frame", func(t *testing.T) {
		// A server that writes frames straight to the transport, since this
		// package's own WriteMessage correctly never masks on the server side and
		// so cannot produce the frame under test.
		send := make(chan []byte, 2)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := testUpgrade(w, r)
			if !ok {
				return
			}
			defer c.Close()
			for b := range send {
				if _, err := c.UnderlyingConn().Write(b); err != nil {
					return
				}
			}
		}))
		defer srv.Close()
		c := dial(t, srv)
		defer c.Close()
		_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(5 * time.Second))
		// The control, as above: unmasked is what a server may send.
		send <- frame(false, true, OpBinary, []byte("hi"))
		if op, got, err := c.ReadMessage(); err != nil || op != OpBinary || string(got) != "hi" {
			t.Fatalf("unmasked frame: op=%v payload=%q err=%v, want a binary %q -- if an unmasked server frame is not read, the refusal below is not about masking", op, got, err, "hi")
		}
		send <- frame(true, true, OpBinary, []byte("hi"))
		_, got, err := c.ReadMessage()
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("masked frame: payload=%q err=%v, want %v -- a masked server frame must fail the connection here rather than be unmasked and delivered", got, err, ErrProtocol)
		}
		close(send)
	})

	t.Run("the mask bit is checked before the declared length", func(t *testing.T) {
		// A frame can break both rules at once, and then which one is reported is a
		// choice. This one is masked the wrong way AND declares a length past
		// MaxMessageSize, so the mask check answers 1002 and the size check answers
		// 1009. The mask check runs first, which is the right order: the frame came
		// from a peer whose side of the connection is already established, so "you
		// are masking the wrong way" is actionable, where 1009 tells a client its
		// message was too big when the size was never the reason it was refused --
		// and a client that believes it would retry with a smaller message and be
		// refused again.
		//
		// Reading the length as a signal of anything at all is the deeper problem: a
		// frame masked the wrong way has already failed, so its header past that
		// point is not a description of anything the peer is entitled to have read.
		//
		// The header is built by hand because frame() only emits the 7-bit and
		// 16-bit forms, and this needs the 64-bit one. No payload follows: both
		// checks act on the declared length, so a reader that waited for the bytes
		// would hang here rather than answer, which is itself worth pinning.
		srv := echoServer(t)
		defer srv.Close()
		c := dial(t, srv)
		defer c.Close()
		h := []byte{0x80 | byte(OpBinary), 0x7f} // unmasked, 64-bit length
		h = binary.BigEndian.AppendUint64(h, MaxMessageSize+1)
		if _, err := c.UnderlyingConn().Write(h); err != nil {
			t.Fatal(err)
		}
		_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := c.ReadMessage()
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Code != CloseProtocolError {
			t.Fatalf("unmasked oversize frame: err=%v, want close %d not %d -- the mask bit must be checked before the declared length, so a frame that breaks both rules is reported as the protocol violation it is rather than as a message that was too large", err, CloseProtocolError, CloseTooBig)
		}
	})
}

// A refused handshake reaches the caller as an error carrying the status and
// the body, now that the response is read without net/http.
func TestClientReportsARefusedHandshake(t *testing.T) {
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such window", http.StatusNotFound)
	}))
	defer srv2.Close()
	nc2, err := net.Dial("tcp", strings.TrimPrefix(srv2.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc2.Close()
	_, err = Client(nc2, "shelld", "/x")
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") || !strings.Contains(err.Error(), "no such window") {
		t.Fatalf("err = %v, want the status and body", err)
	}
}

// A 101 whose accept value is wrong is refused.
func TestClientRefusesABadAcceptKey(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		_, _ = b.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: nope\r\n\r\n"))
	}()
	if _, err := Client(a, "shelld", "/x"); err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Fatalf("err = %v", err)
	}
}

func TestAcceptKeyRFCExample(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal(got)
	}
}

func BenchmarkMask(b *testing.B) {
	data := make([]byte, 64<<10)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		maskBytes([4]byte{1, 2, 3, 4}, data)
	}
}
