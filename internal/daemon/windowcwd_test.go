//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A window with no cwd of its own starts in the directory the daemon was
// started in, which is what tmux does, rather than in $HOME. `cd repo && shelld
// start` and then creating a window has to put the program in repo: a
// client that asks for no directory means "where you were started", and
// $HOME is both useless for that and slow to sit in when it is large.
//
// Three cases, because the interesting one cannot be read on its own. A window
// with no cwd must land in DefaultCwd; one that names a cwd must still land
// there and not in DefaultCwd, or the default would be overriding the request;
// and with DefaultCwd unset the old $HOME default must remain, since that is
// what a Manager driven as a library gets. Each directory is distinct and none
// of them is the test process's own cwd, so no case can pass by inheriting it.
func TestCreateWindowInheritsTheDaemonsStartDirectory(t *testing.T) {
	base := t.TempDir()
	// Resolved because a /tmp that is a symlink (macOS) makes the shell's pwd
	// the target while t.TempDir reports the link, and this test would then
	// fail on the path rather than on the behaviour.
	realpath := func(p string) string {
		t.Helper()
		got, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("resolve %s: %v", p, err)
		}
		return got
	}
	mkdir := func(name string) string {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return realpath(p)
	}
	startDir, askedDir, home := mkdir("start"), mkdir("asked"), mkdir("home")

	// HOME is set for the daemon's own process, which is what pty's fallback
	// reads, so the unset-DefaultCwd case has a $HOME that is not any of the
	// others and not the real one.
	t.Setenv("HOME", home)

	p := filepath.Join(base, "pwd.sh")
	body := "#!/bin/sh\n" +
		`printf 'CWD=%s END\n' "$PWD"` + "\n" +
		"exec sleep 600\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	cwdOf := func(t *testing.T, defaultCwd, reqCwd string) string {
		t.Helper()
		sock := bootWith(t, func(s *State) { s.DefaultCwd = defaultCwd })
		if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
			t.Fatalf("create session: %d %s", st, raw)
		}
		extra := ""
		if reqCwd != "" {
			extra = `,"cwd":"` + reqCwd + `"`
		}
		st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
			`{"name":"w","cols":120,"rows":24,"shell":"`+p+`"`+extra+`}`)
		if st != 200 {
			t.Fatalf("create window: %d %s", st, raw)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, v, raw := request(t, sock, "GET", "/sessions/s/windows/w/capture?lines=10", "")
			for l := range strings.SplitSeq(v["output"].(string), "\n") {
				if l = strings.TrimSpace(l); strings.HasSuffix(l, "END") {
					return strings.TrimSuffix(strings.TrimPrefix(l, "CWD="), " END")
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("window never printed its directory: %s", raw)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Run("absent cwd takes the start directory", func(t *testing.T) {
		if got := cwdOf(t, startDir, ""); got != startDir {
			t.Errorf("window ran in %q, want the daemon's start directory %q", got, startDir)
		}
	})
	t.Run("a cwd in the request wins", func(t *testing.T) {
		got := cwdOf(t, startDir, askedDir)
		if got == startDir {
			t.Fatalf("window ran in the start directory %q, ignoring the requested cwd %q", got, askedDir)
		}
		if got != askedDir {
			t.Errorf("window ran in %q, want the requested cwd %q", got, askedDir)
		}
	})
	t.Run("no start directory falls back to $HOME", func(t *testing.T) {
		if got := cwdOf(t, "", ""); got != home {
			t.Errorf("window ran in %q, want $HOME %q: an unset DefaultCwd must leave WindowOpts's own default alone", got, home)
		}
	})
}

// A DefaultCwd that has been removed fails the create rather than quietly
// falling back, because it reaches NewWindow as an ordinary cwd. The window
// that would otherwise run in an unrequested directory is the case this
// protects; the assertion is on the status, since a 200 here is the silent
// outcome.
func TestCreateWindowRejectsAMissingStartDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "removed")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := bootWith(t, func(s *State) { s.DefaultCwd = gone })
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	// The control: while it exists, the same request succeeds, so the 400 below
	// is the directory going away and not something else about this body.
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"before","cols":80,"rows":24,"shell":"/bin/sh"}`); st != 200 {
		t.Fatalf("create with an existing DefaultCwd: %d %s, want 200", st, raw)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"after","cols":80,"rows":24,"shell":"/bin/sh"}`); st != 400 {
		t.Errorf("create with a removed DefaultCwd: %d %s, want 400", st, raw)
	}
}
