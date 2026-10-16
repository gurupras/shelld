//go:build windows

package daemon

import (
	"context"
	"fmt"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/gurupras/shelld/internal/sock"
)

// currentUser returns this process's user SID and whether its token is
// elevated.
func currentUser() (*windows.SID, bool, error) { return sock.CurrentUser() }

func listen(pipe string, opts ListenOptions) (net.Listener, error) {
	sid, elevated, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("pipe security for %s: %w", pipe, err)
	}
	// go-winio creates the first instance exclusively (FILE_CREATE), so a
	// stale or foreign server cannot silently share the name, and keeps a
	// free instance ready for each new client.
	ln, err := winio.ListenPipe(pipe, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL(sid.String(), elevated, opts.SharedAccess),
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		if !pipeGone(pipe) {
			return nil, fmt.Errorf("create pipe %s: the name is in use by another process (%v): %w", pipe, err, ErrInstanceHeld)
		}
		return nil, fmt.Errorf("create pipe %s: %w", pipe, err)
	}
	return ln, nil
}

// Dial connects to a daemon pipe within timeout and refuses a pipe that
// another user owns; see sock.Dial.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	return sock.Dial(ctx, addr, timeout)
}

func checkPipeOwner(conn net.Conn, addr string) error { return sock.CheckPipeOwner(conn, addr) }
