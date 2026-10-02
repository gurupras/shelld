package ws

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

// closePayload builds a close frame body: the code big-endian, then the reason.
func closePayload(code uint16, reason string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, code), reason...)
}

// The reply to a peer's close, byte for byte, matching shelld-rs (tungstenite's
// do_close): the peer's frame comes back as it was sent, an empty one empty, and one
// whose code RFC 6455 §7.4 does not allow on the wire is answered 1002 "Protocol
// violation" instead of being repeated.
func TestCloseReplyEchoesTheFrameOrRefusesAForbiddenCode(t *testing.T) {
	violation := closePayload(CloseProtocolError, "Protocol violation")
	cases := []struct {
		name string
		sent []byte
		want []byte
	}{
		{"a normal close with a reason is echoed whole", closePayload(1000, "bye"), closePayload(1000, "bye")},
		{"going away is echoed", closePayload(1001, ""), closePayload(1001, "")},
		{"a private-use code is echoed", closePayload(4000, "x"), closePayload(4000, "x")},
		{"a registered code is echoed", closePayload(3000, ""), closePayload(3000, "")},
		{"IANA's 1013 is echoed", closePayload(1013, ""), closePayload(1013, "")},
		{"an empty close is answered empty", nil, nil},
		{"1005 is never sent, so it is refused", closePayload(1005, ""), violation},
		{"1006 is never sent, so it is refused", closePayload(1006, ""), violation},
		{"1015 is never sent, so it is refused", closePayload(1015, ""), violation},
		{"unassigned 1004 is refused", closePayload(1004, ""), violation},
		{"unassigned 1014 is refused", closePayload(1014, ""), violation},
		{"reserved 1016 is refused", closePayload(1016, ""), violation},
		{"reserved 2999 is refused", closePayload(2999, ""), violation},
		{"a code below 1000 is refused", closePayload(999, ""), violation},
		{"a code above 4999 is refused", closePayload(5000, ""), violation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := echoServer(t)
			defer srv.Close()
			c := dial(t, srv)
			defer c.Close()
			nc := c.UnderlyingConn()
			if _, err := nc.Write(rawFrame(true, OpClose, tc.sent)); err != nil {
				t.Fatal(err)
			}
			_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
			var h [2]byte
			if _, err := io.ReadFull(nc, h[:]); err != nil {
				t.Fatalf("no reply to a close: %v", err)
			}
			if Opcode(h[0]&0x0f) != OpClose || h[1]&0x80 != 0 || h[1]&0x7f > 125 {
				t.Fatalf("reply header %x, want an unmasked close frame", h)
			}
			got := make([]byte, h[1]&0x7f)
			if _, err := io.ReadFull(nc, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("sent close payload %x, the reply carried %x, want %x", tc.sent, got, tc.want)
			}
		})
	}
}

// A close body that is malformed is a protocol violation like any other frame, so the
// reply is the code ReadMessage fails every such frame with, not an echo: one byte is
// half a code, and a reason that is not UTF-8 would be sent straight back.
func TestAMalformedCloseIsRefusedNotEchoed(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent []byte
		want uint16
	}{
		{"a one-byte body", []byte{0x03}, CloseProtocolError},
		{"a reason that is not UTF-8", closePayload(1000, "\xff"), CloseInvalidData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := echoServer(t)
			defer srv.Close()
			c := dial(t, srv)
			defer c.Close()
			nc := c.UnderlyingConn()
			if _, err := nc.Write(rawFrame(true, OpClose, tc.sent)); err != nil {
				t.Fatal(err)
			}
			_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
			var h [4]byte
			if _, err := io.ReadFull(nc, h[:]); err != nil {
				t.Fatalf("no reply to a close: %v", err)
			}
			if got := binary.BigEndian.Uint16(h[2:]); Opcode(h[0]&0x0f) != OpClose || h[1] != 2 || got != tc.want {
				t.Errorf("sent close payload %x, the reply was %x, want a close carrying exactly %d", tc.sent, h, tc.want)
			}
		})
	}
}
