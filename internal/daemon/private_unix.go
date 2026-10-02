//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// DefaultDir is <tmp>/shelld-<uid>, the per-user directory holding the
// default socket, pidfile and logfile.
func DefaultDir() string {
	return filepath.Join(os.TempDir(), "shelld-"+strconv.Itoa(os.Getuid()))
}

// CheckPrivateDir refuses dir unless it is a directory owned by this user
// with no group or other permissions; a missing dir wraps os.ErrNotExist.
// It uses lstat, so a symlink is refused. Between this check and the use of
// a path inside the directory, a directory in a sticky /tmp cannot be
// swapped by another user; under a non-sticky, shared TMPDIR it can (tmux
// has the same gap).
func CheckPrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: no owner", dir)
	}
	return checkPrivate(dir, st.Mode(), sys.Uid, uint32(os.Getuid()))
}

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
