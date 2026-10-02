package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// A lockfile names the daemon only by pid, and pids are reused: after a
// crash or a reboot the pid in a stale lockfile can belong to any process.
// Commands that signal the daemon or remove its files first work out which
// of these holds.
type DaemonState int

const (
	// DaemonGone: nothing is serving the socket, so its stale files may be
	// removed.
	DaemonGone DaemonState = iota
	// DaemonVerified: Pid is the daemon, and may be signalled.
	DaemonVerified
	// DaemonUnverified: something may still be serving; touch nothing.
	DaemonUnverified
)

// ErrNotDaemon is returned when the process a command would signal is not
// shown to be the daemon.
var ErrNotDaemon = errors.New("not shown to be the shelld daemon")

// procInfo describes a live process.
type procInfo struct {
	created time.Time
	image   string // executable path or name
}

// idSlack absorbs startedAt's truncation to seconds and clock granularity.
const idSlack = 2 * time.Second

// sameDaemon reports whether a process with info, looked at now, is the
// daemon that wrote lock, and why not when it isn't. The daemon writes
// startedAt after it starts, so it was created no later than that; a process
// that reused the pid was created after the daemon exited, which is later.
// Its executable's name must start with "shelld" too, which guards against a
// clock step. A creation time after now means the clock cannot be trusted,
// so nothing is verified.
func sameDaemon(info procInfo, now time.Time, lock LockInfo) (bool, string) {
	started := time.Unix(lock.StartedAt, 0)
	if info.created.After(now.Add(idSlack)) {
		return false, fmt.Sprintf("its creation time %s is in the future; cannot verify", info.created.Format(time.RFC3339))
	}
	if info.created.After(started.Add(idSlack)) {
		return false, fmt.Sprintf("it started at %s, after the lockfile's %s", info.created.Format(time.RFC3339), started.Format(time.RFC3339))
	}
	if !shelldImage(info.image) {
		return false, fmt.Sprintf("its executable is %q, not shelld", info.image)
	}
	return true, ""
}

// shelldImage reports whether image is a shelld executable. It is named apart from
// sameDaemon because it is the only one of sameDaemon's three tests that says
// anything about a process identified some other way than by a pid out of a
// lockfile -- the two time comparisons are against that file's startedAt, which a
// process found by holding the instance lock has no counterpart for.
func shelldImage(image string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(strings.TrimSuffix(image, " (deleted)"), `\`, "/")))
	return strings.HasPrefix(base, "shelld")
}

// holderLocated says what the holder of a socket's instance lock is, given what
// could be read about that process. The lock's holder is the daemon, and that is
// sound as far as it goes: nothing takes the lock except a daemon starting up. But
// the lock is a file, and holding a write lock on a file is something any process
// of this user can do -- so the kernel naming a holder is a candidate, not a
// verdict, and it was being read as a verdict on the path whose next step is to
// send a signal.
//
// Only a positive answer demotes. A read that failed, or a process that has exited
// since the kernel named it, says nothing about whether the holder was the daemon,
// and the caller acts on "gone" by removing files a live daemon may still be
// serving on. So an unreadable holder stays verified, which leaves the signal
// guarded by the lock alone exactly as before, and the demotion is reserved for
// the case where the executable was read and is not shelld's.
func holderLocated(holder int, info procInfo, alive bool, err error) Located {
	if err == nil && alive && !shelldImage(info.image) {
		return Located{
			State:  DaemonUnverified,
			Pid:    holder,
			Reason: fmt.Sprintf("pid %d holds the instance lock, but its executable is %q, not shelld", holder, info.image),
		}
	}
	return Located{State: DaemonVerified, Pid: holder}
}

// Located is LocateDaemon's result.
type Located struct {
	State  DaemonState
	Pid    int
	Reason string // why the state is not verified
}

// NotDaemonError is returned instead of signalling a process not shown to be
// the daemon.
type NotDaemonError struct{ Located Located }

func (e *NotDaemonError) Error() string {
	if e.Located.State == DaemonGone {
		return fmt.Sprintf("no shelld daemon to signal: %s", e.Located.Reason)
	}
	return fmt.Sprintf("pid %d %s: %s", e.Located.Pid, ErrNotDaemon, e.Located.Reason)
}

func (e *NotDaemonError) Unwrap() error { return ErrNotDaemon }
