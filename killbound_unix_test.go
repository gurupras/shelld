//go:build linux || darwin

package shelld

import (
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/pty"
)

// startUnkillable starts a window whose tree kill is a no-op, as it is when
// SIGKILL answers EPERM for a root that became another user.
func startUnkillable(t *testing.T, shell string) *Window {
	t.Helper()
	oldWait, oldReap := killWait, reapTree
	killWait, reapTree = 300*time.Millisecond, func(*pty.PTY, uint64) {}
	t.Cleanup(func() { killWait, reapTree = oldWait, oldReap })
	w, err := NewWindow(WindowOpts{Name: "root", Shell: shell, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond) // past the shell's exec, so a trap is set
	return w
}

func killWithin(t *testing.T, w *Window, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { w.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Kill still blocked after %v on a root its SIGKILL cannot reach", d)
	}
}

// TestKillHangsUpARootItCannotSignal: the hangup ends a root that does not
// ignore SIGHUP, which is what a shell under sudo does.
func TestKillHangsUpARootItCannotSignal(t *testing.T) {
	w := startUnkillable(t, "sleep 30")
	killWithin(t, w, 3*time.Second)
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the hangup did not end the window's process")
	}
}

// TestKillReturnsWhenTheRootSurvivesTheHangup: Kill must not wait on a root
// that ignores SIGHUP too; its DELETE would never answer.
func TestKillReturnsWhenTheRootSurvivesTheHangup(t *testing.T) {
	w := startUnkillable(t, `/bin/sh -c "trap '' HUP; while :; do sleep 1; done"`)
	killWithin(t, w, 3*time.Second)
	if w.IsAlive() {
		t.Error("Kill returned but the window still reads alive")
	}
}
