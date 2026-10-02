package shelld

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A cwd that is not a directory fails NewWindow before anything runs, where
// it used to start the program in the home directory.
func TestNewWindowRejectsBadCwd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	for _, cwd := range []string{filepath.Join(dir, "missing"), file} {
		w, err := NewWindow(WindowOpts{Name: "w", Cwd: cwd, Command: []string{"shelld-test-no-such-program", marker}})
		var cwdErr *CwdError
		if !errors.As(err, &cwdErr) || cwdErr.Cwd != cwd {
			if w != nil {
				w.Kill()
			}
			t.Fatalf("cwd %s: got %v, want a CwdError", cwd, err)
		}
	}
}
