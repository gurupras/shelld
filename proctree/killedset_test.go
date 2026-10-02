//go:build linux

package proctree

import (
	"maps"
	"slices"
	"testing"
)

// KillTree's answer must name only the pids it actually signalled. The doc promises
// "the targeted pids", and reapChildTree (window.go) keys on that promise rather than
// on the bool: its fallback to pty.PTY.Kill fires on an EMPTY answer, so a pid
// reported without having been signalled suppresses the fallback on the strength of a
// pid the loop had just refused to touch. That is the one shape KillTree's own doc
// rules out when it says a refusal is not silently a no-op.
//
// Before the per-pid identity check existed the two sets were identical by
// construction — every enumerated pid was signalled — so nothing had to assert it and
// nothing did. Adding a `continue` to that loop is what made them separable, and the
// first version of it returned the enumeration rather than the signalled set. The
// defect is therefore invisible to every test that looks at one set or the other: it
// is only visible in the RELATION, which is why this asserts equality rather than
// membership.
//
// Two SUBTESTS, not two legs in one function, and not for tidiness. withIDMap restores
// through t.Cleanup, which runs when the test ends rather than when its body returns —
// so a second leg in the same function would still be reading the first leg's
// substituted map, and its fixture would look absent from the snapshot. A subtest per
// leg is what scopes the substitution. Each leg also needs its own tree, because the
// armed leg SIGKILLs everything it does not skip.
//
// The control leg is not optional. Without it, a KillTree that skipped every pid would
// satisfy the armed leg's equality assertion with two empty slices, and so would one
// that signalled nothing at all. The control pins that the same fixture, untampered,
// both signals and reports the very pid the armed leg expects to see withheld.
func TestKillTreeAnswersOnlyThePidsItSignalled(t *testing.T) {
	// One descendant's recorded start disagrees with its live one, which is what a pid
	// reissued during the walk looks like from inside the loop.
	t.Run("a skipped pid is left out of the answer", func(t *testing.T) {
		root, victim, snap, start := killTreeFixture(t)
		tampered := make(map[int]procIDs, len(snap))
		maps.Copy(tampered, snap)
		rec := tampered[victim]
		rec.start++ // +1 for the reason the other tests here give: start times only grow.
		tampered[victim] = rec

		// The premise the rest rests on: the victim is in the set the loop walks. A
		// victim the enumeration never names would make every assertion below pass
		// without the skip ever happening.
		if !setOf(unionDescendants(root, tampered))[victim] {
			t.Fatalf("premise: the enumeration does not name victim %d, so nothing would skip it", victim)
		}

		var answered []int
		var okKill bool
		signalled := withRecordedKills(t, func() {
			withIDMap(t, tampered, func() { answered, okKill = KillTree(root, start) })
		})
		if !okKill {
			t.Fatalf("KillTree reported false on a live root: %v", answered)
		}
		if setOf(signalled)[victim] {
			t.Errorf("KillTree signalled %d, whose recorded start disagreed with its live one: %v",
				victim, signalled)
		}
		if !slices.Equal(answered, signalled) {
			t.Errorf("KillTree answered %v but signalled %v; the answer must name only the "+
				"pids it signalled, because reapChildTree's fallback fires on an empty "+
				"answer and a pid reported unsignalled suppresses it", answered, signalled)
		}
		// Not vacuous: the tree really was killed, so the equality above is between two
		// non-empty sets rather than between two empty ones.
		if !setOf(answered)[root] {
			t.Fatalf("KillTree answered %v, which omits root %d: the fixture was not killed "+
				"at all, so the equality above proves nothing", answered, root)
		}
	})

	// The same mechanism and the same fixture shape, with the snapshot left alone.
	t.Run("an untampered snapshot withholds nothing", func(t *testing.T) {
		root, victim, snap, start := killTreeFixture(t)
		var answered []int
		signalled := withRecordedKills(t, func() {
			withIDMap(t, snap, func() { answered, _ = KillTree(root, start) })
		})
		if !setOf(signalled)[victim] {
			t.Errorf("with the snapshot untampered KillTree did not signal %d: %v. The armed "+
				"leg's withholding is then not attributable to the start-time mismatch",
				victim, signalled)
		}
		if !slices.Equal(answered, signalled) {
			t.Errorf("untampered: KillTree answered %v but signalled %v", answered, signalled)
		}
	})
}

// killTreeFixture starts a session-leading tree and returns root, a descendant of root
// to tamper with, the live id map, and root's live start time. The descendant is chosen
// here rather than by each caller so that both legs pick it the same way.
func killTreeFixture(t *testing.T) (root, victim int, snap map[int]procIDs, start uint64) {
	t.Helper()
	root, _ = startPgroupTree(t, "sleep 300 & wait", 2)
	start, ok := StartTicks(root)
	if !ok {
		t.Fatalf("premise: StartTicks(%d) failed on a live fixture", root)
	}
	snap = readIDMap()
	if _, ok := snap[root]; !ok {
		t.Fatalf("premise: root %d absent from the id map", root)
	}
	// Not root: root's identity is owned by the gate and by the snapshot check above
	// the loop, so tampering with it returns early and never reaches the loop at all.
	for _, pid := range unionDescendants(root, snap) {
		if pid != root {
			victim = pid
			break
		}
	}
	if victim == 0 {
		t.Fatalf("premise: the fixture named no descendant below root %d", root)
	}
	// The live read the loop makes has to succeed, or the loop skips nothing and the
	// armed leg would pass for the wrong reason.
	if _, ok := StartTicks(victim); !ok {
		t.Fatalf("premise: StartTicks(%d) failed for a live descendant", victim)
	}
	return root, victim, snap, start
}
