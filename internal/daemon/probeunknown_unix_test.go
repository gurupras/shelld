//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L8-L11: a probe connect that times out is undetermined, as shelld-rs's
// Bound::Unknown is, and not bound. listen refuses without unlinking and
// without ErrInstanceHeld, so run exits 1 rather than reporting a holder no
// probe saw; isBound, which every stop, kill and cleanup caller asks, still
// answers it as bound, so none of them removes or signals on it.
//
// The timeout is dialWithin's own, around a connect that never returns: a
// socket under a hung NFS or FUSE mount, which a test cannot mount.
func TestAProbeThatTimesOutIsUndetermined(t *testing.T) {
	path := filepath.Join(shortDir(t), "hung.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()

	hang := make(chan struct{})
	defer close(hang)
	probeDial = func(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
		return dialWithin(ctx, "unix", addr, timeout, func(context.Context) (net.Conn, error) {
			<-hang
			return nil, errors.New("released")
		})
	}
	t.Cleanup(func() { probeDial = dialUnix })

	if b, why := isBoundWith(path, addressHeld); b != boundUnknown || !errors.Is(why, os.ErrDeadlineExceeded) {
		t.Errorf("isBoundWith = %v, %v; want boundUnknown and the timeout", b, why)
	}
	if !isBound(path) {
		t.Error("isBound reads an undetermined probe as not bound, so stop, kill and cleanup would remove the file")
	}
	if NothingServes(path) {
		t.Error("NothingServes answers true for an undetermined probe")
	}

	ln, err := listen(path, ListenOptions{})
	if err == nil {
		ln.Close()
		t.Fatal("listen replaced a socket whose probe never finished")
	}
	if errors.Is(err, ErrInstanceHeld) {
		t.Errorf("listen refused with ErrInstanceHeld (%v); a timeout shows no holder, so it is undetermined", err)
	}
	if want := "bind " + path + ": cannot tell whether a process is listening on it; not replacing it"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("the refusal is %q; want it to begin %q", err, want)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("after the refusal, Lstat(%q): %v -- the file was removed anyway", path, err)
	}
}

// ENOTDIR and ENOTSOCK prove nothing listens, as they do to shelld-rs, where
// they used to read as bound: a path through a regular file, and (on darwin,
// where a connect to a non-socket answers ENOTSOCK) a file that is no socket.
func TestAPathNothingCanListenOnIsNotBound(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	through := filepath.Join(file, "s.sock")
	if b, why := isBoundWith(through, addressHeld); b != notBound {
		t.Errorf("a path through a regular file: %v (%v), want notBound", b, why)
	}
	if b, why := isBoundWith(file, addressHeld); b != notBound {
		t.Errorf("a regular file: %v (%v), want notBound", b, why)
	}
}
