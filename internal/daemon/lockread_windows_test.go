//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnreadableLockfileOnWindows is the Windows half of the three-outcome read.
// It is its own file because the Unix coverage cannot reach these functions:
// discovery_unix_test.go is unix-only, so `GOOS=windows go list -f
// '{{.TestGoFiles}}' ./internal/daemon` does not list it, and openDaemon and
// RemoveStale are separate implementations of the same rule. Without this the
// rule would be stated on Windows only in their comments, and a platform test
// gate copied along with the code is how a defect survives a port.
//
// Nothing is created on the pipe, deliberately: the unreadable arm is answered
// before either function looks at one, which is the property. A read that failed
// says nothing about what is serving, so it must not be answered by asking.
func TestUnreadableLockfileOnWindows(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "shelld.pid")
	// Unparseable rather than an ACL that denies the read: a suite running as an
	// administrator opens the file anyway, and the fixture would stop reproducing
	// without failing.
	if err := os.WriteFile(pidfile, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := ReadLock(pidfile)
	if lock.Err == nil {
		t.Fatalf("setup: the damaged lockfile read as %+v", lock.Info)
	}
	pipe := fmt.Sprintf(`\\.\pipe\shelld-unreadable-%d`, os.Getpid())
	if !pipeGone(pipe) {
		t.Fatalf("setup: something already serves %s", pipe)
	}

	loc := LocateDaemon(pipe, lock)
	if loc.State != DaemonUnverified {
		t.Errorf("LocateDaemon = %+v, want unverified: gone sends the caller on to delete the lockfile it could not read", loc)
	}
	if !strings.Contains(loc.Reason, pidfile) || !strings.Contains(loc.Reason, lock.Err.Error()) {
		t.Errorf("reason %q names neither %s nor why it could not be read", loc.Reason, pidfile)
	}
	if strings.Contains(loc.Reason, "no lockfile") {
		t.Errorf("reason %q says there is no lockfile, and one is sitting at %s", loc.Reason, pidfile)
	}

	// Gone is the state cmdKill answers by removing the files and exiting 0, so the
	// refusal has to carry a state that is not gone, not merely be an error.
	pid, err := KillDaemon(pipe, lock)
	var nd *NotDaemonError
	switch {
	case err == nil:
		t.Errorf("KillDaemon terminated pid %d on a lockfile it could not read", pid)
	case !errors.As(err, &nd):
		t.Errorf("KillDaemon = %v, want a *NotDaemonError a caller can classify", err)
	case nd.Located.State == DaemonGone:
		t.Errorf("KillDaemon = %v with state gone, which makes `kill` remove the files and exit 0", err)
	}

	// RemoveStale reports whether it removed the leftovers. It could not read the
	// file it would remove, so it has not; true here makes `stop` and `kill` print
	// success with the lockfile still sitting there.
	if ok, err := RemoveStale(pipe, pidfile); ok || err == nil {
		t.Errorf("RemoveStale = %v, %v; want it to report it cleaned up nothing, and say why", ok, err)
	}
	if _, err := os.Stat(pidfile); err != nil {
		t.Errorf("the lockfile was removed by a call that could not read it: %v", err)
	}
}
