//go:build windows

package pty

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/gurupras/shelld/internal/winexec"
	"golang.org/x/sys/windows"
)

// ConPTY creation flags (consoleapi.h), as portable-pty passes them.
const (
	pseudoconsoleResizeQuirk    = 0x2
	pseudoconsoleWin32InputMode = 0x4
)

// PTY is a process attached to a Windows pseudo console, contained in a Job
// Object with the rest of its process tree.
//
// Handle lifetimes. The HPCON, process and job are raw handles: once one is
// closed its value can name another window's object, so every use checks
// under mu that it is still open. The pseudo console closes when the child
// exits (Wait) or on Close, whichever is first. The process and job handles
// are released by Close, but never while Wait is using the process handle:
// if Wait is running, it releases them when it returns.
//
// The job's lifetime is the window's. Releasing it kills every process still
// in it (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), so Close must be called when
// the window is deleted, not when its output ends: background processes of
// an exited shell keep running until then, as on Linux. If the daemon dies,
// the system closes the job and the tree dies with it.
//
// Threads: the output pipe is a synchronous anonymous pipe, so the reader
// blocks an OS thread in ReadFile, and Wait blocks another in
// WaitForSingleObject: about two threads per window, plus one briefly while
// the console closes. Overlapped named pipes on Go's I/O completion port and
// a thread-pool wait (RegisterWaitForSingleObject) would remove them; not
// done, as nothing can be measured on Windows here.
type PTY struct {
	hpc windows.Handle
	in  *os.File // our write end of the console's input
	out *os.File // our read end of the console's output
	pid int

	mu            sync.RWMutex
	process       windows.Handle // 0 once released
	job           windows.Handle // 0 when job setup failed, or once released
	consoleClosed bool
	closed        bool // Close has been called
	waitStarted   bool
	waitDone      bool

	closeOnce sync.Once
}

// errEOF is io.EOF, as open_linux.go and open_darwin.go already declare it, so
// that a caller can check for the end of a program's output the same way on
// either platform. It was a distinct error value spelled "EOF", which no
// errors.Is(err, io.EOF) could match.
//
// No consumer in this module can tell the difference: the read loop breaks on any
// error and reports the child's fate through Wait. This is for a library caller
// doing errors.Is.
var errEOF = io.EOF

// Start creates a pseudo console of the given size and starts cmd on it.
func Start(cmd Command, size Size) (*PTY, error) {
	// Refused before anything is allocated, and refused rather than ignored.
	// Windows has no cgroups; the nearest thing is a job object, which is a
	// different mechanism with a different handle type, so PlaceIn cannot be
	// honoured as given. Dropping it silently would hand the caller a shell it
	// believes is inside a container it can kill.
	if cmd.PlaceIn != nil {
		return nil, fmt.Errorf("pty: PlaceIn is not supported on windows: %w", ErrUnsupported)
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	var hpc windows.Handle
	err := windows.CreatePseudoConsole(coord(size), inR, outW, pseudoconsoleResizeQuirk|pseudoconsoleWin32InputMode, &hpc)
	// The console holds its own references to these ends.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	if err != nil {
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	p := &PTY{hpc: hpc, in: os.NewFile(uintptr(inW), "conpty-in"), out: os.NewFile(uintptr(outR), "conpty-out")}
	if err := p.spawn(cmd); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *PTY) spawn(cmd Command) error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it; read the
	// handle's bits as a pointer.
	hpcValue := *(*unsafe.Pointer)(unsafe.Pointer(&p.hpc))
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpcValue, unsafe.Sizeof(p.hpc)); err != nil {
		return fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	si := &windows.StartupInfoEx{}
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(*si))
	si.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	si.StartupInfo.StdInput = windows.InvalidHandle
	si.StartupInfo.StdOutput = windows.InvalidHandle
	si.StartupInfo.StdErr = windows.InvalidHandle
	si.ProcThreadAttributeList = attrs.List()

	dir := workingDir(cmd)
	exe, err := winexec.LookPath(cmd.Path, dir, cmd.Env, systemDirs(), isFile)
	if err != nil {
		return &ProgramError{Err: err}
	}
	app, line := exe, windows.ComposeCommandLine(append([]string{exe}, cmd.Args[1:]...))
	if winexec.IsBatch(exe) {
		// CreateProcess would run a batch file through cmd.exe with this
		// command line, and cmd re-parses MSVCRT quoting: an argument could
		// run commands or expand variables. Run cmd.exe explicitly with a
		// line escaped for it.
		sys, err := windows.GetSystemDirectory()
		if err != nil {
			return fmt.Errorf("GetSystemDirectory: %w", err)
		}
		app = sys + `\cmd.exe`
		if line, err = winexec.BatchCommandLine(exe, cmd.Args[1:]); err != nil {
			return &ProgramError{Err: err}
		}
	}
	cmdline, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return fmt.Errorf("command line for %s: %w", exe, err)
	}
	exePtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	envBlock := winexec.EnvironmentBlock(cmd.Env)

	// The child starts suspended and joins the job before it runs, so every
	// process it creates is born inside the job. Without a job (setup
	// failed, e.g. inside a parent job that forbids nesting) the window
	// still starts, and Kill ends only the direct child.
	job, err := newKillOnCloseJob()
	if err != nil {
		slog.Warn("pty: no job object; Kill will end only the direct child", "err", err)
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(exePtr, cmdline, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_SUSPENDED,
		&envBlock[0], dirPtr, &si.StartupInfo, &pi)
	if err != nil {
		closeHandles(job)
		err = fmt.Errorf("CreateProcess %s: %w", exe, err)
		if programFault(err) {
			return &ProgramError{Err: err}
		}
		return err
	}
	if job != 0 {
		if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
			slog.Warn("pty: AssignProcessToJobObject failed; Kill will end only the direct child", "err", err)
			closeHandles(job)
			job = 0
		}
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		closeHandles(pi.Thread, pi.Process, job)
		return fmt.Errorf("ResumeThread: %w", err)
	}
	closeHandles(pi.Thread)
	p.process = pi.Process
	p.job = job
	p.pid = int(pi.ProcessId)
	return nil
}

// newKillOnCloseJob creates a job whose processes all die when its last
// handle closes. Descendants cannot leave it: neither breakaway limit is set,
// so a child that asks for CREATE_BREAKAWAY_FROM_JOB gets access denied.
// Processes started outside the tree (services, COM servers, an app that was
// already running, WSL's Linux processes) are not in it.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		closeHandles(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return job, nil
}

// closeHandles closes each non-zero handle.
func closeHandles(hs ...windows.Handle) {
	for _, h := range hs {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
}

// Read reads what the program has written to the pseudo console, as an
// io.Reader: whatever is available, not a line and not a whole escape sequence.
//
// Every read failure is reported as io.EOF, not only the child's exit: the
// console closing, Close cancelling a pending read through CancelIoEx, and a
// genuine I/O fault are one answer here. That is a known gap, left open rather
// than guessed at. It costs this module nothing — the read loop breaks on any
// error and the child's real fate comes from Wait's ExitStatus — and guessing the
// classification from a Linux box risks the two failures that matter, reporting a
// fault as a clean exit or the reverse.
//
// Closing it needs a Windows host in the loop and this shape: ERROR_BROKEN_PIPE,
// ERROR_HANDLE_EOF and ERROR_OPERATION_ABORTED map to io.EOF, and every other
// error is returned as it is.
func (p *PTY) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if err != nil {
		return n, errEOF
	}
	return n, nil
}

// Write writes to the console's input. After Close it fails with
// os.ErrClosed.
func (p *PTY) Write(b []byte) (int, error) { return p.in.Write(b) }

// Resize resizes the pseudo console; once the console is closed it does
// nothing.
func (p *PTY) Resize(size Size) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.consoleClosed {
		return nil
	}
	if err := windows.ResizePseudoConsole(p.hpc, coord(size)); err != nil {
		return fmt.Errorf("ResizePseudoConsole: %w", err)
	}
	return nil
}

// Pid is the child's process id. It stays readable after the child has exited,
// and on Windows a pid may be reused once the process is gone, so it is for
// reporting rather than for addressing the process: use Kill and Wait.
func (p *PTY) Pid() int { return p.pid }

// Kill terminates the child and every process in its job, or only the child
// when there is no job. After Close it does nothing.
func (p *PTY) Kill() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil
	}
	if p.job != 0 {
		if err := windows.TerminateJobObject(p.job, 1); err == nil {
			return nil
		}
	}
	if p.process == 0 {
		return nil
	}
	return windows.TerminateProcess(p.process, 1)
}

// Wait blocks until the child exits and reports its exit code, then closes
// the pseudo console so the output pipe reaches EOF: conhost keeps it open
// until then, even after its client has exited. Call it at most once. If
// Close has already released the process handle, as when a window is killed
// before its wait starts, Wait returns an error and exit code -1.
func (p *PTY) Wait() (ExitStatus, error) {
	p.mu.Lock()
	if p.waitStarted || p.process == 0 {
		p.mu.Unlock()
		return ExitStatus{Code: -1}, errors.New("pty: Wait called twice or after Close")
	}
	p.waitStarted = true
	process := p.process
	p.mu.Unlock()

	defer p.finishWait()
	// The console closes whatever the wait reports: this is the only wait.
	// ClosePseudoConsole can block until conhost's last output is drained,
	// which the caller's reader does, so it must not delay Wait's return.
	defer func() { go p.closeConsole() }()
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return ExitStatus{Code: -1}, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return ExitStatus{Code: -1}, err
	}
	return ExitStatus{Code: int(code)}, nil
}

// finishWait records that Wait no longer uses the process handle, and
// releases the handles if Close has already run.
func (p *PTY) finishWait() {
	p.mu.Lock()
	p.waitDone = true
	var hs []windows.Handle
	if p.closed {
		hs = p.takeHandlesLocked()
	}
	p.mu.Unlock()
	closeHandles(hs...)
}

// closeConsole closes the pseudo console once. Resize sees the flag before
// the HPCON is freed; a second caller returns without waiting.
func (p *PTY) closeConsole() {
	p.mu.Lock()
	if p.consoleClosed {
		p.mu.Unlock()
		return
	}
	p.consoleClosed = true
	p.mu.Unlock()
	windows.ClosePseudoConsole(p.hpc)
}

// takeHandlesLocked zeroes and returns the process and job handles.
func (p *PTY) takeHandlesLocked() []windows.Handle {
	hs := []windows.Handle{p.process, p.job}
	p.process, p.job = 0, 0
	return hs
}

// Close tears down the console and pipes and releases the process and job,
// which kills whatever is still in the job; see PTY. It is idempotent and
// safe to call concurrently with Read, Write, Resize, Kill and Wait.
//
// Order: out.Close is meant to cancel a Read blocked in ReadFile, which
// depends on Go's poll.FD.Close calling CancelIoEx on a pipe cancelling a
// synchronous ReadFile from another thread; that is untested on Windows
// (TestKillIdleThenClose is the must-pass check). If it doesn't, Close waits
// for the reader, and the console below never closes. Closing the console
// ends conhost, which breaks the input pipe, so a writer blocked in WriteFile
// fails before in.Close waits for it.
//
// Close can block until conhost exits: before Windows 11 24H2,
// ClosePseudoConsole waits for it, and Window.Kill calls Close holding its
// PTY lock, so a console client that ignores CTRL_CLOSE_EVENT can stall Kill
// for conhost's close timeout (rare once the job has been terminated).
func (p *PTY) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.out.Close()
		p.closeConsole()
		p.in.Close()
		p.mu.Lock()
		var hs []windows.Handle
		if !p.waitStarted || p.waitDone {
			hs = p.takeHandlesLocked()
		}
		p.mu.Unlock()
		closeHandles(hs...)
	})
	return nil
}

// coord converts a size to a COORD, whose fields are int16.
func coord(s Size) windows.Coord {
	return windows.Coord{X: int16(min(s.Cols, 0x7fff)), Y: int16(min(s.Rows, 0x7fff))}
}

func envLookup(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// workingDir is the requested cwd when it is a directory, else %USERPROFILE%.
func workingDir(cmd Command) string {
	for _, d := range []string{cmd.Dir, func() string { v, _ := envLookup(cmd.Env, "USERPROFILE"); return v }()} {
		if d == "" {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			if abs, err := filepath.Abs(d); err == nil {
				return abs
			}
			return d
		}
	}
	return ""
}

// systemDirs are searched for a program when the child has no PATH.
func systemDirs() []string {
	var dirs []string
	if d, err := windows.GetSystemDirectory(); err == nil {
		dirs = append(dirs, d)
	}
	if d, err := windows.GetWindowsDirectory(); err == nil {
		dirs = append(dirs, d)
	}
	return dirs
}

// isFile reports whether p exists and can be a program: not a directory.
// App Execution Aliases stat as irregular files; see winexec.CandidateMode.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && winexec.CandidateMode(st.Mode())
}

// programFault reports whether a failed CreateProcess failed because of the
// program: a path that names nothing, a file the caller may not run, or one
// that is not an executable image for this machine.
func programFault(err error) bool {
	var errno windows.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_ACCESS_DENIED,
		windows.ERROR_BAD_EXE_FORMAT, windows.ERROR_EXE_MACHINE_TYPE_MISMATCH:
		return true
	}
	return false
}
