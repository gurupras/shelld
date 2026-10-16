package daemon

import (
	"context"
	"net"
	"time"

	"github.com/gurupras/shelld/internal/sock"
)

// dialWithin is sock.DialWithin, which says why a dial needs it.
func dialWithin(ctx context.Context, network, addr string, timeout time.Duration, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	return sock.DialWithin(ctx, network, addr, timeout, dial)
}
