//go:build windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// currentUser returns this process's user SID and whether its token is
// elevated.
func currentUser() (*windows.SID, bool, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, false, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, false, err
	}
	sid, err := u.User.Sid.Copy()
	if err != nil {
		return nil, false, err
	}
	return sid, tok.IsElevated(), nil
}

// defaultSocket is \\.\pipe\shelld-<user SID>. The name only keeps users'
// daemons apart; access is controlled by the pipe's security descriptor and
// the client's owner check, since anyone can compute another user's SID.
func defaultSocket() string {
	sid, _, err := currentUser()
	if err != nil {
		return `\\.\pipe\shelld`
	}
	return `\\.\pipe\shelld-` + sid.String()
}

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

// Dial connects to a daemon pipe within timeout, retrying while all
// instances are busy, and refuses a pipe that another user owns: a pipe name
// can be created first by anyone, so the name alone proves nothing.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	conn, err := dialWithin(ctx, "pipe", addr, timeout, func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, addr)
	})
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, fmt.Errorf("%w (the daemon may run as another user, or elevated while this process is not; only its user, and for an elevated daemon only elevated processes, can connect unless it was started with --shared-access)", err)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPipeOwner(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkPipeOwner(conn net.Conn, addr string) error {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("pipe %s: cannot check its owner", addr)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("pipe %s: read owner: %w", addr, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("pipe %s: read owner: %w", addr, err)
	}
	me, _, err := currentUser()
	if err != nil {
		return fmt.Errorf("pipe %s: current user: %w", addr, err)
	}
	if !owner.Equals(me) {
		return fmt.Errorf("pipe %s is owned by %s, not the current user %s; refusing to use it", addr, owner, me)
	}
	return nil
}
