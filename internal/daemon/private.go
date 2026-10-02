package daemon

import (
	"fmt"
	"io/fs"
)

// checkPrivate is the rule for the per-user default directory, as tmux's
// make_label applies it to /tmp/tmux-<uid> but stricter (no group bits
// either): a real directory, owned by the user, with no group or other
// permissions. Anything else may have been created by another user to
// intercept the daemon's socket.
func checkPrivate(dir string, mode fs.FileMode, ownerUID, uid uint32) error {
	switch {
	case mode&fs.ModeSymlink != 0, !mode.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case ownerUID != uid:
		return fmt.Errorf("directory %s has unsafe permissions: owned by uid %d, not %d", dir, ownerUID, uid)
	case mode.Perm()&0o077 != 0:
		return fmt.Errorf("directory %s has unsafe permissions: mode %04o", dir, mode.Perm())
	}
	return nil
}
