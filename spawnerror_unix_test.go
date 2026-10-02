//go:build unix

package shelld

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A spawn that fails has to say why. NewWindow is a public entry point, and it used
// to answer both of its spawn failures with a bare "spawn shell" -- the cause went to
// the debug log and nowhere a caller could reach, so a shell that is not installed and
// one that may not be executed were the same error. The daemon returns that text to
// the client, which had no way to tell a request it could fix from one it could not.
//
// unix only because it asserts on the errno behind the failure: the permission arm has
// no meaning where a mode bit does not decide who may execute a file.
func TestNewWindowSpawnFailureKeepsItsCause(t *testing.T) {
	dir := t.TempDir()

	unexecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(unexecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what  string
		shell string
		want  error
	}{
		{"a shell that is not there", filepath.Join(dir, "no-such-shell"), fs.ErrNotExist},
		{"a shell that may not be executed", unexecutable, fs.ErrPermission},
	} {
		w, err := NewWindow(WindowOpts{Name: "w", Command: []string{tc.shell}})
		if w != nil {
			w.Kill()
		}
		if err == nil {
			t.Errorf("%s: NewWindow succeeded", tc.what)
			continue
		}
		// errors.Is and not a string match: the point is that the chain survives, so
		// that a caller can branch on the errno rather than on prose.
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: NewWindow failed with %v, which does not wrap %v", tc.what, err, tc.want)
		}
	}
}
