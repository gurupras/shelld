//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gurupras/shelld/internal/sock"
)

// DefaultDir is <tmp>/shelld-<uid>, the per-user directory holding the
// default socket, pidfile and logfile. See sock.DefaultDir.
func DefaultDir() string { return sock.DefaultDir() }

// CheckPrivateDir refuses dir unless it is a directory owned by this user
// with no group or other permissions; see sock.CheckPrivateDir.
func CheckPrivateDir(dir string) error { return sock.CheckPrivateDir(dir) }

// EnsurePrivateDir creates dir with mode 0700 if it is missing, then checks
// it as CheckPrivateDir does. An existing directory is never chmodded into
// shape: one someone else created is exactly what the check is for.
func EnsurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return CheckPrivateDir(dir)
}

// UnsafeParentWarning describes why an explicitly given socket's directory
// lets other users replace the socket, or returns "".
func UnsafeParentWarning(path string) string {
	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return ""
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	switch {
	case ok && int(sys.Uid) != os.Getuid() && st.Mode()&os.ModeSticky == 0 && st.Mode().Perm()&0o022 != 0:
		return fmt.Sprintf("socket directory %s is owned by uid %d and writable by others; they can replace the socket", dir, sys.Uid)
	case ok && int(sys.Uid) != os.Getuid():
		return fmt.Sprintf("socket directory %s is owned by uid %d, not this user", dir, sys.Uid)
	case st.Mode()&os.ModeSticky == 0 && st.Mode().Perm()&0o022 != 0:
		return fmt.Sprintf("socket directory %s is writable by group or others without the sticky bit; they can replace the socket", dir)
	}
	return ""
}
