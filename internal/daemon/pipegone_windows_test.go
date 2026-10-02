//go:build windows

package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// pipeGone is bounded even when the open never returns, which go-winio's ctx
// alone cannot do, and the timeout answers "not gone": "gone" is what lets the
// stop, kill and cleanup paths remove a daemon's files. A missing pipe still
// answers "gone".
func TestPipeGoneIsBoundedAndFailsSafe(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	orig := dialPipe
	t.Cleanup(func() { dialPipe = orig })
	dialPipe = func(context.Context, string) (net.Conn, error) {
		<-hang
		return nil, windows.ERROR_FILE_NOT_FOUND
	}
	// On a goroutine, so an unbounded pipeGone is a red here and not a hang:
	// the deferred close releases it.
	result := make(chan bool, 1)
	go func() { result <- pipeGone(`\\.\pipe\shelld-test-hung`) }()
	var gone bool
	select {
	case gone = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeGone is still waiting after 5s on an open that never returns; want about 500ms")
	}
	if gone {
		t.Error("pipeGone answered gone for an open that timed out; callers would remove a live daemon's files")
	}

	dialPipe = func(context.Context, string) (net.Conn, error) { return nil, windows.ERROR_FILE_NOT_FOUND }
	if !pipeGone(`\\.\pipe\shelld-test-absent`) {
		t.Error("pipeGone answered not gone for a pipe that does not exist")
	}
}
