//go:build linux || darwin

package pty

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// PTY is a running process on a pseudo-terminal. Read and Write address the
// master side; they are safe to call concurrently with each other.
type PTY struct {
	master *os.File
	proc   *os.Process
	// exit reports the child's exit without holding a thread (see
	// awaitExit). Wait or Close takes it, whichever comes first; nil where
	// pidfds are unavailable.
	exit atomic.Pointer[os.File]
}

// Start opens a PTY of the given size and starts cmd on it as the leader of
// a new session with the PTY as its controlling terminal.
func Start(cmd Command, size Size) (*PTY, error) {
	master, slaveName, err := openMaster()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open %s: %w", slaveName, err)
	}
	defer slave.Close()
	// Size after opening the slave, not before. On darwin the master alone is not
	// yet a terminal -- opening the slave is what instantiates the tty behind it --
	// so TIOCSWINSZ on the master fails with ENOTTY until that has happened, and
	// Start returned "inappropriate ioctl for device" for every caller. Measured on
	// macOS 26.4 arm64: set and get on the master both fail before the slave is
	// opened and both succeed after, and they keep succeeding once the slave is
	// closed again, so it is the first open that matters rather than the slave
	// staying open. Linux instantiates the tty with the master and accepts the
	// ioctl in either order, which is why this survived.
	//
	// Still before StartProcess, so the child sees the right size on its first read
	// and there is no SIGWINCH to race.
	if err := setSize(master, size); err != nil {
		master.Close()
		return nil, err
	}

	dir := cmd.Dir
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		dir = homeDir(cmd.Env)
	}
	path, err := resolve(cmd.Path, dir, cmd.Env)
	if err != nil {
		master.Close()
		return nil, &ProgramError{Err: err}
	}
	sys, err := sysAttr(cmd)
	if err != nil {
		master.Close()
		return nil, err
	}
	proc, err := os.StartProcess(path, cmd.Args, &os.ProcAttr{
		Dir:   dir,
		Env:   cmd.Env,
		Files: []*os.File{slave, slave, slave},
		Sys:   sys,
	})
	if err != nil {
		master.Close()
		err = fmt.Errorf("spawn %s: %w", path, err)
		if programFault(err) {
			return nil, &ProgramError{Err: err}
		}
		return nil, err
	}
	p := &PTY{master: master, proc: proc}
	// Nothing can reap the child before this returns the PTY, and an unreaped
	// child, even a zombie, keeps its PID from being recycled, so the pidfd
	// names this child. Opening it later, in Wait, could name another process
	// once a first Wait had reaped this one.
	if f := openExit(proc.Pid); f != nil {
		p.exit.Store(f)
	}
	return p, nil
}

// Read reads what the program has written to its terminal, as an io.Reader:
// whatever is available, not a line and not a whole escape sequence, so a caller
// parsing the output has to carry a partial sequence across reads.
//
// It returns io.EOF once the last process holding the slave side has gone. Linux
// reports that as EIO rather than as end of file, which a caller checking for
// io.EOF would take for a real error, so it is translated here — the one place
// that knows the EIO came from a terminal and not from the disk.
func (p *PTY) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	// Linux reports the slave side closing as EIO; present it as EOF.
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, errEOF
	}
	return n, err
}

// Write sends bytes to the program as if they had been typed, as an io.Writer.
// The line discipline is in the way: with echo on the bytes come back out of
// Read, and with ISIG on a 0x03 raises SIGINT in the foreground job rather than
// reaching the program.
//
// It does not return a short write: the master is an *os.File, whose Write loops
// until every byte is gone or it fails. What it does instead is park — a program
// that is not reading drains nothing, so once the terminal's input buffer fills the
// write waits for room rather than returning early, and there is no bound on how
// long that is. So a caller must not call this from a goroutine it needs back. The
// master is non-blocking and registered with the netpoller, which is what makes
// the wait interruptible: Close releases a parked Write. openMaster asks for
// neither and gets both — it passes no O_NONBLOCK — because package os switches
// an opened file to non-blocking in order to register it with the netpoller, and
// reverts that when the kernel refuses the registration (os/file_unix.go, newFile).
// What epoll refuses is a descriptor whose file has no poll support, which is not
// the same set as "not a device": a regular file and /dev/null are both refused
// with EPERM and come back blocking through the identical os.OpenFile call, and a
// terminal is accepted and keeps the flag.
//
// It is a property this package then has to keep. File.Fd on the master would
// hand the descriptor back in blocking mode and out of the poller, after which
// Close would no longer interrupt a parked Read or Write, which is why the ioctls
// reach it through control instead. Nothing exported returns the master, so only
// code in this package is in a position to undo it.
//
// Inside the library the only caller is Window.writeInput, which runs on a
// goroutine of its own for exactly this reason. The tests call it directly, as
// does a benchmark harness kept outside this repo.
func (p *PTY) Write(b []byte) (int, error) { return p.master.Write(b) }

// Resize sets the terminal size, delivering SIGWINCH to the foreground job.
func (p *PTY) Resize(size Size) error { return setSize(p.master, size) }

// Pid is the child's process id.
func (p *PTY) Pid() int { return p.proc.Pid }

// Kill SIGKILLs the direct child.
func (p *PTY) Kill() error { return p.proc.Kill() }

// Wait blocks until the child exits, reaps it, and reports how it ended. Call
// it once. After Close, it blocks an OS thread until the child exits.
func (p *PTY) Wait() (ExitStatus, error) {
	if f := p.exit.Swap(nil); f != nil {
		awaitExit(f)
		f.Close()
	}
	st, err := p.proc.Wait()
	if err != nil {
		return ExitStatus{Code: -1}, err
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Code: -1, Signal: signalName(ws.Signal())}, nil
	}
	return ExitStatus{Code: st.ExitCode()}, nil
}

// signalName is the platform's name for sig, or "signal N" for one it has no
// name for (a realtime signal on Linux). unix.SignalName answers "" there, and
// an empty Signal drops the key from exitStatus, which then reads as a normal
// exit of a process a signal ended. shelld-rs renders the same fallback
// (signal_name in its window.rs).
func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// Close closes the master, unblocking pending reads.
func (p *PTY) Close() error {
	if f := p.exit.Swap(nil); f != nil {
		f.Close()
	}
	return p.master.Close()
}

func setSize(f *os.File, size Size) error {
	ws := &unix.Winsize{Col: size.Cols, Row: size.Rows}
	return control(f, func(fd int) error {
		if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws); err != nil {
			return fmt.Errorf("ioctl(TIOCSWINSZ): %w", err)
		}
		return nil
	})
}

// control runs fn on f's descriptor without File.Fd, which would switch the
// descriptor to blocking mode and stop Close from interrupting a Read.
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := rc.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}

func envLookup(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func homeDir(env []string) string {
	if h, ok := envLookup(env, "HOME"); ok {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

// resolve finds the executable the way os/exec.LookPath does, with dir (the
// child's working directory) in the role of the caller's current directory:
//
//   - "", "." and ".." are not programs.
//   - A name with a slash is taken as given: an absolute path, or a path
//     relative to dir. PATH is not searched.
//   - A bare name is looked up only in PATH, in order, and never in dir
//     implicitly: a file in an agent's worktree must not shadow a program.
//     An empty or relative PATH entry names a directory relative to dir;
//     if the first match is found through one, resolve fails, as
//     exec.ErrDot makes LookPath fail. GODEBUG=execerrdot=0 is not honored.
//
// A candidate must be a file, not a directory, executable by the daemon's
// effective ids. The result is always absolute.
func resolve(prog, dir string, env []string) (string, error) {
	switch prog {
	case "", ".", "..":
		return "", fmt.Errorf("unable to spawn %q: not a program name", prog)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if strings.Contains(prog, "/") {
		p := prog
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if err := findExecutable(p); err != nil {
			return "", fmt.Errorf("unable to spawn %s because it doesn't exist on the filesystem or is not executable (%w)", prog, err)
		}
		return p, nil
	}
	if path, ok := envLookup(env, "PATH"); ok {
		for _, d := range filepath.SplitList(path) {
			relative := !filepath.IsAbs(d)
			if relative {
				d = filepath.Join(dir, d) // "" is "."
			}
			c := filepath.Join(d, prog)
			if findExecutable(c) != nil {
				continue
			}
			if relative {
				return "", fmt.Errorf("unable to spawn %s: it resolves to %s through a relative PATH entry, which is refused", prog, c)
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("unable to spawn %s because it doesn't exist on the filesystem and was not found in PATH", prog)
}

// programFault reports whether a failed StartProcess failed because of the
// program: the errnos an execve returns for a path or a file it cannot run.
// EPERM is deliberately absent — setsid and the controlling-terminal ioctl
// fail with it in the child before the exec, and neither is the caller's to
// fix — as are the fork's own EAGAIN and ENOMEM.
func programFault(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ENOENT, syscall.ENOTDIR, syscall.EACCES, syscall.EISDIR, syscall.ELOOP,
		syscall.ENAMETOOLONG, syscall.ENOEXEC, syscall.EINVAL, syscall.E2BIG:
		return true
	}
	return false
}

// findExecutable is os/exec's check on Unix: a file that is not a directory
// and is executable by the effective ids, falling back to the mode bits where
// faccessat with AT_EACCESS is unavailable or blocked (ENOSYS, or EPERM from
// a seccomp filter).
func findExecutable(file string) error {
	st, err := os.Stat(file)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return syscall.EISDIR
	}
	err = unix.Faccessat(unix.AT_FDCWD, file, unix.X_OK, unix.AT_EACCESS)
	if err == nil || (err != unix.ENOSYS && err != unix.EPERM) {
		return err
	}
	if st.Mode()&0o111 != 0 {
		return nil
	}
	return fs.ErrPermission
}
