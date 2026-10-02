//go:build linux || darwin

package daemon

// Daemon discovery and shutdown: working out whether a daemon is still there,
// signalling it, and removing what a dead one left behind.
//
// This is the code a user reaches when something has gone wrong — a socket
// left by a killed daemon, a `shelld kill` that has to decide whether the pid
// in a lockfile is still the daemon — and none of it was executed by any test.
// The states it distinguishes cannot be reached without real processes, so
// these tests use four fixtures:
//
//   - holdInstanceLock: this test binary re-executed as a child that takes the
//     socket's instance lock. The kernel reports a process's own record lock as
//     free, so asking it is the one arm of InstanceHolder a child is needed for:
//     when this process is the holder the answer comes from heldLocks and the
//     kernel is never asked.
//   - shelldNamed: this test binary re-executed under the name shelld-*, which
//     is what sameDaemon asks of a process it cannot see holding the lock (an
//     older daemon, or shelld-rs).
//   - exitedPid: the pid of a reaped child, for the stale-lockfile cases.
//   - busyListener (private_unix_test.go): a listener that never accepts, for a
//     daemon that is up and not keeping up.
//
// Both arms are exercised: Linux, and darwin as of 2026-09-24.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	holdLockEnv     = "SHELLD_TEST_HOLD_INSTANCE_LOCK"
	namedFixtureEnv = "SHELLD_TEST_NAMED_FIXTURE"
)

// TestInstanceLockHolderChild is not a test: it is the body of the child
// process holdInstanceLock starts. It takes the instance lock named by the
// environment and holds it until its stdin closes, so the parent controls
// exactly when the lock goes away. Run normally it skips.
func TestInstanceLockHolderChild(t *testing.T) {
	socket := os.Getenv(holdLockEnv)
	if socket == "" {
		t.Skip("child-process body, run by holdInstanceLock")
	}
	l, err := AcquireInstance(socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: %v\n", err)
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	l.Release()
	os.Exit(0)
}

// testProc is a child process a test controls: its pid, its stdin (closing it
// is how a cooperative child is asked to stop), and a channel closed when it
// has exited and been reaped.
type testProc struct {
	cmd  *exec.Cmd
	pid  int
	in   io.WriteCloser
	done chan struct{}
	err  error
}

// startChild runs cmd with a stdin pipe, reaps it in the background, and makes
// sure it is gone by the end of the test however the test ends.
func startChild(t *testing.T, cmd *exec.Cmd) *testProc {
	t.Helper()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	p := &testProc{cmd: cmd, pid: cmd.Process.Pid, in: in, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Errorf("child pid %d did not exit", p.pid)
		}
	})
	return p
}

// stop closes the child's stdin and waits for it to exit.
func (p *testProc) stop(t *testing.T) {
	t.Helper()
	_ = p.in.Close()
	p.waitExit(t, 10*time.Second)
}

// waitExit fails unless the child has exited within d.
func (p *testProc) waitExit(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(d):
		t.Fatalf("pid %d is still running after %s", p.pid, d)
	}
}

// running reports whether the child is still there, after giving it a moment
// to exit if it was about to.
func (p *testProc) running(t *testing.T) bool {
	t.Helper()
	select {
	case <-p.done:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

// holdInstanceLock starts a child holding socket's instance lock under a name
// that makes it shelld's, and waits until the lock is visibly held, so a test
// never races the child's startup.
//
// The name matters and used to be left as the test binary's. Holding a write lock
// on a file is something any process of this user can do, so the kernel naming a
// holder says that something took the lock and not that a daemon did -- and a
// fixture that is not named like a daemon cannot tell a test that reads the lock
// as sufficient apart from one that also checks. Use holdInstanceLockAs for the
// holder that is deliberately not shelld.
func holdInstanceLock(t *testing.T, socket string) *testProc {
	t.Helper()
	return holdInstanceLockAs(t, socket, "shelld-lockholder")
}

// holdInstanceLockAs is holdInstanceLock with the holder's executable name chosen.
func holdInstanceLockAs(t *testing.T, socket, name string) *testProc {
	t.Helper()
	cmd := exec.Command(namedBinary(t, filepath.Dir(socket), name), "-test.run=^TestInstanceLockHolderChild$")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+socket)
	p := startChild(t, cmd)
	deadline := time.Now().Add(20 * time.Second)
	for {
		holder, err := InstanceHolder(socket)
		if err == nil && holder == p.pid {
			return p
		}
		if !p.running(t) {
			t.Fatalf("child pid %d exited without taking the instance lock on %s (holder %d, %v)", p.pid, socket, holder, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d never took the instance lock on %s (holder %d, %v)", p.pid, socket, holder, err)
		}
	}
}

// shelldNamed starts a long-lived process whose executable's basename starts
// with "shelld", which is all sameDaemon reads: using a real daemon here would
// make the test depend on building one.
func shelldNamed(t *testing.T, dir string) *testProc {
	t.Helper()
	return sleepNamed(t, dir, "shelld-fixture")
}

// sleepNamed starts a long-lived process as name in dir, so a test can choose
// what /proc/<pid>/exe and the process's comm say.
//
// The process is this test binary rather than a system one. Copying /bin/sleep,
// which is what this did, cannot work on macOS: a byte copy of a platform binary
// is SIGKILLed at exec, measured as exit 137 on macOS 26.4. That is a launch
// constraint rather than a signature that fails to verify -- codesign -v on the
// copy passes -- so there is nothing to re-sign and no way to keep using a
// system binary here. A Go-linked binary is ad-hoc signed over its own contents,
// so a copy or a link of this one execs normally on both platforms.
//
// Worth knowing because of how it presented: the fixture died immediately, so
// every test needing a process with a chosen name failed on darwin as "the
// process has exited", which reads like LocateDaemon getting liveness wrong when
// LocateDaemon was reporting exactly what was true.
func sleepNamed(t *testing.T, dir, name string) *testProc {
	t.Helper()
	cmd := exec.Command(namedBinary(t, dir, name), "-test.run=^TestNamedFixtureChild$")
	cmd.Env = append(os.Environ(), namedFixtureEnv+"=1")
	return startChild(t, cmd)
}

// namedBinary links this test binary into dir as name and returns the path, so a
// caller can start a process whose /proc/<pid>/exe and comm say what the test
// needs them to say.
func namedBinary(t *testing.T, dir, name string) string {
	t.Helper()
	self := os.Args[0]
	if _, err := os.Stat(self); err != nil {
		t.Skipf("cannot re-execute this test binary (%s): %v", self, err)
	}
	bin := filepath.Join(dir, name)
	// Callers reuse one dir across subtests, so the entry may already be there
	// from a previous call. Unlink it rather than writing through it: unlink works
	// on a file that is being executed, where a write would get ETXTBSY. The old
	// copy-of-sleep did not need this only because WriteFile truncates in place.
	_ = os.Remove(bin)
	// A link is enough, since it is the path given to exec that names the process
	// and not the inode, and it avoids copying a test binary for every call. A
	// different filesystem under dir is the expected failure, so fall back.
	if err := os.Link(self, bin); err != nil {
		data, rerr := os.ReadFile(self)
		if rerr != nil {
			t.Skipf("cannot link (%v) or read (%v) %s", err, rerr, self)
		}
		if err := os.WriteFile(bin, data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestNamedFixtureChild is not a test: it is the body of the child process
// sleepNamed starts, and is skipped unless it is running as that child. It
// blocks on stdin, which startChild closes, so the child stops when asked
// rather than outliving the test on a fixed sleep.
func TestNamedFixtureChild(t *testing.T) {
	if os.Getenv(namedFixtureEnv) == "" {
		t.Skip("only meaningful as sleepNamed's child process")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// exitedPid is the pid of a child that has run and been reaped, so nothing is
// there any more. Pids are reused, so the caller is skipped rather than given
// a pid the kernel has handed out again.
func exitedPid(t *testing.T) int {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	cmd := exec.Command(sh, "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c 'exit 0': %v", err)
	}
	pid := cmd.ProcessState.Pid()
	if _, alive, err := processInfo(pid); err != nil || alive {
		t.Skipf("pid %d was reused before it could be used as a dead pid (alive=%v, %v)", pid, alive, err)
	}
	return pid
}

// deadSocketFile leaves a socket file behind that nothing listens on, and none
// of shelld's other files beside it. That is the state another program's socket
// is in once that program has died -- a killed shelld daemon also leaves its
// <socket>.lock, and a caller that wants the leftovers of a shelld daemon has to
// add that itself.
func deadSocketFile(t *testing.T, path string) {
	t.Helper()
	ln, err := listen(path, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A killed daemon never runs Close, so its socket file outlives it. Closing a
	// listener from listen removes the file, which is how a daemon that exits
	// cleanly leaves nothing behind, so clear that here to leave the file the way a
	// SIGKILL does. The Lstat below is what holds this helper to its name.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("lstat %s: %v (%v)", path, err, st)
	}
}

// TestLocateDaemonTrustsTheInstanceLock covers the branch that makes the
// lockfile's pid advisory: whoever holds the fcntl lock on <socket>.lock is
// the daemon, whatever a lockfile says and whether or not anything answers on
// the socket. Nothing here binds a socket, so a LocateDaemon that reached for
// /health instead of the lock would report the daemon gone and the caller
// would delete a running daemon's files.
func TestLocateDaemonTrustsTheInstanceLock(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	pidfile := filepath.Join(dir, "shelld.pid")
	deadSocketFile(t, sock)
	if err := WriteLockfile(pidfile, LockInfo{Pid: 1, Socket: sock, Version: "v", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	held := holdInstanceLock(t, sock)

	for _, c := range []struct {
		name string
		lock *LockInfo
	}{
		{"no lockfile at all", nil},
		// pid 1 is not this user's shelld by any measure sameDaemon applies, and
		// the state is still verified: the lock outranks the file.
		{"a lockfile naming another process", &LockInfo{Pid: 1, StartedAt: 1}},
	} {
		loc := LocateDaemon(sock, LockRead{Info: c.lock})
		if loc.State != DaemonVerified || loc.Pid != held.pid {
			t.Errorf("%s: LocateDaemon = %+v, want verified pid %d", c.name, loc, held.pid)
		}
	}

	// The lock is what keeps a second daemon off the socket, and the refusal
	// names the holder so a user can look it up.
	if _, err := AcquireInstance(sock); !errors.Is(err, ErrInstanceHeld) || !strings.Contains(err.Error(), fmt.Sprint(held.pid)) {
		t.Errorf("AcquireInstance while held = %v, want ErrInstanceHeld naming pid %d", err, held.pid)
	}
	if NothingServes(sock) {
		t.Error("NothingServes is true while the instance lock is held")
	}
	// RemoveStale cannot take the lock, so it removes nothing: this is the
	// guard that stops a stale-file cleanup from deleting a live daemon's
	// socket while the daemon is only briefly not answering.
	if ok, err := RemoveStale(sock, pidfile); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want it to refuse the files of a daemon holding the instance lock, and say why", ok, err)
	}
	for _, path := range []string{sock, pidfile} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(path), err)
		}
	}

	held.stop(t)

	// With the holder gone and nothing answering, the same socket reads as
	// gone and its files may be removed.
	if loc := LocateDaemon(sock, LockRead{}); loc.State != DaemonGone {
		t.Errorf("after the holder exited: LocateDaemon = %+v, want gone", loc)
	}
	if !NothingServes(sock) {
		t.Error("NothingServes is false after the lock holder exited")
	}
	if ok, err := RemoveStale(sock, pidfile); !ok || err != nil {
		t.Fatalf("RemoveStale = %v, %v; want it to remove a stale socket with no holder", ok, err)
	}
	for _, path := range []string{sock, pidfile} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived RemoveStale: %v", filepath.Base(path), err)
		}
	}
	if err := removeSocketFile(sock); err != nil { // removing a socket that is already gone is not an error
		t.Errorf("removeSocketFile on an already-removed socket: %v", err)
	}
}

// TestLocateDaemonWithoutTheLock covers the fallback for a daemon whose
// instance lock is free — an older Go daemon, or shelld-rs — where the only
// evidence is the pid in the lockfile plus whether anything is bound to the
// socket. The distinction that matters is unverified versus gone: gone licenses
// the caller to delete files and reuse the socket, so anything bound has to hold
// the state at unverified even when the named process is refuted. The arms below
// use a daemon that answers, which is the easy half of "bound";
// TestABusyPeerIsNotStale covers the half that only the errno distinguishes.
func TestLocateDaemonWithoutTheLock(t *testing.T) {
	live := boot(t) // listen does not take the instance lock; the caller does
	if holder, err := InstanceHolder(live); holder != 0 || err != nil {
		t.Fatalf("setup: InstanceHolder(%s) = %d, %v, want 0", live, holder, err)
	}
	if !IsAlive(live, 2*time.Second) {
		t.Fatal("setup: the booted daemon does not answer")
	}
	dir := shortDir(t)
	dead := filepath.Join(dir, "dead.sock") // nothing has ever listened here
	gonePid := exitedPid(t)
	self := int32(os.Getpid()) // alive, but its image is the test binary
	now := time.Now().Unix()
	old := time.Now().Add(-time.Hour).Unix()
	named := shelldNamed(t, dir)

	for _, c := range []struct {
		name   string
		socket string
		lock   *LockInfo
		want   DaemonState
		reason string
	}{
		{"nothing answers and no lockfile", dead, nil, DaemonGone, ""},
		{"the socket answers but no lockfile names its process", live, nil, DaemonUnverified, "no lockfile"},
		{"the lockfile's process has exited", dead, &LockInfo{Pid: int32(gonePid), StartedAt: now}, DaemonGone, "the process has exited"},
		{"it has exited but the socket answers", live, &LockInfo{Pid: int32(gonePid), StartedAt: now}, DaemonUnverified, "has exited"},
		{"the pid belongs to something else", dead, &LockInfo{Pid: self, StartedAt: now}, DaemonGone, "not shelld"},
		{"something else, and the socket answers", live, &LockInfo{Pid: self, StartedAt: now}, DaemonUnverified, "not shelld"},
		// Named shelld but started after the lockfile was written: that is what
		// a reused pid looks like.
		{"a shelld started after the lockfile", dead, &LockInfo{Pid: int32(named.pid), StartedAt: old}, DaemonGone, "after the lockfile"},
		{"a live shelld the lockfile names", dead, &LockInfo{Pid: int32(named.pid), Socket: dead, StartedAt: now}, DaemonVerified, ""},
		// The lockfile was rewritten by a daemon on another socket after the
		// caller read its socket from it: that daemon is not this socket's, and
		// its lockfile is not a leftover to remove.
		{"a live shelld the lockfile names for another socket", dead, &LockInfo{Pid: int32(named.pid), Socket: live, StartedAt: now}, DaemonUnverified, "names the socket"},
	} {
		loc := LocateDaemon(c.socket, LockRead{Info: c.lock})
		if loc.State != c.want || !strings.Contains(loc.Reason, c.reason) {
			t.Errorf("%s: LocateDaemon = %+v, want state %d with reason containing %q", c.name, loc, c.want, c.reason)
		}
		if c.lock != nil && loc.Pid != int(c.lock.Pid) {
			t.Errorf("%s: LocateDaemon reported pid %d, want the lockfile's %d", c.name, loc.Pid, c.lock.Pid)
		}
	}

	// A live socket is the other reason RemoveStale refuses: it takes the lock
	// (nobody holds it) and then finds the daemon answering.
	if ok, err := RemoveStale(live, filepath.Join(dir, "absent.pid")); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want it to refuse the files of a daemon that answers on its socket", ok, err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Errorf("a live daemon's socket was removed: %v", err)
	}
	if NothingServes(live) {
		t.Error("NothingServes is true for a socket that answers")
	}
	if !NothingServes(dead) {
		t.Error("NothingServes is false for a socket nothing ever listened on")
	}
}

// TestLocateDaemonWillNotCallAnUnreadableLockfileGone covers the outcome a
// *LockInfo could not express. With no lockfile and nothing bound the answer is
// DaemonGone, and gone is what licenses a caller to delete the socket and the
// lockfile and start a second daemon -- so a read that failed must not arrive
// here as absence. Both arms below stay unverified, including the one where
// nothing is bound, because an unreadable file has shown nothing either way.
//
// The reason has to carry the path and the read error: a damaged file and a file
// this user may not open need different repairs, and the sentinel alone cannot
// tell them apart.
func TestLocateDaemonWillNotCallAnUnreadableLockfileGone(t *testing.T) {
	live := boot(t) // listen does not take the instance lock; the caller does
	dir := shortDir(t)
	dead := filepath.Join(dir, "dead.sock") // nothing has ever listened here
	pidfile := filepath.Join(dir, "shelld.pid")
	// Unparseable rather than mode 000: a suite running as root reads a mode-000
	// file, and the fixture would stop reproducing without failing.
	if err := os.WriteFile(pidfile, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := ReadLock(pidfile)
	if lock.Err == nil {
		t.Fatalf("setup: the damaged lockfile read as %+v", lock.Info)
	}
	for _, c := range []struct {
		name   string
		socket string
	}{
		{"nothing is bound", dead},
		{"the socket answers", live},
	} {
		loc := LocateDaemon(c.socket, lock)
		if loc.State != DaemonUnverified {
			t.Errorf("%s: LocateDaemon = %+v, want unverified: gone sends the caller on to delete the files of a daemon this never looked for", c.name, loc)
		}
		if !strings.Contains(loc.Reason, pidfile) || !strings.Contains(loc.Reason, lock.Err.Error()) {
			t.Errorf("%s: reason %q names neither %s nor why it could not be read", c.name, loc.Reason, pidfile)
		}
		if strings.Contains(loc.Reason, "no lockfile") {
			t.Errorf("%s: reason %q says there is no lockfile, and one is sitting at %s", c.name, loc.Reason, pidfile)
		}
	}
}

// TestABusyPeerIsNotStale is TestABusyDaemonIsNotDead from the cleanup side: a
// daemon that does not take the instance lock -- an older shelld, or shelld-rs
// sharing the socket path -- whose backlog has filled because it is not keeping
// up. The lock is free, so what is bound to the socket is the only evidence
// there is, and a probe that asks for a reply rather than asking what is bound
// reports it gone.
//
// Gone is not where that stops, which is why this checks all three predicates
// and not the removal alone: they are three spellings of one question, and each
// of them gates the same deletion. `shelld stop` gets no answer to /shutdown,
// asks LocateDaemon, is told gone, and calls RemoveStale -- which unlinks a
// running daemon's socket and exits 0, leaving it serving under a name no client
// can reach.
func TestABusyPeerIsNotStale(t *testing.T) {
	dir := shortDir(t)
	sock, held := busyListener(t, filepath.Join(dir, "s.sock"))
	pidfile := filepath.Join(dir, "shelld.pid")
	before, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	// Both halves of the premise: the lock is free, so nothing but the socket can
	// refuse the removal, and a request to it really does fail. Without either this
	// is not the state it names -- it would be passing on the lock, or on a daemon
	// that was answering all along.
	if holder, err := InstanceHolder(sock); err != nil || holder > 0 {
		t.Fatalf("setup: the instance lock is not free (holder %d, %v), so the lock alone would refuse the removal and nothing here would rest on the socket", holder, err)
	}
	if IsAlive(sock, 500*time.Millisecond) {
		t.Fatalf("setup: a request to a listener holding %d queued connects was answered, so its backlog is not full and this is not a daemon that fails to reply", held)
	}

	if loc := LocateDaemon(sock, LockRead{}); loc.State != DaemonUnverified {
		t.Errorf("LocateDaemon = %+v, want unverified: it is bound and running, and gone sends every caller on to delete its socket", loc)
	}
	if NothingServes(sock) {
		t.Errorf("NothingServes is true for a listener bound with %d queued connects, so waitGone reports a daemon gone while it is still serving", held)
	}
	if ok, err := RemoveStale(sock, pidfile); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want it to refuse the files of a daemon that is bound and running", ok, err)
	}
	if after, err := os.Lstat(sock); err != nil || inodeOf(after) != inodeOf(before) {
		t.Errorf("a running daemon's socket is gone or replaced: inode %d, was %d (%v)", inodeOf(after), inodeOf(before), err)
	}
}

// TestLocateDaemonCannotReadTheLockFile covers the one arm where the lock
// itself is unreadable. openLockFile refuses to follow a symlink, because the
// socket can live in a directory another user can write to and following one
// would take a lock on a file of their choosing. The point of the assertion is
// that an unreadable lock is unverified rather than gone: a caller must not
// read "I cannot tell" as "nothing is there".
func TestLocateDaemonCannotReadTheLockFile(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	if err := os.Symlink(filepath.Join(dir, "target"), lockPath(sock)); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if holder, err := InstanceHolder(sock); err == nil {
		t.Fatalf("InstanceHolder followed a symlinked lock file: holder %d", holder)
	}
	loc := LocateDaemon(sock, LockRead{Info: &LockInfo{Pid: int32(os.Getpid()), StartedAt: time.Now().Unix()}})
	if loc.State != DaemonUnverified || !strings.Contains(loc.Reason, filepath.Base(lockPath(sock))) {
		t.Errorf("LocateDaemon = %+v, want unverified naming %s", loc, lockPath(sock))
	}
	if ok, err := RemoveStale(sock, filepath.Join(dir, "shelld.pid")); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want it to refuse without being able to take the instance lock", ok, err)
	}
}

// TestSignalDaemonRefusesWhatItCannotVerify is the guard on `shelld kill`:
// with only a lockfile's pid to go on, an unverified process must not be
// signalled, and the error has to carry ErrNotDaemon so a caller can tell this
// apart from a failed signal. The named process is checked to be still running
// afterwards, since the defect this prevents is killing a stranger that
// inherited the pid.
//
// The refusal is doubly guarded: signalDaemon checks the state, pins the
// process, and checks again. A mutant that deletes either check alone survives
// this test, because the other one still refuses — a redundancy in the code
// rather than a gap in the test. Deleting both is killed here.
func TestSignalDaemonRefusesWhatItCannotVerify(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	named := shelldNamed(t, dir)
	// Named shelld, alive, but the lockfile claims to predate it by an hour.
	lock := &LockInfo{Pid: int32(named.pid), Socket: sock, StartedAt: time.Now().Add(-time.Hour).Unix()}

	for _, c := range []struct {
		name string
		fn   func(string, LockRead) (int, error)
	}{
		{"TerminateDaemon", TerminateDaemon},
		{"KillDaemon", KillDaemon},
	} {
		pid, err := c.fn(sock, LockRead{Info: lock})
		if !errors.Is(err, ErrNotDaemon) {
			t.Fatalf("%s: err = %v, want ErrNotDaemon", c.name, err)
		}
		if pid != named.pid {
			t.Errorf("%s: reported pid %d, want the refused %d", c.name, pid, named.pid)
		}
		if !strings.Contains(err.Error(), "after the lockfile") {
			t.Errorf("%s: error %q does not say why the process was refused", c.name, err)
		}
	}
	if !named.running(t) {
		t.Fatalf("pid %d was signalled even though it was not shown to be the daemon", named.pid)
	}

	// With nothing at all to signal the error reads as gone rather than as a
	// refusal about some pid.
	if pid, err := TerminateDaemon(sock, LockRead{}); !errors.Is(err, ErrNotDaemon) || pid != 0 || !strings.Contains(err.Error(), "no shelld daemon to signal") {
		t.Errorf("TerminateDaemon with no daemon = %d, %v", pid, err)
	}
}

// TestSignalDaemonSignalsTheLockHolder is the other half: a verified daemon is
// signalled, and the pid returned is the one that was signalled. Each arm uses
// its own child, since the first arm kills its own.
func TestSignalDaemonSignalsTheLockHolder(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func(string, LockRead) (int, error)
	}{
		{"TerminateDaemon", TerminateDaemon},
		{"KillDaemon", KillDaemon},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock := filepath.Join(shortDir(t), "s.sock")
			held := holdInstanceLock(t, sock)
			pid, err := c.fn(sock, LockRead{})
			if err != nil || pid != held.pid {
				t.Fatalf("%s = %d, %v, want the holder %d and no error", c.name, pid, err, held.pid)
			}
			held.waitExit(t, 10*time.Second)
			// The lock dies with the process, so the socket now reads as free.
			if holder, err := InstanceHolder(sock); holder != 0 || err != nil {
				t.Errorf("InstanceHolder after the signal = %d, %v, want 0", holder, err)
			}
		})
	}
}

// TestKillPidAndTerminatePid covers the unconditional signal helpers, which
// differ in what they make of a process that is already gone: KillPid is used
// on the way out of a failed start, where the pid may well have exited
// already, so ESRCH is success there and an error in TerminatePid.
func TestKillPidAndTerminatePid(t *testing.T) {
	dir := shortDir(t)
	for _, c := range []struct {
		name string
		fn   func(int32) error
	}{
		{"KillPid", KillPid},
		{"TerminatePid", TerminatePid},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := shelldNamed(t, dir)
			if err := c.fn(int32(p.pid)); err != nil {
				t.Fatalf("%s(%d) = %v", c.name, p.pid, err)
			}
			p.waitExit(t, 10*time.Second)
		})
	}
	gone := int32(exitedPid(t))
	if err := KillPid(gone); err != nil {
		t.Errorf("KillPid on an exited pid = %v, want nil: a process that is already gone is what the caller wanted", err)
	}
	if err := TerminatePid(gone); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("TerminatePid on an exited pid = %v, want ESRCH", err)
	}
}

// TestSignalHelpersRefuseANonPositivePid pins the guard on the two helpers,
// because the kernel would accept what it refuses and would not report an error
// for it: kill(0, sig) signals every process in the caller's own group -- this
// test binary included -- and kill(-N, sig) signals process group N. Asserting the
// error is the whole test; the signal is deliberately never sent, since the way to
// observe it firing is for the test process and its siblings to die.
//
// Signal 0 is used for the control instead, which delivers nothing and runs only
// the kernel's target resolution, so the premise that these targets are accepted
// rather than rejected is checked rather than assumed.
func TestSignalHelpersRefuseANonPositivePid(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func(int32) error
	}{
		{"KillPid", KillPid},
		{"TerminatePid", TerminatePid},
	} {
		for _, pid := range []int32{0, -1, -1198699, math.MinInt32} {
			if err := c.fn(pid); err == nil {
				t.Errorf("%s(%d) = nil, want an error: that pid is a process group, not a process", c.name, pid)
			}
		}
	}
	// The control: the kernel does accept 0 and -1 as targets, so the guard above
	// is what stops them rather than an ESRCH that would have come anyway.
	for _, pid := range []int{0, -1} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("kill(%d, 0) = %v, want nil; the guard's premise is that the kernel accepts this target", pid, err)
		}
	}
	if err := syscall.Kill(exitedPid(t), 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill(exited pid, 0) = %v, want ESRCH; without this the control above says nothing", err)
	}
}

// TestSpawnDetached covers how the CLI starts a daemon: in its own session, so
// it survives the terminal that started it, with stdout and stderr appended to
// the log and stdin on /dev/null. A daemon started with the caller's stdin
// would block or read the user's keystrokes, and a truncating open would throw
// away the log of the previous run at exactly the moment it is wanted.
func TestSpawnDetached(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	dir := shortDir(t)
	logFile := filepath.Join(dir, "shelld.log")
	if err := os.WriteFile(logFile, []byte("from the previous run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The child reports what its stdin is rather than what it reads from it:
	// reading proves very little, since a write-only fd and /dev/null both come
	// back empty (a mutant passing the log file as stdin survived a byte-count
	// assertion for exactly that reason).
	script := `echo to stdout; echo to stderr >&2; printf 'stdin=%s\n' "$(readlink /proc/self/fd/0)"`
	proc, err := SpawnDetached(sh, []string{"-c", script}, logFile)
	if err != nil {
		t.Fatal(err)
	}
	// The process is a zombie until Wait, so its session is still readable.
	sid, err := unix.Getsid(proc.Pid)
	if err != nil {
		t.Fatalf("getsid(%d): %v", proc.Pid, err)
	}
	if sid != proc.Pid {
		t.Errorf("session %d, want the child's own %d: it is not a session leader and dies with the terminal", sid, proc.Pid)
	}
	if mine, err := unix.Getsid(os.Getpid()); err == nil && sid == mine {
		t.Errorf("the child shares this process's session %d", mine)
	}
	state, err := proc.Wait()
	if err != nil || !state.Success() {
		t.Fatalf("child: %v, %v", state, err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := []string{"from the previous run", "to stdout", "to stderr"}
	if runtime.GOOS == "linux" {
		want = append(want, "stdin=/dev/null")
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("log %q does not contain %q", got, w)
		}
	}

	if _, err := SpawnDetached(sh, nil, filepath.Join(dir, "no-such-dir", "x.log")); err == nil || !strings.Contains(err.Error(), "open log") {
		t.Errorf("unwritable log = %v, want an error naming the log", err)
	}
	if _, err := SpawnDetached(filepath.Join(dir, "no-such-binary"), nil, logFile); err == nil || !strings.Contains(err.Error(), "spawn") {
		t.Errorf("missing binary = %v, want an error naming the binary", err)
	}
}

// TestDefaultLogfile pins the log's location and its override, the way
// TestDefaultPathsPerUser pins the socket's and the lockfile's: a user who has
// pointed the daemon somewhere else has to be able to find its log.
func TestDefaultLogfile(t *testing.T) {
	t.Setenv("SHELLD_LOGFILE", "")
	if got, want := DefaultLogfile(), filepath.Join(DefaultDir(), "shelld.log"); got != want {
		t.Errorf("DefaultLogfile = %s, want %s", got, want)
	}
	t.Setenv("SHELLD_LOGFILE", "/x/y.log")
	if got := DefaultLogfile(); got != "/x/y.log" {
		t.Errorf("DefaultLogfile = %s, SHELLD_LOGFILE ignored", got)
	}
}

// TestTailLines covers the log excerpt a failed start prints. The cases that
// matter are the ones where a naive split misleads: the trailing newline every
// log line ends with must not show up as a blank last line (which would cost
// the user one line of the n they asked for), a blank line inside the excerpt
// must survive, and asking for more lines than there are must not panic.
func TestTailLines(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		n    int
		want []string
	}{
		{"the trailing newline is not a line", "a\nb\nc\n", 2, []string{"b", "c"}},
		{"no trailing newline", "a\nb\nc", 2, []string{"b", "c"}},
		{"fewer lines than asked for", "a\n", 5, []string{"a"}},
		{"empty", "", 3, nil},
		// One newline is one blank line: only the final line terminator is
		// discarded, so text that is all terminator still has a line in it.
		{"only a newline", "\n", 3, []string{""}},
		{"a blank line inside the excerpt", "a\n\nb\n", 3, []string{"a", "", "b"}},
		{"crlf", "a\r\nb\r\n", 2, []string{"a", "b"}},
		{"none asked for", "a\nb\n", 0, nil},
	} {
		got := TailLines(c.text, c.n)
		if len(got) != len(c.want) {
			t.Errorf("%s: TailLines(%q, %d) = %q, want %q", c.name, c.text, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: TailLines(%q, %d) = %q, want %q", c.name, c.text, c.n, got, c.want)
				break
			}
		}
	}
}

// TestShutdownRouteStopsServing covers POST /shutdown, which had no test at
// all. It answers before it acts, on purpose: the client has to receive the
// 200 on the socket the daemon is about to stop serving. Serve sets Shutdown so
// the daemon can remove its lockfile and socket on the way out, and when
// nothing set it the route falls back to exiting — a fallback that, taken by
// mistake, would leave the files behind for the next start to clean up.
func TestShutdownRouteStopsServing(t *testing.T) {
	t.Run("Shutdown runs and the process does not exit", func(t *testing.T) {
		called := make(chan struct{}, 1)
		sock := bootWith(t, func(st *State) {
			st.Shutdown = func() { called <- struct{}{} }
			st.Exit = func(code int) { t.Errorf("Exit(%d) with a Shutdown hook set", code) }
		})
		if status, body, _ := request(t, sock, "POST", "/shutdown", ""); status != 200 || body["ok"] != true {
			t.Fatalf("POST /shutdown = %d %v", status, body)
		}
		select {
		case <-called:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown was not called")
		}
	})

	t.Run("without a hook the daemon exits 0", func(t *testing.T) {
		codes := make(chan int, 1)
		sock := bootWith(t, func(st *State) { st.Exit = func(code int) { codes <- code } })
		if status, _, raw := request(t, sock, "POST", "/shutdown", ""); status != 200 {
			t.Fatalf("POST /shutdown = %d: %s", status, raw)
		}
		select {
		case code := <-codes:
			if code != 0 {
				t.Errorf("Exit(%d), want 0: a requested shutdown is not a failure", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("neither Shutdown nor Exit was called")
		}
	})
}

// TestRemoveStaleRefusesAPathThatHoldsNoSocket arms the case that needs no lock
// holder, no unreadable file and no race: a --socket that names something other
// than a socket. Both of RemoveStale's guards ask whether anything is serving the
// path, and every one of these answers no -- nothing is bound to a text file and
// nobody holds its instance lock -- so each read as a stale socket and was
// unlinked.
//
// Each case also asserts that no <socket>.lock was created beside it. That is not
// a tidiness check: taking the instance lock is what creates that file, so its
// absence is the evidence that the type was established before the lock rather
// than after it. A version of this fix that checked the type at the unlink would
// leave the file behind and pass every other assertion here.
func TestRemoveStaleRefusesAPathThatHoldsNoSocket(t *testing.T) {
	dir := shortDir(t)
	const body = "these bytes are not a socket and not shelld's\n"

	for _, c := range []struct {
		name   string
		make   func(t *testing.T, path string)
		expect string
	}{
		{"a regular file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "a regular file"},
		{"a directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "a directory"},
		{"a symbolic link", func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
				t.Skipf("cannot create a symlink: %v", err)
			}
		}, "a symbolic link"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
			c.make(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			// The premise: nothing serves it. Without this the refusal below could be
			// coming from the "still served" arm, which would make the assertion pass
			// for the wrong reason and say nothing about the type check.
			if !NothingServes(path) {
				t.Fatalf("something serves %s, so this case is not testing what it means to", path)
			}

			ok, err := RemoveStale(path, filepath.Join(dir, "absent.pid"))
			if ok || err == nil {
				t.Fatalf("RemoveStale = %v, %v; want a refusal naming what it found", ok, err)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Errorf("error %q does not say it found %s", err, c.expect)
			}
			after, lerr := os.Lstat(path)
			if lerr != nil {
				t.Fatalf("%s was removed: %v", c.name, lerr)
			}
			if !os.SameFile(before, after) {
				t.Errorf("%s was replaced", c.name)
			}
			if after.Mode().IsRegular() {
				if got, rerr := os.ReadFile(path); rerr != nil || string(got) != body {
					t.Errorf("contents are %q (%v), want %q: existing is not surviving", got, rerr, body)
				}
			}
			if _, err := os.Lstat(lockPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was created beside a path RemoveStale refused, so the instance lock was taken before the type was established: %v", lockPath(path), err)
			}
		})
	}

	// The control: a real socket with nothing serving it is still removed. Without
	// it every assertion above is satisfied by a RemoveStale that refuses
	// everything, which would leave every stale socket on the box behind.
	t.Run("a stale socket is still removed", func(t *testing.T) {
		sock := filepath.Join(dir, "stale.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		// Closed without unlinking, which is what a killed daemon leaves behind.
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
		// The other thing a killed daemon leaves: acquireHeld creates this before
		// the bind and nothing ever removes it. It is what tells a stale shelld
		// socket apart from another program's socket whose process died, which the
		// file's own type cannot -- see socketWasServed. Without it this fixture is
		// the foreign socket RemoveStale must not touch, so the arm would be
		// asserting the opposite of what it is for.
		if err := os.WriteFile(lockPath(sock), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if st, err := os.Lstat(sock); err != nil || st.Mode().Type() != fs.ModeSocket {
			t.Fatalf("the fixture is not a leftover socket: %v, %v", st, err)
		}
		if ok, err := RemoveStale(sock, filepath.Join(dir, "absent.pid")); !ok || err != nil {
			t.Fatalf("RemoveStale = %v, %v; want a stale socket removed", ok, err)
		}
		if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the stale socket survived: %v", err)
		}
	})
}

// TestLocateDaemonDoesNotTrustANonShelldLockHolder is the other side of
// TestLocateDaemonTrustsTheInstanceLock, and the pair is the point: the lock is
// how the daemon is found, and holding a write lock on a file is something any
// process of this user can do, so the kernel naming a holder is a candidate and
// not a verdict. It was read as a verdict on the path whose next step is to send a
// signal, which is how a process that had done nothing but take a lock was
// SIGTERMed while the operator was told the daemon was being terminated.
//
// Nothing is ever bound here and no lockfile is readable, so a caller has only the
// lock to go on -- which is what the arm under test had.
func TestLocateDaemonDoesNotTrustANonShelldLockHolder(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	held := holdInstanceLockAs(t, sock, "not-shelld-at-all")

	// The premise: the kernel really does name this process as the holder. Without
	// it the refusal below could be a socket with no holder at all, which is a
	// different arm and would make this test pass while saying nothing.
	if holder, err := InstanceHolder(sock); holder != held.pid || err != nil {
		t.Fatalf("InstanceHolder = %d, %v, want the holder %d: this test is not reaching the arm it is about", holder, err, held.pid)
	}

	loc := LocateDaemon(sock, LockRead{})
	if loc.State != DaemonUnverified {
		t.Errorf("LocateDaemon = %+v, want unverified: a process that only holds a lock is not shown to be the daemon", loc)
	}
	// Unverified and not gone. Gone is what sends every caller on to remove files,
	// so demoting this to gone would trade a wrong signal for a wrong delete.
	if loc.State == DaemonGone {
		t.Error("LocateDaemon reported gone, which makes the caller remove files instead")
	}
	if loc.Pid != held.pid {
		t.Errorf("LocateDaemon reported pid %d, want the holder %d so the message can name it", loc.Pid, held.pid)
	}
	if !strings.Contains(loc.Reason, "not-shelld-at-all") {
		t.Errorf("reason %q does not name the executable that was found", loc.Reason)
	}

	// The whole point: it is not signalled. Both signals, because the refusal has to
	// hold for the one `stop` sends as well as the one `kill` does.
	for _, c := range []struct {
		name string
		fn   func(string, LockRead) (int, error)
	}{
		{"TerminateDaemon", TerminateDaemon},
		{"KillDaemon", KillDaemon},
	} {
		pid, err := c.fn(sock, LockRead{})
		if !errors.Is(err, ErrNotDaemon) {
			t.Errorf("%s = %d, %v, want ErrNotDaemon", c.name, pid, err)
		}
		if pid != held.pid {
			t.Errorf("%s reported pid %d, want the refused %d", c.name, pid, held.pid)
		}
	}
	if !held.running(t) {
		t.Fatalf("pid %d was signalled for holding a lock, which is what this test exists to stop", held.pid)
	}

	// And the files it holds the lock for are left alone, since the socket is still
	// being served as far as anything here can tell.
	if ok, err := RemoveStale(sock, filepath.Join(dir, "absent.pid")); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want a refusal while a process holds the instance lock", ok, err)
	}
}

// TestHolderLocatedDemotesOnlyOnAPositiveAnswer covers what holderLocated does
// when the holder cannot be read, which no fixture can produce on demand: the
// process would have to exit between the kernel naming it and processInfo being
// called. Splitting the decision out from the syscall is what makes that arm
// reachable at all.
//
// It must stay verified. An unreadable holder says nothing about whether the
// holder was the daemon, and the caller acts on anything other than verified by
// removing files or by reporting a daemon that is there as gone -- so a failed
// read has to leave the signal guarded by the lock alone, exactly as it was.
func TestHolderLocatedDemotesOnlyOnAPositiveAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		info  procInfo
		alive bool
		err   error
		want  DaemonState
	}{
		{"a shelld image", procInfo{image: "/usr/bin/shelld"}, true, nil, DaemonVerified},
		{"a shelld image with a suffix", procInfo{image: "/tmp/shelld-fixture"}, true, nil, DaemonVerified},
		{"a deleted shelld image", procInfo{image: "/usr/bin/shelld (deleted)"}, true, nil, DaemonVerified},
		{"the image could not be read", procInfo{}, false, errors.New("permission denied"), DaemonVerified},
		{"the holder has exited since", procInfo{}, false, nil, DaemonVerified},
		{"read, alive, and not shelld", procInfo{image: "/usr/bin/python3"}, true, nil, DaemonUnverified},
	} {
		got := holderLocated(4242, c.info, c.alive, c.err)
		if got.State != c.want {
			t.Errorf("%s: holderLocated = %+v, want state %v", c.name, got, c.want)
		}
		if got.Pid != 4242 {
			t.Errorf("%s: holderLocated dropped the holder's pid: %+v", c.name, got)
		}
	}
}
