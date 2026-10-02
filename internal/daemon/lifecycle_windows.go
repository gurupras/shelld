//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// KillPid terminates pid; a process that no longer exists is success.
func KillPid(pid int32) error {
	if pid <= 0 {
		return fmt.Errorf("kill pid %d: invalid", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("OpenProcess pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess pid %d: %w", pid, err)
	}
	return nil
}

// TerminatePid has no graceful signal on Windows; it terminates.
func TerminatePid(pid int32) error { return KillPid(pid) }

// SpawnDetached starts bin detached from the console in a new process group
// with stdout/stderr appended to logFile. The caller may Wait for it, or
// Release it.
func SpawnDetached(bin string, args []string, logFile string) (*os.Process, error) {
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logFile, err)
	}
	defer logf.Close()
	proc, err := os.StartProcess(bin, append([]string{bin}, args...), &os.ProcAttr{
		Files: []*os.File{nil, logf, logf},
		Sys: &syscall.SysProcAttr{
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}
	return proc, nil
}
