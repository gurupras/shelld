//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A window with no shell of its own runs the daemon's --shell, and a window
// that names one is unaffected by it.
//
// Four cases, because the interesting one cannot be read on its own, and each
// asserts on something the window PRINTED rather than on the absence of
// something: a fixture that never reached the shell at all would satisfy "the
// default did not run" while proving nothing.
//
//   - no shell in the request takes DefaultShell;
//   - a shell in the request wins, or the default would be overriding an
//     explicit choice;
//   - DefaultShell empty leaves shelld.DefaultShell() in place, which is what a
//     Manager driven as a library gets -- observed by pointing the daemon's own
//     $SHELL at a marker, since that is the first link of the chain
//     shelld.DefaultShell follows;
//   - DefaultShell is split on whitespace, so a shell carrying arguments works;
//     the flag's help text promises this and one string passed whole would
//     instead be looked up as a program whose name contains a space.
func TestCreateWindowInheritsTheDaemonsDefaultShell(t *testing.T) {
	base := t.TempDir()
	// Each script identifies itself and echoes its first argument, then holds the
	// terminal open so the capture has something to read from.
	script := func(name string) string {
		t.Helper()
		p := filepath.Join(base, name+".sh")
		body := "#!/bin/sh\n" +
			`printf 'WHICH=` + name + ` ARG=[%s] END\n' "$1"` + "\n" +
			"exec sleep 600\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dflt, asked, viaenv := script("default"), script("asked"), script("viaenv")

	// ranWith boots a daemon with the given DefaultShell, creates a window with
	// the given "shell" member (omitted when empty) and returns the marker line
	// the shell printed.
	ranWith := func(t *testing.T, defaultShell, reqShell string) string {
		t.Helper()
		sock := bootWith(t, func(s *State) { s.DefaultShell = defaultShell })
		if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
			t.Fatalf("create session: %d %s", st, raw)
		}
		extra := ""
		if reqShell != "" {
			extra = `,"shell":"` + reqShell + `"`
		}
		st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
			`{"name":"w","cols":120,"rows":24`+extra+`}`)
		if st != 200 {
			t.Fatalf("create window: %d %s", st, raw)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, v, raw := request(t, sock, "GET", "/sessions/s/windows/w/capture?lines=10", "")
			for l := range strings.SplitSeq(v["output"].(string), "\n") {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "WHICH=") && strings.HasSuffix(l, "END") {
					return l
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("window never identified its shell: %s", raw)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Run("absent shell takes the daemon default", func(t *testing.T) {
		if got := ranWith(t, dflt, ""); got != "WHICH=default ARG=[] END" {
			t.Errorf("window ran %q, want the daemon's --shell", got)
		}
	})
	t.Run("a shell in the request wins", func(t *testing.T) {
		got := ranWith(t, dflt, asked)
		if strings.HasPrefix(got, "WHICH=default") {
			t.Fatalf("window ran the daemon default %q, ignoring the requested shell %s", got, asked)
		}
		if got != "WHICH=asked ARG=[] END" {
			t.Errorf("window ran %q, want the requested shell %s", got, asked)
		}
	})
	t.Run("no daemon default falls back to $SHELL", func(t *testing.T) {
		// Read by shelld.DefaultShell in the daemon's own process, which in this
		// harness is the test process.
		t.Setenv("SHELL", viaenv)
		if got := ranWith(t, "", ""); got != "WHICH=viaenv ARG=[] END" {
			t.Errorf("window ran %q, want $SHELL %s: an unset --shell must leave shelld.DefaultShell in place", got, viaenv)
		}
	})
	t.Run("a default shell may carry arguments", func(t *testing.T) {
		if got := ranWith(t, dflt+" hello", ""); got != "WHICH=default ARG=[hello] END" {
			t.Errorf("window ran %q, want the argument split off and passed through", got)
		}
	})
}
