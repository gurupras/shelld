package ws

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// net.Pipe is the transport here because it has no buffer at all: a write blocks
// until the peer reads, so "the peer stopped reading" needs no megabytes to reach
// and no assumption about how much a socket will take. The daemon-side test pays
// that cost to show the consequence; this one is about the mechanism.
func newPipeConn(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	return newConn(ours, nil, true), theirs
}

// A blocking write with no deadline cannot be recovered from above, because the
// goroutine that would notice is the one parked in the write -- and it is parked
// holding the write mutex, so it is not one message that is stuck but the
// connection. The two arms are the same write against the same unread peer,
// differing only in whether a bound was set.
func TestAWriteToAPeerThatStopsReadingIsBounded(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		c, _ := newPipeConn(t)
		c.SetWriteTimeout(50 * time.Millisecond)
		// On its own goroutine, because the failure this guards against is a write
		// that never returns: called inline it would hang the package until the
		// go test timeout, and a timeout is not a verdict on which test broke.
		done := make(chan error, 1)
		go func() { done <- c.WriteMessage(OpBinary, []byte("nobody is reading this")) }()
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("write to an unread peer returned %v, want a deadline error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the write did not return with a 50ms bound in force, so the bound is not reaching the transport")
		}
	})

	t.Run("unbounded", func(t *testing.T) {
		c, theirs := newPipeConn(t)
		done := make(chan error, 1)
		go func() { done <- c.WriteMessage(OpBinary, []byte("nobody is reading this")) }()
		select {
		case err := <-done:
			t.Fatalf("the write returned %v with no bound set, so the arm above is not measuring the bound", err)
		case <-time.After(500 * time.Millisecond):
		}
		// Left blocked, the goroutine outlives the test. Closing the peer is what
		// ends it, and is also the only thing that could have: see the doc comment.
		theirs.Close()
		if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("the blocked write ended with %v, want the peer's close", err)
		}
	})
}

// A write that stops partway leaves the peer holding an incomplete frame, so
// there is no frame boundary left to resynchronize on. The first failure is
// therefore the connection's last, and every later write has to say so -- to the
// caller that was blocked, which has an error of its own, but above all to the
// next caller, which does not and would otherwise be told the connection is fine.
func TestAFailedWriteIsLatchedForEveryLaterCaller(t *testing.T) {
	c, _ := newPipeConn(t)
	c.SetWriteTimeout(50 * time.Millisecond)
	first := make(chan error, 1)
	go func() { first <- c.WriteMessage(OpBinary, []byte("first")) }()
	select {
	case err := <-first:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("setup: the first write returned %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		// Inline this would hang the package rather than name itself, and a package
		// that times out has said nothing about which behaviour broke.
		t.Fatal("setup: the first write never returned, so there is no latched failure to test")
	}

	// Long enough that a write of its own would not time out: what fails it is the
	// latch, not a second deadline.
	c.SetWriteTimeout(time.Hour)
	done := make(chan error, 1)
	go func() { done <- c.WriteMessage(OpText, []byte("second")) }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the second write returned %v, want the latched first failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the second write blocked, so the first failure was not latched and this connection has two writers stuck on it")
	}

	// Including the close: a caller winding the connection down must not be told
	// the goodbye was delivered when the peer is holding half a frame. Bounded for
	// the reason the write above is.
	closed := make(chan error, 1)
	go func() { closed <- c.SendClose() }()
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("SendClose returned %v after a failed write, want the latched failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("SendClose blocked after a failed write, so a caller winding the connection down joins the writer that is already stuck")
	}
}

// A deadline is an absolute time, so one set once at setup expires during the
// connection's life and fails every write after it. The bound here is shorter than
// the span the writes are spread over, which is the whole of what separates a
// per-write deadline from a per-connection one: set at setup, the third write
// onwards would fail against a peer that is reading everything.
func TestTheBoundIsPerWriteAndNotPerConnection(t *testing.T) {
	c, theirs := newPipeConn(t)
	c.SetWriteTimeout(200 * time.Millisecond)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := theirs.Read(buf); err != nil {
				return
			}
		}
	}()

	for i := range 6 {
		if err := c.WriteMessage(OpBinary, []byte("keepalive")); err != nil {
			t.Fatalf("write %d of 6 failed at %s into the connection: %v", i+1, time.Duration(i)*100*time.Millisecond, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Asking for no bound has to withdraw the one already installed. A deadline is an
// absolute time and the write path only sets one when the bound is non-zero, so a
// bound that is removed after it has expired would otherwise stay in force for the
// life of the connection -- failing writes instantly, on a connection whose caller
// has just asked for no deadline at all.
//
// The sleep is what makes the stale deadline expired rather than merely present:
// with it unexpired the write succeeds either way and the test cannot tell.
func TestRemovingTheBoundWithdrawsTheDeadlineItInstalled(t *testing.T) {
	c, theirs := newPipeConn(t)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := theirs.Read(buf); err != nil {
				return
			}
		}
	}()

	c.SetWriteTimeout(50 * time.Millisecond)
	if err := c.WriteMessage(OpBinary, []byte("bounded")); err != nil {
		t.Fatalf("setup: a bounded write to a reading peer failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	c.SetWriteTimeout(0)
	if err := c.WriteMessage(OpBinary, []byte("unbounded")); err != nil {
		t.Fatalf("a write after the bound was removed failed with %v: the deadline the bound installed is still in force", err)
	}
}
