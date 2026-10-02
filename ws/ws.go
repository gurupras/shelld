// Package ws implements the WebSocket protocol (RFC 6455) over any
// net.Conn: a server upgrade for net/http handlers and a client handshake
// for an existing connection, such as a Unix socket or named pipe.
// Extensions and subprotocols are not negotiated.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Opcode is a frame opcode.
type Opcode byte

// The frame opcodes of RFC 6455 §5.2. Opcodes from OpClose up are control
// frames: the protocol caps them at 125 bytes, forbids fragmenting them, and
// allows them in the middle of a fragmented data message. Only the read side
// enforces the first two — readFrame rejects a control frame that is longer or
// not final as a protocol error. Nothing enforces them on the write side, so
// WriteMessage(OpPing, payload) with a payload over 125 bytes puts a frame on the
// wire that this library's own reader would reject.
//
// Of these, WriteMessage takes OpText, OpBinary and — for a keepalive, since
// there is no Ping method — OpPing. OpPong is sent for you: ReadMessage answers
// an incoming ping and drops an incoming pong, so neither is ever returned to a
// caller. Send a close with SendClose or CloseHandshake rather than OpClose:
// either one encodes the two-byte big-endian status code that a close frame
// carries, which WriteMessage(OpClose, nil) omits and which a caller passing its
// own payload has to get right itself. Marking the connection closed is not the
// difference — writeFrame does that for any OpClose, so a later write fails with
// net.ErrClosed either way. OpContinuation only appears on the way in: every
// frame this writes is final, so writing a continuation would put a frame on the
// wire that no peer can make sense of. ReadMessage reassembles continuations and
// reports the opcode the message began with.
const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xa
)

// Close status codes.
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseInvalidData   = 1007
	CloseTooBig        = 1009
)

// MaxMessageSize bounds a reassembled message (tungstenite's default).
const MaxMessageSize = 64 << 20

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// CloseError reports a close frame from the peer. ReadMessage returns one
// instead of a message once the peer has closed, so it is the ordinary end of a
// connection and not necessarily a fault; read Code to tell them apart.
type CloseError struct {
	// Code is the status the peer sent, one of the Close constants or another
	// RFC 6455 §7.4 code. A close frame with no payload is allowed and reported
	// as CloseNormal, since that is what an empty close means.
	Code int
	// Reason is the peer's free-text explanation, which is usually empty. It is
	// the peer's text, so a program showing it should treat it as untrusted.
	Reason string
}

// Error renders the code and reason. Match with errors.As to read the code.
func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket: closed (%d) %s", e.Code, e.Reason)
}

// ErrProtocol is returned for frames that violate RFC 6455.
var ErrProtocol = errors.New("websocket: protocol error")

// Conn is a WebSocket connection. ReadMessage must be called from a single
// goroutine; WriteMessage and Close are safe for concurrent use.
type Conn struct {
	conn   net.Conn
	br     *bufio.Reader
	server bool // server side: expects masked frames, sends unmasked

	wmu       sync.Mutex
	wbuf      []byte
	closeSent bool
	readBuf   []byte
	header    [2]byte

	// writeTimeout bounds one write to the transport; zero waits for ever, which
	// is what a Conn does until a caller sets it.
	writeTimeout time.Duration
	// writeErr latches the first write that failed. A write that stops partway
	// leaves the peer holding an incomplete frame, so there is no frame boundary
	// left to resynchronize on and every later write would extend the damage
	// rather than report it. Latching also makes the failure reportable to a
	// caller that was not the one blocked -- with a deadline in force the
	// blocked writer returns an error of its own, but a second writer arriving
	// afterwards must not be told the connection is fine.
	writeErr error
}

func newConn(c net.Conn, br *bufio.Reader, server bool) *Conn {
	if br == nil {
		br = bufio.NewReaderSize(c, 32<<10)
	}
	return &Conn{conn: c, br: br, server: server}
}

// UnderlyingConn exposes the transport.
func (c *Conn) UnderlyingConn() net.Conn { return c.conn }

// SetWriteTimeout bounds how long one write to the transport may block. Zero,
// the default, waits for ever.
//
// Without it a peer that stops reading stops the writer: once the socket buffer
// fills, Write blocks with no deadline, and it blocks while holding the write
// mutex, so it is not one message that is stuck but the connection. Nothing
// above can recover from that, because the goroutine that would notice is the
// one parked in the write -- which is why this is a property of the Conn and not
// something a caller can arrange with a timer.
//
// A timeout that fires is not recoverable and is not meant to be: see writeErr.
// The caller's remedy is to close the connection and drop that peer.
func (c *Conn) SetWriteTimeout(d time.Duration) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.writeTimeout = d
	if d == 0 {
		// An earlier non-zero setting leaves an absolute deadline behind, and with
		// the bound removed no later write sets one of its own -- so without this,
		// asking for no deadline installs a permanent one and every write after it
		// fails on a connection the caller has just said not to bound.
		_ = c.conn.SetWriteDeadline(time.Time{})
	}
}

// UpgradeError is a failed handshake precondition with the HTTP status to
// answer. Neither CheckUpgrade nor Upgrade writes that answer — the handler
// does, which is why the status travels with the error.
type UpgradeError struct {
	// Status is the HTTP status the request should be answered with: 405 for a
	// method other than GET, 400 for a missing or wrong header.
	Status int
	// Message says which precondition failed, word for word as axum's
	// extract::ws says it, so that a client sees the same text from either
	// daemon. It names only the header at fault and so is safe to return to the
	// client.
	Message string
}

// Error is the Message. Match with errors.As to read the Status.
func (e *UpgradeError) Error() string { return e.Message }

func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// CheckUpgrade validates a client's opening handshake without responding.
func CheckUpgrade(r *http.Request) error {
	switch {
	case r.Method != http.MethodGet:
		return &UpgradeError{http.StatusMethodNotAllowed, "Request method must be `GET`"}
	case !headerContainsToken(r.Header, "Connection", "upgrade"):
		return &UpgradeError{http.StatusBadRequest, "Connection header did not include 'upgrade'"}
	case !strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		return &UpgradeError{http.StatusBadRequest, "`Upgrade` header did not include 'websocket'"}
	case r.Header.Get("Sec-WebSocket-Version") != "13":
		return &UpgradeError{http.StatusBadRequest, "`Sec-WebSocket-Version` header did not include '13'"}
	case r.Header.Get("Sec-WebSocket-Key") == "":
		return &UpgradeError{http.StatusBadRequest, "`Sec-WebSocket-Key` header missing"}
	}
	return nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Upgrade completes the server handshake. Call CheckUpgrade first to answer
// bad requests yourself; Upgrade re-validates and fails without writing a
// response.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if err := CheckUpgrade(r); err != nil {
		return nil, err
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("websocket: response does not support hijacking")
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " +
		acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := nc.Write([]byte(resp)); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return newConn(nc, brw.Reader, true), nil
}

// badInRequestLine reports a rune that cannot appear in a request-line target.
// Space ends the target, CR and LF end the line, and the remaining C0 and C1
// controls are rejected as well rather than reasoned about one at a time.
func badInRequestLine(r rune) bool {
	return r == ' ' || r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// Client performs the client handshake over an established connection.
// host and path form the request line and Host header.
//
// path is rejected if it holds a space or a control character. The request line is
// assembled here by string concatenation rather than by net/http, so nothing else
// would catch it: a CR LF in path ends the request line and everything after it is
// read as headers, or as a second request. Callers build path by interpolating
// session and window names, which shelld does not restrict, so the check is on the
// value rather than on the caller remembering to escape -- and it is an error
// return instead of a silent scrub because a path that needed scrubbing is not the
// path the caller meant to ask for.
func Client(nc net.Conn, host, path string) (*Conn, error) {
	if i := strings.IndexFunc(path, badInRequestLine); i >= 0 {
		// Decoded rather than indexed. IndexFunc returns a BYTE offset while the
		// predicate above judges a RUNE, so path[i] is the first byte of a possibly
		// multi-byte character -- and %q on a byte formats it as a rune, so a C1 such
		// as U+0085 would be reported as 'Â', its 0xC2 lead byte, which is not a
		// character the path contains. The 0x80..0x9f arm exists for exactly those
		// runes, so that is the arm whose message would have been wrong. The offset is
		// right either way and stays in bytes, which is what it is.
		//
		// The decode cannot fail here: an invalid byte decodes to U+FFFD, which no arm
		// of the predicate matches, so a fired IndexFunc means i begins a valid rune.
		r, _ := utf8.DecodeRuneInString(path[i:])
		return nil, fmt.Errorf("path contains %q at byte %d; it must be URL-escaped", r, i)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n\r\n"
	if _, err := nc.Write([]byte(req)); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(nc, 32<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("websocket: handshake failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return nil, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	return newConn(nc, br, false), nil
}

// ReadMessage returns the next text or binary message, reassembling
// fragments. Pings are answered and pongs dropped. A close frame is echoed,
// see closeReply, and reported as *CloseError.
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	var msgOp Opcode
	msg := c.readBuf[:0]
	inMessage := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			if err := c.writeFrame(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			// A body is a code and then a UTF-8 reason (§5.5.1), so one byte is no
			// code at all, and echoing a reason that is not UTF-8 would send it on.
			// Both are refused the way every other malformed frame is.
			if len(payload) == 1 {
				return 0, nil, c.fail(CloseProtocolError)
			}
			code, reason := CloseNormal, ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
				reason = string(payload[2:])
				if !utf8.ValidString(reason) {
					return 0, nil, c.fail(CloseInvalidData)
				}
			}
			_ = c.writeFrame(OpClose, closeReply(payload, code))
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case OpText, OpBinary:
			if inMessage {
				return 0, nil, c.fail(CloseProtocolError)
			}
			if fin {
				// Unfragmented: the frame payload is already a fresh buffer.
				if op == OpText && !utf8.Valid(payload) {
					return 0, nil, c.fail(CloseInvalidData)
				}
				return op, payload, nil
			}
			msgOp = op
			inMessage = true
		case OpContinuation:
			if !inMessage {
				return 0, nil, c.fail(CloseProtocolError)
			}
		default:
			return 0, nil, c.fail(CloseProtocolError)
		}
		if len(msg)+len(payload) > MaxMessageSize {
			return 0, nil, c.fail(CloseTooBig)
		}
		msg = append(msg, payload...)
		if fin {
			if msgOp == OpText && !utf8.Valid(msg) {
				return 0, nil, c.fail(CloseInvalidData)
			}
			out := make([]byte, len(msg))
			copy(out, msg)
			if cap(msg) <= 1<<16 {
				c.readBuf = msg[:0]
			} else {
				c.readBuf = nil
			}
			return msgOp, out, nil
		}
	}
}

func (c *Conn) fail(code int) error {
	_ = c.writeClose(code)
	return ErrProtocol
}

func (c *Conn) readFrame() (fin bool, op Opcode, payload []byte, err error) {
	h := c.header[:2]
	if _, err = io.ReadFull(c.br, h); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, c.fail(CloseProtocolError)
	}
	op = Opcode(h[0] & 0x0f)
	masked := h[1]&0x80 != 0
	// Masking is fixed by direction rather than negotiated: a client masks every
	// frame it sends, a server masks none, and each side must fail the connection
	// on a frame masked the wrong way (RFC 6455 section 5.1). So the mask bit is
	// not a property of the frame to be honoured but a claim about who sent it,
	// and the only correct value is c.server -- which is what that field already
	// says it expects.
	//
	// The spec's own reason for masking does not apply to this daemon: a mask
	// makes a client's bytes unpredictable on the wire so an intermediary cannot
	// be fed a chosen byte sequence it would parse as a request of its own, and
	// there is no intermediary in a Unix socket or a named pipe. The reason to
	// check anyway is that this package is a WebSocket implementation others will
	// write clients against, and a server that accepts unmasked frames silently
	// blesses a client that forgets to mask -- one that then fails against every
	// conforming server, with the bug attributed here. An optional requirement is
	// not a requirement, and the cost of holding it is this comparison.
	if masked != c.server {
		return false, 0, nil, c.fail(CloseProtocolError)
	}
	length := uint64(h[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if op >= OpClose && (length > 125 || !fin) {
		return false, 0, nil, c.fail(CloseProtocolError)
	}
	if length > MaxMessageSize {
		return false, 0, nil, c.fail(CloseTooBig)
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		maskBytes(mask, payload)
	}
	return fin, op, payload, nil
}

func maskBytes(key [4]byte, b []byte) {
	k := binary.LittleEndian.Uint32(key[:])
	k64 := uint64(k)<<32 | uint64(k)
	i := 0
	for ; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], binary.LittleEndian.Uint64(b[i:])^k64)
	}
	for ; i < len(b); i++ {
		b[i] ^= key[i&3]
	}
}

// WriteMessage sends a single-frame message.
func (c *Conn) WriteMessage(op Opcode, data []byte) error {
	return c.writeFrame(op, data)
}

func (c *Conn) writeFrame(op Opcode, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return net.ErrClosed
	}
	if c.writeErr != nil {
		return c.writeErr
	}
	if op == OpClose {
		c.closeSent = true
	}
	return c.writeFrameLocked(op, data)
}

func (c *Conn) writeFrameLocked(op Opcode, data []byte) error {
	buf := c.wbuf[:0]
	buf = append(buf, 0x80|byte(op))
	var maskBit byte
	if !c.server {
		maskBit = 0x80
	}
	switch n := len(data); {
	case n < 126:
		buf = append(buf, maskBit|byte(n))
	case n <= 0xffff:
		buf = append(buf, maskBit|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, maskBit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if c.server {
		// One write for header and payload.
		buf = append(buf, data...)
	} else {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		buf = append(buf, key[:]...)
		start := len(buf)
		buf = append(buf, data...)
		maskBytes(key, buf[start:])
	}
	// Per write rather than once at setup: a deadline is an absolute time, so one
	// set at setup would expire during the connection's life and fail every write
	// after it. Not cleared afterwards, which would be a second syscall per write
	// to guard against a deadline no write can see: every write that can reach
	// Write sets its own first. What a stale deadline can reach is a later
	// SetWriteTimeout(0), and that is where it is cleared.
	if c.writeTimeout > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			c.writeErr = err
			return err
		}
	}
	_, err := c.conn.Write(buf)
	if cap(buf) <= 1<<20 {
		c.wbuf = buf[:0]
	} else {
		c.wbuf = nil
	}
	if err != nil {
		c.writeErr = err
	}
	return err
}

// closeReply is the payload answering a peer's close whose payload is payload and
// whose code is code, the way shelld-rs's tungstenite answers one: the frame
// echoed as it came, empty for empty, and 1002 "Protocol violation" for a code
// RFC 6455 §7.4 does not allow in a close frame. 1005, 1006 and 1015 are the
// codes §7.4.1 says must never be sent -- they stand for "no code", "no close
// frame" and a TLS failure -- and every code §7.4.2 leaves unassigned or reserved
// is just as meaningless on the wire, so repeating one sends the peer a code
// that its own sender may not use.
func closeReply(payload []byte, code int) []byte {
	if len(payload) < 2 || closeCodeAllowed(code) {
		return payload
	}
	return append(binary.BigEndian.AppendUint16(nil, CloseProtocolError), "Protocol violation"...)
}

// closeCodeAllowed reports whether code may appear in a close frame, by
// tungstenite's is_allowed: 1000-1003 and 1007-1011 from RFC 6455 §7.4.1, 1012
// and 1013 from the IANA registry, and the 3000-4999 range §7.4.2 gives to
// registration and private use.
func closeCodeAllowed(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1013:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}

func (c *Conn) writeClose(code int) error {
	var payload [2]byte
	binary.BigEndian.PutUint16(payload[:], uint16(code))
	return c.writeFrame(OpClose, payload[:])
}

// CloseHandshake sends a normal close frame and waits up to timeout for the
// peer's reply, then closes the transport.
func (c *Conn) CloseHandshake(timeout time.Duration) error {
	if err := c.writeClose(CloseNormal); err != nil {
		return c.conn.Close()
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		if _, _, _, err := c.readFrame(); err != nil {
			break
		}
	}
	return c.conn.Close()
}

// SendClose sends a normal close frame without waiting; the peer's reply
// surfaces from ReadMessage.
func (c *Conn) SendClose() error { return c.writeClose(CloseNormal) }

// WriteClose sends a close frame carrying code and reason without waiting, as
// SendClose does. reason must be valid UTF-8 and short enough for the frame's
// 125-byte limit with the code.
func (c *Conn) WriteClose(code int, reason string) error {
	return c.writeFrame(OpClose, append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...))
}

// Close closes the transport immediately.
func (c *Conn) Close() error { return c.conn.Close() }
