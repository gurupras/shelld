//go:build linux || darwin

package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// What `shelld stop` reaches is a question with two answers, and the pair is the
// point: the window's own process goes, and a job that has detached itself from the
// window's session stays. Both were measured rather than decided, and both were
// measured from tmux 3.8-rc, on a private socket, by running a pane program that
// records its own pid and setsid's a grandchild that records its own:
//
//	pane child=3086663 setsid grandchild=3086665
//	  before:            child=ALIVE gc=ALIVE
//	  after kill-server: child=dead  gc=ALIVE
//
// So tmux's own kill-server leaves the detached grandchild running, and shelld does
// the same thing for the same reason — CloseCgroups says it in the other direction,
// that a still-populated window cgroup holds either a window nobody closed or a job
// that outlived its shell and shutdown cannot tell them apart.
//
// These are here because the second half reads like a leak, and the obvious repair
// for it — killing every window on the way out, which Manager.KillAll is exactly the
// tool for — is a deviation from tmux that nothing else in the tree would catch.

// windowProgram writes a program to run as a window's shell. It records its own
// pid, which is the process shelld started and the session leader of the pty, and
// setsid's a grandchild that records its own pid and leaves that session.
//
// A grandchild rather than a child because a child cannot get out of reach: it
// keeps the pty as its controlling terminal, so a hangup finds it. setsid is what
// puts it in a session with no controlling terminal, which is the shape a launcher
// that detaches a long job produces, and the only shape that outlives the daemon.
// The detaching half is this test binary and not setsid(1), which is util-linux
// and does not exist on macOS: the grandchild never started there, the mark file
// was never written, and all three tests failed as "the window program never
// reported both pids" -- a message about the fixture that reads like a message
// about shelld. See TestMain for the two branches it takes.
func windowProgram(t *testing.T, dir string) (program, marks string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot re-execute this test binary: %v", err)
	}
	marks, program = filepath.Join(dir, "pids"), filepath.Join(dir, "prog.sh")
	body := "#!/bin/sh\necho $$ > " + marks + ".win\n" +
		detachMarkEnv + "=" + marks + ".detached " + self + " &\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(program, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return program, marks
}

// apiPost is one request on the daemon's socket. The tests here need the API and
// not the CLI, because what they are about is a window the daemon owns.
func apiPost(t *testing.T, sock, path, body string) int {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: unix\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		path, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// startWindowWithDetachedJob creates one window running windowProgram and returns
// the window's pid and its detached grandchild's, both read from the program's own
// files so that neither depends on the daemon still being there to ask.
func startWindowWithDetachedJob(t *testing.T, p paths) (win, detached int) {
	t.Helper()
	program, marks := windowProgram(t, p.dir)
	if code := apiPost(t, p.socket, "/sessions", `{"name":"s"}`); code != http.StatusOK {
		t.Fatalf("create session: %d", code)
	}
	body := fmt.Sprintf(`{"name":"w","shell":%q,"cols":80,"rows":24}`, program)
	if code := apiPost(t, p.socket, "/sessions/s/windows", body); code != http.StatusOK {
		t.Fatalf("create window: %d", code)
	}
	readPid := func(name string) int {
		raw, _ := os.ReadFile(name)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return pid
	}
	waitUntil(t, 10*time.Second, func() bool {
		win, detached = readPid(marks+".win"), readPid(marks+".detached")
		return win > 0 && detached > 0
	}, "the window program never reported both pids")
	// A run that fails leaks exactly the process it is about, so take it back.
	t.Cleanup(func() {
		_ = syscall.Kill(detached, syscall.SIGKILL)
		_ = syscall.Kill(win, syscall.SIGKILL)
	})
	if !running(win) || !running(detached) {
		t.Fatalf("setup: window %d and detached job %d are not both running", win, detached)
	}
	// Without this the surviving half could be surviving because setsid did
	// nothing and it is an ordinary child of a process that happens to still be
	// there. The session id is what setsid changes, so it is what says it worked.
	if ws, ds := sessionID(t, win), sessionID(t, detached); ws == ds {
		t.Fatalf("setup: the detached job is still in the window's session (%d), so it never left the pty and nothing here is about a job out of reach", ws)
	}
	return win, detached
}

// running reports whether pid is a live process, counting a zombie as gone: a
// killed process stays visible to kill(pid, 0) until whoever it reparented to
// reaps it, and these are not this test's children, so lifecycle_test.go's alive
// cannot see them and this cannot use Wait4.
func running(pid int) bool {
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		return false
	}
	return state(pid) != "Z"
}

// state is a process's single-letter state, or "" when it cannot be read.
//
// ps rather than /proc/pid/stat, so it answers on darwin as well. The letter
// wanted here is the first, and ps appends flags to it ("Ss", "Z+"), so callers
// compare a prefix and not the whole field.
func state(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "" // no such process, or ps refused: the caller's kill(pid, 0) decides
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[0][:1]
}

// sessionID is pid's session id, the thing setsid changes.
//
// getsid(2) rather than /proc/pid/stat's session field: the same number, on both
// platforms, without a procfs or a parse. (The note this replaces said the syscall
// package does not wrap getsid, which is true -- golang.org/x/sys/unix does.)
func sessionID(t *testing.T, pid int) int {
	t.Helper()
	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("getsid(%d): %v", pid, err)
	}
	return sid
}

// TestStopLeavesNoWindowProcessRunning is the guarantee half. The window's process
// is the one shelld started and the session leader of its pty, so closing the
// master hangs it up; nothing in shelld kills it, which is why this is worth
// pinning — the guarantee rests on the daemon still owning the last reference to
// that master when it exits, and a copy of the descriptor handed anywhere that
// outlives the daemon would keep the window alive with no other symptom.
func TestStopLeavesNoWindowProcessRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a daemon and a pty")
	}
	p := newPaths(t)
	p.startRun(t)
	win, _ := startWindowWithDetachedJob(t, p)

	if code, out := p.shelld(t, nil, "stop"); code != 0 {
		t.Fatalf("stop: %d %s", code, out)
	}
	waitUntil(t, 10*time.Second, func() bool { return !running(win) },
		fmt.Sprintf("the window's own process %d outlived the daemon that started it", win))
}

// TestStopLeavesADetachedJobRunningAsTmuxDoes is the other half, and it is an
// assertion that something does NOT happen, so it is worth saying what would make
// it fail: killing the window trees on the way out. Manager.KillAll is that in one
// call, and a window kill reaches this job — it walks the window's cgroup, where
// membership survives setsid, exit of the parent and reparenting to pid 1. Wiring
// it into the shutdown path is the repair the surviving process invites, and this
// is the test that says it is a deviation from tmux rather than a fix.
//
// It is the daemonized path rather than `shelld run`, because that is the one a
// supervisor uses and the one where the job is left holding nothing of the
// caller's: the run path's daemon is a child of the test binary, and a job that
// survived it would be inside this process's own tree.
func TestStopLeavesADetachedJobRunningAsTmuxDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a daemon and a pty")
	}
	p := newPaths(t)
	if code, out := p.shelld(t, nil, "daemonize"); code != 0 {
		t.Fatalf("daemonize: %d %s", code, out)
	}
	t.Cleanup(func() { p.shelld(t, nil, "kill", "--force") })
	win, detached := startWindowWithDetachedJob(t, p)

	if code, out := p.shelld(t, nil, "stop"); code != 0 {
		t.Fatalf("stop: %d %s", code, out)
	}
	// The window going first is what makes the survival meaningful: it dates the
	// observation after the shutdown reached the window, rather than before
	// anything had happened yet.
	waitUntil(t, 10*time.Second, func() bool { return !running(win) },
		fmt.Sprintf("the window's own process %d outlived the daemon", win))
	if !running(detached) {
		t.Errorf("the detached job %d was killed by stop (state %q); tmux 3.8-rc's kill-server leaves it running, and shutdown cannot tell a job someone detached on purpose from one a window leaked",
			detached, state(detached))
	}
}
