package daemon

import (
	"io/fs"

	"github.com/gurupras/shelld/internal/sock"
)

// checkPrivate is sock.CheckPrivate, the rule for the per-user default
// directory.
func checkPrivate(dir string, mode fs.FileMode, ownerUID, uid uint32) error {
	return sock.CheckPrivate(dir, mode, ownerUID, uid)
}
