//go:build linux

package shelld

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/pty"
)

// shortWriter writes 11 bytes every ~20us forever, so the window is always
// mid-read when it is killed.
// Four writers (forked, each dying with its parent), so that a read always
// finds something even when one writer is descheduled on a loaded host.
var shortWriter = []string{"/usr/bin/python3", "-c", `
import ctypes, os, time
for _ in range(3):
    if os.fork() == 0:
        ctypes.CDLL(None).prctl(1, 9)
        break
while True:
    os.write(1, b'0123456789\n')
    t = time.monotonic() + 0.00002
    while time.monotonic() < t:
        pass
`}

func needShortWriter(t *testing.T) {
	if _, err := os.Stat(shortWriter[0]); err != nil {
		t.Skipf("needs %s: %v", shortWriter[0], err)
	}
}

// Hangup kill closes the master while the program runs: Kill must return
// promptly and the hangup must end a program that is writing.
func TestHangupKillOfAWritingWindow(t *testing.T) {
	needShortWriter(t)
	hangupMode(t)
	for i := 0; i < 5; i++ {
		w, err := NewWindow(WindowOpts{Name: "hupw", Command: shortWriter, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		pid := w.Pid()
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		time.Sleep(200 * time.Millisecond)
		start := time.Now()
		killWithin(t, w, time.Second)
		t.Logf("Kill returned in %v", time.Since(start))
		select {
		case <-w.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("the hangup did not end the writing program")
		}
	}
}

// The awaitExit fallback closes the master to hang up a root SIGKILL could not
// reach. That close is only a lever if it returns while the root is writing.
func TestKillHangsUpAWritingRootItCannotSignal(t *testing.T) {
	needShortWriter(t)
	oldWait, oldReap := killWait, reapTree
	killWait, reapTree = 300*time.Millisecond, func(*pty.PTY, uint64) {}
	t.Cleanup(func() { killWait, reapTree = oldWait, oldReap })
	w, err := NewWindow(WindowOpts{Name: "wroot", Command: shortWriter, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	time.Sleep(300 * time.Millisecond)
	// Two killWaits (600ms) is the whole of Kill when the hangup lands at once.
	killWithin(t, w, time.Second)
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the hangup did not end the window's writing process")
	}
}
