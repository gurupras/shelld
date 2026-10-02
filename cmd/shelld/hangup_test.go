//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// A window's shell dies because closing the pty master hangs its session up, and
// SIG_IGN is the one signal disposition execve preserves. So a daemon that inherited
// an ignored SIGHUP hands one to every shell it starts, and those shells stop being
// reachable by the only mechanism that ends them: the window survives its pty, its
// daemon and the process that asked for the shutdown, with its controlling terminal
// already gone.
//
// This is reachable from outside with no privileges and nothing exotic --
// `nohup shelld daemonize` does it, and so does any launcher, supervisor or CI runner
// that ignores SIGHUP before starting its children. It was found the other way round:
// the two tests in stopwindows_test.go failed only in sweeps whose launcher was
// nohup'd, which looked like load-dependent flakiness for as long as the ignored
// signal was invisible.

// startRunWithHangupIgnored starts `shelld run` with SIGHUP ignored, the way nohup(1)
// would.
//
// Through a shell rather than nohup: trap with an empty handler is the POSIX spelling
// of SIG_IGN and exec preserves it, which is the whole mechanism under test, and it
// does not depend on nohup being installed or on where it puts its output. Otherwise
// this is startRun, including the session of its own -- see there for why that matters.
//
// The launcher records its own ignored set before exec'ing, and that file is the only
// honest witness that the condition was built: the mask execve hands over is the
// launcher's, and a daemon that then clears it -- which is the fix -- is
// indistinguishable afterwards from a daemon that was never handed one.
func (p paths) startRunWithHangupIgnored(t *testing.T) *exec.Cmd {
	t.Helper()
	handoff := filepath.Join(p.dir, "launcher.sigign")
	// sh -c <script> <name> <args...> sets $0 to name and $@ to the rest, so the
	// daemon is exec'd with exactly the arguments startRun would give it.
	//
	// The shell reads its own status rather than running awk on it, so that the pid on
	// the record is the pid that exec becomes the daemon. A helper's mask is the
	// inherited one too, but its pid is its own, and then the record cannot say which
	// process was handed the disposition: a launcher that trapped in a subshell writes a
	// byte-identical mask while exec'ing a daemon whose SIGHUP is at SIG_DFL. Reading
	// Pid and SigIgn from one open of /proc/self/status is what ties them to one
	// process. A redirection on a compound command does not start a subshell, so the
	// loop runs in the shell whose pid is being recorded, and $$ would not do: POSIX
	// keeps it at the invoking shell's pid inside a subshell, which is exactly the shape
	// being ruled out.
	script := `trap "" HUP
if [ -r /proc/self/status ]; then
	while read -r key value; do
		case $key in
		Pid:) pid=$value ;;
		SigIgn:) ign=$value ;;
		esac
	done < /proc/self/status
	printf '%s %s\n' "$pid" "$ign" > "$SHELLD_TEST_HANDOFF"
fi
exec "$0" "$@"`
	c := exec.Command("/bin/sh", append([]string{"-c", script, bin}, p.args("run")...)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Env = append(os.Environ(), "SHELLD_TEST_HANDOFF="+handoff)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	waitUntil(t, 10*time.Second, func() bool {
		return daemon.IsAlive(p.socket, 200*time.Millisecond)
	}, "daemon never answered")
	// A trap that did nothing would make the whole test pass for no reason, so the
	// witness is checked -- and its absence is a failure wherever it should exist.
	// Reading it and believing only what it says would leave two ways to assert
	// nothing: no file at all, and a file the launcher could not fill. Both are
	// reachable from one missing awk, which is why this is fail-closed rather than a
	// guarded comparison.
	//
	// "Should exist" is asked as the launcher asked it -- can this process read its own
	// /proc -- rather than by naming an OS, so a host where the witness is genuinely
	// unavailable skips the check and a host where it is merely missing does not.
	raw, readErr := os.ReadFile(handoff)
	witness, wrote := parseHangupWitness(string(raw))
	ignored, known := hangupIgnoredInMask(witness.mask)
	if _, err := os.Stat("/proc/self/status"); err == nil && (!wrote || !known) {
		t.Fatalf("setup: the launcher left no readable pid and ignored-signal mask at %s (read: %v, contents %q), so this test cannot show it was handed an ignored SIGHUP",
			handoff, readErr, strings.TrimSpace(string(raw)))
	}
	if wrote && witness.pid != c.Process.Pid {
		t.Fatalf("setup: SigIgn %s was recorded by process %d and the daemon was exec'd from %d, so the witness is some other process's disposition and not the one the daemon inherited",
			witness.mask, witness.pid, c.Process.Pid)
	}
	if known && !ignored {
		t.Fatalf("setup: the launcher handed the daemon SigIgn %s, which does not ignore SIGHUP, so nothing here is about an inherited disposition",
			witness.mask)
	}
	return c
}

// hangupWitness is what the launcher recorded about itself: the pid that read its own
// /proc status, and the SigIgn mask from that same read.
type hangupWitness struct {
	pid  int
	mask string
}

// parseHangupWitness reads the launcher's record, reporting whether there is one to
// believe at all. A record it cannot parse is not a record: the caller treats it the same
// as a missing file, which is the fail-closed answer, because the alternative is a test
// that asserts nothing whenever the launcher's shell behaves unexpectedly.
func parseHangupWitness(s string) (hangupWitness, bool) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return hangupWitness{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return hangupWitness{}, false
	}
	return hangupWitness{pid: pid, mask: fields[1]}, true
}

// ignoredSignals is pid's SigIgn mask, or "" when it cannot be read. Best effort and
// only ever used to describe a failure: /proc/pid/status is Linux-only, and the
// guarantee this file is about is a process that lives or dies, which is observable
// everywhere.
func ignoredSignals(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "SigIgn:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// hangupIgnored reports whether pid ignores SIGHUP, and whether that could be read at
// all. On a system without /proc the answer is unknown rather than false, so a caller
// cannot mistake "cannot tell" for "not ignored".
func hangupIgnored(pid int) (ignored, known bool) {
	return hangupIgnoredInMask(ignoredSignals(pid))
}

// hangupIgnoredInMask is hangupIgnored for a mask already read. Signal n is bit n-1.
func hangupIgnoredInMask(mask string) (ignored, known bool) {
	bits, err := strconv.ParseUint(mask, 16, 64)
	if err != nil {
		return false, false
	}
	return bits&(1<<(uint(syscall.SIGHUP)-1)) != 0, true
}

// TestWindowShellDoesNotInheritAnIgnoredHangup is the guarantee that survives a
// hostile launcher: whatever the daemon was started with, the shell it starts can
// still be hung up.
//
// The assertion is the window's death and not the signal mask, because the mask is
// the mechanism and the death is the promise -- a future fix that cleared the
// disposition somewhere else, or reached the window some other way, should keep this
// green. The mask is read anyway, to say in the failure message which of the two
// broke.
func TestWindowShellDoesNotInheritAnIgnoredHangup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a daemon and a pty")
	}
	p := newPaths(t)
	p.startRunWithHangupIgnored(t)

	win, _ := startWindowWithDetachedJob(t, p)
	winMask := ignoredSignals(win)
	if ignored, known := hangupIgnored(win); known && ignored {
		t.Errorf("the window's shell %d ignores SIGHUP (SigIgn %s): it inherited the daemon's disposition, and closing the pty master cannot end it",
			win, winMask)
	}

	if code, out := p.shelld(t, nil, "stop"); code != 0 {
		t.Fatalf("stop: %d %s", code, out)
	}
	waitUntil(t, 10*time.Second, func() bool { return !running(win) },
		fmt.Sprintf("the window's own process %d outlived the daemon that started it; the daemon was started with SIGHUP ignored and the window's SigIgn was %s",
			win, winMask))
}
