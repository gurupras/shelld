//go:build linux || darwin

package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// L8-L11: run against a socket file it cannot probe exits 1 in shelld-rs's
// words and leaves the file, rather than exit 3 ("held"), which tells
// daemonize that another daemon won a race nobody ran. Mode 000 is the
// undetermined state a test can build: connect is denied before the listener's
// state is looked at, dead or alive.
func TestRunRefusesASocketItCannotProbe(t *testing.T) {
	p := newPaths(t)
	l, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.Chmod(p.socket, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("unix", p.socket, 250*time.Millisecond); !errors.Is(err, syscall.EACCES) {
		t.Skipf("connect to a mode 000 socket gave %v, not EACCES, so this is not the state it names", err)
	}

	code, out := p.shelld(t, nil, "run")
	want := "shelld: bind " + p.socket + ": cannot tell whether a process is listening on it; not replacing it"
	if code != 1 || !strings.Contains(out, want) {
		t.Errorf("run: exit %d, %q; want exit 1 and %q", code, out, want)
	}
	if _, err := os.Lstat(p.socket); err != nil {
		t.Errorf("after the refusal, Lstat(%q): %v -- the file was removed anyway", p.socket, err)
	}
}
