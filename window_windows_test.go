//go:build windows

package shelld

import (
	"testing"
	"time"
)

// A window whose program exits by itself reports its exit code and stops
// being alive (the listing's "alive" is IsAlive).
func TestSelfExitMarksDeadWindows(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "exit3", Command: []string{"cmd.exe", "/c", "exit 3"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	select {
	case <-w.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit")
	}
	if st, ok := w.ExitStatus(); !ok || st.Code != 3 {
		t.Fatalf("exit status %+v %v, want code 3", st, ok)
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.IsAlive() {
		if time.Now().After(deadline) {
			t.Fatal("window still alive 10 s after its process exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.Capture(10)
	if err := w.Resize(100, 30); err != nil {
		t.Fatalf("Resize after exit: %v", err)
	}
}
