//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// KillPid SIGKILLs pid; an already-gone process is success.
//
// A non-positive pid is refused rather than handed to the kernel, which does not
// read one as a pid at all: syscall.Kill takes 0 as every process in the caller's
// own group -- the daemon and every window it serves -- and -N as process group N.
// Neither returns ESRCH, so without this the widest possible signal is also the
// one that reports success (measured: kill(0, 0) and kill(-1, 0) both return nil,
// while a pid that cannot exist returns ESRCH). The Windows twin already refuses
// one, and proctree's unionDescendants guards the same shape for the same reason.
//
// pid 1 is not refused: it is a real pid, and the kernel's own permission check is
// the right arbiter for it.
func KillPid(pid int32) error {
	if pid <= 0 {
		return fmt.Errorf("kill pid %d: invalid", pid)
	}
	if err := syscall.Kill(int(pid), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}

// TerminatePid sends SIGTERM (the graceful-stop fallback). It carries KillPid's
// non-positive guard: which processes kill(0) reaches does not depend on the
// signal being sent. Unlike KillPid it still reports ESRCH for a pid that is
// already gone, which is the difference its caller relies on.
func TerminatePid(pid int32) error {
	if pid <= 0 {
		return fmt.Errorf("terminate pid %d: invalid", pid)
	}
	return syscall.Kill(int(pid), syscall.SIGTERM)
}

// CheckSocketPath reports whether a socket is at path, and refuses a path that
// holds something else. A path that is not there is not an error: there is no
// socket to remove, which is the outcome its caller wanted.
//
// A socket file carries nothing that names the program that bound it, so this can
// only establish the type. That is the whole of what is available, and it is what
// separates "this may be a stale shelld socket" from "this is a file somebody
// typed by mistake" -- a distinction the caller had no way to make, because the
// only thing it checked was that nothing was listening, which is true of every
// file that is not a socket.
//
// Lstat rather than Stat, so a symlink is refused as a symlink instead of being
// followed to a socket somewhere else and then unlinked in its own directory.
func CheckSocketPath(path string) (bool, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot tell what %s is, so not removing it: %w", path, err)
	}
	if st.Mode().Type() != fs.ModeSocket {
		return false, fmt.Errorf("%s is %s, not a socket, so not removing it", path, describeMode(st.Mode()))
	}
	return true, nil
}

// removeSocketFile unlinks the socket at path, and unlinks nothing else.
func removeSocketFile(path string) error {
	if _, err := CheckSocketPath(path); err != nil {
		return err
	}
	if err := removeFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// SpawnDetached starts bin in its own session with stdout/stderr appended to
// logFile and stdin on /dev/null. The caller may Wait for it, or Release it.
func SpawnDetached(bin string, args []string, logFile string) (*os.Process, error) {
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logFile, err)
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	proc, err := os.StartProcess(bin, append([]string{bin}, args...), &os.ProcAttr{
		Files: []*os.File{devnull, logf, logf},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}
	return proc, nil
}
