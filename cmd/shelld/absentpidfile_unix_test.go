//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// L13: a pidfile the operator named that is absent names no daemon, so with no
// --socket the default socket's daemon is one nothing they named points at.
// stop and restart refuse in shelld-rs's words and kill acts on nothing, where
// all three used to act on the default daemon. SHELLD_PIDFILE names a pidfile
// as the flag does. With nothing serving, stop stays idempotent.
func TestAnAbsentPidfileDoesNotAimAtTheDefaultDaemon(t *testing.T) {
	tmp, err := os.MkdirTemp("", "sda")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	if code, out := runDefault(t, tmp, "daemonize"); code != 0 {
		t.Fatalf("daemonize: exit %d: %s", code, out)
	}
	defer runDefault(t, tmp, "kill", "--force")
	sock := filepath.Join(tmp, "shelld-"+strconv.Itoa(os.Getuid()), "shelld.sock")
	absent := filepath.Join(tmp, "absent.pid")
	refusal := "shelld: " + absent + " names no daemon, and something is serving " + sock +
		"; refusing to stop a daemon nothing you named points at (pass --socket " + sock + " to stop it anyway)"

	for _, args := range [][]string{
		{"stop", "--pidfile", absent},
		{"restart", "--pidfile", absent},
		{"SHELLD_PIDFILE", "stop"},
	} {
		var code int
		var out string
		if args[0] == "SHELLD_PIDFILE" {
			code, out = runDefaultEnv(t, tmp, []string{"SHELLD_PIDFILE=" + absent}, args[1:]...)
		} else {
			code, out = runDefault(t, tmp, args...)
		}
		if code != 1 || !strings.Contains(out, refusal) {
			t.Errorf("%v: exit %d, %q; want exit 1 and %q", args, code, out, refusal)
		}
	}
	if code, out := runDefault(t, tmp, "kill", "--force", "--pidfile", absent); code != 0 || !strings.Contains(out, "shelld: no lockfile, nothing to kill") {
		t.Errorf("kill --force --pidfile <absent>: exit %d, %q; want exit 0 and nothing killed", code, out)
	}
	if _, err := os.Stat(absent); err == nil {
		t.Error("the absent pidfile was written: restart started a daemon under it")
	}
	if code, out := runDefault(t, tmp, "status"); code != 0 {
		t.Fatalf("the default daemon is gone after acting on an absent --pidfile: exit %d: %s", code, out)
	}

	// With the daemon gone, the same stop is the retry of one that succeeded.
	if code, out := runDefault(t, tmp, "stop"); code != 0 {
		t.Fatalf("stop: exit %d: %s", code, out)
	}
	if code, out := runDefault(t, tmp, "stop", "--pidfile", absent); code != 0 {
		t.Errorf("stop --pidfile <absent> with nothing serving: exit %d, %q; want 0", code, out)
	}
}

// runDefaultEnv is runDefault with env applied after its own, so a variable it
// clears can be set.
func runDefaultEnv(t *testing.T, tmp string, env []string, cmd ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, cmd...)
	c.Env = append(append(os.Environ(), "TMPDIR="+tmp, "SHELLD_SOCKET=", "SHELLD_PIDFILE=", "SHELLD_LOGFILE="), env...)
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}
