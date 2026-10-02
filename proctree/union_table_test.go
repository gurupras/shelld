package proctree

import (
	"os"
	"slices"
	"testing"
)

// The other tests of unionDescendants spawn real processes and read the ids back
// from /proc, which is what makes them worth having and also what pins them to
// linux. The safety argument in unionDescendants' comment is about the function's
// treatment of an id map, though, so it can be stated against a map the test
// writes -- which runs on every platform, including the two where this package
// otherwise reports no test files at all.
//
// These are not a substitute for the live fixtures. They are a regression guard
// on the documented rules: root must be positive, the session term applies only
// when root leads, there is no process-group term, and the second expansion
// exists so a session member's own children are not lost.

// idRow is one process's ids, in the order they read in a table.
type idRow struct{ pid, ppid, pgid, sid int }

func idMapOf(rows ...idRow) map[int]procIDs {
	m := make(map[int]procIDs, len(rows))
	for _, r := range rows {
		m[r.pid] = procIDs{ppid: r.ppid, pgid: r.pgid, sid: r.sid}
	}
	return m
}

func unionOn(t *testing.T, root int, rows ...idRow) []int {
	t.Helper()
	return unionDescendants(root, idMapOf(rows...))
}

func wantUnion(t *testing.T, got []int, want ...int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("unionDescendants = %v, want %v", got, want)
	}
}

// A non-positive root is refused rather than answered. The map here is the one
// that makes root == 0 dangerous: pid 1's ppid is 0, so children[0] holds init and
// the closure would be every process in the map.
//
// guard_test.go's TestUnionDescendantsRefusesANonPositiveRootOnAFabricatedTree
// makes the same assertion and is the fuller one, but it is linux-gated along with
// the rest of that file; this is the portable copy, so the guard is covered on the
// platforms where the package would otherwise run nothing.
func TestUnionRefusesANonPositiveRoot(t *testing.T) {
	rows := []idRow{
		{pid: 1, ppid: 0, sid: 1},
		{pid: 2, ppid: 1, sid: 1},
		{pid: 3, ppid: 2, sid: 1},
	}
	for _, root := range []int{0, -1, -1198699} {
		if got := unionOn(t, root, rows...); len(got) != 0 {
			t.Fatalf("root %d: unionDescendants = %v, want empty", root, got)
		}
	}
	// The same map still answers for a positive root, so the guard is a guard and
	// not a way of returning empty for everything.
	wantUnion(t, unionOn(t, 1, rows...), 1, 2, 3)
}

// An empty map is the Windows idMap, and the answer there is root alone. Asserted
// here rather than only in the platform file because the emptiness, not the
// platform, is what produces it.
func TestUnionOfAnEmptyIDMapIsRootAlone(t *testing.T) {
	wantUnion(t, unionDescendants(42, map[int]procIDs{}), 42)
}

// root need not have a row of its own: it may have been reaped between the
// snapshot and the question. Its children are still keyed by its pid, so the
// closure holds them, and the session term is skipped because there is no sid to
// compare.
func TestUnionWithNoRowForRootIsTheClosureAlone(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 200, ppid: 100, sid: 100},
		idRow{pid: 300, ppid: 1, sid: 100}, // in root's session, but root has no row
	)
	wantUnion(t, got, 100, 200)
}

// When root leads no session the session term is skipped entirely, because
// `sid == root` would then name processes root never forked. An orphan of root is
// out of reach in that configuration, and the comment says so rather than
// papering over it.
func TestUnionIsTheParentClosureWhenRootLeadsNoSession(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 10, sid: 50}, // root, led by someone else
		idRow{pid: 200, ppid: 100, sid: 50},
		idRow{pid: 300, ppid: 1, sid: 100}, // orphan whose sid is root's pid
	)
	wantUnion(t, got, 100, 200)
}

// The case the union exists for: an orphan's parent link points at init, and the
// sid that survived the reparent is root's own pid.
func TestUnionAddsASessionOrphanWhenRootLeads(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 10, sid: 100},
		idRow{pid: 200, ppid: 100, sid: 100},
		idRow{pid: 300, ppid: 1, sid: 100},
	)
	wantUnion(t, got, 100, 200, 300)
}

// Why expand is called a second time: a session member's child may have left the
// session, so it is in neither the closure nor the session set, and is reachable
// only from the member that was spliced on.
func TestUnionReachesAnOrphansChildThatLeftTheSession(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 10, sid: 100},
		idRow{pid: 300, ppid: 1, sid: 100},   // orphan, still in root's session
		idRow{pid: 400, ppid: 300, sid: 400}, // its child, which called setsid
	)
	wantUnion(t, got, 100, 300, 400)
}

// The limit of that second expand, and the pinned twin of the test above: the
// rescue works, and it needs the intermediate to be in the map. Take 300 out and
// 400 is in neither term — not in the session set, because setsid moved it out of
// root's session, and not in the parent closure, because the only row that linked
// it to root is gone.
//
// This is the residual proctree.go documents in the paragraph beginning "The other
// residual, unchanged", and the trigger for it is the daemonize pattern, so it is
// how any background server started in a window looks. The assertion is TODAY's
// answer on purpose, not the desired one: a test written red for the behaviour we
// want gets skipped or deleted, while this one sits green and makes whoever lands
// the cgroup membership set edit the row to say so.
func TestUnionMissesASetsidDescendantWhoseIntermediateExited(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 10, sid: 100},
		idRow{pid: 400, ppid: 1, sid: 400}, // setsid'd, and the intermediate has since exited
	)
	wantUnion(t, got, 100)
}

// There is no process-GROUP term, and this is the property that rules out the
// unsafe version: a pid whose pgid is root's but which root never forked must not
// appear, however root's own session is configured.
func TestUnionIgnoresProcessGroupMembership(t *testing.T) {
	for _, tc := range []struct {
		name string
		root idRow
	}{
		{"root leads its session", idRow{pid: 100, ppid: 10, pgid: 100, sid: 100}},
		{"root leads no session", idRow{pid: 100, ppid: 10, pgid: 100, sid: 50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unionOn(t, 100,
				tc.root,
				idRow{pid: 500, ppid: 999, pgid: 100, sid: 777}, // root's group, a stranger's tree
			)
			wantUnion(t, got, 100)
		})
	}
}

// Session membership is the only id term, so a process outside root's session is
// excluded even when it is parentless and looks like an orphan.
func TestUnionExcludesAnOrphanOutsideRootsSession(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 10, sid: 100},
		idRow{pid: 300, ppid: 1, sid: 999},
	)
	wantUnion(t, got, 100)
}

// Sibling order is sorted rather than left to map iteration, so a caller sees the
// same list twice for the same tree. Run repeatedly because map iteration order
// is randomised per range, not per process.
func TestUnionIsOrderedAndRepeatable(t *testing.T) {
	rows := []idRow{
		{pid: 100, ppid: 10, sid: 100},
		{pid: 400, ppid: 100, sid: 100},
		{pid: 200, ppid: 100, sid: 100},
		{pid: 300, ppid: 100, sid: 100},
		{pid: 900, ppid: 1, sid: 100}, // session extras are sorted too
		{pid: 700, ppid: 1, sid: 100},
	}
	want := []int{100, 200, 300, 400, 700, 900}
	for i := 0; i < 32; i++ {
		got := unionOn(t, 100, rows...)
		if !slices.Equal(got, want) {
			t.Fatalf("iteration %d: unionDescendants = %v, want %v", i, got, want)
		}
	}
}

// A cycle in the parent links -- which a reused pid can fabricate in a snapshot
// taken across two reads -- must not hang or duplicate. seen is what prevents it.
func TestUnionTerminatesOnACycleInParentLinks(t *testing.T) {
	got := unionOn(t, 100,
		idRow{pid: 100, ppid: 200, sid: 100},
		idRow{pid: 200, ppid: 100, sid: 100},
	)
	wantUnion(t, got, 100, 200)
}

// StartTimeSupported's whole job is to be the answer StartTicks' (0, false) cannot
// give, so the one thing it must never do is disagree with StartTicks about this
// build. A reader that works while the predicate says it cannot is the direction
// that matters: it silences the spawn site's warning on a platform where a failed
// read IS a real failure, which is the defect this predicate was added to fix,
// pointed the other way.
//
// Asserted on the caller's own pid because that is the one process guaranteed to be
// alive and readable while the test runs, so a false from StartTicks here cannot be
// a dead or foreign pid and is therefore a statement about the platform. In this
// file rather than beside the live fixtures because the platforms it discriminates
// -- darwin, and whatever unix takes the stub -- are exactly the ones those
// fixtures' build tags exclude.
func TestStartTimeSupportedAgreesWithStartTicks(t *testing.T) {
	_, ok := StartTicks(os.Getpid())
	if ok && !StartTimeSupported() {
		t.Error("StartTicks read this process's start time while StartTimeSupported reports " +
			"the build cannot: the spawn site would treat a real failed read as a platform " +
			"that never had the field")
	}
	// The converse is deliberately not asserted: on a platform that HAS a reader a
	// single read may still fail -- that is the case the warning exists for -- so
	// supported-and-!ok is legal, and only ok-and-!supported is a contradiction.
}
