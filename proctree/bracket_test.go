//go:build linux

package proctree

import "testing"

// TestFastDescendantsMatchesSnapshotBracketed replaces
// TestFastDescendantsMatchesSnapshot's single-snapshot comparison. The original
// reads the walk, then the snapshot, and reports anything that arrived in
// between as "walk missed N pids the snapshot found" — a ~15 ms window (measured:
// walk 0.05 ms, snapshot 14.6-16.3 ms), which is one failure in ~430 runs and,
// worse, an assertion that cannot tell a lost subtree from a concurrent fork.
//
// Bracketing applies the same before/after discipline the cgroup experiment
// used: a pid present in both snapshots was there for the whole of the walk, so
// the walk owes it; a pid in neither was never there, so the walk must not
// invent it. Pids that appear in exactly one snapshot arrived or left during the
// window and are excluded — the only pids the comparison cannot rule on.
func TestFastDescendantsMatchesSnapshotBracketed(t *testing.T) {
	requireChildrenLists(t)
	root, _ := startPgroupTree(t, `
lvl() {
  d=$1
  [ "$d" -le 0 ] && { sleep 300; return; }
  i=0
  while [ $i -lt 2 ]; do ( lvl $((d-1)) ) & i=$((i+1)); done
  sleep 300
}
lvl 3 &
setsid sleep 300 &
wait`, 8)

	before := setOf(snapshotDescendants(root))
	walk, ok := fastDescendants(root)
	if !ok {
		t.Fatal("fastDescendants declined on a live root")
	}
	after := setOf(snapshotDescendants(root))
	w := setOf(walk)

	// Stable: in both snapshots. The walk must contain every one.
	stable := map[int]bool{}
	for pid := range before {
		if after[pid] {
			stable[pid] = true
		}
	}
	if missing := diff(stable, w); len(missing) > 0 {
		t.Errorf("walk missed %d pids present before AND after it ran: %v", len(missing), missing)
	}
	// Either snapshot is an upper bound: a pid in neither never existed.
	union := map[int]bool{}
	for pid := range before {
		union[pid] = true
	}
	for pid := range after {
		union[pid] = true
	}
	if extra := diff(w, union); len(extra) > 0 {
		t.Errorf("walk invented %d pids absent from both snapshots: %v", len(extra), extra)
	}
	if len(stable) < 8 {
		t.Fatalf("premise failed: only %d pids were stable across the walk; too small to "+
			"exercise it, and a stopped-early walk could hide", len(stable))
	}
	if walk[0] != root {
		t.Errorf("walk must lead with root %d, got %d", root, walk[0])
	}
}
