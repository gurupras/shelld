// Package sock is the client's half of reaching a shelld daemon: where the
// socket is, whether its directory is safe to trust, and the owner-checked
// dial. It is split from internal/daemon so that a client (shellctl) can reach
// the daemon without linking the server: package init of everything
// internal/daemon pulls in (net/http, the root shelld package's regexps, the
// crypto stack) runs on every client start, whatever the client does next.
//
// internal/daemon delegates to this package, so there is one implementation of
// each rule.
package sock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"time"
)

// DefaultSocket is $SHELLD_SOCKET, else shelld.sock in DefaultDir, or on Windows
// \\.\pipe\shelld-<user SID>.
func DefaultSocket() string {
	if v := os.Getenv("SHELLD_SOCKET"); v != "" {
		return v
	}
	return defaultSocket()
}

// CheckPrivate is the rule for the per-user default directory, as tmux's
// make_label applies it to /tmp/tmux-<uid> but stricter (no group bits
// either): a real directory, owned by the user, with no group or other
// permissions. Anything else may have been created by another user to
// intercept the daemon's socket.
func CheckPrivate(dir string, mode fs.FileMode, ownerUID, uid uint32) error {
	switch {
	case mode&fs.ModeSymlink != 0, !mode.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case ownerUID != uid:
		return fmt.Errorf("directory %s has unsafe permissions: owned by uid %d, not %d", dir, ownerUID, uid)
	case mode.Perm()&0o077 != 0:
		return fmt.Errorf("directory %s has unsafe permissions: mode %04o", dir, mode.Perm())
	}
	return nil
}

// DialWithin runs dial with a timeout that also ends a connect blocked in the
// kernel. On unix connect(2) resolves the path before it returns, so a socket
// under a hung NFS or FUSE mount blocks it there, and net.Dialer's timeout and
// ctx reach only a connect that has returned. On Windows go-winio checks ctx
// only between CreateFile attempts, and one CreateFile on a remote pipe whose
// host does not answer blocks for 20 s or more. Either way the dial waited as
// long as the kernel did. dial runs on its own goroutine instead, and a connection it makes after the
// deadline is closed when it arrives. A zero timeout is none, as it is to
// net.Dialer, and leaves ctx the only bound.
func DialWithin(ctx context.Context, network, addr string, timeout time.Duration, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	cancel := context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := dial(ctx)
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.conn != nil {
				r.conn.Close()
			}
		}()
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			err = os.ErrDeadlineExceeded // "i/o timeout", as net.Dialer reports its own
		}
		return nil, &net.OpError{Op: "dial", Net: network, Addr: dialAddr{network, addr}, Err: err}
	}
}

// dialAddr names the address a DialWithin timeout was for.
type dialAddr struct{ network, addr string }

func (a dialAddr) Network() string { return a.network }
func (a dialAddr) String() string  { return a.addr }
