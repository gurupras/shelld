package daemon

import (
	"net"

	"github.com/gurupras/shelld/internal/sock"
)

// peerUID is the uid of the process at the other end of a Unix socket.
func peerUID(c *net.UnixConn) (int, error) { return sock.PeerUID(c) }
