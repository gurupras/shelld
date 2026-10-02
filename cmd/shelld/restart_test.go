//go:build linux || darwin

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// killDaemons SIGKILLs the daemons serving p's socket, for a restart that left a
// detached one behind.
//
// By pid from daemonProcesses rather than by `shelld kill --force`, because the
// command reads the lockfile and a test that has just asserted something about the
// lockfile's state would have its cleanup depend on it. The pids it returns are
// this test's own daemons twice over: the match is on argv[0] being bin, the binary
// TestMain built into its own temp directory, and on the socket path, which is this
// test's temp directory. Nothing outside the test can satisfy both.
func killDaemons(p paths) {
	for _, pid := range daemonProcesses(p.socket) {
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// restart is stop followed by daemonize, and its contract is that the daemon
// serving the socket afterwards is a different process from the one before.
//
// The pid is what carries that. A restart that shut nothing down but started
// nothing either would leave the socket answering, so asserting only that it
// answers cannot tell the two apart; the new pid is what says the old process is
// gone and its successor is up. The old pid is checked dead through alive(),
// which is Wait4 on a child of this test, so a daemon that exited but has not
// been reaped does not read as still running.
func TestRestartReplacesTheDaemonWithANewProcess(t *testing.T) {
	p := newPaths(t)
	old := p.startRun(t)
	t.Cleanup(func() { killDaemons(p) })

	before, err := daemon.Probe(p.socket, time.Second)
	if err != nil || int(before.Pid) != old.Process.Pid {
		t.Fatalf("before restart: /health %+v %v, want pid %d", before, err, old.Process.Pid)
	}
	if code, out := p.shelld(t, nil, "restart"); code != 0 {
		t.Fatalf("restart: exit %d: %s", code, out)
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(old.Process.Pid) }, "the old daemon still runs after restart")

	after, err := daemon.Probe(p.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("after restart nothing answers %s: %v", p.socket, err)
	}
	if after.Pid == before.Pid {
		t.Fatalf("restart left pid %d serving: the daemon was not replaced", after.Pid)
	}
	// The lockfile has to name the daemon that is actually up, not the one that
	// was: every command that decides whether to signal reads it, so a restart
	// that left the old pid there would aim the next stop at a dead process.
	if l := readLock(t, p); l == nil || l.Pid != after.Pid {
		t.Fatalf("lockfile %+v, want the new daemon's pid %d", l, after.Pid)
	}
	if pids := daemonProcesses(p.socket); len(pids) != 1 || pids[0] != int(after.Pid) {
		t.Fatalf("daemons %v after restart, /health pid %d: restart started one beside the other", pids, after.Pid)
	}
}

// With nothing running, restart starts a daemon rather than refusing.
//
// stop exits 0 when it finds nothing to stop -- there is no daemon, which is what
// it was asked to arrange -- so restart goes on to daemonize. That makes restart
// usable as start, which matters because it is what a service wrapper calls
// without knowing the current state.
func TestRestartWithNoDaemonStartsOne(t *testing.T) {
	p := newPaths(t)
	t.Cleanup(func() { killDaemons(p) })
	if code, out := p.shelld(t, nil, "restart"); code != 0 {
		t.Fatalf("restart with no daemon: exit %d: %s", code, out)
	}
	info, err := daemon.Probe(p.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("restart with no daemon left nothing serving %s: %v", p.socket, err)
	}
	if l := readLock(t, p); l == nil || l.Pid != info.Pid {
		t.Fatalf("lockfile %+v, /health pid %d", l, info.Pid)
	}
}

// A daemon killed outright leaves its socket and lockfile behind, and restart has
// to get past both.
//
// This is the state a crash leaves, so it is the state restart is reached in when
// it is most needed. The stale socket file is the obstacle: bind(2) on a path that
// already exists fails with EADDRINUSE whether or not anything is listening on it,
// so something has to remove it before the new daemon can bind.
//
// Two places do, and this pins that one of them did rather than which. cmdStop
// reaches daemon.RemoveStale through its DaemonGone arm, and daemon.listen calls
// removeDeadSocket before it binds. Measured: deleting the removeStale call from
// that arm leaves this test green, because the bind cleans up after it. So a
// change that took the removal out of stop would not fail here -- what would is
// taking it out of both, or teaching removeDeadSocket to refuse a socket nothing
// holds.
//
// SIGKILL rather than SIGTERM because the point is to skip the daemon's own
// cleanup -- a terminated daemon removes these files, and then there is nothing
// for this test to be about.
func TestRestartRecoversFromAKilledDaemon(t *testing.T) {
	p := newPaths(t)
	old := p.startRun(t)
	t.Cleanup(func() { killDaemons(p) })
	if err := old.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(old.Process.Pid) }, "the daemon survived SIGKILL")
	// The premise, asserted rather than assumed: without the leftover socket this
	// test is the no-daemon case above, and would pass without exercising anything.
	if _, err := os.Lstat(p.socket); err != nil {
		t.Fatalf("the killed daemon's socket is already gone, so there is no stale file to recover from: %v", err)
	}

	if code, out := p.shelld(t, nil, "restart"); code != 0 {
		t.Fatalf("restart after SIGKILL: exit %d: %s", code, out)
	}
	info, err := daemon.Probe(p.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("restart after SIGKILL left nothing serving %s: %v", p.socket, err)
	}
	if int(info.Pid) == old.Process.Pid {
		t.Fatalf("/health reports the killed pid %d", info.Pid)
	}
}

// When stop fails, restart must not start a daemon.
//
// This is the whole of what `if code = cmdStop(c); code == 0` buys, and it is the
// arm the other cases cannot reach: they all have stop succeeding, so dropping the
// condition and daemonizing unconditionally leaves every one of them green --
// measured, with `if code = cmdStop(c); code == 0` replaced by an unconditional
// call, which fails here and nowhere else in this file.
// Here it would start a second daemon beside a process that is already serving the
// socket, which is the outcome the guard exists to prevent.
//
// The squatter is what makes stop fail, and it fails it the way a foreign or
// renamed daemon does: it answers /health, so something is plainly there, while the
// lockfile names an unrelated process, so nothing establishes that the answering
// process is the daemon. stop will not signal on that evidence and exits 1. Using a
// real process for the lockfile's pid, rather than an invented number, keeps the
// refusal about identity rather than about a pid that does not exist.
func TestRestartDoesNotStartADaemonWhenStopFails(t *testing.T) {
	p := newPaths(t)
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			// No nonce, so the answer cannot be matched to the lockfile.
			fmt.Fprintf(w, `{"status":"ok","pid":1,"socket":%q,"version":"x","startedAt":0}`, p.socket)
			return
		}
		http.Error(w, "busy", http.StatusInternalServerError)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	other := startSleep(t, p.dir, "")
	writeLock(t, p, other.Process.Pid, time.Now().Add(-time.Minute))
	t.Cleanup(func() { killDaemons(p) })

	code, out := p.shelld(t, nil, "restart")
	if code == 0 {
		t.Fatalf("restart exited 0 with a socket it could not stop: %s", out)
	}
	if !alive(other.Process.Pid) {
		t.Fatalf("restart signalled pid %d, which the lockfile names but nothing shows to be the daemon: %s", other.Process.Pid, out)
	}
	if pids := daemonProcesses(p.socket); len(pids) != 0 {
		t.Fatalf("restart started daemons %v beside a socket it could not stop: %s", pids, out)
	}
	// The socket is the squatter's listener and the lockfile is evidence about a
	// process still running, so a failed restart leaves both: removing either would
	// be acting on a conclusion stop declined to reach.
	for _, f := range []string{p.pidfile, p.socket} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed by a restart that refused to stop anything: %v", f, err)
		}
	}
	if !strings.Contains(out, "graceful shutdown failed") {
		t.Errorf("restart's output does not say stop is what failed: %s", out)
	}
}
