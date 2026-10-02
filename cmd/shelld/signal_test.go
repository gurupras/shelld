//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMAfterHealthyExitsCleanly: once /health has answered 200, a
// SIGTERM must be handled — exit 0 with the socket and the lockfile (pidfile)
// removed — not kill the daemon by the default action. Supervisors and
// scripts stop the daemon right after it reports healthy, so the signal
// handler has to be installed before the API can answer.
//
// Keep the child's GOMAXPROCS=1, the delay-free /health polling and the trial
// count: they are what makes the test detect the bug. Against a build that
// started the accept loop before installing signal handling (go-parse
// proposal 3, dropped), per go test run:
//
//	20 trials, 100 µs poll sleep, default GOMAXPROCS: failed 2 of 10 runs
//	200 trials, same:                                  failed 6 of 10 runs
//	GOMAXPROCS=1, no sleep, 40 trials:                 failed 19 of 20 to
//	                                                   20 of 20 runs, first
//	                                                   failure at trials 0-34
//
// and it passed every run on the correct order, loaded or not. The child runs
// with GORACE=atexit_sleep_ms=0: a race build otherwise sleeps 1 s at exit.
func TestSIGTERMAfterHealthyExitsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the daemon repeatedly")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for trial := range 40 {
		// Not t.TempDir: its long name can push the socket path past the
		// sun_path limit (108 bytes on Linux, 104 on darwin).
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
			// One P makes the accept loop and signal setup contend, which is
			// when an ordering bug shows.
			"HOME="+dir, "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		exited := false
		healthy := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !healthy; {
			select {
			case err = <-done:
				exited = true
			default:
				healthy = healthOK(sock)
				continue
			}
			break
		}
		if !healthy {
			if !exited {
				_ = cmd.Process.Kill()
				<-done
			}
			log, _ := os.ReadFile(logfile)
			t.Fatalf("trial %d: daemon never became healthy (exited: %v, %v)\nstderr:\n%s\nlog:\n%s", trial, exited, err, stderr.Bytes(), log)
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
			t.Fatalf("trial %d: SIGTERM right after the first /health 200 killed the daemon (%v) instead of being handled", trial, ws.Signal())
		}
		if err != nil {
			t.Fatalf("trial %d: exit: %v\nstderr:\n%s", trial, err, stderr.Bytes())
		}
		for _, p := range []string{sock, pidfile} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("trial %d: %s left behind after a clean shutdown (stat: %v)", trial, filepath.Base(p), err)
			}
		}
	}
}

func healthOK(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("GET /health HTTP/1.1\r\nHost: shelld\r\nConnection: close\r\n\r\n")); err != nil {
		return false
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
