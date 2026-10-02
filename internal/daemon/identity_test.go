package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestSameDaemon(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	lock := LockInfo{Pid: 42, StartedAt: now.Add(-time.Hour).Unix()}
	started := time.Unix(lock.StartedAt, 0)
	for _, c := range []struct {
		name    string
		info    procInfo
		ok      bool
		because string
	}{
		{"daemon", procInfo{started.Add(-300 * time.Millisecond), "/usr/local/bin/shelld"}, true, ""},
		{"started within the slack", procInfo{started.Add(1500 * time.Millisecond), "shelld"}, true, ""},
		{"windows image", procInfo{started, `C:\tools\shelld.exe`}, true, ""},
		{"traced build", procInfo{started, "/opt/bin/shelld-traced"}, true, ""},
		{"replaced binary", procInfo{started, "/usr/bin/shelld (deleted)"}, true, ""},
		{"upper case", procInfo{started, `C:\X\SHELLD.EXE`}, true, ""},
		// Each factor alone refuses.
		{"reused pid named shelld", procInfo{started.Add(time.Minute), "/tmp/shelld-sleep"}, false, "after the lockfile"},
		{"old process, other image", procInfo{started.Add(-time.Minute), "/usr/bin/sleep"}, false, "not shelld"},
		{"test binary", procInfo{started, "/tmp/go-build/daemon.test"}, false, "not shelld"},
		{"clock stepped back", procInfo{now.Add(time.Minute), "shelld"}, false, "future"},
	} {
		ok, why := sameDaemon(c.info, now, lock)
		if ok != c.ok || !strings.Contains(why, c.because) {
			t.Errorf("%s: sameDaemon = %v %q", c.name, ok, why)
		}
	}
}
