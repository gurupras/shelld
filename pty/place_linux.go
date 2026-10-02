//go:build linux

package pty

import (
	"fmt"
	"syscall"
)

// sysAttr builds the SysProcAttr for a PTY child: a new session with the PTY
// as its controlling terminal, and on Linux optionally a cgroup to be created
// inside.
//
// This is a separate file per OS rather than one literal with a conditional
// field because SysProcAttr is a different struct on each platform, and
// UseCgroupFD/CgroupFD exist only on Linux — a shared literal mentioning them
// would not compile for darwin.
func sysAttr(cmd Command) (*syscall.SysProcAttr, error) {
	sys := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if cmd.PlaceIn == nil {
		return sys, nil
	}
	// Fd rather than SyscallConn: the fd number is what goes into the struct
	// and has to still be that fd when clone3 runs, so it cannot be borrowed
	// for the duration of a callback. Putting a directory into blocking mode is
	// no loss — a cgroup directory has no readiness to wait on, and this handle
	// is never read.
	fd := int(cmd.PlaceIn.Fd())
	if fd < 0 {
		return nil, fmt.Errorf("pty: PlaceIn is closed")
	}
	// There is no fallback below this line, and that is a property of the
	// runtime rather than a choice: Go retries a failed clone3 with clone only
	// when UseCgroupFD is unset, so on a kernel or under a seccomp policy
	// without CLONE_INTO_CGROUP this spawn fails outright instead of quietly
	// producing an unplaced shell. That is why the cgroup package probes with a
	// real spawn at startup and declines there, where a decline costs nothing.
	sys.UseCgroupFD = true
	sys.CgroupFD = fd
	return sys, nil
}
