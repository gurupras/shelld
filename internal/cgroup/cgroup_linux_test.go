//go:build linux

package cgroup

// The two claims this package rests on are that placement happens AT FORK and that
// membership survives every way a process can leave a tree. Neither is provable from
// the code, so every test here works on a real cgroup with real processes.
//
// Safety, stated once because it governs the whole file: cgroup.kill is only ever
// written to a directory created by this test through Probe/New, every fabricated
// process is spawned into one of those directories, and the one test that exercises
// the guard against writing outside the base points it at a path where the
// UNGUARDED behaviour is harmless. A test that checks a safety guard by attempting
// the unsafe thing is the bug it is looking for.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/proctree"
)

// selfBase is the base path Probe will choose for this process, computed without
// calling Probe so it can be checked before and after one.
func selfBase(t *testing.T) string {
	t.Helper()
	own, err := ownCgroup()
	if err != nil {
		t.Skipf("no cgroup v2 path for this process: %v", err)
	}
	start, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Fatalf("cannot read this process's own start time")
	}
	return filepath.Join(mountPoint, own, baseName(os.Getpid(), start))
}

// probeOrSkip declines rather than fails when the HOST has no usable cgroup: a
// session scope owned by uid 0 is a real deployment shape, measured on this box one
// directory away from the friendly case, and the daemon's answer there is to run
// with the /proc union alone. The skip carries the reason so a CI host that declines
// for a NEW reason is still readable.
//
// What it refuses to skip for is our own mess, and that distinction was measured
// rather than anticipated. Every test in the file shares one base, because the base
// is named for this process, so a teardown that fails in the first test leaves it
// behind for the second test's Probe. Under one mutant that broke exactly that, the
// first test failed and the remaining five SKIPPED — a run that reports a defect once
// and then quietly stops exercising the code, with the skips reading like a host
// without cgroups.
//
// The check needs no bookkeeping about whether we have probed before, and that is a
// consequence of the naming: a base carries the pid AND the start time, so a
// directory with our own name cannot have been made by any other process. Its
// existence before our first Probe is therefore always our own leak. Probe itself
// tolerates it — same pid, same start time, same process — which is right for Probe
// and would let a test run on top of a leak.
func probeOrSkip(t *testing.T) *Manager {
	t.Helper()
	base := selfBase(t)
	if _, err := os.Stat(base); err == nil {
		t.Fatalf("our own base %s already exists before Probe: an earlier test in this "+
			"process did not tear it down, and this test would otherwise run on top of it", base)
	}
	m, err := Probe()
	if err != nil {
		if _, serr := os.Stat(base); serr == nil {
			t.Fatalf("Probe declined with %v, and our own base %s exists: that is a "+
				"teardown failure in this process, not a host without cgroups. Skipping "+
				"here would hide which tests never ran", err, base)
		}
		t.Skipf("no usable cgroup here: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
		if _, err := os.Stat(m.Base()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("base %s survived Close: %v", m.Base(), err)
		}
	})
	return m
}

// startInto spawns a shell into c the way pty.Start will: placed at fork, in its own
// session, so it is a session leader exactly like a window shell.
func startInto(t *testing.T, c *Cgroup, script string) *os.Process {
	t.Helper()
	dir, err := c.Open()
	if err != nil {
		t.Fatalf("Cgroup.Open: %v", err)
	}
	defer dir.Close()
	proc, err := os.StartProcess("/bin/sh", []string{"/bin/sh", "-c", script}, &os.ProcAttr{
		Files: []*os.File{nil, nil, nil},
		Sys: &syscall.SysProcAttr{
			Setsid:      true,
			UseCgroupFD: true,
			CgroupFD:    int(dir.Fd()),
		},
	})
	if err != nil {
		t.Fatalf("spawning into %s: %v", c.Dir(), err)
	}
	// Registered before any assertion: from here on a t.Fatalf must not leave a
	// `sleep 300` behind, and the cgroup is the one handle that reaches every process
	// this shell will fork, including the ones no /proc relation can find.
	t.Cleanup(func() {
		// A cgroup that is GONE is the other success case, and not merely by convention:
		// rmdir of a populated cgroup is EBUSY, so a directory that was removed had no
		// members to kill. Tests that tear their own cgroup down in the body reach here
		// with ENOENT, and treating that as a failure would make every one of them red.
		if err := c.Kill(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cleanup: Cgroup.Kill on %s: %v", c.Dir(), err)
		}
		// Bounded, and then by pid. The cgroup is the handle that reaches every process
		// this shell forked, but a cleanup that TRUSTS it cannot survive a defect in it:
		// an unreached `sleep 300` turns the Wait below into a five-minute block, and a
		// test that hangs reports nothing at all — not even the failure that hung it.
		// The pgroup fallback is deliberately weaker than the cgroup and cannot reach a
		// setsid'd escapee; it is here to keep a broken Kill readable, not to replace it.
		if !waitFor(2*time.Second, func() bool { return !alive(proc.Pid) }) {
			t.Errorf("cleanup: %d survived Cgroup.Kill on %s, falling back to its pgroup",
				proc.Pid, c.Dir())
			// The sign is what makes this worth a guard the direct kill below does
			// not need: kill(-1) is every process this uid owns, so a Pid of 1 here
			// would not be a missed cleanup but the session.
			if proc.Pid > 1 {
				_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
			}
			_ = proc.Kill()
		}
		_, _ = proc.Wait()
	})
	return proc
}

// members reads cgroup.procs. It is the membership oracle and shares nothing with
// the /proc walks in proctree, which is the point: the two arms have to be able to
// disagree.
func members(t *testing.T, dir string) []int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read %s/cgroup.procs: %v", dir, err)
	}
	var out []int
	for _, line := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("cgroup.procs holds %q, which is not a pid", line)
		}
		out = append(out, pid)
	}
	return out
}

// cgroupOf reads a process's own view of where it is.
func cgroupOf(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// statIDs reads a process's ppid and session from /proc/<pid>/stat, after the LAST
// ')': comm is arbitrary bytes in parentheses, so splitting on the first one reads
// the comm's own text as ids. Fields after it are state, ppid, pgrp, session.
func statIDs(pid int) (ppid, sid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(b)[i+1:])
	if len(f) < 4 {
		return 0, 0, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	sid, err2 := strconv.Atoi(f[3])
	return ppid, sid, err1 == nil && err2 == nil
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(string(b)[i+1:])
	return len(f) > 0 && f[0] != "Z"
}

// waitFor polls until cond, and returns whether it ever held. Every fixture here
// forks, and a fork is not instantaneous.
func waitFor(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestProbeGivesABaseWithTheFilesACloseNeeds(t *testing.T) {
	m := probeOrSkip(t)
	if _, err := os.Stat(filepath.Join(m.Base(), "cgroup.kill")); err != nil {
		t.Errorf("base has no cgroup.kill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Base(), "cgroup.events")); err != nil {
		t.Errorf("base has no cgroup.events: %v", err)
	}
	// The probe child must be gone: it is spawned inside the base, so a leftover
	// would be counted by every WaitEmpty this daemon ever does.
	if got := members(t, m.Base()); len(got) != 0 {
		t.Errorf("base holds %v after Probe, want nothing", got)
	}
	if _, err := os.Stat(filepath.Join(m.Base(), "probe")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the probe cgroup survived Probe: %v", err)
	}
}

// TestPlacementIsAtFork is the assertion Probe itself cannot make. Probe spawns
// /bin/true, which has usually exited before its cgroup can be read, so it can only
// conclude from the spawn succeeding. Here the subject is held still by `sleep`, and
// both directions are checked: the cgroup says it holds the process, and the process
// says it is in the cgroup. The second is the one that says "at fork" rather than
// "eventually" — a shell that had been moved after starting would have been
// observable outside first, and there is no window in which this one was.
func TestPlacementIsAtFork(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(1)
	if err != nil {
		t.Fatalf("Manager.New: %v", err)
	}
	// `exec`, so the cgroup holds exactly one process for the whole test and the
	// count below is not a race: `sh -c 'sleep 300'` on this box forks the sleep
	// rather than exec'ing it, and an early read sees one member while a later one
	// sees two.
	proc := startInto(t, c, "exec sleep 300")

	if got := members(t, c.Dir()); len(got) != 1 || got[0] != proc.Pid {
		t.Fatalf("cgroup.procs = %v, want exactly [%d]", got, proc.Pid)
	}
	own, ok := cgroupOf(proc.Pid)
	if !ok {
		t.Fatalf("cannot read /proc/%d/cgroup", proc.Pid)
	}
	want := strings.TrimPrefix(c.Dir(), mountPoint)
	if own != want {
		t.Errorf("the shell's own /proc/<pid>/cgroup is %q, want %q", own, want)
	}
}

// TestKillReachesTheUnionsResidual is one half of "co-primary", and it is the half
// that justifies the package existing at all.
//
// The fixture builds the one process the /proc union cannot reach from root: a
// `setsid`'d process whose parent — the subshell that forked it — exits immediately.
// It is out of root's session, so the session term does not name it, and its parent
// link now points at init, so the closure does not either. The test asserts that
// absence as a PREMISE rather than assuming it, because if proctree could reach the
// process, the test would be measuring nothing.
func TestKillReachesTheUnionsResidual(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(2)
	if err != nil {
		t.Fatalf("Manager.New: %v", err)
	}
	// The subshell exits as soon as it has forked the setsid'd sleep, which is what
	// orphans it; the trailing `exec sleep 300` keeps root itself alive. The residual
	// writes its own pid to a file, and that file is the only record of it that survives
	// a defect anywhere else: it is in no group, session or parent link this test can
	// reach from root — that is the whole point of the fixture — so the alternatives are
	// the cgroup it was placed in, which is exactly what a mutant redirects, and a scan
	// of the whole host, which a test does not get to do.
	residFile := filepath.Join(t.TempDir(), "residual.pid")
	proc := startInto(t, c, "( setsid sh -c 'echo $$ > "+residFile+"; exec sleep 300' & ) ; exec sleep 300")
	root := proc.Pid
	// Registered before the first assertion, and reading the FILE rather than closing
	// over the variable the body fills in: both halves were measured. The mutants that
	// hand back the base instead of a fresh cgroup (C10, C11 in the driver) make the
	// premise below Fatal — under C11 the shell is placed in the base, so the window
	// cgroup this test scans is empty — and at that point `residual` is still 0 and
	// startInto's cleanup cannot help: it kills the cgroup the mutant redirected, and
	// its pgroup fallback cannot reach a process that called setsid. Both mutants left a
	// `sleep 300` on the host for exactly that reason.
	t.Cleanup(func() {
		b, err := os.ReadFile(residFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err == nil && pid > 1 && alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	var residual int
	found := waitFor(10*time.Second, func() bool {
		union := map[int]bool{}
		for _, pid := range proctree.Descendants(root) {
			union[pid] = true
		}
		for _, pid := range members(t, c.Dir()) {
			if pid != root && !union[pid] && alive(pid) {
				residual = pid
				return true
			}
		}
		return false
	})
	if !found {
		t.Fatalf("premise failed: every member of %s is in proctree.Descendants(%d) = %v; "+
			"members %v. Without a residual this test cannot say what the cgroup adds",
			c.Dir(), root, proctree.Descendants(root), members(t, c.Dir()))
	}
	// And it is the shape claimed, not just any pid the union happened to miss: out of
	// root's session, and with a parent that is not in root's tree. Either one alone
	// would still be reachable — the union finds a setsid'd process by its parent link
	// while that link holds, and an orphan by the session it never left.
	// The pid the residual wrote down is the pid the cgroup scan found. Two independent
	// records of the same process, and the check is here so that the cleanup's file is
	// known to name the thing this test is about rather than some other member.
	if b, err := os.ReadFile(residFile); err != nil {
		t.Errorf("the residual %d never wrote its pid to %s: %v", residual, residFile, err)
	} else if got := strings.TrimSpace(string(b)); got != strconv.Itoa(residual) {
		t.Errorf("the residual found in the cgroup is %d and the one that wrote %s is %s",
			residual, residFile, got)
	}
	ppid, sid, ok := statIDs(residual)
	if !ok {
		t.Fatalf("cannot read /proc/%d/stat for the residual", residual)
	}
	if sid == root {
		t.Errorf("the residual %d is still in root %d's session; the session term reaches it "+
			"and the premise above is measuring something else", residual, root)
	}
	if ppid == root {
		t.Errorf("the residual %d is still root %d's child by parent link", residual, root)
	}

	if err := c.Kill(); err != nil {
		t.Fatalf("Cgroup.Kill: %v", err)
	}
	if !waitFor(5*time.Second, func() bool { return !alive(residual) }) {
		t.Errorf("the union's residual %d survived cgroup.kill", residual)
	}
	if err := c.WaitEmpty(5 * time.Second); err != nil {
		t.Errorf("WaitEmpty after Kill: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Errorf("Cgroup.Remove: %v", err)
	}
}

// TestKillDescendsIntoANestedCgroup pins the reason a window creating a cgroup under
// itself is nesting rather than escape: cgroup.kill descends. rmdir does not, which
// is why Remove is depth-first — an rmdir of the parent alone is EBUSY while a child
// directory merely exists.
func TestKillDescendsIntoANestedCgroup(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(3)
	if err != nil {
		t.Fatalf("Manager.New: %v", err)
	}
	nested := filepath.Join(c.Dir(), "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", nested, err)
	}
	proc := startInto(t, c, "exec sleep 300")
	// Migrated by pid, which is all a process can do: writing one pid into
	// cgroup.procs moves that process and nothing else.
	if err := os.WriteFile(filepath.Join(nested, "cgroup.procs"),
		[]byte(strconv.Itoa(proc.Pid)), 0); err != nil {
		t.Fatalf("migrating %d into %s: %v", proc.Pid, nested, err)
	}
	if got := members(t, nested); len(got) != 1 || got[0] != proc.Pid {
		t.Fatalf("nested cgroup.procs = %v, want [%d]", got, proc.Pid)
	}
	if contains(members(t, c.Dir()), proc.Pid) {
		t.Fatalf("the window cgroup still holds root %d after the migration: %v",
			proc.Pid, members(t, c.Dir()))
	}
	// The parent's own rmdir before anything is killed: EBUSY, so the ordering in
	// Remove is required rather than tidy.
	if err := os.Remove(c.Dir()); err == nil {
		t.Errorf("rmdir of a cgroup with a child directory succeeded; Remove's depth-first "+
			"order is answering a problem that does not exist: %s", c.Dir())
	}

	if err := c.Kill(); err != nil {
		t.Fatalf("Cgroup.Kill: %v", err)
	}
	if !waitFor(5*time.Second, func() bool { return !alive(proc.Pid) }) {
		t.Errorf("the process in the nested cgroup %d survived the parent's cgroup.kill", proc.Pid)
	}
	if err := c.WaitEmpty(5 * time.Second); err != nil {
		t.Errorf("WaitEmpty: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Errorf("Cgroup.Remove: %v", err)
	}
	if _, err := os.Stat(nested); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the nested cgroup survived Remove: %v", err)
	}
}

// TestMigrationTakesOnlyTheWriter is the other half of "co-primary": the case the
// cgroup loses and the /proc union keeps.
//
// A process in a delegated subtree can write its own pid into any cgroup.procs it
// can open, and every ancestor of ours qualifies — measured for the per-daemon base,
// a sibling window and the daemon's own scope, all with no error. What it cannot do
// is take its children with it: the write moves one process, so anything it forked
// before moving stays where it was. The destination here is the daemon's own base,
// chosen because it is a real escape from the window's cgroup AND still inside a
// directory this test is responsible for.
func TestMigrationTakesOnlyTheWriter(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(4)
	if err != nil {
		t.Fatalf("Manager.New: %v", err)
	}
	// The fork FIRST and then `exec`, so root outlives its child: with `sleep 300 &
	// wait` the shell exits the moment the child is killed, and a first version of
	// this test read that as "the escapee died with the cgroup it had left".
	proc := startInto(t, c, "sleep 300 & exec sleep 600")
	root := proc.Pid
	// The child must exist before the migration: the claim is about what a process
	// leaves behind, so it has to have something to leave.
	var child int
	if !waitFor(10*time.Second, func() bool {
		for _, pid := range members(t, c.Dir()) {
			if pid != root {
				child = pid
				return true
			}
		}
		return false
	}) {
		t.Fatalf("premise failed: root %d never forked a member; cgroup.procs = %v",
			root, members(t, c.Dir()))
	}

	if err := os.WriteFile(filepath.Join(m.Base(), "cgroup.procs"),
		[]byte(strconv.Itoa(root)), 0); err != nil {
		t.Fatalf("migrating %d out of its window cgroup: %v", root, err)
	}
	if got := members(t, c.Dir()); len(got) != 1 || got[0] != child {
		t.Fatalf("the window cgroup holds %v after root migrated out, want exactly the "+
			"child [%d]: migration is supposed to move one process", got, child)
	}

	// Read while root is still alive and unreaped, which is the only time the value
	// identifies this process rather than whoever holds the number later.
	start, ok := proctree.StartTicks(root)
	if !ok {
		t.Fatalf("cannot read start ticks for %d", root)
	}

	// cgroup.kill on the window no longer names the escapee, and saying so is the
	// point rather than a caveat: this is the residual the /proc arm exists for.
	if err := c.Kill(); err != nil {
		t.Fatalf("Cgroup.Kill: %v", err)
	}
	if !waitFor(5*time.Second, func() bool { return !alive(child) }) {
		t.Errorf("the child left behind %d survived the window's cgroup.kill", child)
	}
	if !alive(root) {
		t.Fatalf("the migrated root %d died with the cgroup it had already left, so this "+
			"test is not measuring an escape", root)
	}

	// And the /proc arm reaches it. This is the assertion that makes the two arms
	// co-primary rather than one a subset of the other: the escapee is outside the
	// cgroup, cgroup.kill has already run, so if KillTree does not name it nothing
	// will.
	killed, ok := proctree.KillTree(root, start)
	if !ok || !contains(killed, root) {
		t.Fatalf("proctree.KillTree(%d) = %v, %v: the escaped root is not in the answer",
			root, killed, ok)
	}
	if !waitFor(5*time.Second, func() bool { return !alive(root) }) {
		t.Errorf("the escapee %d survived proctree.KillTree", root)
	}
}

func contains(pids []int, want int) bool {
	for _, pid := range pids {
		if pid == want {
			return true
		}
	}
	return false
}

// deadPid returns the pid of a process that has exited AND been reaped, so /proc has
// no entry for it and no start time can be read for it. That is what "the daemon that
// made this directory is gone" looks like from the outside.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "exit 0")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if err := c.Wait(); err != nil {
		t.Fatalf("waiting for the stand-in: %v", err)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Skipf("premise: pid %d is still in /proc after being reaped", pid)
	}
	return pid
}

// livePid starts a process that stays alive for the whole test and returns the two
// numbers a base name is made of.
func livePid(t *testing.T) (int, uint64) {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "exec sleep 300")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	start, ok := proctree.StartTicks(c.Process.Pid)
	if !ok {
		t.Fatalf("cannot read start ticks for the live stand-in %d", c.Process.Pid)
	}
	return c.Process.Pid, start
}

// TestGCCollectsDeadDaemonsAndKillsNothing is the crashed-daemon case, in five arms
// that between them state the whole policy.
//
// A daemon that was SIGKILLed ran no teardown, so its base is still there — sometimes
// with processes in it. The GC removes the directories and refuses to kill: a cgroup
// cannot tell a shell nobody can reach any more from a job the user deliberately
// detached, and a name in a directory is not authority to SIGKILL either one. So a
// stale base with anything alive in it is left for a later GC.
//
// The fourth arm is the one a pid-only name could not have: a base whose pid is LIVE
// but whose start time is not that process's. The daemon that made it is gone and the
// number has come round to something else, and the only way to know is to compare
// what the /proc arm compares.
// The GC is asked about a parent of this test's own making rather than the real one
// at filepath.Dir(m.Base()), and that is a repair to a failure that was measured, not
// a precaution. GC's contract is "scan the cgroup this daemon is in", so two daemons
// in one cgroup scan the SAME directory — which is the behaviour this test asserts, and
// the whole reason a GC collects other daemons' leavings. `go test ./...` runs package
// binaries in parallel, and this package's binary and the shelld package's were both in
// the terminal's cgroup at once: each ran a GC over it and legitimately collected the
// other's fabricated stale bases, so this test saw `removed 1` where it wanted 2. It
// passed on the run before, which is the tell — the two had not overlapped.
//
// A private parent removes the interaction without weakening an assertion: GC is asked
// about a directory nothing else is looking at, and every arm below is fabricated the
// same way. What it does cost is the "our own base survives" claim, since our real base
// is no longer under the parent being scanned, so arm 5 restates it inside this parent.
func TestGCCollectsDeadDaemonsAndKillsNothing(t *testing.T) {
	m := probeOrSkip(t)
	parent := filepath.Join(m.Base(), "gc")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("making a private parent for the fixtures: %v", err)
	}
	// Registered before the fixtures so it runs after theirs (cleanups are LIFO), and
	// after probeOrSkip's for the same reason: Close insists the base be empty.
	t.Cleanup(func() {
		if left, err := removeIfEmpty(parent); err != nil || left > 0 {
			t.Errorf("the private parent %s did not clear: %d left, err %v", parent, left, err)
		}
	})
	gc := &Manager{parent: parent, base: m.Base()}

	// 1. A dead daemon that left an empty window cgroup behind: removed, and the
	//    nested directory is why removal has to recurse.
	empty := filepath.Join(parent, baseName(deadPid(t), 1))
	if err := os.MkdirAll(filepath.Join(empty, "win-1"), 0o755); err != nil {
		t.Fatalf("fabricating an empty stale base: %v", err)
	}
	t.Cleanup(func() { _, _ = removeIfEmpty(empty) })

	// 2. A dead daemon with a process still running in one of its windows: LEFT, with
	//    the process still running. Registered before startInto so this cleanup runs
	//    after the one that kills the process — cleanups are LIFO, and a directory
	//    cannot be removed before the process in it is gone.
	busyBase := filepath.Join(parent, baseName(deadPid(t), 1))
	// deadPid hands out a fresh pid each call, but nothing guarantees the two calls
	// differ — a pid the kernel has just recycled would give both arms one directory,
	// and the test would then assert that the same path is both removed and kept. That
	// is a premise, so it fails here rather than as a contradiction 40 lines down.
	if busyBase == empty {
		t.Fatalf("premise: the empty and busy arms are the same directory %s, so the two "+
			"reaped stand-ins got the same pid", empty)
	}
	busyWin := filepath.Join(busyBase, "win-1")
	if err := os.MkdirAll(busyWin, 0o755); err != nil {
		t.Fatalf("fabricating a busy stale base: %v", err)
	}
	t.Cleanup(func() { _, _ = removeIfEmpty(busyBase) })
	victim := startInto(t, &Cgroup{dir: busyWin, base: busyBase}, "exec sleep 300")

	// 3. A live daemon: pid and start time both match a running process, so it is
	//    skipped. Our own base is skipped by this same test.
	lp, ls := livePid(t)
	kept := filepath.Join(parent, baseName(lp, ls))
	if err := os.Mkdir(kept, 0o755); err != nil {
		t.Fatalf("fabricating a live base: %v", err)
	}
	t.Cleanup(func() { _, _ = removeIfEmpty(kept) })

	// 4. A pid that came round to somebody else: live pid, wrong start time.
	reused := filepath.Join(parent, baseName(lp, ls+1))
	if err := os.Mkdir(reused, 0o755); err != nil {
		t.Fatalf("fabricating a reused-pid base: %v", err)
	}
	t.Cleanup(func() { _, _ = removeIfEmpty(reused) })

	// 5. Us. A base named for THIS process's (pid, start) is the one a running daemon's
	//    GC must never touch, and removing it would take the windows of the daemon doing
	//    the collecting. It is fabricated inside the private parent rather than asserted
	//    of m.Base() itself, because m.Base() is no longer under the directory being
	//    scanned; the claim is unchanged, since the arm is skipped by exactly the
	//    comparison that skips the real base.
	ownStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Fatalf("cannot read our own start ticks")
	}
	ours := filepath.Join(parent, baseName(os.Getpid(), ownStart))
	if err := os.Mkdir(ours, 0o755); err != nil {
		t.Fatalf("fabricating a base with our own name: %v", err)
	}
	t.Cleanup(func() { _, _ = removeIfEmpty(ours) })

	removed, busy, err := gc.GC()
	if err != nil {
		t.Fatalf("Manager.GC: %v", err)
	}
	// Exact counts rather than lower bounds, which the private parent is what buys: the
	// directory holds five fabricated bases and nothing else, so "at least" no longer has
	// anything to tolerate, and a GC that removed one arm too many is a wrong count
	// before it is a wrong directory.
	if removed != 2 {
		t.Errorf("GC removed %d bases, want exactly 2: the empty stale one and the reused-pid one", removed)
	}
	if busy != 1 {
		t.Errorf("GC reported %d busy bases, want exactly 1: the one with a live process", busy)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty stale base %s survived GC: %v", empty, err)
	}
	if _, err := os.Stat(reused); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the reused-pid base %s survived GC: pid %d is live but started at %d, not %d",
			reused, lp, ls, ls+1)
	}
	if _, err := os.Stat(busyBase); err != nil {
		t.Errorf("GC removed the busy stale base %s: %v", busyBase, err)
	}
	if !alive(victim.Pid) {
		t.Errorf("GC killed %d, a process in a stale base. Nothing asked for it to die: "+
			"a stale base is left for a later GC precisely so that this cannot happen", victim.Pid)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("GC removed the base of the live daemon %d: %v", lp, err)
	}
	if !alive(lp) {
		t.Errorf("the live stand-in %d died; the third arm proved nothing", lp)
	}
	// Our own name is a live one by the same test, and removing it would take the
	// windows of the daemon that is running the GC.
	if _, err := os.Stat(ours); err != nil {
		t.Errorf("GC removed %s, a base named for our own pid %d and start time %d: %v",
			ours, os.Getpid(), ownStart, err)
	}
}

// TestCloseLeavesAPopulatedBaseAndKillsNothing is the shutdown half of the same
// policy the GC test states for startup. A daemon shutting down with a window cgroup
// that still holds a process has two candidate answers, and the loud one is wrong:
// cgroup.kill there would SIGKILL whatever outlived its shell — a `nohup`ed job, a
// window the caller never closed — on the strength of the daemon exiting. So Close
// removes what is empty, leaves what is not, and RETURNS AN ERROR saying so, because
// a teardown that silently leaves a directory behind is how the leak this package
// exists to fix looked in the first place.
//
// It cannot use probeOrSkip: that helper's cleanup requires the base to be gone after
// Close, which is precisely what this test denies.
func TestCloseLeavesAPopulatedBaseAndKillsNothing(t *testing.T) {
	base := selfBase(t)
	if _, err := os.Stat(base); err == nil {
		t.Fatalf("our own base %s exists before Probe: an earlier test leaked it", base)
	}
	m, err := Probe()
	if err != nil {
		t.Skipf("no usable cgroup here: %v", err)
	}
	c, err := m.New(9)
	if err != nil {
		t.Fatalf("Manager.New: %v", err)
	}
	// Registered before the spawn, so it runs AFTER startInto's kill: cleanups are
	// LIFO, and this one is the removal that Close is expected not to perform.
	t.Cleanup(func() {
		if left, err := removeIfEmpty(m.Base()); err != nil || left > 0 {
			t.Errorf("cleaning up %s: %d left, err %v", m.Base(), left, err)
		}
	})
	proc := startInto(t, c, "exec sleep 300")

	if err := m.Close(); err == nil {
		t.Errorf("Manager.Close returned nil with %d still running in %s: the caller is "+
			"then told teardown finished while its base is still on the filesystem",
			proc.Pid, c.Dir())
	}
	if !alive(proc.Pid) {
		t.Errorf("Close killed %d. Shutting down is not a decision that a process which "+
			"outlived its shell should die, and the next daemon's GC leaves it too", proc.Pid)
	}
	if _, err := os.Stat(m.Base()); err != nil {
		t.Errorf("Close removed the base %s with a process still in it: %v", m.Base(), err)
	}
}

// TestKillRefusesOutsideTheBase checks the guard that keeps a Cgroup from becoming a
// write to an ancestor. Both cases point at a directory in t.TempDir(), which is not
// a cgroup at all: without the guard the write is ENOENT and nothing dies, and that
// is deliberate. Pointing this test at a real ancestor — the daemon's own scope, say
// — would mean that a run with the guard removed kills the session running the test,
// so the test would be the defect it is looking for.
func TestKillRefusesOutsideTheBase(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		name string
		c    Cgroup
		want string
	}{
		{
			name: "outside_the_base",
			c:    Cgroup{dir: tmp, base: filepath.Join(tmp, namePrefix+"1")},
			want: "is not under",
		},
		{
			name: "base_is_not_ours",
			c:    Cgroup{dir: filepath.Join(tmp, "elsewhere", "win-1"), base: filepath.Join(tmp, "elsewhere")},
			want: "is not a shelld base",
		},
		{
			name: "base_is_a_prefix_but_not_a_parent",
			c:    Cgroup{dir: filepath.Join(tmp, namePrefix+"1-evil"), base: filepath.Join(tmp, namePrefix+"1")},
			want: "is not under",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Kill()
			if err == nil {
				t.Fatalf("Kill on %s with base %s succeeded", tc.c.dir, tc.c.base)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Kill error is %q, want it to mention %q", err, tc.want)
			}
			if err := tc.c.Remove(); err == nil {
				t.Errorf("Remove on %s with base %s succeeded", tc.c.dir, tc.c.base)
			}
		})
	}
}
