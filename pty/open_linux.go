package pty

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

var errEOF = io.EOF

func openMaster() (*os.File, string, error) {
	// os.OpenFile registers the tty with the runtime poller, so a Close
	// unblocks a concurrent Read.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var n uint32
	err = control(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlockpt: %w", err)
		}
		var err error
		if n, err = unix.IoctlGetUint32(fd, unix.TIOCGPTN); err != nil {
			return fmt.Errorf("ptsname: %w", err)
		}
		return nil
	})
	if err != nil {
		master.Close()
		return nil, "", err
	}
	return master, "/dev/pts/" + strconv.Itoa(int(n)), nil
}
