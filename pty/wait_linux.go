package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

// openExit returns a descriptor for process pid that becomes readable when
// the process exits, or nil where pidfds are unavailable (Linux < 5.3, or
// denied by seccomp). Start calls it before anyone can reap the child, so pid
// cannot name a recycled process.
//
// The descriptor comes from pidfd_open rather than SysProcAttr.PidFD: that
// one shares its open file description with the copy os.Process keeps, and
// O_NONBLOCK on a shared description turns os.Process.Wait's waitid into a
// non-blocking call that fails with EAGAIN while the child runs.
func openExit(pid int) *os.File {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil
	}
	// A non-blocking descriptor is registered with the poller by NewFile.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil
	}
	return os.NewFile(uintptr(fd), "pidfd")
}

// awaitExit parks the calling goroutine in the runtime poller until f, from
// openExit, reports that the process exited. os.Process.Wait instead blocks
// an OS thread in waitid for the process's whole life, so a daemon with N
// running windows held N threads, and the runtime never gives idle threads
// back. If the poller cannot wait on f, awaitExit returns early and Wait
// blocks as before.
func awaitExit(f *os.File) {
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Read(func(fd uintptr) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		return err != nil || n > 0
	})
}
