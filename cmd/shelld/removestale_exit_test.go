//go:build unix

package main

import (
	"bytes"
	"net"
	"os"
	"strings"
	"testing"
)

// A refusal to remove the files has to reach the exit code. stop and kill both
// end by clearing the daemon's leftovers, and both used to exit 0 whether that
// happened or not -- so pointing either at a path shelld declines to touch
// reported success for doing nothing, and a script that reads the exit code as
// "the daemon is gone and its files are cleared" could not tell the two apart.
//
// The socket path is the one to point at because it is the one a person types.
// A regular file there is what a typo produces, and it is the case where the
// old code was not merely silent but wrong: it removed the file.
//
// Each case asserts three things, and needs all three. The exit code is the new
// behaviour. The file surviving byte-for-byte is what the exit code is about --
// an exit 1 that still deleted the file would satisfy the code assertion alone.
// And <socket>.lock being absent pins that the refusal came before
// AcquireInstance, which creates that file: a check ordered after it still
// litters the directory it decided not to touch.
func TestStopAndKillExitNonZeroWhenTheyRefuseToRemoveFiles(t *testing.T) {
	const body = "this is not a socket\n"
	for _, tc := range []struct {
		name string
		cmd  []string
	}{
		{"stop", []string{"stop"}},
		{"kill", []string{"kill", "--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t)
			if err := os.WriteFile(p.socket, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(p.socket)
			if err != nil {
				t.Fatal(err)
			}

			code, out := p.shelld(t, nil, tc.cmd...)
			if code != 1 {
				t.Errorf("exit %d, want 1: %s", code, out)
			}
			if !strings.Contains(out, "not a socket") {
				t.Errorf("output does not say why it refused: %s", out)
			}

			after, err := os.Lstat(p.socket)
			if err != nil {
				t.Fatalf("%s was removed: %v", p.socket, err)
			}
			if !os.SameFile(before, after) {
				t.Errorf("%s was replaced", p.socket)
			}
			got, err := os.ReadFile(p.socket)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(body)) {
				t.Errorf("%s now holds %q, wrote %q", p.socket, got, body)
			}
			if _, err := os.Lstat(p.socket + ".lock"); err == nil {
				t.Errorf("%s.lock was created beside a path shelld refused to act on", p.socket)
			}
			// kill's own claim about the request. "nothing to kill" says the state
			// is clean, and shelld has just said it is not.
			if strings.Contains(out, "nothing to kill") {
				t.Errorf("refused and still reported nothing to kill: %s", out)
			}
		})
	}
}

// The same refusal has to reach the exit code for the case the type check cannot
// see: a socket file that is a socket, that nothing answers on, and that shelld
// never created. Another program's socket is in exactly that state once that
// program has died, and the file names no program that bound it, so the type is
// all a type check establishes -- which is why the old code removed it and exited
// 0. Here the socket is left and the exit code says so.
//
// <socket>.lock is asserted absent for the reason above, and it carries more here:
// the file is the witness shelld looks for, so a check ordered after
// AcquireInstance would be satisfied on the next run by the litter of this one,
// and the same wrong removal would go through the second time.
//
// TestRestartRecoversFromAKilledDaemon is the end-to-end other side: a SIGKILLed
// daemon leaves that lock file beside its socket, and restart clears both and
// starts a daemon. Without it this refusal is satisfiable by refusing every socket.
func TestStopAndKillRefuseASocketShelldDidNotCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  []string
	}{
		{"stop", []string{"stop"}},
		{"kill", []string{"kill", "--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t)
			ln, err := net.Listen("unix", p.socket)
			if err != nil {
				t.Fatal(err)
			}
			// Closed without unlinking, so the file outlives the process that
			// bound it -- and with no lockfile and no <socket>.lock written, it
			// is a socket with nothing of shelld's beside it.
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			ln.Close()
			before, err := os.Lstat(p.socket)
			if err != nil {
				t.Fatal(err)
			}

			code, out := p.shelld(t, nil, tc.cmd...)
			if code != 1 {
				t.Errorf("exit %d, want 1: %s", code, out)
			}
			if !strings.Contains(out, "nothing shows shelld served it") {
				t.Errorf("output does not say why it refused: %s", out)
			}
			if !strings.Contains(out, p.socket) {
				t.Errorf("output does not name the file the operator has to deal with: %s", out)
			}

			after, err := os.Lstat(p.socket)
			if err != nil {
				t.Fatalf("%s was removed: %v", p.socket, err)
			}
			if !os.SameFile(before, after) {
				t.Errorf("%s was replaced", p.socket)
			}
			if _, err := os.Lstat(p.socket + ".lock"); err == nil {
				t.Errorf("%s.lock was created beside a socket shelld refused to act on, so the next run would find its own litter and remove the file", p.socket)
			}
			if strings.Contains(out, "nothing to kill") {
				t.Errorf("refused and still reported nothing to kill: %s", out)
			}
		})
	}
}

// The control, and the half that keeps restart working: a box where no daemon has
// run has nothing to remove, which is not a refusal. Without this the fix above is
// satisfiable by exiting 1 from every call, and restart -- which runs its start
// half only when stop exits 0 -- could never start a first daemon.
//
// TestRestartWithNoDaemonStartsOne is the other side of that and asserts the
// daemon appears; this asserts the quiet, so that the reason stays legible when
// the wording of a message changes.
func TestStopAndKillStaySilentWhenThereIsNothingToRemove(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  []string
		want string
	}{
		{"stop", []string{"stop"}, ""},
		{"kill", []string{"kill", "--force"}, "nothing to kill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t)
			code, out := p.shelld(t, nil, tc.cmd...)
			if code != 0 {
				t.Errorf("exit %d on a path that was never there, want 0: %s", code, out)
			}
			if tc.want == "" {
				if out != "" {
					t.Errorf("output %q, want none", out)
				}
			} else if !strings.Contains(out, tc.want) {
				t.Errorf("output %q does not contain %q", out, tc.want)
			}
		})
	}
}
