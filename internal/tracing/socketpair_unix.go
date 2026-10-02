//go:build shelld_trace && (linux || darwin)

package tracing

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func socketpair() (net.Conn, net.Conn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	conn := func(fd int) (net.Conn, error) {
		f := os.NewFile(uintptr(fd), "canary")
		defer f.Close()
		return net.FileConn(f)
	}
	a, err := conn(fds[0])
	if err != nil {
		return nil, nil, err
	}
	b, err := conn(fds[1])
	if err != nil {
		return nil, nil, err
	}
	return a, b, nil
}
