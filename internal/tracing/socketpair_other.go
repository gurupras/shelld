//go:build shelld_trace && !(linux || darwin)

package tracing

import (
	"errors"
	"net"
)

func socketpair() (net.Conn, net.Conn, error) { return nil, nil, errors.New("no socketpair") }
