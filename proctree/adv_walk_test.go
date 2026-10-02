//go:build linux

package proctree

// Tests added by the adversarial review of walk_test.go. Each one names the
// mutant it kills; each mutant survived the pre-existing suite.

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// statFieldsAfterComm splits /proc/<pid>/stat after the comm field, which may
// itself contain spaces and parentheses. Field 0 is state, 1 is ppid, 2 is
// pgrp, 3 is session.
func statFieldsAfterComm(stat []byte) []string {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(stat[i+1:]))
}

// impossiblePid is above PID_MAX_LIMIT, which is 4*1024*1024 = 2^22 on 64-bit
// Linux, so no pid can ever equal it.
const impossiblePid = 1 << 23

// requireChildrenLists asserts the kernel feature is present by probing /proc
// directly, and fails — never skips — when it is present but the package cannot
// use it. The skips in walk_test.go ask the code under test whether the feature
// exists, so a mutation that disables the walk turns those tests into skips
// instead of failures. This helper is the independent oracle.
func requireChildrenLists(t *testing.T) {
	t.Helper()
	if _, err := os.ReadFile("/proc/1/task/1/children"); err != nil {
		t.Skipf("kernel has no /proc/<pid>/task/<tid>/children: %v", err)
	}
	if _, ok := childrenOf(1); !ok {
		t.Fatal("/proc/1/task/1/children is readable but childrenOf(1) reported false: " +
			"the walk is disabled on a kernel that supports it")
	}
	if _, ok := fastDescendants(1); !ok {
		t.Fatal("/proc/1/task/1/children is readable but fastDescendants(1) reported false: " +
			"the walk is disabled on a kernel that supports it")
	}
}

// startPgroupTree starts a shell in its own session and process group and
// returns its pid. Cleanup kills the whole process group with a raw killpg,
// not with KillTree: cleaning up after the code under test using the code under
// test means a discovery regression leaks the fixtures it fails to find.
func startPgroupTree(t *testing.T, script string, want int) (int, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root := cmd.Process.Pid
	t.Cleanup(func() {
		// Enumerate with testUnion, and do it *before* the killpg. All three of
		// those matter. testUnion is a /proc walk written in this file, so a
		// regression in Descendants cannot make cleanup leak the same processes the
		// regression loses. It is the union and not the parent closure because a
		// setsid'd grandchild of an exited intermediate is off root's parent tree
		// entirely, and reachable only as a child of a session member. And the
		// order is forced: the moment root dies its session dissolves and even the
		// union cannot find that process. Killing the pgroup first left one escapee
		// alive per ~10 runs of the fixture — the shell usually, but not always,
		// finished exiting before a second enumeration.
		tree := testUnion(t, root)
		for _, pid := range tree {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		// Setsid made root a process group leader, so -root sweeps up anything
		// forked between the enumeration and now.
		_ = syscall.Kill(-root, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		assertPgroupGone(t, root)
		assertAllGone(t, root, tree)
	})
	deadline := time.Now().Add(10 * time.Second)
	for countPgroup(root) < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := countPgroup(root); n < want {
		t.Fatalf("tree never reached %d processes in pgroup %d, got %d", want, root, n)
	}
	return root, cmd
}

// countPgroup counts live processes whose process group is pgid, by reading
// /proc directly. It shares no code with Descendants, so it can witness a
// process Descendants fails to find.
func countPgroup(pgid int) int { return len(pgroupPids(pgid)) }

// pgroupPids is countPgroup's answer itemised, so a leak can name what leaked.
func pgroupPids(pgid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if g, ok := pgroupOf(pid); ok && g == pgid {
			out = append(out, pid)
		}
	}
	return out
}

// describePids renders pids as "<pid> <comm> <state>". A leak report that says
// only "1 process remains" cannot distinguish a fixture shell that outlived its
// cleanup from a zombie awaiting a reap, and those need different fixes.
func describePids(pids []int) []string {
	out := make([]string, 0, len(pids))
	for _, pid := range pids {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			out = append(out, strconv.Itoa(pid)+" (gone)")
			continue
		}
		comm := "?"
		if i, j := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')'); i >= 0 && j > i {
			comm = string(stat[i+1 : j])
		}
		state := "?"
		if f := statFieldsAfterComm(stat); len(f) > 0 {
			state = f[0]
		}
		out = append(out, strconv.Itoa(pid)+" "+comm+" "+state)
	}
	return out
}

// pgroupOf reads /proc/<pid>/stat's pgrp: fields after the last ')' are state,
// ppid, pgrp.
func pgroupOf(pid int) (int, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	f := statFieldsAfterComm(stat)
	if len(f) < 3 {
		return 0, false
	}
	g, err := strconv.Atoi(f[2])
	return g, err == nil
}

// testDescendants walks /proc's ppid links down from root. It duplicates what
// Descendants does, deliberately: a fixture that finds the processes it must
// clean up by calling the function under test leaks exactly what a discovery
// regression loses, which is when cleanup matters most. Unlike countPgroup it
// crosses process-group and session boundaries, so it sees a setsid'd escapee.
func testDescendants(root int) []int {
	parent := map[int]int{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return []int{root}
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		f := statFieldsAfterComm(stat)
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			parent[pid] = ppid
		}
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for pid, ppid := range parent {
			if ppid == out[i] && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	return out
}

// testUnion enumerates the fixture the way the oracle does, in this file's own
// code: the parent closure below root, plus the members of root's session when
// root leads it, plus the closure below each of those.
//
// Cleanup needs the union and not the closure, because the one process a cleanup
// is most likely to leak is the one the closure cannot see. A setsid'd grandchild
// whose intermediate parent has exited has been reparented off root's tree, so no
// parent link reaches it, and it has left root's group and session, so the killpg
// cannot reach it either. The only path to it is: member of root's session, then
// that member's children. deepOrphanScript builds exactly that process, and the
// test that uses it had to kill the escapee itself because this enumeration could
// not — a leak check scoped to a set that excludes the leak by construction.
//
// The sid(root) == root guard is load-bearing here in a way it is not in the
// oracle, because these pids get SIGKILL: a root that does not lead its session
// sits in the TEST BINARY's session, so seeding by session would enumerate the
// test binary, `go test` and the shell that started it. When the guard declines
// this degrades to testDescendants' answer, which is all the pgroup-only fixture
// needs — orphanScript never calls setsid, so its whole tree is in root's group.
func testUnion(t *testing.T, root int) []int {
	t.Helper()
	if root <= 1 {
		t.Errorf("testUnion refusing root %d: not a fixture pid", root)
		return nil
	}
	type ent struct{ ppid, sid int }
	all := map[int]ent{}
	dirents, err := os.ReadDir("/proc")
	if err != nil {
		return []int{root}
	}
	for _, e := range dirents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		f := statFieldsAfterComm(stat)
		if len(f) < 4 {
			continue
		}
		ppid, errP := strconv.Atoi(f[1])
		sid, errS := strconv.Atoi(f[3])
		if errP != nil || errS != nil {
			continue
		}
		all[pid] = ent{ppid: ppid, sid: sid}
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	if all[root].sid == root {
		ourSid := all[os.Getpid()].sid
		for pid, e := range all {
			if e.sid != root || seen[pid] {
				continue
			}
			if pid <= 1 || (ourSid != 0 && e.sid == ourSid) {
				// Unreachable unless a read above is wrong: root's session was
				// created by Setsid at fork, so it cannot also be ours. Say so
				// rather than SIGKILL our own session.
				t.Errorf("testUnion refusing pid %d claiming root %d's session: sid %d, our sid %d",
					pid, root, e.sid, ourSid)
				continue
			}
			seen[pid] = true
			out = append(out, pid)
		}
	}
	// The closure below every seed. A child of a fixture process is a fixture
	// process, so following children can never leave the tree even though the
	// session seeds above already have.
	for i := 0; i < len(out); i++ {
		for pid, e := range all {
			if e.ppid == out[i] && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	return out
}

// liveNotZombie reports whether pid is running. A zombie is excluded: a fixture
// child whose parent has just been killed is briefly a zombie, and /proc still
// carries its directory, so an existence check alone flakes.
func liveNotZombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	f := statFieldsAfterComm(stat)
	return len(f) > 0 && f[0] != "Z"
}

// assertAllGone is the leak check for descendants outside root's process group,
// which assertPgroupGone cannot see. root itself is skipped, and that skip is
// belt rather than a bound: every caller has already reaped root with
// cmd.Process.Wait, so /proc holds no entry for it and liveNotZombie(root) is
// false regardless. A root that somehow outlived its own Wait is still in its
// process group, so assertPgroupGone is the check that would name it.
func assertAllGone(t *testing.T, root int, tree []int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive []int
		for _, pid := range tree {
			if pid == root {
				continue
			}
			if liveNotZombie(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("LEAK: %d fixture processes survived cleanup: %v", len(alive), alive)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertPgroupGone requires the group to be EMPTY, and the exact bound is the
// point. It used to allow one survivor, for the zombie root that is still in the
// group until it is reaped — but every caller reaps root with cmd.Process.Wait
// immediately before calling this, so by the time the count is taken the zombie
// has left /proc and the allowance is spent slack. What it bought instead was
// silence for exactly one leaked fixture process, which is the common case: the
// fixtures here are a shell plus one or two sleeps, so "one survivor" is a whole
// escapee, not a rounding error. Any caller that has NOT reaped root must reap it
// before calling, not widen this bound.
func assertPgroupGone(t *testing.T, pgid int) {
	t.Helper()
	if left := waitPgroupEmpty(pgid, 5*time.Second); len(left) > 0 {
		t.Errorf("LEAK: %d processes remain in pgroup %d after cleanup: %v",
			len(left), pgid, describePids(left))
	}
}

// waitPgroupEmpty waits for pgid to hold nothing and returns whatever is still
// there when it gives up. The bound lives here, in a function that RETURNS its
// answer, rather than inline in assertPgroupGone: a helper whose only output is
// t.Errorf can be checked by no test, and this bound was wrong for as long as it
// existed. TestWaitPgroupEmptyReportsASingleSurvivor is the check.
//
// The two returns are deliberately separate, and a first version of this function
// combined them — `if len(left) == 0 || time.Now().After(deadline) { return left }`
// — which is the same behaviour and untestable. Restoring the old allowance there
// (`<= 1`) changes only WHEN the loop gives up, never what it hands back, so it
// returns the one survivor either way and the witness test stays green. Measured,
// not reasoned: that mutant survived. Below, the allowance returns nil instead.
//
// What this still does not reach: assertPgroupGone's own `len(left) > 0`. Widening
// that to `> 1` restores the defect and survives, because nothing can observe an
// Errorf that was not made. It is one comparison in one caller, next to the
// comment that says why it is 0.
func waitPgroupEmpty(pgid int, within time.Duration) []int {
	deadline := time.Now().Add(within)
	for {
		left := pgroupPids(pgid)
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWaitPgroupEmptyReportsASingleSurvivor is the witness for the bound every
// fixture's cleanup leak check rests on. assertPgroupGone allowed one survivor,
// nominally for the zombie root — which each caller has already reaped by then —
// so in practice the allowance covered exactly one leaked process, and the suite
// could not tell: the fixtures leave the group empty when cleanup works, so no
// existing test distinguishes "0 allowed" from "1 allowed".
//
// One process, deliberately: the count that used to be excused.
func TestWaitPgroupEmptyReportsASingleSurvivor(t *testing.T) {
	// exec, so the group holds the sleep itself and nothing else; Setsid makes it
	// the leader of a fresh group whose pgid no other process can be in.
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for countPgroup(pgid) < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// A short `within`: this arm is the timeout path, so the whole point is that it
	// gives up and names what it found.
	left := waitPgroupEmpty(pgid, 200*time.Millisecond)
	if len(left) != 1 || left[0] != pgid {
		t.Fatalf("waitPgroupEmpty on a group holding one process = %v (%v), want exactly [%d]",
			left, describePids(left), pgid)
	}

	// And the empty case, against the same group, because "it returned something"
	// is not evidence on its own: a waitPgroupEmpty that returned the group's
	// members unconditionally would pass the block above and make every cleanup
	// leak check a permanent failure.
	//
	// The kill lands from a goroutine partway through the call, so the group is
	// occupied when the wait starts and empty well before its deadline. That
	// ordering is what makes this arm cover the WAIT rather than one final read: a
	// first version killed and reaped first, found an already-empty group, and a
	// mutant that returned the group's members immediately without ever waiting
	// survived it. Measured, not reasoned.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	if left := waitPgroupEmpty(pgid, 5*time.Second); len(left) != 0 {
		t.Errorf("waitPgroupEmpty on a group emptied during the call = %v (%v), want none: "+
			"it read the group once instead of waiting", left, describePids(left))
	}
	<-done
	killed = true
}

// chainScript builds a tree of exactly one child per level, depth levels deep.
const chainScript = `
chain() {
  d=$1
  if [ "$d" -le 0 ]; then sleep 300 & wait; return; fi
  ( chain $((d-1)) ) &
  wait
}
chain 6
`

// TestFastDescendantsWalksASingleChildChain kills "M-V": reading
// childrenOf(out[i-1]) instead of childrenOf(out[i]). That lag is invisible on
// the bushy, uniform-depth tree in TestFastDescendantsMatchesSnapshot, because
// the lag is absorbed as long as out keeps growing and the last pid discovered
// is a leaf. On a chain, out grows by one per level, so the walk terminates at
// depth 1 and loses the entire tail.
func TestFastDescendantsWalksASingleChildChain(t *testing.T) {
	requireChildrenLists(t)
	root, _ := startPgroupTree(t, chainScript, 8)

	walk, ok := fastDescendants(root)
	if !ok {
		t.Fatal("fastDescendants declined on a live root")
	}
	snap := snapshotDescendants(root)
	if missing := diff(setOf(snap), setOf(walk)); len(missing) > 0 {
		t.Errorf("walk missed %d pids of the chain that the snapshot found: %v", len(missing), missing)
	}
	// The chain is 6 recursions plus the root shell plus the final sleep: at
	// least 8 nodes, each the only child of the one above it.
	if len(walk) < 8 {
		t.Errorf("walk found %d pids on a chain of at least 8: %v", len(walk), walk)
	}
	if len(snap) < 8 {
		t.Fatalf("premise failed: the fixture is not a chain of 8, snapshot found %d: %v", len(snap), snap)
	}
}

// TestDescendantsIsBreadthFirst kills "M-I": replacing the FIFO frontier with a
// LIFO one. Depth-first returns the same set, so every set-comparison test in
// the suite passes; only the documented order changes. The assertion is that
// depth is non-decreasing along the result, which is order-agnostic between
// siblings and so is not flaky.
func TestDescendantsIsBreadthFirst(t *testing.T) {
	requireChildrenLists(t)
	// TWO deep branches, not one deep and one shallow. With a single deep
	// branch plus a leaf sibling, depth-first happens to visit the leaf first
	// and its order is monotone too, so the assertion below cannot see the
	// difference — that fixture let the DFS mutant survive. With two branches
	// of equal depth, depth-first must return from the bottom of one to the top
	// of the other whichever it descends first, and the violation is forced.
	root, _ := startPgroupTree(t, `
chain() {
  d=$1
  if [ "$d" -le 0 ]; then sleep 300 & wait; return; fi
  ( chain $((d-1)) ) &
  wait
}
( chain 3 ) &
( chain 3 ) &
wait`, 10)

	got := Descendants(root)
	parents := parentMap()
	depth := func(pid int) int {
		d := 0
		for p := pid; p != root; d++ {
			pp, ok := parents[p]
			if !ok || d > 64 {
				return -1
			}
			p = pp
		}
		return d
	}
	last := 0
	for i, pid := range got {
		d := depth(pid)
		if d < 0 {
			continue // raced away; its depth is unknowable now
		}
		if d < last {
			t.Errorf("Descendants is not breadth-first: %v; pid %d at index %d has depth %d after depth %d",
				got, pid, i, d, last)
			break
		}
		last = d
	}
	if last < 2 {
		t.Fatalf("premise failed: fixture is not deep enough to distinguish BFS from DFS, max depth %d in %v", last, got)
	}
}

// TestDescendantsDoesNotUseTheFastPath is this test with its sign reversed, and
// the reversal is the decision it now pins: the children-list walk skips live
// pids (see fastDescendants' comment) and the 12 ms it saves is paid once per
// window close, so it must not be on the path a kill takes. It was
// TestDescendantsUsesTheFastPath, asserting the opposite, while the walk was
// still a candidate.
//
// The discriminator is unchanged, a non-leader thread id. ReadDir("/proc") lists
// only thread-group leaders, so procIDMap has no entry for a tid and the parent
// closure of a tid is just [tid]. /proc/<tid>/task, in contrast, exists and lists
// the whole thread group, so fastDescendants(tid) returns the process's children.
// The two answers differ in length, which says which path ran — so this kills
// "Descendants calls fastDescendants again", the one-line regression that would
// otherwise be invisible because both paths agree on every ordinary tree.
func TestDescendantsDoesNotUseTheFastPath(t *testing.T) {
	requireChildrenLists(t)

	// A tid to discriminate with has to come from a thread that is not the group
	// leader, so the forkers must be on a thread each: at most one of those can be
	// the leader. Without the barrier forkedFromLockedThreads holds them on, they
	// can all be handed the same thread, and at GOMAXPROCS=1 they were -- 5 runs in
	// 12 found no non-leader tid and died on the premise below.
	const forkers = 8
	nonLeader := 0
	for _, k := range forkedFromLockedThreads(t, forkers) {
		if k.tid != os.Getpid() {
			nonLeader = k.tid
		}
	}
	if nonLeader == 0 {
		t.Fatalf("all %d forks landed on the thread-group leader; no tid available to discriminate", forkers)
	}
	// The premises, both halves: the /proc-listing paths cannot see a tid at all,
	// and the walk can. Without the second one the assertion below would pass on a
	// kernel that had simply stopped exposing the thread group.
	if snap := snapshotDescendants(nonLeader); len(snap) != 1 {
		t.Fatalf("premise failed: snapshotDescendants(tid %d) = %v, want just [tid]", nonLeader, snap)
	}
	walk, ok := fastDescendants(nonLeader)
	if !ok || len(walk) == 1 {
		t.Fatalf("premise failed: fastDescendants(tid %d) = %v, %v; the discriminator needs the walk to "+
			"see the thread group", nonLeader, walk, ok)
	}
	if got := Descendants(nonLeader); len(got) != 1 || got[0] != nonLeader {
		t.Errorf("Descendants(tid %d) = %v, want just [tid]: it took the children-list walk, which loses "+
			"live pids and must not be on a kill path", nonLeader, got)
	}
}

// TestDescendantsFallsBackWhenTheWalkDeclines kills "M-S" (Descendants returns
// the walk's nil instead of snapshotting) and "M-T" (Descendants uses the
// walk's result even when it reported false). On this box the walk always
// succeeds, so no test in walk_test.go ever observes the fallback being taken;
// it only checks that fastDescendants reports false, not what Descendants then
// does. KillTree returning an empty set would silently kill nothing.
func TestDescendantsFallsBackWhenTheWalkDeclines(t *testing.T) {
	if _, ok := fastDescendants(impossiblePid); ok {
		t.Fatal("premise failed: fastDescendants(impossible pid) reported true")
	}
	got := Descendants(impossiblePid)
	if len(got) != 1 || got[0] != impossiblePid {
		t.Errorf("Descendants(%d) = %v; want exactly [%d] from the snapshot fallback",
			impossiblePid, got, impossiblePid)
	}
}

// TestKillTreeKillsRootItself kills "M-AB": skipping root in KillTree's kill
// loop. TestKillTreeReapsGrandchildren explicitly `continue`s past root when it
// counts survivors, and TestKillTreeReapsSetsidEscapee only looks at the
// escapee, so no existing test asserts that root dies. Root is our direct
// child, so /proc/<root> outlives the kill as a zombie; the wait status is the
// only honest witness.
func TestKillTreeKillsRootItself(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		_ = syscall.Kill(-root, syscall.SIGKILL)
		if !reaped {
			_, _ = cmd.Process.Wait()
		}
		assertPgroupGone(t, root)
	})
	deadline := time.Now().Add(10 * time.Second)
	for countPgroup(root) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := countPgroup(root); n < 2 {
		t.Fatalf("fixture never reached 2 processes, got %d", n)
	}

	killed, ok := killTreeNow(t, root)
	if !ok {
		t.Fatal("KillTree reported unsupported on linux")
	}
	if !setOf(killed)[root] {
		t.Fatalf("root %d absent from the killed set %v", root, killed)
	}
	err := cmd.Wait()
	reaped = true
	ee, isExit := err.(*exec.ExitError)
	if !isExit {
		t.Fatalf("root %d exited with err %v; want a signal", root, err)
	}
	ws, okWS := ee.Sys().(syscall.WaitStatus)
	if !okWS || !ws.Signaled() {
		t.Fatalf("root %d wait status %v; want Signaled", root, ee.Sys())
	}
	if ws.Signal() != syscall.SIGKILL {
		t.Errorf("root %d died of %v; KillTree must SIGKILL it", root, ws.Signal())
	}
}

// TestKillTreeDefeatsASigtermTrap kills "M-AD": sending SIGTERM instead of
// SIGKILL. Every fixture in the suite is `sh` or `sleep`, both of which die on
// SIGTERM, so the whole suite is blind to the difference — yet SIGKILL is the
// package's entire promise: a process must not be able to decline. The
// descendant here ignores SIGTERM and restarts its sleep, so it outlives a
// SIGTERM sweep.
func TestKillTreeDefeatsASigtermTrap(t *testing.T) {
	root, cmd := startPgroupTree(t,
		`sh -c "trap '' TERM; while :; do sleep 300; done" & wait`, 3)

	var stubborn int
	for _, pid := range Descendants(root) {
		if pid == root {
			continue
		}
		if c, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil &&
			string(c) == "sh\n" {
			stubborn = pid
			break
		}
	}
	if stubborn == 0 {
		t.Fatalf("no TERM-ignoring shell among %v; the test cannot see the defect it covers",
			Descendants(root))
	}
	// The premise the assertion rests on: this process really does ignore TERM.
	if err := syscall.Kill(stubborn, syscall.SIGTERM); err != nil {
		t.Fatalf("probe SIGTERM to %d: %v", stubborn, err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat("/proc/" + strconv.Itoa(stubborn)); err != nil {
		t.Skipf("premise failed: shell %d died of the probe SIGTERM, so the trap did not take", stubborn)
	}

	killed, ok := killTreeNow(t, root)
	if !ok {
		t.Fatal("KillTree reported unsupported on linux")
	}
	// Asserted BEFORE the Wait below, and the ordering is the whole point. root is
	// `sh -c '... & wait'`, which never exits on its own, so a KillTree that named
	// nothing at all leaves this test blocked in Wait until the package's own timeout
	// panics — and a panic prints no `--- FAIL:` line, so a mutation driver reads the
	// hang as "no test went red" and reports the mutant as caught by whatever
	// finished first. Found exactly that way: an off-by-one in KillTree's start-time
	// comparison reddened one kill test and hung this one.
	if !setOf(killed)[stubborn] {
		t.Fatalf("TERM-ignoring descendant %d absent from the killed set %v", stubborn, killed)
	}
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := os.ReadFile("/proc/" + strconv.Itoa(stubborn) + "/stat")
		if err != nil {
			return // gone
		}
		if f := statFieldsAfterComm(st); len(f) > 0 && f[0] == "Z" {
			return // dead, awaiting a reap by its reparented ancestor
		}
		if time.Now().After(deadline) {
			t.Fatalf("TERM-ignoring descendant %d survived KillTree; it must SIGKILL, not SIGTERM", stubborn)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
