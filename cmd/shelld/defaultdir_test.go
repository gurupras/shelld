//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Every other test in this package passes --socket, --pidfile and --logfile, so
// none of them uses the per-user default directory and none reaches the half of
// checkPaths that creates it. This one is about that directory, in the one state
// that tells the commands which create it apart from the commands that only read
// it: a box where it does not exist yet.
//
// The daemon-starting commands have to create it, because cmdDaemonize opens the
// log file in the process that spawns, before the child that would have created the
// directory exists. restart was not on that list and so could not start a first
// daemon — it stopped at "open log ...: no such file or directory" — while the
// removeStale comment in main.go describes starting a first daemon as something it
// is meant to be able to do.
//
// TMPDIR is the seam: daemon.DefaultDir builds on os.TempDir, so a child with
// TMPDIR set to an empty directory is a child on a box where no shelld has ever
// run. The directory comes from os.MkdirTemp rather than t.TempDir because the
// socket goes inside it and t.TempDir's name carries the test's own, which would
// put the path over sun_path's 108 bytes.
func TestTheCommandsThatStartADaemonCreateTheDefaultDirectory(t *testing.T) {
	for _, cmd := range []string{"daemonize", "restart"} {
		t.Run(cmd, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "sd")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			// The daemon this starts is stopped whatever the assertions below do,
			// and SIGKILLed if stop did not reach it. The pids are matched on
			// argv[0] being the binary this package built into its own temp
			// directory and on a socket path inside dir, so nothing outside this
			// subtest can satisfy both.
			t.Cleanup(func() {
				_, _ = shelldIn(t, dir, "stop")
				socks, _ := filepath.Glob(filepath.Join(dir, "*", "*.sock"))
				for _, s := range socks {
					for _, pid := range daemonProcesses(s) {
						if pid > 1 {
							_ = syscall.Kill(pid, syscall.SIGKILL)
						}
					}
				}
			})

			// Premise: nothing in it. A directory that already held a shelld
			// directory would be answering a different question.
			if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
				t.Fatalf("setup: %s holds %d entries (%v), want an empty directory",
					dir, len(ents), err)
			}

			code, out := shelldIn(t, dir, cmd)
			if code != 0 {
				t.Fatalf("%s on a box with no default directory exited %d: %s\n"+
					"it has to create that directory itself: cmdDaemonize opens the log"+
					" file before it spawns the child that would have created it", cmd, code, out)
			}
			// The socket is found rather than named, because naming it would mean
			// repeating daemon.DefaultDir's own formula here, and an expectation
			// built out of the thing it checks cannot notice that thing changing.
			socks, err := filepath.Glob(filepath.Join(dir, "*", "*.sock"))
			if err != nil || len(socks) != 1 {
				t.Fatalf("%s exited 0 but left %v under %s (%v), want one socket",
					cmd, socks, dir, err)
			}
		})
	}
}

// shelldIn runs the built shelld with no path flags at all, so the child uses its
// own default directory, under the given TMPDIR. Errors other than a non-zero exit
// are returned as an exit code of -1 with the message as output: its callers include
// a cleanup, where a t.Fatalf would report on the teardown rather than on the test.
func shelldIn(t *testing.T, tmpdir string, args ...string) (int, string) {
	t.Helper()
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		return -1, err.Error()
	}
	return 0, string(out)
}
