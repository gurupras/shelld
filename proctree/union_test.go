//go:build linux

package proctree

// Tests for the union oracle: the parent-link closure plus the members of root's
// session, the latter only when root leads that session. Each fixture is a shape
// that one part of the oracle is the only way to reach, so dropping a part shows
// up as a specific test going red rather than as a general loss of coverage.
// That per-mutant red set was recorded by a driver script kept outside this
// repo, so a clone cannot re-run it; what a clone can check is the property the
// script relied on, which is that each fixture below names the part it reaches.
// Two of them are safety fixtures rather than reach fixtures — a wider
// oracle has to fail them, because the failure mode here is SIGKILLing a process
// shelld never started.

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/pty"
)

// orphanScript is the F3 shape, and it needs no race. The subshell forks
// `sleep 300` into the background and exits; the root shell reaps the subshell;
// forget_original_parent has already spliced the sleep onto the nearest reaper,
// which is `systemd --user` here because nothing sets PR_SET_CHILD_SUBREAPER. So
// the sleep is alive, it is not a child of anything in root's tree, and no
// parent-link walk can reach it. Its pgid and sid are still root's.
const orphanScript = `( sleep 300 & ) ; sleep 300 & wait`

// deepOrphanScript is orphanScript with one more level: the orphan is a shell
// that itself starts a setsid'd sleep. That sleep is in neither root's group nor
// root's session, and its parent — the orphan — is in neither the parent closure
// nor anything reachable from it, so the only route to it is "find the orphan by
// its session, then follow ITS parent links". It is the fixture for the second
// expand() pass.
const deepOrphanScript = `( sh -c 'setsid sleep 300 & sleep 300' & ) ; sleep 300 & wait`

// pgroupMembers lists live pids whose process group is pgid, read from /proc
// directly so it shares nothing with the code under test.
func pgroupMembers(pgid int) []int {
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
		if g, ok := pgroupOf(pid); ok && g == pgid && liveNotZombie(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// waitForOrphan returns the fixture's reparented process: a live member of
// root's group, outside the parent-link closure, whose own parent is outside the
// group as well. That last clause is what makes it the reparent rather than a
// child of one — in deepOrphanScript the orphan's own sleep is also in the group
// and also outside the closure, and it is not the process this identifies.
// Polling, because the subshell has to exit and be reaped before the reparent
// has happened; asserting exactly one, so a fixture that produced two or none
// fails as a bad premise instead of passing on the wrong process.
func waitForOrphan(t *testing.T, root int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := setOf(snapshotDescendants(root))
		group := setOf(pgroupMembers(root))
		var found []int
		for _, pid := range pgroupMembers(root) {
			if snap[pid] {
				continue
			}
			if id, ok := readIDs(pid); ok && !group[id.ppid] && !snap[id.ppid] {
				found = append(found, pid)
			}
		}
		if len(found) == 1 {
			return found[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise failed: %d orphans outside the parent closure of %d, want exactly 1 (pgroup %v)",
				len(found), root, pgroupMembers(root))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDescendantsFindsAnOrphanedSubtree is the F3 case: the defect that made a
// parent-link oracle the wrong oracle. Both /proc parent paths are asserted to
// lose the process first — without that, a test that only checks Descendants
// finds it would still pass if the reparent had silently stopped happening, and
// then it would be measuring nothing.
func TestDescendantsFindsAnOrphanedSubtree(t *testing.T) {
	root, _ := startPgroupTree(t, orphanScript, 3)
	orphan := waitForOrphan(t, root)

	if ppid, ok := readIDs(orphan); !ok || ppid.ppid == root {
		t.Fatalf("premise failed: orphan %d's ppid is %d; it has not reparented away from root %d",
			orphan, ppid.ppid, root)
	}
	if setOf(snapshotDescendants(root))[orphan] {
		t.Fatalf("premise failed: the parent snapshot found orphan %d", orphan)
	}
	if walk, ok := fastDescendants(root); ok && setOf(walk)[orphan] {
		t.Fatalf("premise failed: the children-list walk found orphan %d", orphan)
	}
	if !setOf(Descendants(root))[orphan] {
		t.Fatalf("Descendants(%d) = %v missed the orphan %d, which is still in root's session and group",
			root, Descendants(root), orphan)
	}
}

// stableScript is orphanScript widened in the one way the order claim needs:
// THREE live children under root, and THREE orphans for the session sweep to add.
// unionDescendants sorts two different lists — each parent's children, and the
// swept pids — and a list of one element is sorted whichever way the map was
// walked, so a fixture with one child and one orphan cannot tell a sorted
// implementation from an unsorted one. That is exactly how the first version of
// this test let a mutant that drops the sort survive five runs — the call was
// sort.Ints when that was run and is slices.Sort now. Three rather than
// two because the mutant's escape route is drawing the same order twice from Go's
// randomised map iteration: at two elements that is a coin flip per call pair, at
// three it is about one in six.
const stableScript = `( sleep 300 & ) ; ( sleep 300 & ) ; ( sleep 300 & ) ; sleep 300 & sleep 300 & sleep 300 & wait`

// TestDescendantsIsASetAndIsStable pins the two properties the doc comment claims
// and nothing else asserted. A duplicate pid is nearly free — one redundant
// SIGKILL — and an unstable order only ever surfaces as a flake in some other
// test, so both are exactly the kind of claim that rots unwatched. The fixture is
// sleeps that outlive the test, so two consecutive calls must agree.
func TestDescendantsIsASetAndIsStable(t *testing.T) {
	root, _ := startPgroupTree(t, stableScript, 7) // root, three children, three orphans

	// Premise, polled: at least three pids that are root's children by parent link,
	// and at least three that the closure does not hold at all, so only the session
	// sweep can have contributed them. Below either count one of the two sorts has
	// nothing to order and this test is not looking at its output.
	var first []int
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The union first, the closure second, and the order is the premise rather
		// than style. `swept` counts pids in the union that the closure does not
		// hold, and the fixture is still forking while this loop runs: with the
		// closure sampled first, a process forked between the two reads is in the
		// union and not in the closure, and counts as swept when the session sweep
		// contributed nothing. Read the closure last and it is the later sample, so
		// anything still missing from it is missing because parent links do not
		// reach it.
		first = Descendants(root)
		closure := setOf(snapshotDescendants(root))
		kids, swept := 0, 0
		for _, pid := range first {
			if id, ok := readIDs(pid); ok && id.ppid == root {
				kids++
			}
			if !closure[pid] {
				swept++
			}
		}
		if kids >= 3 && swept >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise failed: Descendants(%d) = %v has %d children of root and %d swept pids, "+
				"want >= 3 of each, or a sort has nothing to order (closure %v)",
				root, first, kids, swept, snapshotDescendants(root))
		}
		time.Sleep(20 * time.Millisecond)
	}

	seen := map[int]bool{}
	for _, pid := range first {
		if seen[pid] {
			t.Fatalf("Descendants(%d) = %v contains %d twice", root, first, pid)
		}
		seen[pid] = true
	}
	// Several repeats, not one: the way an unsorted implementation passes this test
	// is by drawing the same order twice out of Go's randomised map iteration, and
	// each additional call divides that chance again. Every call is compared
	// against the first, so a run that is stable in pairs but drifts is still red.
	for i := 0; i < 4; i++ {
		next := Descendants(root)
		if len(first) != len(next) {
			t.Fatalf("call %d on an unchanged tree returned %v, after %v", i+2, next, first)
		}
		for j := range first {
			if first[j] != next[j] {
				t.Fatalf("call %d on an unchanged tree returned %v, after %v: the order is not stable",
					i+2, next, first)
			}
		}
	}
}

// TestKillTreeReapsAnOrphanedSubtree is the same shape end to end: the pids the
// oracle names are the pids that die. Separate from the oracle test because a
// union that finds the orphan and a KillTree that signals it are two claims, and
// the second one is the one a user notices.
func TestKillTreeReapsAnOrphanedSubtree(t *testing.T) {
	root, cmd := startPgroupTree(t, orphanScript, 3)
	orphan := waitForOrphan(t, root)

	killed, ok := killTreeNow(t, root)
	if !ok {
		t.Fatal("KillTree reported unsupported on linux")
	}
	if !setOf(killed)[orphan] {
		t.Fatalf("orphan %d not among the killed pids %v", orphan, killed)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for liveNotZombie(orphan) {
		if time.Now().After(deadline) {
			t.Fatalf("orphan %d survived KillTree", orphan)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDescendantsFollowsParentLinksFromASessionMember covers the second expand()
// pass. The setsid'd sleep is in no set of root's: not the parent closure (its
// parent is the orphan, which is not in it), not the session, not the group. It
// is reachable only by finding the orphan through the session and then walking
// down from there, which is the one thing a single-pass implementation cannot do.
func TestDescendantsFollowsParentLinksFromASessionMember(t *testing.T) {
	// 4: root, root's own sleep, the orphaned sh, and the sh's non-setsid sleep.
	// The setsid'd one has left the group and is not counted.
	root, _ := startPgroupTree(t, deepOrphanScript, 4)
	orphan := waitForOrphan(t, root)

	rootIDs, ok := readIDs(root)
	if !ok {
		t.Fatalf("cannot read root %d's ids", root)
	}
	// The deep escapee: a child of the orphan that left root's session.
	var deep int
	deadline := time.Now().Add(10 * time.Second)
	for deep == 0 {
		for _, pid := range testDescendants(orphan) {
			if pid == orphan {
				continue
			}
			id, ok := readIDs(pid)
			if ok && id.sid != rootIDs.sid && id.pgid != rootIDs.pgid && liveNotZombie(pid) {
				deep = pid
				break
			}
		}
		if deep != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise failed: no descendant of orphan %d left root's session; tree %v",
				orphan, testDescendants(orphan))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Registered here, the moment deep exists, and not at the end of the test: the
	// premise and assertion blocks below hold four t.Fatalf sites, and a cleanup
	// registered after them never runs on the paths that take them. A premise
	// failure would then leak a `sleep 300` in no group or session this suite
	// tracks — the exact process the whole test is about. This is a net, not the
	// mechanism: startPgroupTree's cleanup enumerates the union and reaches this
	// process too, and the assertion below is what says so.
	t.Cleanup(func() { _ = syscall.Kill(deep, syscall.SIGKILL) })
	// The enumeration cleanup relies on must be able to see this process. Asserted
	// in the body, because cleanups run LIFO and the net registered above runs
	// first — so no cleanup here can witness whether the outer one had the reach.
	// Without this, replacing testUnion by testDescendants in that cleanup is a
	// change no test in the package can observe.
	if !setOf(testUnion(t, root))[deep] {
		t.Errorf("testUnion(%d) = %v does not contain the deep escapee %d: the fixture's "+
			"cleanup cannot reach the process this test creates", root, testUnion(t, root), deep)
	}
	// Premises, in the order that makes the assertion mean something: the deep
	// escapee is in neither of root's id sets, and it is not in the parent
	// closure either.
	if setOf(snapshotDescendants(root))[deep] {
		t.Fatalf("premise failed: the parent snapshot found the deep escapee %d", deep)
	}
	got := setOf(Descendants(root))
	if !got[orphan] {
		t.Fatalf("Descendants(%d) missed the orphan %d; the deep case cannot be tested", root, orphan)
	}
	if !got[deep] {
		t.Fatalf("Descendants(%d) = %v found the orphan %d but not its own child %d: "+
			"parent links are not followed from a session member", root, Descendants(root), orphan, deep)
	}
}

// TestDescendantsRefusesToSweepASessionItDoesNotLead is the safety half. Against
// a root that is not the session leader, `same session as root` names the
// daemon, its siblings and every other window — so the union has to require that
// root leads the set before it uses it. The fixture asks for the descendants of
// one of the shell's children, which leads neither its group nor its session.
func TestDescendantsRefusesToSweepASessionItDoesNotLead(t *testing.T) {
	root, _ := startPgroupTree(t, "sleep 300 & sleep 300 & wait", 3)
	kids := snapshotDescendants(root)
	if len(kids) < 3 {
		t.Fatalf("premise failed: %v is not root plus two children", kids)
	}
	member := kids[1]
	id, ok := readIDs(member)
	if !ok {
		t.Fatalf("cannot read ids for %d", member)
	}
	if id.sid == member || id.pgid == member {
		t.Fatalf("premise failed: %d leads its own session or group (sid %d, pgid %d)", member, id.sid, id.pgid)
	}
	got := Descendants(member)
	if len(got) != 1 || got[0] != member {
		t.Errorf("Descendants(%d) = %v; a non-leader must answer itself alone, not sweep its session "+
			"(root %d and its siblings are in it)", member, got, root)
	}
	if setOf(got)[root] {
		t.Errorf("Descendants(%d) named its own parent %d: a kill would take the whole window down", member, root)
	}
}

// sessionMembers lists live pids in session sid, read from /proc directly. It
// duplicates what unionDescendants does for the same reason testDescendants
// duplicates the parent walk: a fixture that finds what it must clean up by
// calling the code under test leaks exactly what a discovery bug loses.
func sessionMembers(sid int) []int {
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
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		f := statFieldsAfterComm(stat)
		if len(f) < 4 {
			continue
		}
		if s, err := strconv.Atoi(f[3]); err == nil && s == sid && liveNotZombie(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// TestDescendantsFindsAJobControlOrphanBySessionAlone is the closest fixture here
// to what shelld actually runs: an interactive shell on a PTY. With a controlling
// terminal the shell turns job control on and every job gets its own process
// group, so an orphan left by a job is in root's session while its own group died
// with the job. It is the shape that shows the session is the right id to key on:
// a group is a job's lifetime, a session is the window's.
func TestDescendantsFindsAJobControlOrphanBySessionAlone(t *testing.T) {
	p, err := pty.Start(pty.Command{
		Path: "/bin/sh",
		Args: []string{"/bin/sh", "-i"},
		Dir:  "/",
		Env:  []string{"PS1=$ ", "PATH=/usr/bin:/bin", "HOME=/", "TERM=dumb"},
	}, pty.Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	root := p.Pid()
	// Drain, or the shell blocks writing its prompts once the pty buffer fills.
	go func() { _, _ = io.Copy(io.Discard, p) }()
	rootIDs, ok := readIDs(root)
	if !ok {
		t.Fatalf("cannot read ids for the shell %d", root)
	}
	t.Cleanup(func() {
		for _, pid := range sessionMembers(rootIDs.sid) {
			if pid != root {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = p.Kill()
		_ = p.Close()
		_, _ = p.Wait()
	})
	if _, err := p.Write([]byte("( sleep 300 & ) ; sleep 300 &\n")); err != nil {
		t.Fatalf("write to the shell: %v", err)
	}

	// The orphan: in root's session, not in root's group, and not reachable by
	// parent links. Polling for all three, because the subshell has to be reaped
	// before the reparent is visible.
	var orphan int
	deadline := time.Now().Add(10 * time.Second)
	for orphan == 0 {
		snap := setOf(snapshotDescendants(root))
		for _, pid := range sessionMembers(rootIDs.sid) {
			if pid == root || snap[pid] {
				continue
			}
			id, ok := readIDs(pid)
			if ok && id.pgid != rootIDs.pgid && id.ppid != root && !snap[id.ppid] {
				orphan = pid
				break
			}
		}
		if orphan != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise failed: no job-control orphan in session %d; members %v, closure %v. "+
				"If job control is off (dash needs a controlling terminal for it) this fixture is not "+
				"testing the session term at all", rootIDs.sid, sessionMembers(rootIDs.sid), snapshotDescendants(root))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !setOf(Descendants(root))[orphan] {
		t.Fatalf("Descendants(%d) = %v missed the job-control orphan %d (pgid %d, session %d): only the "+
			"session term can reach it", root, Descendants(root), orphan, mustIDs(t, orphan).pgid, rootIDs.sid)
	}
}

// startSiblingsInOneGroup starts two `sleep 300` children of the test binary and
// puts the second into the FIRST one's process group. setpgid is legal for a
// parent onto its own child as long as the target group is in the same session,
// which is how every job-control shell builds a pipeline: the leader is one
// member and the rest are its siblings, forked by the shell.
//
// It returns (leader, sibling). The leader leads the group and does NOT lead the
// session — the test binary does — which is the only configuration in which a
// group-membership test can name something root did not fork.
func startSiblingsInOneGroup(t *testing.T) (int, int) {
	t.Helper()
	lead := exec.Command("sleep", "300")
	lead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := lead.Start(); err != nil {
		t.Fatal(err)
	}
	leader := lead.Process.Pid
	sib := exec.Command("sleep", "300")
	sib.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader}
	if err := sib.Start(); err != nil {
		_ = lead.Process.Kill()
		_, _ = lead.Process.Wait()
		t.Fatal(err)
	}
	sibling := sib.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(sibling, syscall.SIGKILL)
		_ = syscall.Kill(leader, syscall.SIGKILL)
		_, _ = sib.Process.Wait()
		_, _ = lead.Process.Wait()
	})
	// Premises, all three read from /proc rather than assumed: the sibling really
	// is in the leader's group, it really is not a descendant of the leader, and
	// the leader really does not lead its session.
	deadline := time.Now().Add(5 * time.Second)
	for {
		lid, ok1 := readIDs(leader)
		sid, ok2 := readIDs(sibling)
		if ok1 && ok2 && lid.pgid == leader && sid.pgid == leader {
			if lid.sid == leader {
				t.Fatalf("premise failed: leader %d leads its own session (sid %d); a session leader's "+
					"group is a subset of its session and this fixture proves nothing", leader, lid.sid)
			}
			if sid.ppid != os.Getpid() {
				t.Fatalf("premise failed: sibling %d's ppid is %d, want the test binary %d",
					sibling, sid.ppid, os.Getpid())
			}
			if setOf(snapshotDescendants(leader))[sibling] {
				t.Fatalf("premise failed: sibling %d is inside the parent closure of %d", sibling, leader)
			}
			return leader, sibling
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise failed: sibling %d never joined group %d (leader ids %v, sibling ids %v)",
				sibling, leader, mustIDs(t, leader), mustIDs(t, sibling))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDescendantsRefusesASiblingInRootsProcessGroup is the false positive the
// group term made possible, and it is why there is no longer a group term.
//
// The package comment used to argue that both id sets are closed under fork from
// a member, so everything they add descends from root. That is true of the
// session — setsid always creates a fresh one — and FALSE of the process group:
// setpgid(child, sibling) puts processes a common parent forked into one group
// whose leader is one of them, which is what a shell does for every pipeline. A
// group-membership test against such a leader names its siblings, and KillTree
// SIGKILLs them.
//
// The term was not merely unsafe, it was unsafe exactly where it was load-bearing:
// when root DOES lead its session, every member of root's group is in root's
// session too (setpgid cannot cross a session), so the group adds nothing the
// session has not already added. The only configuration in which the group term
// contributes a pid is the one in which it may contribute a wrong pid.
func TestDescendantsRefusesASiblingInRootsProcessGroup(t *testing.T) {
	leader, sibling := startSiblingsInOneGroup(t)

	got := Descendants(leader)
	if setOf(got)[sibling] {
		t.Fatalf("Descendants(%d) = %v named %d, which is in root's process group but was forked by the "+
			"test binary, not by root: KillTree would SIGKILL a process shelld never started",
			leader, got, sibling)
	}
	if len(got) != 1 || got[0] != leader {
		t.Errorf("Descendants(%d) = %v; a leaf that leads no session must answer itself alone", leader, got)
	}
}

func mustIDs(t *testing.T, pid int) procIDs {
	t.Helper()
	id, ok := readIDs(pid)
	if !ok {
		t.Fatalf("cannot read ids for %d", pid)
	}
	return id
}

// startPgroupOnlyTree starts a shell as a process-group leader in the TEST
// BINARY's session — a root that leads its group and not its session, which is
// the configuration in which the oracle has no usable id set and must fall back
// to the parent closure.
func startPgroupOnlyTree(t *testing.T, script string, want int) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root := cmd.Process.Pid
	t.Cleanup(func() {
		tree := testDescendants(root)
		for _, pid := range tree {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		// Setpgid with a zero Pgid makes root its own group leader, so this
		// reaches the orphan too — it never left the group.
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
	if id, ok := readIDs(root); !ok || id.sid == root {
		t.Fatalf("premise failed: root %d leads its own session (sid %d); the group term is not isolated", root, id.sid)
	}
	return root
}

// TestDescendantsDegradesToTheClosureWithoutASession pins the price of dropping
// the group term, so that the loss is a recorded decision rather than a silent
// regression. Root leads its group but not its session, so no id set is usable:
// the session set is switched off by the leadership guard, and a group set would
// be unsafe here for the reason
// TestDescendantsRefusesASiblingInRootsProcessGroup demonstrates. The answer must
// therefore be exactly the parent closure — no wider (safety) and no narrower.
//
// The F3 orphan of such a root is consequently unreachable by any /proc oracle.
// That is asserted here too, as the loss it is: the case belongs to the cgroup
// membership set, which is unaffected by both reparenting and setsid.
func TestDescendantsDegradesToTheClosureWithoutASession(t *testing.T) {
	root := startPgroupOnlyTree(t, orphanScript, 3)
	orphan := waitForOrphan(t, root)

	closure := setOf(snapshotDescendants(root))
	got := Descendants(root)
	for _, pid := range got {
		if !closure[pid] {
			t.Errorf("Descendants(%d) = %v named %d, which is outside the parent closure %v; with no "+
				"session to lead there is no id set here that only root's own processes can be in",
				root, got, pid, snapshotDescendants(root))
		}
	}
	for pid := range closure {
		if !setOf(got)[pid] {
			t.Errorf("Descendants(%d) = %v lost %d, which IS in the parent closure", root, got, pid)
		}
	}
	if setOf(got)[os.Getpid()] {
		t.Fatalf("Descendants(%d) named the test process %d: the leadership guard is not holding",
			root, os.Getpid())
	}
	// The documented loss, asserted so that a future change which starts finding
	// it has to come here and say why it is safe.
	if setOf(got)[orphan] {
		t.Errorf("Descendants(%d) found the orphan %d. That is the right answer for a session leader, but "+
			"root leads no session here, so whatever reached it can also reach a stranger's process — "+
			"see TestDescendantsRefusesASiblingInRootsProcessGroup", root, orphan)
	}
}
