// Package wsserver is the server half of the ws package's WebSocket
// handshake: the precondition check and the upgrade of a net/http request.
// It is apart from ws so that a client, which needs only ws.Client and the
// frame layer, does not link net/http.
package wsserver

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gurupras/shelld/ws"
)

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

// Upgrade completes the server handshake. Call CheckUpgrade first to answer
// bad requests yourself; Upgrade re-validates and fails without writing a
// response.
func Upgrade(w http.ResponseWriter, r *http.Request) (*ws.Conn, error) {
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
		ws.AcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := nc.Write([]byte(resp)); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return ws.NewServerConn(nc, brw.Reader), nil
}
