//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// run with a regular file at --socket, or under it, exits non-zero and leaves
// the file as it was.
func TestRunLeavesAFileThatIsNotASocket(t *testing.T) {
	p := newPaths(t)
	const content = "not a socket\n"
	if err := os.WriteFile(p.socket, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ what, socket string }{
		{"a regular file at the socket path", p.socket},
		{"a socket path whose parent is a regular file", filepath.Join(p.socket, "s.sock")},
	} {
		q := p
		q.socket = tc.socket
		code, out := q.shelld(t, nil, "run")
		if code == 0 {
			t.Errorf("%s: run exited 0: %s", tc.what, out)
		}
		if got, err := os.ReadFile(p.socket); err != nil || string(got) != content {
			t.Fatalf("%s: the regular file is now %q, %v; want it untouched (run said: %s)", tc.what, got, err, out)
		}
		t.Logf("%s: exit %d: %s", tc.what, code, out)
	}
}
