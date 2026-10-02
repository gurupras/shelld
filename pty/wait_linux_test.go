package pty

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// skipWithoutPidfd skips where Wait falls back to holding a thread by design.
func skipWithoutPidfd(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd_open unavailable: %v", err)
	}
	unix.Close(fd)
}

func startSh(t *testing.T, script string) *PTY {
	t.Helper()
	p, err := Start(Command{Path: "/bin/sh", Args: []string{"sh", "-c", script}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func countDir(dir string) int {
	es, _ := os.ReadDir(dir)
	return len(es)
}

// Waiting on many running processes must not hold an OS thread each.
func TestWaitHoldsNoThread(t *testing.T) {
	if _, err := os.Stat("/proc/self/task"); err != nil {
		t.Skip("no /proc")
	}
	skipWithoutPidfd(t)
	const n = 64
	before := countDir("/proc/self/task")
	ps := make([]*PTY, 0, n)
	done := make(chan ExitStatus, n)
	for range n {
		p, err := Start(Command{Path: "/bin/sleep", Args: []string{"sleep", "30"}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		ps = append(ps, p)
		go func() {
			st, _ := p.Wait()
			done <- st
		}()
	}
	time.Sleep(200 * time.Millisecond)
	during := countDir("/proc/self/task")
	for _, p := range ps {
		_ = p.Kill()
	}
	for range n {
		select {
		case st := <-done:
			if st.Signal != "SIGKILL" {
				t.Fatalf("status %+v, want SIGKILL", st)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Wait did not return after Kill")
		}
	}
	if during-before >= n/2 {
		t.Fatalf("%d waiting processes added %d threads (%d → %d)", n, during-before, before, during)
	}
}

// A child that exited before Wait is reaped at once with its status, and a
// second Wait fails instead of waiting again.
func TestWaitAlreadyExited(t *testing.T) {
	p := startSh(t, "exit 7")
	defer p.Close()
	time.Sleep(300 * time.Millisecond)
	t0 := time.Now()
	st, err := p.Wait()
	if err != nil || st != (ExitStatus{Code: 7}) {
		t.Fatalf("Wait = %+v, %v; want code 7", st, err)
	}
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("Wait on an exited child took %v", d)
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.Wait()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("second Wait succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Wait hung")
	}
}

// Start, Wait and Close leave no descriptors behind, whether Wait runs before
// Close or after it. Closing first may hang the child up before it exits.
func TestWaitLeaksNoDescriptors(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc")
	}
	before := countDir("/proc/self/fd")
	for i := range 300 {
		p := startSh(t, "exit 3")
		closeFirst := i%2 == 1
		if closeFirst {
			// Close releases the exit descriptor too when Wait has not taken it.
			open, owned := countDir("/proc/self/fd"), 1
			if p.exit.Load() != nil {
				owned = 2
			}
			p.Close()
			if n := countDir("/proc/self/fd"); n != open-owned {
				t.Fatalf("cycle %d: Close released %d descriptors, want %d", i, open-n, owned)
			}
		}
		st, err := p.Wait()
		hungUp := closeFirst && st == ExitStatus{Code: -1, Signal: "SIGHUP"}
		if err != nil || st != (ExitStatus{Code: 3}) && !hungUp {
			t.Fatalf("cycle %d: Wait = %+v, %v; want code 3", i, st, err)
		}
		p.Close()
	}
	if after := countDir("/proc/self/fd"); after > before+2 {
		t.Fatalf("300 cycles grew descriptors %d → %d", before, after)
	}
}
