//go:build linux

package proctree

// Tests for the second way a root can name the wrong processes: not a bad sign
// (guard_test.go) but a stale pid.
//
// A pid does not name a process. /proc/sys/kernel/pid_max on this box wraps after
// about 11 days of steady forking, so an integer recorded at spawn time may, by the
// time a window is closed, belong to something else entirely — and KillTree's answer
// is not just that pid but its parent closure UNION its session, so a stranger that
// happens to lead a session takes its whole session with it. The identity KillTree
// requires is the pid together with its start time, read while the process was still
// the caller's unreaped child (a pid cannot be recycled before its parent reaps it,
// so that read is durable for as long as the caller holds the zombie).
//
// Every test here runs against the killProc recorder, for the reason given in
// guard_test.go: if the guard under test is absent, the call signals, and a test that
// looked at the aftermath would be the bug rather than the witness.

import (
	"maps"
	"os/exec"
	"testing"
)

func TestKillTreeRefusesARootWhoseStartTimeMoved(t *testing.T) {
	root, _ := startPgroupTree(t, "sleep 300 & wait", 2)
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("premise: StartTicks(%d) failed on a live fixture", root)
	}

	// The refusal. start+1 rather than 0 or a random number: the point is that any
	// value other than the live one is refused, and a neighbouring tick is the value
	// a real recycled pid is most likely to carry, since the recycled process was
	// started later and start times only grow.
	var pids []int
	var okRefuse bool
	signalled := withRecordedKills(t, func() { pids, okRefuse = KillTree(root, start+1) })
	if !okRefuse {
		t.Errorf("KillTree reported false for a mismatched start time; a refusal is "+
			"still a successful kill of a tree that no longer exists (pids %v)", pids)
	}
	if len(pids) != 0 {
		t.Errorf("KillTree named %d pids for a mismatched start time: %v", len(pids), pids)
	}
	if len(signalled) != 0 {
		t.Errorf("KillTree signalled %d pids for a mismatched start time: %v",
			len(signalled), signalled)
	}

	// And the acceptance, in the same test and against the same fixture, because
	// "nothing was signalled" is not evidence on its own: a KillTree that refused
	// every root would pass the block above. This half is what makes the refusal a
	// refusal of the mismatch rather than of the tree.
	var got []int
	accepted := withRecordedKills(t, func() { got, _ = KillTree(root, start) })
	if len(got) < 2 || got[0] != root {
		t.Fatalf("KillTree with the live start time named %v, want root %d and at "+
			"least the sleep below it", got, root)
	}
	if len(accepted) != len(got) {
		t.Errorf("KillTree named %d pids but signalled %d: %v vs %v",
			len(got), len(accepted), got, accepted)
	}
}

func TestKillTreeRefusesAReapedRoot(t *testing.T) {
	// A child of this test process and nothing else, so the pid this test hands to
	// KillTree is one it is entitled to have an opinion about.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root := cmd.Process.Pid
	// Read while it is our unreaped child — either still running or a zombie. That
	// is the window in which the pid cannot have been reused, which is the whole
	// reason a caller records the value at spawn time rather than at kill time.
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("premise: StartTicks(%d) failed while it was still our child", root)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("premise: waiting for the fixture failed: %v", err)
	}
	// From here on `root` is an integer the kernel may hand to anyone.

	var pids []int
	var okRefuse bool
	signalled := withRecordedKills(t, func() { pids, okRefuse = KillTree(root, start) })
	if !okRefuse {
		t.Errorf("KillTree reported false for a reaped root; want a refusal reported "+
			"as success (pids %v)", pids)
	}
	if len(pids) != 0 {
		t.Errorf("KillTree named %d pids for a reaped root: %v", len(pids), pids)
	}
	if len(signalled) != 0 {
		t.Errorf("KillTree signalled %d pids for a reaped root: %v", len(signalled), signalled)
	}
	// Which branch refused it is logged rather than asserted, because both are
	// correct and this test cannot choose: usually /proc no longer has the pid, but
	// on a box forking fast enough it may already belong to a stranger with a later
	// start time. The acceptance half of
	// TestKillTreeRefusesARootWhoseStartTimeMoved is what rules out a KillTree that
	// refuses everything.
	if live, ok := StartTicks(root); ok {
		t.Logf("pid %d was already reused: start %d, ours was %d", root, live, start)
	}
}

// withIDMap substitutes the snapshot KillTree enumerates from. Restored via
// t.Cleanup for the same reason withRecordedKills is: a t.Fatalf inside the body
// must not leave the package's enumeration pointed at a test's closure for every
// later test in the binary.
func withIDMap(t *testing.T, m map[int]procIDs, body func()) {
	t.Helper()
	orig := readIDMap
	readIDMap = func() map[int]procIDs { return m }
	t.Cleanup(func() { readIDMap = orig })
	body()
}

// The third way a root can name the wrong processes, and the only one the two tests
// above cannot reach: root is reaped AND its pid reused in the window between
// KillTree's /proc gate and its enumeration. The gate passes, because at the instant
// it ran the pid really was the caller's process; the snapshot the answer is derived
// from is then a stranger's.
//
// This needs the readIDMap seam and cannot be written without it. On a live box the
// gate and the check read the same /proc microseconds apart and therefore always
// agree, so there is no fixture that makes the second one fire — which is exactly
// why it is worth a test rather than an argument: the check is unreachable by
// accident, so nothing else would notice it being deleted.
//
// The seam is also what makes this test discriminating by construction rather than by
// luck. Substituting readIDMap does not touch StartTicks, so against a live unchanged
// root the gate reads real /proc and passes whatever the map says. The check below it
// is therefore the ONLY thing downstream of a passing gate that can see the stranger,
// which is why the pids assertion is the load-bearing one here and does not depend on
// how the per-pid loop happens to behave on this fixture.
//
// Both legs substitute a map, differing only in root's start time. A single armed leg
// would be satisfied by a KillTree that refused every root, which is the failure this
// package has actually shipped before on darwin.
//
// One way the fixture is NOT faithful, measured rather than reasoned. In a real reuse
// the snapshot is read after the stranger exists, so ids[root].start is the stranger's
// own start and AGREES with a live /proc read, disagreeing only with KillTree's
// argument. Here it disagrees with both, because nothing seams StartTicks. The
// consequence is that deleting the check under test leaks one pid fewer than reality
// would: the per-pid loop happens to catch root on this fixture, so the observed leak
// was the sleep alone, where the real one takes root and its whole spliced-in session.
// That makes this test a valid detector — it demands zero signals, and one is a
// wrong-target SIGKILL either way — but not a demonstration of the full blast radius.
func TestKillTreeRefusesARootReusedInsideTheEnumerationWindow(t *testing.T) {
	root, _ := startPgroupTree(t, "sleep 300 & wait", 2)
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("premise: StartTicks(%d) failed on a live fixture", root)
	}
	snap := readIDMap()
	if _, ok := snap[root]; !ok {
		t.Fatalf("premise: root %d absent from the id map", root)
	}

	// The stranger. start+1 for the reason the first test in this file gives: a
	// recycled pid's process was started later, and start times only grow.
	stranger := make(map[int]procIDs, len(snap))
	maps.Copy(stranger, snap)
	rec := stranger[root]
	rec.start = start + 1
	stranger[root] = rec

	var pids []int
	var okRefuse bool
	signalled := withRecordedKills(t, func() {
		withIDMap(t, stranger, func() { pids, okRefuse = KillTree(root, start) })
	})
	if !okRefuse {
		t.Errorf("KillTree reported false for a root reused inside the window; a "+
			"refusal is still a successful kill of a tree that no longer exists (pids %v)", pids)
	}
	if len(pids) != 0 {
		t.Errorf("KillTree named %d pids for a reused root: %v", len(pids), pids)
	}
	if len(signalled) != 0 {
		t.Errorf("KillTree signalled %d pids for a reused root: %v", len(signalled), signalled)
	}

	// The differing leg: the same substitution mechanism, the same fixture, the live
	// start time. Without this half, a KillTree that returned early for every root
	// would pass everything above.
	var got []int
	accepted := withRecordedKills(t, func() {
		withIDMap(t, snap, func() { got, _ = KillTree(root, start) })
	})
	if len(got) < 2 || got[0] != root {
		t.Fatalf("KillTree with the live start time named %v, want root %d and at "+
			"least the sleep below it", got, root)
	}
	if len(accepted) != len(got) {
		t.Errorf("KillTree named %d pids but signalled %d: %v vs %v",
			len(got), len(accepted), got, accepted)
	}
}

// root absent from the snapshot, which is root fully reaped and therefore root's pid
// free to be reissued. The loop's permissive default -- a pid the snapshot does not
// know gets signalled -- is reachable for root and only root, because unionDescendants
// seeds its answer with root unconditionally and every other element it appends is a
// key of the map. So this is the one pid in the tree that could be signalled with no
// kill-time identity check, and it is the pid whose misidentification costs the most:
// KillTree's answer is root's parent closure UNION root's session.
//
// Not covered by TestKillTreeRefusesAReapedRoot, which reaches the same real-world
// event through the gate: there root is absent from /proc entirely, so StartTicks
// fails and KillTree returns before enumerating. Here the gate passes -- root was
// alive when it ran -- and the reap lands between the gate and the loop. That is the
// interleaving, and the SNAPSHOT is what makes it visible to the loop.
//
// This is the test that needs the startTicksOf seam, and the reason is that CALL ORDER
// is the interleaving: the gate's read must succeed and the loop's must return
// something else, which on real /proc cannot happen because both read the same field
// microseconds apart. The fixture therefore answers the gate with the real value and
// every later read with a moved one, which is what a reap plus a wrap onto root's
// number looks like from inside the function. It pins that KillTree reads root's start
// time twice with the enumeration in between; a rewrite that read it once would have
// to replace this fixture, which is the right amount of friction for a change to a
// TOCTOU guard.
//
// The sibling property -- that a pid the loop refuses is left out of the ANSWER and
// not merely unsignalled -- is asserted for descendants in killedset_test.go. It is
// asserted again here for root because root reaches the refusal down a different
// branch, and because root is the pid whose presence in a non-empty answer would
// suppress reapChildTree's fallback.
func TestKillTreeDoesNotSignalARootMissingFromTheSnapshot(t *testing.T) {
	root, _, snap, start := killTreeFixture(t)

	// Root reaped: its row is gone, while the sleep it fathered is still listed with
	// root's pid as its ppid, exactly as /proc reads in the instant between root's exit
	// and init reparenting the child.
	reaped := make(map[int]procIDs, len(snap))
	maps.Copy(reaped, snap)
	delete(reaped, root)
	if !setOf(unionDescendants(root, reaped))[root] {
		t.Fatalf("premise: root %d is not in the enumeration built from a snapshot that "+
			"omits it, so the permissive default this test is about is unreachable", root)
	}

	var reads int
	orig := startTicksOf
	startTicksOf = func(pid int) (uint64, bool) {
		if pid != root {
			return orig(pid)
		}
		reads++
		if reads == 1 {
			return start, true // the gate, before the reap
		}
		return start + 1, true // after the reap and a wrap onto root's number
	}
	t.Cleanup(func() { startTicksOf = orig })

	var answered []int
	var okKill bool
	signalled := withRecordedKills(t, func() {
		withIDMap(t, reaped, func() { answered, okKill = KillTree(root, start) })
	})
	if !okKill {
		t.Fatalf("KillTree reported false; a refusal is still a successful kill")
	}
	if reads < 2 {
		t.Fatalf("premise: KillTree read root's start time %d time(s), so the second read "+
			"this test is about did not happen and nothing below is discriminating", reads)
	}
	if setOf(signalled)[root] {
		t.Errorf("root %d was SIGKILLed on the strength of an integer: it is absent from "+
			"the snapshot, so it has been reaped, and its live start time no longer matches "+
			"the one it was created with. signalled %v", root, signalled)
	}
	// Stated as the requirement rather than as a narration of what happened: under a
	// KillTree whose root arm does not refuse, root is BOTH signalled and answered, so
	// "answered although unsignalled" would be a false sentence on the very run this
	// line exists to fail.
	if setOf(answered)[root] {
		t.Errorf("root %d is in KillTree's answer; the answer names the pids it signalled "+
			"and this root must not be among them, because a non-empty answer suppresses "+
			"reapChildTree's fallback: %v", root, answered)
	}
}

// The platform this package cannot run a test on, tested anyway. idMap's last
// fallback is psParentMap, which fills ppid and leaves start, pgid and sid at zero;
// that is what darwin uses when the bulk sysctl fails, and what platform_other
// always has.
//
// The identity check must therefore treat a zero start as "not known" and not as "a
// value that disagrees". Without that, every row mismatches, KillTree refuses every
// tree it is asked to kill, and it reports the refusal as a successful kill — a
// silent total leak, on the one configuration no leg of this suite executes. So the
// substituted map is the assertion: it is the fallback's exact shape.
func TestKillTreeKillsWhenTheSnapshotHasNoStartTimes(t *testing.T) {
	root, _ := startPgroupTree(t, "sleep 300 & wait", 2)
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("premise: StartTicks(%d) failed on a live fixture", root)
	}

	// ppid alone, exactly as psParentMap builds it.
	bare := map[int]procIDs{}
	for pid, rec := range readIDMap() {
		bare[pid] = procIDs{ppid: rec.ppid}
	}

	var got []int
	var okKill bool
	signalled := withRecordedKills(t, func() {
		withIDMap(t, bare, func() { got, okKill = KillTree(root, start) })
	})
	if !okKill {
		t.Fatalf("KillTree reported false against a startless snapshot")
	}
	// The parent closure only: a zero sid means root leads no session as far as the
	// snapshot is concerned, so the session term is skipped and the sleep is reached
	// through its ppid. That is the fallback's real answer, not a degraded one.
	if len(got) < 2 || got[0] != root {
		t.Errorf("KillTree named %v against a startless snapshot, want root %d and "+
			"the sleep below it", got, root)
	}
	if len(signalled) != len(got) {
		t.Errorf("KillTree named %d pids but signalled %d: %v vs %v",
			len(got), len(signalled), got, signalled)
	}
}
