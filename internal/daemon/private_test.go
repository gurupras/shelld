package daemon

import (
	"io/fs"
	"strings"
	"testing"
)

func TestCheckPrivate(t *testing.T) {
	for _, c := range []struct {
		name    string
		mode    fs.FileMode
		owner   uint32
		wantErr string
	}{
		{"private dir", fs.ModeDir | 0o700, 1000, ""},
		{"no permissions at all", fs.ModeDir, 1000, ""},
		{"other user's dir", fs.ModeDir | 0o700, 1001, "owned by uid 1001"},
		{"group readable", fs.ModeDir | 0o750, 1000, "mode 0750"},
		{"world writable", fs.ModeDir | 0o777, 1000, "mode 0777"},
		{"symlink", fs.ModeSymlink | 0o777, 1000, "not a directory"},
		{"regular file", 0o600, 1000, "not a directory"},
	} {
		err := checkPrivate("/tmp/shelld-1000", c.mode, c.owner, 1000)
		if (c.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
