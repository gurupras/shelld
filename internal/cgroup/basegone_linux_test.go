package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurupras/shelld/proctree"
)

// withStartTicks substitutes the start-time read for one test. Restoring it is a
// t.Cleanup rather than a defer so that a t.Fatal inside the body cannot leave the
// substitution in place for the rest of the package.
func withStartTicks(t *testing.T, f func(int) (uint64, bool)) {
	t.Helper()
	saved := startTicks
	startTicks = f
	t.Cleanup(func() { startTicks = saved })
}

// baseGone decides whether GC removes a directory a live peer daemon may still
// need, so it has to answer on positive evidence of death. The case that matters
// is the fourth one: a read that merely failed is not evidence, and the version
// before this read it as one.
func TestBaseGoneAnswersOnlyOnPositiveEvidence(t *testing.T) {
	selfStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's own start time")
	}
	// A pid past pid_max's ceiling, so it names no process and never will.
	const noSuchPid = 1 << 30

	t.Run("a live pid whose start time matches is not gone", func(t *testing.T) {
		if baseGone(os.Getpid(), selfStart) {
			t.Error("baseGone said this very process is gone")
		}
	})

	t.Run("a live pid whose start time differs is gone", func(t *testing.T) {
		if !baseGone(os.Getpid(), selfStart+1) {
			t.Error("baseGone kept a base whose pid has been recycled")
		}
	})

	t.Run("a pid with no /proc entry is gone", func(t *testing.T) {
		if !baseGone(noSuchPid, 1) {
			t.Error("baseGone kept a base for a pid that does not exist, " +
				"which is the ordinary case this GC exists for")
		}
	})

	// The fix. EMFILE in this process is the case to picture: the open fails while
	// stat, which needs no descriptor, still answers, so the read fails against a
	// process that is alive, and the old `ok && live == start` fell through to the
	// removal. (hidepid was cited here and is not an example of this; see baseGone's
	// doc, which splits it by value.)
	t.Run("a failed read against a live pid is not evidence", func(t *testing.T) {
		withStartTicks(t, func(int) (uint64, bool) { return 0, false })
		if baseGone(os.Getpid(), selfStart) {
			t.Error("baseGone said a live process is gone because its start time " +
				"could not be read")
		}
	})

	// And a failed read still collects a dead one, so the existence check has to
	// be a separate syscall rather than a second look at the same bool.
	t.Run("a failed read against a dead pid is still evidence", func(t *testing.T) {
		withStartTicks(t, func(int) (uint64, bool) { return 0, false })
		if !baseGone(noSuchPid, 1) {
			t.Error("baseGone stopped collecting dead bases")
		}
	})
}

// withProcDir points baseGone's procfs stats at a directory for one test.
func withProcDir(t *testing.T, dir string) {
	t.Helper()
	saved := procDir
	procDir = dir
	t.Cleanup(func() { procDir = saved })
}

// An absent /proc entry is evidence about a pid only if this /proc answers for a pid
// that certainly exists. Against a procfs that answers for nothing -- not mounted, or
// an empty directory standing where the mount should be -- every pid stats ENOENT, so
// without the control baseGone reports every base gone at once, which is the failure
// the paragraph in GC describes arriving on one peer arriving on all of them. A
// negative answer from a missing instrument reads exactly like a healthy one.
//
// Both legs need the read to fail as well, because a successful read answers before
// either stat is reached: procfs being unusable and StartTicks working is not a state
// that exists.
func TestBaseGoneDoesNotReadAnUnusableProcAsEveryDaemonBeingDead(t *testing.T) {
	selfStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's own start time")
	}
	const noSuchPid = 1 << 30

	// The liveness half, first because it is what makes the other leg mean anything:
	// a procfs that answers for us and not for the pid is the healthy shape, and it
	// must still collect. Without this, a seam that broke collection outright -- or a
	// baseGone that answered false unconditionally -- would satisfy the leg below.
	t.Run("a procfs that answers for us still collects a dead pid", func(t *testing.T) {
		withStartTicks(t, func(int) (uint64, bool) { return 0, false })
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
			t.Fatal(err)
		}
		withProcDir(t, root)
		if !baseGone(noSuchPid, 1) {
			t.Error("baseGone stopped collecting a pid that does not exist, so the " +
				"leg below cannot tell the control from a function that answers false")
		}
	})

	t.Run("a procfs that answers for nothing is not evidence", func(t *testing.T) {
		withStartTicks(t, func(int) (uint64, bool) { return 0, false })
		// Empty: our own entry is missing too, which is what separates an unusable
		// procfs from a pid that has exited.
		withProcDir(t, t.TempDir())
		if baseGone(os.Getpid(), selfStart) {
			t.Error("baseGone said this very process is gone, on a procfs that " +
				"answers for no pid at all: every live peer's base reads as " +
				"collectable at the same moment")
		}
	})
}

// The consequence at the level GC acts on, which is a directory being removed. A
// peer between windows has an EMPTY base, so removeIfEmpty takes it and the peer
// holds no descriptor to it: its next Manager.New mkdirs under a parent that is
// gone and fails ENOENT, for every window it opens from then on, and each of those
// windows spawns with no cgroup.kill to reach an orphan that setsid'd away.
func TestGCKeepsABaseItCannotDecideAbout(t *testing.T) {
	selfStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's own start time")
	}
	root := t.TempDir()
	// Empty, not populated: populated bases are counted busy and survive for a
	// reason that has nothing to do with this. Empty is the removable case, and
	// it is also the state a healthy peer is in between windows.
	peer := fakeCgroup(t, filepath.Join(root, baseName(os.Getpid(), selfStart)), false)
	// A second base whose pid really is gone, as the liveness check: a GC that
	// collected nothing at all would satisfy the assertion below for the wrong
	// reason.
	dead := fakeCgroup(t, filepath.Join(root, baseName(1<<30, 1)), false)

	withStartTicks(t, func(int) (uint64, bool) { return 0, false })
	m := &Manager{parent: root, base: filepath.Join(root, "not-a-base")}
	removed, busy, err := m.GC()

	// What GC ATTEMPTED is the observable here, not what it deleted. In a real
	// cgroup2 tree the files are virtual and rmdir on an unpopulated directory
	// succeeds; in a temp tree fakeCgroup's cgroup.events is a real file, so every
	// attempted removal fails with ENOTEMPTY and names its directory. That is the
	// same signal TestGCReportsWhatItCannotRemove relies on, and it is the one
	// that separates the two bases: the attempt is the decision baseGone made.
	if err == nil {
		t.Fatalf("GC = %d, %d, nil: neither base was attempted, so this test "+
			"cannot tell the two decisions apart", removed, busy)
	}
	if strings.Contains(err.Error(), peer) {
		t.Errorf("GC tried to remove a live peer's base although it could not "+
			"read the peer's start time: %v", err)
	}
	// The liveness half: a GC that decided "cannot tell" about everything would
	// satisfy the assertion above for the wrong reason.
	if !strings.Contains(err.Error(), dead) {
		t.Errorf("GC did not try to remove the base of a pid with no /proc "+
			"entry: %v", err)
	}
	if _, serr := os.Stat(peer); serr != nil {
		t.Errorf("the peer's base is gone: %v", serr)
	}
	if removed != 0 || busy != 0 {
		t.Errorf("GC = %d removed, %d busy, want 0 and 0: a failed attempt is "+
			"neither", removed, busy)
	}
}
