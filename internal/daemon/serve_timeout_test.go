package daemon

import (
	"net/http"
	"testing"
	"time"
)

// TestServerSetsBothConnectionBounds pins that the bounds are set at all, and
// that the idle one keeps the relationship its value was chosen for.
// TestNewServerBoundsBothWaysAConnectionCanStall shortens both durations to run
// at all, so on its own it cannot see either constant change -- including to
// zero, which is what "no bound" is spelled as. It also only builds on Unix,
// where a socket to stall is available; this one runs everywhere the daemon does.
//
// The idle bound is checked against http.Transport's own IdleConnTimeout rather
// than against 120s, because the reason for the value is the relationship and not
// the number: a Go client left at the standard library's defaults must retire an
// idle connection before this server would, so a request can never race a close
// from this end. Reading it from net/http is right here, where a change to that
// default would genuinely invalidate the choice. It would be wrong for a limit
// fixed by the reference daemon, which is why maxControlBody asserts its number
// directly instead.
//
// The header bound gets no value assertion beyond being set: a local peer writes
// its headers in a single write, so every finite value is generous and none is
// load-bearing. The lower bound only says it is not so tight that a client
// descheduled on a loaded machine would be cut off mid-header.
func TestServerSetsBothConnectionBounds(t *testing.T) {
	st, _ := api(t, nil)
	srv := newServer(st)

	if srv.ReadHeaderTimeout != readHeaderTimeout || readHeaderTimeout < time.Second {
		t.Errorf("ReadHeaderTimeout = %v (readHeaderTimeout = %v), want it set from the constant and at least 1s -- unset, a peer that sends half a request line is held until the daemon exits; under a second, a client descheduled mid-header on a loaded machine is cut off", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v -- unset, a connection that completed a request and went quiet is held forever, which ReadHeaderTimeout does not cover", srv.IdleTimeout, idleTimeout)
	}

	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skipf("http.DefaultTransport is %T, not *http.Transport: the relationship idleTimeout was chosen for cannot be read", http.DefaultTransport)
	}
	if idleTimeout <= tr.IdleConnTimeout {
		t.Errorf("idleTimeout = %v, want more than http.Transport's IdleConnTimeout of %v -- at or below it this server can close an idle connection a stdlib client still believes it owns, and a request issued at that moment races the close", idleTimeout, tr.IdleConnTimeout)
	}
}
