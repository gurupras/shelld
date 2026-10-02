//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMWhenFirstConnectableExitsCleanly: a SIGTERM must be handled from
// the moment a client can reach the socket at all, not from the moment the API
// answers. The kernel queues a connect as soon as the address is bound (see
// daemon.isBound, which had to count connects to find the backlog limit), so
// there is a stretch of startup where the daemon is reachable and still setting
// itself up -- and a signal landing there with its default disposition ends the
// process on the spot, leaving the socket file and the lockfile behind.
//
// This is the sibling of TestSIGTERMAfterHealthyExitsCleanly and differs in one
// line: the gate is a bare successful connect rather than a /health 200. That
// test cannot see this window, because a 200 needs the accept loop, which is
// past the handler registration under any ordering. Keep GOMAXPROCS=1, the
// delay-free polling and the trial count for the reason its comment gives.
//
// Measured against a build with the registration moved back below the bind,
// where it sat before it was hoisted, so this is the test the original gap did
// not have:
//
//	40 trials, GOMAXPROCS=1, no poll sleep: failed 10 of 10 runs, first
//	                                        failure at trial 0 in every run
//
// against 0 of 10 runs on the correct order. The window is wider than "a state
// struct and a lockfile write" suggests: that write and its rename go to disk,
// while the connect poll notices the bind immediately, so the signal lands
// inside it on the first attempt every time.
//
// What it cannot reach is the rest of the hoisted span -- InitCgroups spawning
// a child, Listen's own stale-socket probe -- which is before the address
// exists, so no external client can time a signal at it.
func TestSIGTERMWhenFirstConnectableExitsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the daemon repeatedly")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for trial := range 40 {
		// Not t.TempDir, for the sun_path limit its sibling documents.
		dir, err := os.MkdirTemp("", "shelld")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock, pidfile, logfile := filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.pid"), filepath.Join(dir, "s.log")
		var stderr bytes.Buffer
		cmd := exec.Command(self)
		cmd.Stderr = &stderr
		cmd.Env = append(os.Environ(), "SHELLD_TEST_DAEMON_ARGS="+strings.Join([]string{"run", "--socket", sock, "--pidfile", pidfile, "--logfile", logfile}, "\n"),
			"HOME="+dir, "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		exited, reachable := false, false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !reachable; {
			select {
			case err = <-done:
				exited = true
			default:
				reachable = connectOK(sock)
				continue
			}
			break
		}
		if !reachable {
			if !exited {
				_ = cmd.Process.Kill()
				<-done
			}
			log, _ := os.ReadFile(logfile)
			t.Fatalf("trial %d: socket never became connectable (exited: %v, %v)\nstderr:\n%s\nlog:\n%s", trial, exited, err, stderr.Bytes(), log)
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("trial %d: daemon did not exit after SIGTERM", trial)
		}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.Fatalf("trial %d: SIGTERM right after the socket first accepted a connect killed the daemon (%v) instead of being handled", trial, ws.Signal())
		}
		if err != nil {
			t.Fatalf("trial %d: exit: %v\nstderr:\n%s", trial, err, stderr.Bytes())
		}
		// The point of handling it rather than dying: a daemon killed here leaves
		// both of these for the next start to reason about.
		for _, p := range []string{sock, pidfile} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("trial %d: %s left behind after a clean shutdown (stat: %v)", trial, filepath.Base(p), err)
			}
		}
	}
}

// connectOK reports whether the address is bound, which is all a client needs
// for its connect to be queued -- deliberately not a request, so that it
// observes the earliest moment the daemon is reachable.
func connectOK(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
