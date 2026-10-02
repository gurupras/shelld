//go:build darwin

package pty

import (
	"fmt"
	"syscall"
)

// sysAttr builds the SysProcAttr for a PTY child: a new session with the PTY as
// its controlling terminal. darwin has no cgroups, and its SysProcAttr has no
// field that could carry PlaceIn.
//
// A non-nil PlaceIn is an ERROR here rather than something ignored. Nothing in
// this tree passes one on darwin — cgroup.Probe declines on every non-Linux
// platform, so the caller has no handle to pass — which is precisely why
// ignoring it would be the dangerous choice: the branch would never be
// exercised, and a future caller that did pass a handle would get a shell
// outside the container it was told the shell was in.
func sysAttr(cmd Command) (*syscall.SysProcAttr, error) {
	if cmd.PlaceIn != nil {
		return nil, fmt.Errorf("pty: PlaceIn is not supported on darwin: %w", ErrUnsupported)
	}
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}, nil
}
