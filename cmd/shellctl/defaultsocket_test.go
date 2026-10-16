package main

import (
	"testing"

	"github.com/gurupras/shelld/internal/daemon"
	"github.com/gurupras/shelld/internal/sock"
)

// shellctl finds the daemon where shelld puts it by default: shellctl reads
// sock.DefaultSocket, shelld daemon.DefaultSocket, and the two must not drift.
func TestDefaultSocketIsTheDaemons(t *testing.T) {
	for _, env := range []map[string]string{
		{"SHELLD_SOCKET": ""},
		{"SHELLD_SOCKET": "", "TMPDIR": t.TempDir()},
		{"SHELLD_SOCKET": "/x/y.sock"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		if got, want := sock.DefaultSocket(), daemon.DefaultSocket(); got != want {
			t.Errorf("with %v shellctl dials %q, shelld listens on %q", env, got, want)
		}
	}
}
