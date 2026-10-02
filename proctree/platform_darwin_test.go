//go:build darwin

package proctree

// platformIDs replaced a `ps -axo pid=,ppid=` fork with one sysctl, and the parent
// links it now returns are half the oracle. The other half — the session set —
// would hide a break in them: every process in the orphan fixture is in the
// leader's session, so unionDescendants finds them all from sid alone and a
// wholesale wrong ppid would leave those tests green. This is the check that does
// not share that blind spot.

import (
	"fmt"
	"os"
	"testing"
)

// TestTheSysctlSnapshotAgreesWithPsAboutParents holds the new source against the
// one it replaced. Disagreement is what matters, not equality of the two process
// sets: the two snapshots are taken at different instants, so a process that
// starts or exits between them is expected and says nothing. A pid both of them
// report with DIFFERENT parents cannot be explained that way except by a reparent
// in the same window, which is rare enough that a handful is noise and a broad
// sweep is a defect.
func TestTheSysctlSnapshotAgreesWithPsAboutParents(t *testing.T) {
	sysctl, ok := platformIDs()
	if !ok {
		t.Fatal("platformIDs reported false, so idMap silently falls back to the ps fork " +
			"this was meant to replace and the session set goes with it")
	}
	ps := psParentMap()
	if len(ps) == 0 {
		t.Skip("ps gave nothing to compare against")
	}

	var shared, differ int
	var examples []string
	for pid, ppid := range ps {
		id, seen := sysctl[pid]
		if !seen {
			continue // started or exited between the two snapshots
		}
		shared++
		if id.ppid != ppid {
			differ++
			if len(examples) < 5 {
				examples = append(examples,
					fmt.Sprintf("pid %d: sysctl says ppid %d, ps says %d", pid, id.ppid, ppid))
			}
		}
	}
	if shared < 50 {
		t.Fatalf("only %d pids appear in both snapshots, which is too few for the "+
			"comparison to mean anything", shared)
	}
	// A whole-snapshot defect shows up as most of them disagreeing; a genuine
	// reparent between two reads shows up as one or two.
	if differ*20 > shared {
		t.Errorf("%d of %d shared pids have a different parent in the sysctl snapshot "+
			"than in ps: %v", differ, shared, examples)
	}
	t.Logf("%d pids in both snapshots, %d disagree on the parent", shared, differ)

	// And the one link this test can assert absolutely, because it made it: this
	// process's own parent.
	self := os.Getpid()
	if id, seen := sysctl[self]; !seen {
		t.Errorf("the snapshot does not contain this test process %d at all", self)
	} else if id.ppid != os.Getppid() {
		t.Errorf("the snapshot gives this process ppid %d, want %d", id.ppid, os.Getppid())
	}
}

// TestTheSnapshotHasASessionForALeader keeps the session half honest in the same
// place. A zero sid is not an error anywhere in this package — it disables the
// session term — so a getsid that failed for everything would quietly turn the
// oracle back into the parent closure and no other test here would say so.
func TestTheSnapshotHasASessionForALeader(t *testing.T) {
	sysctl, ok := platformIDs()
	if !ok {
		t.Fatal("platformIDs reported false")
	}
	leaders, withSid := 0, 0
	for pid, id := range sysctl {
		if id.sid > 0 {
			withSid++
		}
		if id.sid == pid {
			leaders++
		}
	}
	if withSid*2 < len(sysctl) {
		t.Errorf("only %d of %d processes have a session id: a sid of 0 disables the "+
			"session term, so this is the oracle degrading to the parent closure "+
			"without reporting anything", withSid, len(sysctl))
	}
	if leaders == 0 {
		t.Errorf("no process in %d is its own session leader, so `sid == root` can never "+
			"hold and the session term is dead code here", len(sysctl))
	}
	t.Logf("%d processes, %d with a session id, %d of them session leaders",
		len(sysctl), withSid, leaders)
}
