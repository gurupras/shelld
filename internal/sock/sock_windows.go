//go:build windows

package sock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// DefaultDir is %TEMP%, which is already per-user.
func DefaultDir() string { return os.TempDir() }

// CheckPrivateDir does nothing on Windows: the default directory is the
// user's own %TEMP%, and the pipe is protected by its security descriptor.
func CheckPrivateDir(string) error { return nil }

// CurrentUser returns this process's user SID and whether its token is
// elevated.
func CurrentUser() (*windows.SID, bool, error) {
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
	sid, _, err := CurrentUser()
	if err != nil {
		return `\\.\pipe\shelld`
	}
	return `\\.\pipe\shelld-` + sid.String()
}

// Dial connects to a daemon pipe within timeout, retrying while all
// instances are busy, and refuses a pipe that another user owns: a pipe name
// can be created first by anyone, so the name alone proves nothing.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	conn, err := DialWithin(ctx, "pipe", addr, timeout, func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, addr)
	})
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, fmt.Errorf("%w (the daemon may run as another user, or elevated while this process is not; only its user, and for an elevated daemon only elevated processes, can connect unless it was started with --shared-access)", err)
	}
	if err != nil {
		return nil, err
	}
	if err := CheckPipeOwner(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// CheckPipeOwner refuses a pipe whose owner is not the current user.
func CheckPipeOwner(conn net.Conn, addr string) error {
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
	me, _, err := CurrentUser()
	if err != nil {
		return fmt.Errorf("pipe %s: current user: %w", addr, err)
	}
	if !owner.Equals(me) {
		return fmt.Errorf("pipe %s is owned by %s, not the current user %s; refusing to use it", addr, owner, me)
	}
	return nil
}
