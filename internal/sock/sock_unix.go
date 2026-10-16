//go:build linux || darwin

package sock

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// DefaultDir is <tmp>/shelld-<uid>, the per-user directory holding the
// default socket, pidfile and logfile.
func DefaultDir() string {
	return filepath.Join(os.TempDir(), "shelld-"+strconv.Itoa(os.Getuid()))
}

// CheckPrivateDir refuses dir unless it is a directory owned by this user
// with no group or other permissions; a missing dir wraps os.ErrNotExist.
// It uses lstat, so a symlink is refused. Between this check and the use of
// a path inside the directory, a directory in a sticky /tmp cannot be
// swapped by another user; under a non-sticky, shared TMPDIR it can (tmux
// has the same gap).
func CheckPrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: no owner", dir)
	}
	return CheckPrivate(dir, st.Mode(), sys.Uid, uint32(os.Getuid()))
}

func defaultSocket() string { return filepath.Join(DefaultDir(), "shelld.sock") }

// Dial connects to a daemon socket within timeout and refuses one served by
// another user: whoever created the path first owns what answers there.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	conn, err := DialUnix(ctx, addr, timeout)
	if err != nil {
		return nil, err
	}
	uid, err := PeerUID(conn.(*net.UnixConn))
	if err == nil && uid != os.Getuid() {
		err = fmt.Errorf("%s is served by uid %d, not this user; refusing to use it", addr, uid)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// DialUnix connects to a unix socket within timeout; DialWithin says why it is
// not net.DialTimeout. It makes no owner check: Dial does.
func DialUnix(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	return DialWithin(ctx, "unix", addr, timeout, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", addr)
	})
}
