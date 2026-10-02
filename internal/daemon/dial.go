package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

// dialWithin runs dial with a timeout that also ends a connect blocked in the
// kernel. On unix connect(2) resolves the path before it returns, so a socket
// under a hung NFS or FUSE mount blocks it there, and net.Dialer's timeout and
// ctx reach only a connect that has returned. On Windows go-winio checks ctx
// only between CreateFile attempts, and one CreateFile on a remote pipe whose
// host does not answer blocks for 20 s or more. Either way the dial waited as
// long as the kernel did. dial runs on its own goroutine instead, and a connection it makes after the
// deadline is closed when it arrives. A zero timeout is none, as it is to
// net.Dialer, and leaves ctx the only bound.
func dialWithin(ctx context.Context, network, addr string, timeout time.Duration, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
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

// dialAddr names the address a dialWithin timeout was for.
type dialAddr struct{ network, addr string }

func (a dialAddr) Network() string { return a.network }
func (a dialAddr) String() string  { return a.addr }
