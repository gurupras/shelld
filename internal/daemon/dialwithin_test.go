package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestADialBlockedInTheKernelStillTimesOut: a connect that does not return, as
// connect(2) does not under a hung NFS or FUSE mount, ends at the bound with a
// timeout, and the connection it makes when it does return is closed.
func TestADialBlockedInTheKernelStillTimesOut(t *testing.T) {
	// Released after the bound has long passed, so a dial the bound cannot end
	// still finishes, and reds on the elapsed time instead of hanging.
	release := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(release) })
	late, peer := net.Pipe()
	defer peer.Close()
	start := time.Now()
	conn, err := dialWithin(context.Background(), "unix", "/run/shelld.sock", 100*time.Millisecond, func(context.Context) (net.Conn, error) {
		<-release // ignores ctx, as a blocked syscall does
		return late, nil
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the dial took %v: the bound waited for the blocked connect", elapsed)
	}
	if conn != nil || err == nil {
		t.Fatalf("got conn %v, err %v; want a timeout", conn, err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("err %v (%T) is not a dial timeout", err, err)
	}
	if !strings.Contains(err.Error(), "/run/shelld.sock") {
		t.Errorf("err %q does not name the address", err)
	}

	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("the connection made after the deadline was not closed: read %v", err)
	}
}

// TestADialEndsWhenItsContextIsCancelled: the caller's own cancellation ends a
// blocked dial too, and reads as a cancellation rather than a timeout.
func TestADialEndsWhenItsContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := dialWithin(ctx, "unix", "/run/shelld.sock", time.Minute, func(context.Context) (net.Conn, error) {
		<-release
		return nil, errors.New("the mount answered at last")
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the dial took %v: cancelling did not end the blocked connect", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want a cancellation", err)
	}
}

// TestAZeroDialTimeoutIsNone: zero is no timeout, as it is to net.Dialer, and
// not one that has already expired.
func TestAZeroDialTimeoutIsNone(t *testing.T) {
	c, peer := net.Pipe()
	defer peer.Close()
	conn, err := dialWithin(context.Background(), "unix", "/run/shelld.sock", 0, func(context.Context) (net.Conn, error) {
		time.Sleep(100 * time.Millisecond)
		return c, nil
	})
	if err != nil || conn != c {
		t.Fatalf("got conn %v, err %v; want the dial's connection", conn, err)
	}
	conn.Close()
}
