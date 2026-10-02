package pty

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var errEOF = io.EOF

func openMaster() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var name [128]byte
	err = control(master, func(fd int) error {
		if err := ioctl(uintptr(fd), unix.TIOCPTYGRANT, 0); err != nil {
			return fmt.Errorf("grantpt: %w", err)
		}
		if err := ioctl(uintptr(fd), unix.TIOCPTYUNLK, 0); err != nil {
			return fmt.Errorf("unlockpt: %w", err)
		}
		if err := ioctl(uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
			return fmt.Errorf("ptsname: %w", err)
		}
		return nil
	})
	if err != nil {
		master.Close()
		return nil, "", err
	}
	if i := bytes.IndexByte(name[:], 0); i >= 0 {
		return master, string(name[:i]), nil
	}
	return master, string(name[:]), nil
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}
