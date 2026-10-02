//go:build windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// InstanceLock is a no-op on Windows: the daemon's first pipe instance is
// created exclusively and held while it listens, which already keeps a
// second daemon off the pipe.
type InstanceLock struct{}

// ErrInstanceHeld is returned when another process is serving the pipe.
var ErrInstanceHeld = errors.New("the socket is already held")

// AcquireInstance does nothing on Windows; see InstanceLock.
func AcquireInstance(string) (*InstanceLock, error) { return &InstanceLock{}, nil }

func (*InstanceLock) Release() {}

// InstanceHolder is unknown on Windows and reports 0.
func InstanceHolder(string) (int, error) { return 0, nil }

// pipeGone reports whether no pipe of that name exists. The dial goes through
// dialWithin, as Dial's does: go-winio calls CreateFile inline and checks ctx only
// between attempts, so a ctx alone cannot end an open that blocks. A timeout
// answers "not gone", the safe side: "gone" is what lets the stop, kill and
// cleanup paths remove a daemon's files.
func pipeGone(pipe string) bool {
	dial := dialPipe
	conn, err := dialWithin(context.Background(), "pipe", pipe, 500*time.Millisecond, func(ctx context.Context) (net.Conn, error) {
		return dial(ctx, pipe)
	})
	if err == nil {
		conn.Close()
		return false
	}
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}

// dialPipe is pipeGone's open, a variable so a test can reach an open that
// never returns.
var dialPipe = winio.DialPipeContext

// openDaemon opens lock.Info.Pid for querying and synchronizing and checks it
// with sameDaemon. When the state is DaemonVerified the returned handle is open,
// which keeps the pid from being reused; the caller closes it. A lockfile that
// would not read is unverified with no pid, whatever is on the pipe.
func openDaemon(socket string, lock LockRead) (windows.Handle, Located) {
	gone := func(pid int, why string) (windows.Handle, Located) {
		if pipeGone(socket) {
			return 0, Located{State: DaemonGone, Pid: pid, Reason: why}
		}
		return 0, Located{State: DaemonUnverified, Pid: pid, Reason: why + ", but the pipe still exists"}
	}
	// Answered before anything reads lock.Info, and deliberately not through
	// gone(): gone() returns DaemonGone when no pipe exists, and a lockfile that
	// could not be read has shown nothing about whether a daemon is there.
	if lock.Err != nil {
		return 0, Located{State: DaemonUnverified, Reason: fmt.Sprintf("cannot read the lockfile at %s: %v", lock.Path, lock.Err)}
	}
	if lock.Info == nil || lock.Info.Pid <= 0 {
		return gone(0, "no lockfile names the daemon")
	}
	pid := int(lock.Info.Pid)
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	switch {
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
		return gone(pid, "the process does not exist")
	case err != nil:
		return 0, Located{State: DaemonUnverified, Pid: pid, Reason: fmt.Sprintf("OpenProcess: %v", err)}
	}
	// A process that has exited stays openable while anything holds a handle
	// to it.
	if ev, _ := windows.WaitForSingleObject(h, 0); ev == windows.WAIT_OBJECT_0 {
		windows.CloseHandle(h)
		return gone(pid, "the process has exited")
	}
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		windows.CloseHandle(h)
		return 0, Located{State: DaemonUnverified, Pid: pid, Reason: fmt.Sprintf("GetProcessTimes: %v", err)}
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		windows.CloseHandle(h)
		return 0, Located{State: DaemonUnverified, Pid: pid, Reason: fmt.Sprintf("QueryFullProcessImageName: %v", err)}
	}
	info := procInfo{created: time.Unix(0, created.Nanoseconds()), image: windows.UTF16ToString(buf[:n])}
	if ok, why := sameDaemon(info, time.Now(), *lock.Info); !ok {
		windows.CloseHandle(h)
		return gone(pid, why)
	}
	// As on unix: a live shelld the lockfile names for another socket is that
	// socket's daemon, and its lockfile is not a leftover.
	if lock.AimsAt(socket) != LockfileAgrees {
		windows.CloseHandle(h)
		return 0, Located{State: DaemonUnverified, Pid: pid, Reason: fmt.Sprintf("the lockfile at %s names the socket %s, not this one", lock.Path, lock.Info.Socket)}
	}
	return h, Located{State: DaemonVerified, Pid: pid}
}

// LocateDaemon works out whether the daemon lock describes is still there;
// see openDaemon.
func LocateDaemon(socket string, lock LockRead) Located {
	h, loc := openDaemon(socket, lock)
	if h != 0 {
		windows.CloseHandle(h)
	}
	return loc
}

// terminateDaemon terminates the daemon when it is verified. The terminate
// handle is opened separately from the query handle, which pins the pid, so
// a caller without the right to terminate gets access denied rather than
// "not the daemon".
func terminateDaemon(socket string, lock LockRead) (int, error) {
	h, loc := openDaemon(socket, lock)
	if h == 0 {
		return loc.Pid, &NotDaemonError{Located: loc}
	}
	defer windows.CloseHandle(h)
	t, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(loc.Pid))
	if err != nil {
		return loc.Pid, fmt.Errorf("OpenProcess(PROCESS_TERMINATE) pid %d: %w", loc.Pid, err)
	}
	defer windows.CloseHandle(t)
	if err := windows.TerminateProcess(t, 1); err != nil {
		return loc.Pid, fmt.Errorf("TerminateProcess pid %d: %w", loc.Pid, err)
	}
	return loc.Pid, nil
}

// KillDaemon terminates the daemon when it is verified.
func KillDaemon(socket string, lock LockRead) (int, error) { return terminateDaemon(socket, lock) }

// TerminateDaemon has no graceful signal on Windows; it is KillDaemon.
func TerminateDaemon(socket string, lock LockRead) (int, error) {
	return terminateDaemon(socket, lock)
}

// RemoveStale removes the lockfile at pidfile only when no pipe of the
// socket's name exists, and only if it is still the lockfile read before
// that check (same pid and nonce), so a daemon that started in between
// keeps its new lockfile.
// A lockfile that cannot be read reports false, because this reports whether it
// removed the leftovers and it cannot show that it did. Folding the failed read
// into "no lockfile" would return true -- the arm below for a path with nothing
// at it -- which tells the caller the stale state is gone while the file it could
// not read is still sitting there, and the caller prints success.
// It is removed only when it names this socket, which the pid-and-nonce
// comparison below does not cover: that establishes the file did not change under
// this function, not that it was ever this socket's to remove. Without it a
// --pidfile naming another daemon's pipe was removed because this pipe was gone.
func RemoveStale(socket, pidfile string) (bool, error) {
	before := ReadLock(pidfile)
	if before.Err != nil {
		return false, fmt.Errorf("cannot read the lockfile at %s, so not removing it: %w", pidfile, before.Err)
	}
	if !pipeGone(socket) {
		return false, fmt.Errorf("%s is still served, so its files were left in place", socket)
	}
	if before.Info == nil {
		return true, nil
	}
	now := ReadLock(pidfile)
	if now.Err != nil {
		return false, fmt.Errorf("cannot read the lockfile at %s, so not removing it: %w", pidfile, now.Err)
	}
	if now.Info == nil || now.Info.Pid != before.Info.Pid || now.Info.Nonce != before.Info.Nonce {
		if now.Info == nil {
			return true, nil
		}
		return false, fmt.Errorf("the lockfile at %s changed while it was being removed, so a newer daemon's was left in place", pidfile)
	}
	if err := RemoveLockfileIfOurs(pidfile, socket); err != nil {
		return false, err
	}
	return true, nil
}

// NothingServes reports whether no pipe of the socket's name exists.
func NothingServes(socket string) bool { return pipeGone(socket) }
