package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurupras/shelld/internal/daemon"
)

// The exit code daemonize ends on when nothing answered is advice about what to
// do next: 2 says the child is alive and still starting, so waiting longer may
// work, and 1 says it is gone, so the log is where to look. Every arm here is a
// state the old code could reach with the wrong one of those, and none of them
// needs a process: the decision is a function of what the parent knows about the
// child, so it is asked as one.
func TestDaemonizeGaveUpTellsAWaitApartFromADeath(t *testing.T) {
	const pid = 4242
	c := cli{socket: "/tmp/x/s.sock", logfile: "/tmp/x/s.log", pidfile: "/tmp/x/s.pid"}
	for _, tc := range []struct {
		name     string
		r        spawnResult
		lostRace bool
		code     int
		needles  []string
		absent   []string
	}{{
		// The only arm that may ask the caller to wait.
		name:    "nothing has been reaped",
		code:    2,
		needles: []string{"still starting", c.socket},
	}, {
		// The child is dead, so "still starting" would send a script round a
		// retry loop that can only time out.
		name:    "it exited with a status",
		r:       spawnResult{reaped: true, code: 1},
		code:    1,
		needles: []string{"exited with status 1", c.logfile},
		absent:  []string{"still starting"},
	}, {
		// A status that is not a failure is still a daemon that is not there.
		name:    "it exited zero without ever answering",
		r:       spawnResult{reaped: true, code: 0},
		code:    1,
		needles: []string{"exited with status 0"},
		absent:  []string{"still starting"},
	}, {
		// There is no status, so none is invented. -1 is not an exit status and
		// "exited with status -1" reads as one.
		name:    "the wait itself failed",
		r:       spawnResult{reaped: true, err: errors.New("waitid: no child processes")},
		code:    1,
		needles: []string{"status could not be read", "no child processes", c.logfile},
		absent:  []string{"status -1", "still starting"},
	}, {
		// Losing the socket race is not a failure of this child, so its status
		// is not the thing to report even though it has one.
		name:    "it exited because another daemon holds the socket",
		r:       spawnResult{reaped: true, code: exitAlreadyServing},
		code:    1,
		needles: []string{"is running on " + c.socket, "did not respond"},
		absent:  []string{"exited with status", "still starting"},
	}, {
		// The race was seen inside the loop, so the channel is empty by the time
		// the deadline passes: the zero spawnResult must not read as a live child.
		name:     "the race was already noticed and nothing is left to reap",
		lostRace: true,
		code:     1,
		needles:  []string{"is running on " + c.socket},
		absent:   []string{"still starting"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			msg, code := daemonizeGaveUp(tc.r, tc.lostRace, pid, c)
			if code != tc.code {
				t.Errorf("exit %d, want %d: %s", code, tc.code, msg)
			}
			for _, n := range tc.needles {
				if !strings.Contains(msg, n) {
					t.Errorf("message %q does not contain %q", msg, n)
				}
			}
			for _, n := range tc.absent {
				if strings.Contains(msg, n) {
					t.Errorf("message %q contains %q, which is about a different state", msg, n)
				}
			}
			if !strings.HasSuffix(msg, "\n") {
				t.Errorf("message %q does not end in a newline, so it runs into the next line of output", msg)
			}
		})
	}
}

// lostSocket is the one exit daemonize waits past rather than reporting, so what
// it answers for each field of a spawnResult decides whether a child that died
// for some other reason is waited out to the full deadline.
func TestSpawnResultOnlyCallsTheRaceARace(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    spawnResult
		want bool
	}{
		{"the race's status", spawnResult{reaped: true, code: exitAlreadyServing}, true},
		{"any other status", spawnResult{reaped: true, code: 1}, false},
		// A status is only a status when the wait succeeded. The zero code
		// beside an error must not be read as one.
		{"an unread status that happens to equal the race's", spawnResult{reaped: true, code: exitAlreadyServing, err: errors.New("x")}, false},
		{"nothing reaped", spawnResult{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.lostSocket(); got != tc.want {
				t.Errorf("lostSocket() = %v, want %v", got, tc.want)
			}
		})
	}
}

// onStderr runs f with os.Stderr pointed at a file and returns what it wrote.
// The functions under test report to the operator through os.Stderr, and the
// sentence is half of what they decide, so a test that read only the exit code
// would leave the two sentences interchangeable.
func onStderr(t *testing.T, f func() int) (string, int) {
	t.Helper()
	fh, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	old := os.Stderr
	os.Stderr = fh
	code := f()
	os.Stderr = old
	out, err := os.ReadFile(fh.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// verdictPaths is the three paths these tests name, and nothing more: no socket
// is bound here, so t.TempDir() serves where the end-to-end tests need newPaths's
// short directory (sun_path's limit). Built locally rather than reusing newPaths
// so this file needs no `linux || darwin` tag: every decision below is
// platform-neutral, and Windows -- where these paths are not sockets at all -- is
// where an unverified daemon has the least chance of being caught by anything
// else.
func verdictPaths(t *testing.T) (socket, pidfile, logfile string) {
	dir := t.TempDir()
	return filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.pid"), filepath.Join(dir, "s.log")
}

// A daemon answering after a daemonize is verified by its nonce whichever
// process it is. The pid in a /health response is chosen by whoever answers, so
// matching it against the spawned child's pid establishes nothing -- and the
// version of this that returned 0 on that match alone reported an unverified
// process as the daemon it had just started.
//
// The pid still decides which sentence is printed, so both sentences are
// asserted: a fix that routed everything through the already-running path would
// pass on the codes alone while telling an operator their new daemon was one
// that had been there all along.
func TestDaemonizedVerifiesWhoeverAnswered(t *testing.T) {
	const pid = 4242
	for _, tc := range []struct {
		name   string
		info   daemon.LockInfo
		lock   *daemon.LockInfo // written to the pidfile; nil writes nothing
		raw    string           // written verbatim instead, for a lockfile that cannot be read
		code   int
		needle string
	}{{
		name:   "the daemon we started, verified",
		info:   daemon.LockInfo{Pid: pid, Nonce: "n"},
		lock:   &daemon.LockInfo{Pid: pid, Nonce: "n"},
		code:   0,
		needle: "shelld daemonized (pid 4242",
	}, {
		name:   "the daemon we started, with a nonce that does not match",
		info:   daemon.LockInfo{Pid: pid, Nonce: "n"},
		lock:   &daemon.LockInfo{Pid: pid, Nonce: "something-else"},
		code:   exitUnverified,
		needle: "not verified",
	}, {
		name:   "the daemon we started, with no lockfile to check against",
		info:   daemon.LockInfo{Pid: pid, Nonce: "n"},
		code:   exitUnverified,
		needle: "not verified",
	}, {
		// A lockfile that cannot be read is a separate answer from one that is
		// not there: only one of the two is a true sentence about the file, and
		// both leave the answering process unverified.
		name:   "the daemon we started, with a lockfile that cannot be read",
		info:   daemon.LockInfo{Pid: pid, Nonce: "n"},
		raw:    "{",
		code:   exitUnverified,
		needle: "not verified",
	}, {
		name:   "another daemon, verified",
		info:   daemon.LockInfo{Pid: 99, Nonce: "n"},
		lock:   &daemon.LockInfo{Pid: 99, Nonce: "n"},
		code:   0,
		needle: "shelld already running (pid 99",
	}, {
		name:   "another daemon, with a nonce that does not match",
		info:   daemon.LockInfo{Pid: 99, Nonce: "n"},
		lock:   &daemon.LockInfo{Pid: 99, Nonce: "something-else"},
		code:   exitUnverified,
		needle: "not verified",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			socket, pidfile, logfile := verdictPaths(t)
			c := cli{socket: socket, pidfile: pidfile, logfile: logfile}
			tc.info.Socket = socket
			if tc.lock != nil {
				l := *tc.lock
				l.Socket, l.Version, l.StartedAt = socket, "test", 1
				if err := daemon.WriteLockfile(pidfile, l); err != nil {
					t.Fatal(err)
				}
			}
			if tc.raw != "" {
				if err := os.WriteFile(pidfile, []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
				// The premise: the file is unreadable rather than absent or
				// merely disagreeing, which is what this arm is about.
				if daemon.ReadLock(pidfile).Err == nil {
					t.Fatalf("%q reads as a lockfile, so this arm is not testing what it means to", tc.raw)
				}
			}
			out, code := onStderr(t, func() int { return daemonized(c, &tc.info, pid) })
			if code != tc.code {
				t.Errorf("exit %d, want %d: %s", code, tc.code, out)
			}
			if !strings.Contains(out, tc.needle) {
				t.Errorf("output %q does not contain %q", out, tc.needle)
			}
		})
	}
}
