//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// listen never removes or replaces a file that is not a socket, now that the
// probe reads ENOTSOCK and ENOTDIR as nothing listening: removeDeadSocket's
// Lstat refuses both before the probe is asked. Linux answers a connect to a
// regular file with ECONNREFUSED, and /proc/net/unix does not list it, so
// without that refusal the probe alone would authorise the unlink.
func TestListenNeverRemovesAFileThatIsNotASocket(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "plain")
	const content = "not a socket\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ what, path string }{
		{"a regular file at the socket path", file},
		{"a socket path whose parent is a regular file", filepath.Join(file, "s.sock")},
	} {
		ln, err := listen(tc.path, ListenOptions{})
		if err == nil {
			ln.Close()
			t.Errorf("%s: listen succeeded", tc.what)
		}
		if got, rerr := os.ReadFile(file); rerr != nil || string(got) != content {
			t.Fatalf("%s: the regular file is now %q, %v; want it untouched", tc.what, got, rerr)
		}
	}
}
