//go:build linux || darwin

package shelld

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCreateWindowAfterKillDoesNotOutliveTheSession drives the race's losing
// side directly: a caller that looked the session up before DELETE removed it
// and creates on it afterwards. That create used to add its window to the
// emptied session, answering success for a process nothing could reach.
func TestCreateWindowAfterKillDoesNotOutliveTheSession(t *testing.T) {
	m := NewManager()
	if err := m.CreateSession("s"); err != nil {
		t.Fatal(err)
	}
	s := m.Session("s")
	m.KillSession("s")

	// The check before the spawn refuses this one. Pid file or not, nothing ran.
	pidFile := filepath.Join(t.TempDir(), "pid")
	w, err := s.CreateWindow(WindowOpts{Name: "a", Shell: `/bin/sh -c "echo $$ > ` + pidFile + `; exec sleep 30"`, Cols: 80, Rows: 24})
	if !errors.Is(err, ErrSessionKilled) {
		t.Fatalf("CreateWindow on a killed session: %v, %v; want ErrSessionKilled", w, err)
	}
	if len(s.ListWindows()) != 0 {
		t.Fatalf("killed session holds %d windows", len(s.ListWindows()))
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("the refused create spawned its shell")
	}
}

// TestCreateWindowRacingKillLeavesNoProcess runs creates against Kills with no
// sequencing, so some creates pass the first check and spawn while Kill runs.
// Whatever each create answers, every shell it started must be dead afterwards:
// one that is not is a window a DELETEd session took with it.
func TestCreateWindowRacingKillLeavesNoProcess(t *testing.T) {
	dir := t.TempDir()
	const n = 40
	for i := 0; i < n; i++ {
		s := NewSession("s")
		pidFile := filepath.Join(dir, strconv.Itoa(i))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = s.CreateWindow(WindowOpts{Name: "a", Shell: `/bin/sh -c "echo $$ > ` + pidFile + `; exec sleep 30"`, Cols: 80, Rows: 24})
		}()
		time.Sleep(time.Duration(i%20) * time.Millisecond)
		s.Kill()
		<-done
	}
	// A shell that leaked gets to write its pid however late it started.
	time.Sleep(500 * time.Millisecond)
	var pids []int
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i)))
		if err != nil {
			continue // refused before the spawn, or killed before the shell ran
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	var alive []int
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			alive = append(alive, pid)
		}
	}
	t.Logf("%d of %d creates got as far as running their shell", len(pids), n)
	if len(alive) > 0 {
		t.Fatalf("%d shells outlived their killed session: %v", len(alive), alive)
	}
}
