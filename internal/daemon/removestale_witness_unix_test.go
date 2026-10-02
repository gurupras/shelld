//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLockfileAiming writes a valid lockfile naming socket. Every field ReadLock
// insists on has to be set: a lockfile missing one is unreadable, which is a
// different state from the one an arm setting only Pid and Socket means to build.
func writeLockfileAiming(t *testing.T, pidfile, socket string) {
	t.Helper()
	if err := WriteLockfile(pidfile, LockInfo{Pid: 1, Socket: socket, Version: "test", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

// aimsAt fails unless the lockfile at pidfile is in the state the arm says it
// built. Without this an arm that meant to write a lockfile naming another
// socket, and wrote an invalid one instead, still sees the refusal it expects --
// for the wrong reason, and the state it was added to cover goes untested.
func aimsAt(t *testing.T, pidfile, socket string, want LockfileTarget) {
	t.Helper()
	if got := ReadLock(pidfile).AimsAt(socket); got != want {
		t.Fatalf("the fixture's lockfile is in state %v, not the %v this arm is about", got, want)
	}
}

// TestRemoveStaleNeedsAWitnessThatShelldServedTheSocket pins which socket files
// RemoveStale will unlink. A socket file names no program that bound it, so
// "there is a socket here and nothing answers on it" is equally true of a stale
// shelld socket and of another program's socket whose process died -- and the
// second is not shelld's to delete.
//
// The refusal arms and the removal arms are each other's control: a fix that
// simply refused everything would pass the first two and fail the last two, and
// one that kept removing everything would pass the last two and fail the first.
func TestRemoveStaleNeedsAWitnessThatShelldServedTheSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		// witness sets up the files beside the socket. It returns nothing; the
		// arm's expectations say what should survive.
		witness func(t *testing.T, socket, pidfile string)
		// remove is what RemoveStale should do with the socket file.
		remove bool
		// wantLock is whether <socket>.lock should exist afterwards. On a
		// refusal it must not: AcquireInstance creates it, and litter from one
		// refusal must not become the witness that lets the next run through.
		wantLock bool
		// wantLockfile is whether the pidfile should survive.
		wantLockfile bool
		// needles all appear in the error on a refusal.
		needles []string
	}{{
		name:    "no witness at all",
		witness: func(t *testing.T, socket, pidfile string) {},
		needles: []string{"nothing shows shelld served it"},
	}, {
		name: "the lockfile names another socket",
		witness: func(t *testing.T, socket, pidfile string) {
			other := filepath.Join(filepath.Dir(socket), "other.sock")
			writeLockfileAiming(t, pidfile, other)
			aimsAt(t, pidfile, socket, LockfileNamesAnother)
		},
		wantLockfile: true,
		needles:      []string{"does not name that socket"},
	}, {
		name: "the lockfile cannot be read",
		witness: func(t *testing.T, socket, pidfile string) {
			if err := os.WriteFile(pidfile, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			aimsAt(t, pidfile, socket, LockfileUnreadable)
		},
		wantLockfile: true,
		// An unreadable lockfile is the one refusal arm with a second thing to
		// say, and RemoveStale reports both rather than whichever came first.
		needles: []string{"nothing shows shelld served it", "cannot read the lockfile"},
	}, {
		name: "the instance lock file is beside it",
		witness: func(t *testing.T, socket, pidfile string) {
			// What a Go daemon leaves: acquireHeld creates this before the
			// bind and nothing ever removes it.
			if err := os.WriteFile(lockPath(socket), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		remove:   true,
		wantLock: true,
	}, {
		name: "only a lockfile naming this socket",
		witness: func(t *testing.T, socket, pidfile string) {
			// What a shelld-rs daemon leaves: it writes no instance-lock file
			// at all, so the lockfile's socket field is the only witness.
			writeLockfileAiming(t, pidfile, socket)
			aimsAt(t, pidfile, socket, LockfileAgrees)
		},
		remove: true,
		// AcquireInstance takes the instance lock on the way through.
		wantLock: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortDir(t)
			socket := filepath.Join(dir, "s.sock")
			pidfile := filepath.Join(dir, "s.pid")
			deadSocketFile(t, socket)
			tc.witness(t, socket, pidfile)

			ok, err := RemoveStale(socket, pidfile)

			if tc.remove {
				if !ok || err != nil {
					t.Fatalf("RemoveStale = %v, %v; wanted it to remove the socket", ok, err)
				}
			} else {
				if ok {
					t.Fatalf("RemoveStale reported success; it should have refused")
				}
				if err == nil {
					t.Fatalf("RemoveStale refused without saying why")
				}
				for _, needle := range tc.needles {
					if !strings.Contains(err.Error(), needle) {
						t.Errorf("error %q does not contain %q", err, needle)
					}
				}
				if !strings.Contains(err.Error(), socket) {
					t.Errorf("error %q does not name the socket %s an operator has to deal with", err, socket)
				}
			}

			_, sockErr := os.Lstat(socket)
			if gone := os.IsNotExist(sockErr); gone != tc.remove {
				t.Errorf("socket %s removed = %v, wanted %v (lstat: %v)", socket, gone, tc.remove, sockErr)
			}
			if _, err := os.Lstat(lockPath(socket)); os.IsNotExist(err) == tc.wantLock {
				t.Errorf("%s exists = %v, wanted %v", lockPath(socket), !tc.wantLock, tc.wantLock)
			}
			if _, err := os.Lstat(pidfile); os.IsNotExist(err) == tc.wantLockfile {
				t.Errorf("%s exists = %v, wanted %v", pidfile, !tc.wantLockfile, tc.wantLockfile)
			}
		})
	}
}
