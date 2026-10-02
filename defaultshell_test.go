//go:build linux || darwin

package shelld

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// DefaultShell must return something spawnable, and must decide it from $SHELL
// and the passwd entry rather than from $PATH.
//
// What it used to do was probe $PATH for a file named bash and, if one was
// found anywhere, return the unrelated literal "/bin/bash". The two halves are
// separate defects and need separate assertions:
//
//   - it returned $SHELL without checking it, so a stale passwd entry or a
//     shell since uninstalled became a window that failed to spawn;
//   - the value it returned was never the file the probe found, so a host whose
//     bash lives under /usr/local -- or any host without usrmerge, where /bin
//     and /usr/bin are different directories -- got a path that need not exist,
//     and the /bin/sh floor was unreachable exactly because the probe had
//     succeeded.
//
// The second half cannot be caught by looking at the answer on a usrmerge host,
// where /bin/bash and /usr/bin/bash are the same inode and the wrong answer is
// accidentally right. TestDefaultShellIgnoresPath catches it by its mechanism
// instead, which holds on every host.
func TestDefaultShellReturnsSomethingExecutable(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realShell := filepath.Join(dir, "myshell")
	if err := os.WriteFile(realShell, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		shell string
		want  string // "" means "not $SHELL, but must be executable"
	}{
		{"an executable $SHELL is used as given", realShell, realShell},
		{"$SHELL that does not exist is rejected", filepath.Join(dir, "gone"), ""},
		{"$SHELL that is not executable is rejected", notExec, ""},
		{"$SHELL that is a directory is rejected", dir, ""},
		{"$SHELL unset", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHELL", tc.shell)
			got := DefaultShell()
			if tc.want != "" && got != tc.want {
				t.Fatalf("DefaultShell() = %q, want the executable $SHELL %q", got, tc.want)
			}
			if tc.want == "" && got == tc.shell {
				t.Fatalf("DefaultShell() = %q, which is $SHELL: an unusable $SHELL must not be handed to a window", got)
			}
			// The contract, asserted on every leg including the one that returns
			// $SHELL: whatever comes back is a file a window can execute. A
			// duration or a shape would not catch the literal "/bin/bash"; only
			// stat'ing the answer does.
			st, err := os.Stat(got)
			if err != nil {
				t.Fatalf("DefaultShell() = %q, which cannot be stat'd: %v", got, err)
			}
			if st.IsDir() || st.Mode()&0o111 == 0 {
				t.Fatalf("DefaultShell() = %q, mode %v: not an executable file", got, st.Mode())
			}
		})
	}
}

// With $SHELL unusable, the answer must not depend on $PATH at all. This is the
// assertion that kills the probe-then-hardcode shape on a host where the
// hardcoded path happens to exist.
//
// Under the old code the two legs below disagreed -- a $PATH carrying any file
// named bash produced "/bin/bash" and a $PATH carrying none produced "/bin/sh"
// -- which is the tell that the answer was being chosen by a probe whose result
// it then discarded. loginShell reads $SHELL and /etc/passwd and never $PATH,
// so the two must now agree, whatever this host's passwd entry says.
func TestDefaultShellIgnoresPath(t *testing.T) {
	dir := t.TempDir()
	withBash := filepath.Join(dir, "withbash")
	noBash := filepath.Join(dir, "nobash")
	for _, d := range []string{withBash, noBash} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Named bash and executable, but deliberately not at /bin/bash: this is the
	// file the old probe found and then did not return.
	if err := os.WriteFile(filepath.Join(withBash, "bash"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	answer := func(path string) string {
		t.Helper()
		t.Setenv("SHELL", "")
		t.Setenv("PATH", path)
		return DefaultShell()
	}
	found, absent := answer(withBash), answer(noBash)
	if found != absent {
		t.Errorf("DefaultShell() = %q with bash on $PATH and %q without it: the shell must not be chosen by a $PATH probe",
			found, absent)
	}
	t.Logf("both legs: %q", found)
}

// TestADefaultShellPathWithASpaceIsSpawnedWhole: $SHELL is a path, not a
// command line, so a window that names no shell spawns it unsplit (F07-04).
func TestADefaultShellPathWithASpaceIsSpawnedWhole(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sh := filepath.Join(dir, "sh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\necho spawned-whole\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", sh)
	if got := DefaultShellArgv(); len(got) != 1 || got[0] != sh {
		t.Fatalf("DefaultShellArgv() = %q, want [%q]", got, sh)
	}
	w, err := NewWindow(WindowOpts{Name: "w", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("a window with no shell named, $SHELL=%q: %v", sh, err)
	}
	defer w.Kill()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(w.CaptureVisible(0), "spawned-whole"); {
		if time.Now().After(deadline) {
			t.Fatalf("the default shell %q never ran; grid:\n%q", sh, w.CaptureVisible(0))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
