//go:build linux

package proctree

// Tests for the one input that turns this package from "kills a window's shell
// tree" into "kills the machine": a root that is not a positive pid.
//
// syscall.Kill reads non-positive pids as broadcasts — 0 is the caller's own
// process group (the daemon and its siblings), -1 is every process the uid may
// signal, -N is process group N. The oracle does not bound either shape: root == 0
// has pid 1 as a child, because init's ppid in /proc is 0, so the parent closure is
// every process on the box; root < 0 matches no pid at all, so the closure is
// [root] itself and the whole of the damage is done by Kill's interpretation of the
// sign.
//
// None of these tests may send a signal when the guard they are checking is absent,
// or the test would be the bug. Two of them use pure functions (Descendants and
// unionDescendants signal nothing, ever), and the one that exercises KillTree does
// it against the killProc recorder. That is what the seam is for.

import (
	"os"
	"testing"
)

// withRecordedKills lives in recorder_test.go, which darwin compiles too: the
// tests in this file are about /proc-shaped inputs, but the seam they use is not
// Linux's and orphan_reach_test.go needs the same recorder.

// killTreeNow is KillTree with root's start time read immediately before the call,
// which is the value a caller holding a live unreaped child would have recorded. The
// fixtures in this package all hold one, so this is what "kill the tree" means for
// them; a test that wants the recycled case constructs a start time that does not
// match instead (TestKillTreeRefusesARootWhoseStartTimeMoved).
func killTreeNow(t *testing.T, root int) ([]int, bool) {
	t.Helper()
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("StartTicks(%d) failed: the fixture is already gone, so the kill "+
			"under test would be refused for the wrong reason", root)
	}
	return KillTree(root, start)
}

func TestKillTreeSignalsNothingForANonPositiveRoot(t *testing.T) {
	// -os.Getpid() is in the list on purpose: it is a real process group on this
	// box, the test binary's own, so it is the value that would actually kill
	// something. Under the guard it must reach neither Descendants nor killProc.
	//
	// The subtests are named rather than numbered because own_pgroup's number
	// changes every run, and the mutation runs over this package identify a kill by
	// the exact subtest that reddened: a subtest whose name carries a pid cannot be
	// predicted, only matched loosely, and a loose match is how a mutant reddening a
	// different subtest reads as caught. The driver that did the matching is kept
	// outside this repo, so this naming is the part of it a clone still benefits
	// from.
	cases := []struct {
		name string
		root int
	}{{"zero", 0}, {"minus_one", -1}, {"minus_two", -2}, {"own_pgroup", -os.Getpid()}}
	for _, c := range cases {
		root := c.root
		t.Run(c.name, func(t *testing.T) {
			var pids []int
			var ok bool
			// Start time 1: an arbitrary non-matching value, because for a
			// non-positive root there is no process to read one from. The guard under
			// test is the sign check, which must fire before the start-time check has
			// anything to say.
			signalled := withRecordedKills(t, func() { pids, ok = KillTree(root, 1) })
			if !ok {
				t.Errorf("KillTree(%d) reported false; a refusal is still a success", root)
			}
			if len(pids) != 0 {
				t.Errorf("KillTree(%d) targeted %d pids: %v", root, len(pids), pids)
			}
			if len(signalled) != 0 {
				t.Errorf("KillTree(%d) signalled %d pids: %v", root, len(signalled), signalled)
			}
		})
	}
}

func TestDescendantsRefusesANonPositiveRoot(t *testing.T) {
	// The premise first, because "Descendants(0) is empty" is only interesting if
	// the unguarded answer would have been large. Both halves are read from the id
	// map the oracle itself is given, so the premise cannot pass by looking at a
	// different /proc than the code does.
	ids := idMap()
	if len(ids) < 20 {
		t.Fatalf("premise: only %d processes visible; this box cannot show the hazard", len(ids))
	}
	id, ok := ids[1]
	if !ok {
		t.Fatal("premise: pid 1 is absent from the id map; root 0's closure reaches " +
			"the machine only through init's parent link")
	}
	if id.ppid != 0 {
		t.Fatalf("premise: pid 1's ppid is %d, want 0 — root 0's closure reaches the "+
			"machine only through init's parent link", id.ppid)
	}
	// What the closure would answer for root 0 without the guard, computed here
	// rather than asserted from memory: the number in unionDescendants' comment is
	// this box's, and a reader on another box should see their own.
	// Bounded below at half rather than at "all", and the reason is a real effect
	// rather than slack: idMap is not an atomic snapshot, so a process whose parent
	// exited between two reads has a ppid that is no longer a key, and it is then
	// unreachable from 0 through parent links. The claim being pinned is "unbounded",
	// which half the machine already establishes; the exact number is logged.
	reach := len(closureFrom(0, ids))
	if reach*2 < len(ids) {
		t.Fatalf("premise: root 0's unguarded closure is only %d of %d processes; "+
			"this box cannot show the hazard", reach, len(ids))
	}
	t.Logf("root 0's unguarded closure would be %d pids of %d visible", reach, len(ids))

	for _, root := range []int{0, -1, -os.Getpid()} {
		if got := Descendants(root); len(got) != 0 {
			t.Errorf("Descendants(%d) = %v (%d pids), want empty", root, first(got, 8), len(got))
		}
	}
}

func TestUnionDescendantsRefusesANonPositiveRootOnAFabricatedTree(t *testing.T) {
	// A fabricated map, so the refusal is a property of the oracle and not of what
	// /proc happens to hold. Both roots have a non-empty closure in it: 0 is the
	// declared parent of 1, and -5 is the declared parent of 7 — the latter is a
	// shape /proc cannot produce, which is exactly why it belongs in a fabricated
	// fixture rather than in the live test above.
	ids := map[int]procIDs{
		1: {ppid: 0, pgid: 1, sid: 1},
		2: {ppid: 1, pgid: 1, sid: 1},
		7: {ppid: -5, pgid: 7, sid: 7},
		8: {ppid: 7, pgid: 7, sid: 7},
	}
	for _, root := range []int{0, -5} {
		if got := unionDescendants(root, ids); len(got) != 0 {
			t.Errorf("unionDescendants(%d) = %v, want empty", root, got)
		}
	}
	// And the same map still answers for a positive root, so the guard is a guard
	// and not a way of returning empty for everything.
	if got := unionDescendants(1, ids); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("unionDescendants(1) = %v, want [1 2]", got)
	}
	// snapshotDescendants carries the same guard, and the differential tests compare
	// the two, so it is asserted here rather than left to be discovered by a
	// disagreement of one whole machine.
	if got := snapshotDescendants(0); len(got) != 0 {
		t.Errorf("snapshotDescendants(0) = %d pids, want empty", len(got))
	}
	if got := snapshotDescendants(-1); len(got) != 0 {
		t.Errorf("snapshotDescendants(-1) = %v, want empty", got)
	}
}

// closureFrom is the parent-link closure with no guard, for the premise above. A
// private copy rather than a call into snapshotDescendants, because the point of
// the premise is to show what the guarded function refuses to do — asking the
// guarded function would show nothing.
func closureFrom(root int, ids map[int]procIDs) []int {
	children := make(map[int][]int, len(ids))
	for pid, id := range ids {
		children[id.ppid] = append(children[id.ppid], pid)
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, kid := range children[out[i]] {
			if !seen[kid] {
				seen[kid] = true
				out = append(out, kid)
			}
		}
	}
	return out
}

func first(xs []int, n int) []int {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
