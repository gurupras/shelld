//go:build linux

package shelld

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/pty"
)

// hangupMode switches this test to SHELLD_KILL_MODE=hangup, and fails it if the
// tree kill (the /proc scan) is reached at all.
func hangupMode(t *testing.T) *atomic.Int32 {
	t.Helper()
	var scans atomic.Int32
	prevMode := SetKillMode(KillHangup)
	prevReap := reapTree
	reapTree = func(*pty.PTY, uint64) { scans.Add(1) }
	t.Cleanup(func() {
		SetKillMode(prevMode)
		reapTree = prevReap
		if n := scans.Load(); n != 0 {
			t.Errorf("hangup kill ran the tree kill %d time(s)", n)
		}
	})
	return &scans
}

// stateOf is the state letter of /proc/<pid>/stat, read after the LAST ')', or ""
// when the pid is gone.
func stateOf(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	return s[i+2 : i+3]
}

// A root that ignores SIGHUP is exactly what tmux kill-window leaves running, so
// hangup mode must leave it running too -- and must return while it is: no SIGKILL,
// no wait. That both are absent is what this asserts: the process is still alive
// AFTER Kill returned, and the window's Done has not closed.
func TestHangupKillNeitherKillsNorWaits(t *testing.T) {
	hangupMode(t)
	w, err := NewWindow(WindowOpts{Name: "hupign", Cols: 80, Rows: 24,
		Command: []string{"/bin/sh", "-c", `trap "" HUP; while :; do sleep 1; done`}})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); <-w.Done() })
	time.Sleep(200 * time.Millisecond) // past the exec, so the trap is set

	done := make(chan struct{})
	go func() { w.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("hangup Kill blocked on a root that ignores SIGHUP")
	}
	if w.IsAlive() {
		t.Error("Kill returned and the window still reads alive")
	}
	time.Sleep(100 * time.Millisecond)
	if st := stateOf(pid); st == "" || st == "Z" {
		t.Fatalf("a SIGHUP-ignoring root was ended by a hangup kill (state %q); tmux leaves it running", st)
	}
	select {
	case <-w.Done():
		t.Fatal("Done closed while the root is still running")
	default:
	}
	// The window's own reaper still owns the pid: once the root does exit it is
	// reaped, not left a zombie.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("waitLoop did not reap the root after it exited")
	}
	if st := stateOf(pid); st != "" {
		t.Fatalf("root %d still in /proc after Done (state %q): a zombie", pid, st)
	}
}

// An ordinary shell dies of the hangup, and is reaped by waitLoop: Done closes,
// the exit is SIGHUP, and the pid is gone from /proc -- no zombie.
func TestHangupKillEndsAndReapsAShell(t *testing.T) {
	hangupMode(t)
	w, err := NewWindow(WindowOpts{Name: "plainhup", Shell: "/bin/sh", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond)
	w.Kill()
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the hangup did not end the shell")
	}
	if st, ok := w.ExitStatus(); !ok || st.Signal != "SIGHUP" {
		t.Errorf("exit = %+v ok=%v; want ended by SIGHUP", st, ok)
	}
	if st := stateOf(pid); st != "" {
		t.Fatalf("shell %d still in /proc after Done (state %q): a zombie", pid, st)
	}
	w.Kill() // a second Kill is quiet
}

// The cgroup in hangup mode: not killed, not waited for, and removed by the
// reaper as soon as its last member exits -- here, a survivor this test then ends.
func TestHangupKillRemovesTheCgroupOnceItEmpties(t *testing.T) {
	initCgroupsOrSkip(t)
	hangupMode(t)
	w, err := NewWindow(WindowOpts{Name: "hupcg", Cols: 80, Rows: 24,
		Command: []string{"/bin/sh", "-c", `trap "" HUP; while :; do sleep 1; done`}})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	dir := dirOfCgroup(w)
	if dir == "" {
		t.Fatal("the cgroup arm reported itself active and this window has no cgroup")
	}
	time.Sleep(200 * time.Millisecond)
	w.Kill()
	if dirOfCgroup(w) != "" {
		t.Error("the window still holds its cgroup after a hangup kill")
	}
	// The survivor keeps the directory: it is populated, and nothing kills it.
	time.Sleep(200 * time.Millisecond)
	if pids, exists := procsOf(t, dir); !exists || len(pids) == 0 {
		t.Fatalf("cgroup %s exists=%v holds %v; want the SIGHUP-ignoring root still in it", dir, exists, pids)
	}
	if n := hupReaper.pendingCount(); n < 1 {
		t.Errorf("reaper pending = %d with a populated hung-up cgroup", n)
	}
	// End the survivor; the reaper must take the directory without being asked.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	<-w.Done()
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(dir)
		return errors.Is(err, os.ErrNotExist)
	}, "the hung-up window's cgroup outlived its last member: "+dir)
	waitFor(t, 2*time.Second, func() bool { return hupReaper.pendingCount() == 0 },
		"the reaper still counts a removed cgroup as pending")
}

// The common case: a shell that dies of the hangup leaves an empty cgroup, which
// the reaper removes promptly, with no survivor involved.
func TestHangupKillOfAShellLeavesNoCgroup(t *testing.T) {
	initCgroupsOrSkip(t)
	hangupMode(t)
	w, err := NewWindow(WindowOpts{Name: "hupcg2", Shell: "/bin/sh", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	dir := dirOfCgroup(w)
	if dir == "" {
		t.Fatal("the cgroup arm reported itself active and this window has no cgroup")
	}
	time.Sleep(200 * time.Millisecond)
	w.Kill()
	<-w.Done()
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(dir)
		return errors.Is(err, os.ErrNotExist)
	}, "a hung-up shell's cgroup was not removed: "+dir)
}
