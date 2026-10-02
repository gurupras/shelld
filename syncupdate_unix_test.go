//go:build linux || darwin

package shelld

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startScript runs body under /bin/sh in an 80x24 window; the script is a
// file so its quoting is the shell's, not splitCommandLine's.
func startScript(t *testing.T, body string) *Window {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewWindow(WindowOpts{Name: "sync", Shell: "/bin/sh " + path, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); w.Kill() })
	return w
}

// TestAnUnendedSyncUpdateIsAnsweredAtItsDeadline: a program that queries the
// terminal inside a synchronized update and then waits for the answer writes
// nothing more, so only the deadline can release the update and the reply
// the query is owed.
func TestAnUnendedSyncUpdateIsAnsweredAtItsDeadline(t *testing.T) {
	w := startScript(t, `stty raw -echo
printf '\033[?2026hheld\033[5n'
r=$(dd bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')
printf '\033[?2026l\r\ngot=%s\r\n' "$r"
sleep 30
`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := w.CaptureVisible(0)
		if strings.Contains(text, "got=") {
			if !strings.Contains(text, "held") || !strings.Contains(text, "got=1b5b306e") {
				t.Fatalf("grid after the update:\n%s\nwant the held text and got=1b5b306e (ESC [ 0 n)", text)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply 5s after a query inside an unended update; grid:\n%q", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestASyncUpdateOpenAtExitIsAppliedAtEOF: once the program has exited
// nothing more will be read to end its update, so the read loop applies it
// before the window reads dead, not at the deadline.
func TestASyncUpdateOpenAtExitIsAppliedAtEOF(t *testing.T) {
	w := startScript(t, `printf '\033[?2026hlast words'
`)
	start := time.Now()
	for w.IsAlive() {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the window never read dead")
		}
		time.Sleep(time.Millisecond)
	}
	if text := w.CaptureVisible(0); !strings.Contains(text, "last words") {
		t.Fatalf("the update was not applied by the time the window read dead (%v after start); grid:\n%q",
			time.Since(start), text)
	}
}
