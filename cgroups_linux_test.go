//go:build linux

package shelld

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/proctree"
)

// initCgroupsOrSkip turns the arm on for one test, or skips.
//
// InitCgroups is process-wide and idempotent, and calling it once per test is
// safe for the same reason Probe tolerates ErrExist: the base is named for this
// process's pid AND start time, so a base with that name can only be one this
// process made. What the cleanup is for is not correctness within the run but the
// host: a test binary that exits leaving a cgroup directory behind has leaked one
// that nothing will collect until another daemon starts.
func initCgroupsOrSkip(t *testing.T) {
	t.Helper()
	if !InitCgroups() {
		t.Skip("no usable cgroup v2 here; the /proc union is the whole of teardown")
	}
	t.Cleanup(CloseCgroups)
}

// cgroupOf reads a process's own v2 path from /proc/<pid>/cgroup. It is the answer
// to "where did the kernel actually put it", which is the only form of the
// placement question worth asking: the daemon's intent is not evidence.
func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		t.Fatalf("read the cgroup of %d: %v", pid, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no v2 line in the cgroup of %d: %q", pid, b)
	return ""
}

// TestAWindowWithoutTheArmIsUnchanged is the default path, and it is a test rather
// than an assumption because this is what every embedder of this package gets. No
// InitCgroups has run, so there is no manager, so the window has no cgroup and its
// teardown is the /proc union alone — the behaviour that existed before any of this.
func TestAWindowWithoutTheArmIsUnchanged(t *testing.T) {
	cgMu.Lock()
	had := cgManager != nil
	cgMu.Unlock()
	if had {
		t.Skip("an earlier test left the arm on; this one only means anything with it off")
	}
	w, err := NewWindow(WindowOpts{Name: "plain", Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	// Before the assertions, and idempotent: the body kills the window as part of
	// what it is checking, and a second Kill is a no-op by design.
	t.Cleanup(w.Kill)
	w.mu.RLock()
	cg := w.cg
	w.mu.RUnlock()
	if cg != nil {
		t.Errorf("a window has cgroup %s with no manager initialised", cg.Dir())
	}
	pid := w.Pid()
	w.Kill()
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat("/proc/" + strconv.Itoa(pid))
		return err != nil
	}, "the shell survived a kill on a window with no cgroup")
}

// TestWindowKillReachesTheUnionsResidual is the test this whole arm exists for, and
// it is written so that it cannot pass for the old reason.
//
// The subject is a process that has performed BOTH escapes: it setsid'd into its
// own session, and its parent exited, so it has been reparented away. Either escape
// alone is survivable — the parent closure finds a process that setsid'd while its
// link is intact, and the session term finds one that was orphaned but stayed in the
// shell's session. Both together are reachable by neither, and the test ASSERTS that
// before it kills anything: proctree.Descendants is the union oracle itself, and the
// test requires it to miss the subject. Without that assertion a green result would
// be consistent with the union having done all the work and the cgroup having done
// none.
//
// Then Window.Kill must still kill it, and the only thing left that can is
// membership: the subject was created inside the window's cgroup and no escape it
// performed changes that.
func TestWindowKillReachesTheUnionsResidual(t *testing.T) {
	initCgroupsOrSkip(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("no setsid(1) to build the residual with: %v", err)
	}
	tmp := t.TempDir()
	pidFile, plainFile := tmp+"/residual.pid", tmp+"/plain.pid"
	// Two children, and the second one is not decoration. The residual is built by
	// the subshell: it forks, backgrounds a setsid'd child, and exits, which sends
	// that child to whatever reaper the kernel picks. The PLAIN child is an ordinary
	// descendant that the union must find, and it is here because "Descendants did
	// not contain the residual" is a claim about a working oracle. An earlier version
	// of this test had the shell `exec sleep 300`, leaving it with no descendants at
	// all: the absence assertion then held over an empty set and would have held just
	// as well against a Descendants that always answered nothing. With the plain
	// child, one call to the oracle shows both what it sees and what it cannot.
	script := `( setsid sh -c 'echo $$ > ` + pidFile + `; exec sleep 300' & ) ; ` +
		`sh -c 'echo $$ > ` + plainFile + `; exec sleep 300' & wait`
	w, err := NewWindow(WindowOpts{Name: "resid", Command: []string{"/bin/sh", "-c", script}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	// Registered HERE, before the first assertion that can fail, and this ordering
	// was measured rather than reasoned about. A mutant that stopped the window from
	// recording its cgroup made this test t.Fatal at the check below — before the two
	// pids had been read, so no cleanup existed for them and no Kill was reached. The
	// residual and the plain child were still running when the test binary exited,
	// and the residual is setsid'd, so closing the PTY did not reach it either: one
	// mutant run left a `sleep 300` on the host and a populated cgroup that GC is
	// correctly unwilling to collect. A cleanup that is registered after the
	// assertions is a cleanup for the passing case.
	//
	// The pids are killed as well as the window, because w.Kill IS the mechanism
	// under test: a cleanup that only called it would rely on the thing a mutant
	// breaks. Both are pids this test's own window created.
	// The pids come from the FILES and not from the variables the test body fills in,
	// and that distinction was measured too. Registering the cleanup early was not
	// enough: under the same mutant the t.Fatal below still lands before the pids have
	// been read, so a cleanup closing over those variables sees two zeroes and kills
	// nothing. The files are written by the processes themselves and are readable from
	// the moment each one exists, which makes them the record that survives a failure
	// anywhere in the body.
	t.Cleanup(func() {
		w.Kill()
		for _, path := range []string{pidFile, plainFile} {
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && pid > 0 && alive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	var resid, plain int
	w.mu.RLock()
	cg := w.cg
	w.mu.RUnlock()
	if cg == nil {
		t.Fatal("the arm reported itself active and the window still has no cgroup")
	}
	dir := cg.Dir()

	readPid := func(path, what string) int {
		t.Helper()
		var pid int
		waitFor(t, 5*time.Second, func() bool {
			b, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil && pid > 0
		}, "the "+what+" never wrote its pid")
		return pid
	}
	resid, plain = readPid(pidFile, "residual"), readPid(plainFile, "plain child")
	if sid, own := sessionID(resid), sessionID(w.Pid()); sid == own {
		t.Fatalf("the residual shares the shell's session %d, so it is reachable by the "+
			"session term and this test is not measuring the residual at all", sid)
	}
	// The orphaning is a race with the subshell's exit, not an instant, so it is
	// waited for rather than asserted. The wait is on the union oracle itself: what
	// has to become true is that walking /proc from the shell does not find the
	// subject.
	// Written as an explicit loop rather than with waitFor because the failure has to
	// name the set that was actually seen, and waitFor takes its message by value.
	var union []int
	deadline := time.Now().Add(5 * time.Second)
	for {
		union = proctree.Descendants(w.Pid())
		sawPlain, sawResid := false, false
		for _, p := range union {
			switch p {
			case plain:
				sawPlain = true
			case resid:
				sawResid = true
			}
		}
		if sawPlain && !sawResid {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the /proc union of shell %d is %v, and it has to contain the plain "+
				"child %d and NOT the residual %d. Containing the residual means the "+
				"topology this test needs was never built; missing the plain child means "+
				"the oracle is not answering and the residual's absence from it proves "+
				"nothing", w.Pid(), union, plain, resid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !alive(resid) {
		t.Fatal("the residual died on its own before the kill")
	}
	// Placement, read from the kernel rather than from our intent: the residual is
	// in the window's cgroup even though it is in neither of the /proc sets, which
	// is the whole claim in one line.
	if got := cgroupOf(t, resid); !strings.HasSuffix(dir, got) {
		t.Fatalf("the residual is in cgroup %s, not in the window's %s", got, dir)
	}

	w.Kill()
	waitFor(t, 5*time.Second, func() bool { return !alive(resid) },
		"the residual survived Window.Kill: it is in neither /proc set, so the cgroup "+
			"arm is the only thing that could have reached it")
	// The plain child too, which is the other arm's job and is asserted here so that
	// a change which quietly traded one arm for the other cannot pass.
	waitFor(t, 5*time.Second, func() bool { return !alive(plain) },
		"the plain child survived Window.Kill, and it is the descendant the /proc union "+
			"itself reported")
	// And the directory goes with it. A window that killed correctly and left its
	// cgroup behind would still be a leak, just a quieter one: the base cannot be
	// removed while a child directory is in it, so every window would cost the
	// daemon one permanent directory.
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	}, "the window cgroup "+dir+" is still there after Kill")
}

// TestKillTwiceIsQuiet pins the second Kill, which is reachable in ordinary use:
// nothing removes an exited window from a session, so a client can close a window
// whose shell is already gone. The cgroup is taken out of the window before it is
// killed, so the second call has nothing to work on and must not report anything.
func TestKillTwiceIsQuiet(t *testing.T) {
	initCgroupsOrSkip(t)
	w, err := NewWindow(WindowOpts{Name: "twice", Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	// Before the check below, which t.Fatals: this test's whole subject is calling
	// Kill twice, so a third from the cleanup is exactly as harmless.
	t.Cleanup(w.Kill)
	w.mu.RLock()
	cg := w.cg
	w.mu.RUnlock()
	if cg == nil {
		t.Fatal("the arm reported itself active and the window still has no cgroup")
	}
	dir := cg.Dir()
	w.Kill()
	w.Kill()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stat %s after two kills: %v, want it gone", dir, err)
	}
	w.mu.RLock()
	left := w.cg
	w.mu.RUnlock()
	if left != nil {
		t.Errorf("the window still holds cgroup %s after Kill", left.Dir())
	}
}

// TestAFailedSpawnLeavesNoCgroup covers the path with no window on it. The cgroup
// is made before the spawn, so a spawn that fails has already created a directory,
// and nothing later will be holding a Window that remembers it: the caller gets an
// error and the cgroup would sit under the base until the daemon exited — at which
// point Close would refuse to remove a base with a child in it, so one failed spawn
// would cost the host a permanent directory.
//
// It is a delta rather than an absolute count, because the tests above are entitled
// to have left the base with windows in it.
func TestAFailedSpawnLeavesNoCgroup(t *testing.T) {
	initCgroupsOrSkip(t)
	base := baseDir(t)
	before := countDirs(t, base)
	if _, err := NewWindow(WindowOpts{Name: "nope",
		Command: []string{"/shelld-test-no-such-program"}, Cols: 80, Rows: 24}); err == nil {
		t.Fatal("spawning a program that does not exist succeeded")
	}
	if after := countDirs(t, base); after != before {
		t.Errorf("a failed spawn changed the cgroup count under %s from %d to %d",
			base, before, after)
	}
}

// TestTwoWindowsGetDifferentCgroups pins the counter. Window ids come from a
// process-wide counter that never repeats, and the reason is this test: two live
// windows handed the same id would be handed the same directory, and the second one
// would either adopt the first one's shell — making its Kill reach into another
// window's tree — or, since New refuses a populated directory, get no cgroup at all
// and silently lose the arm.
func TestTwoWindowsGetDifferentCgroups(t *testing.T) {
	initCgroupsOrSkip(t)
	dirs := make([]string, 2)
	for i := range dirs {
		w, err := NewWindow(WindowOpts{Name: "n" + strconv.Itoa(i),
			Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Kill()
		w.mu.RLock()
		cg := w.cg
		w.mu.RUnlock()
		if cg == nil {
			t.Fatal("the arm reported itself active and the window still has no cgroup")
		}
		dirs[i] = cg.Dir()
		// Read from the kernel: the two windows have to be in different cgroups, and
		// each shell has to be in its own window's.
		if got := cgroupOf(t, w.Pid()); !strings.HasSuffix(dirs[i], got) {
			t.Fatalf("window %d's shell is in cgroup %s, not in its own %s", i, got, dirs[i])
		}
	}
	if dirs[0] == dirs[1] {
		t.Errorf("two live windows share cgroup %s", dirs[0])
	}
}

// TestSpawnDoesNotKeepTheCgroupHandle checks that the descriptor used to place the
// shell is closed once the shell exists.
//
// It is asserted by looking for the directory among this process's open
// descriptors, rather than by counting them: a count would have to tolerate every
// other fd a window opens and every goroutine that might be between two of them,
// and a test with a tolerance cannot see a leak of one. A window that kept the
// handle would cost the daemon one descriptor per window forever — the handle is
// deliberately not cached, so nothing would ever come back for it — and no other
// assertion in this file would notice.
func TestSpawnDoesNotKeepTheCgroupHandle(t *testing.T) {
	initCgroupsOrSkip(t)
	w, err := NewWindow(WindowOpts{Name: "fd",
		Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	w.mu.RLock()
	cg := w.cg
	w.mu.RUnlock()
	if cg == nil {
		t.Fatal("the arm reported itself active and the window still has no cgroup")
	}
	dir := cg.Dir()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	for _, e := range ents {
		// A descriptor can close between ReadDir and Readlink, which is an ENOENT
		// and not a finding.
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && target == dir {
			t.Errorf("fd %s is still open on the window's cgroup %s after the spawn",
				e.Name(), dir)
		}
	}
}

// baseDir is the daemon's base directory. Reached through the package variable
// because the test is checking what is on the filesystem under it, which is not
// something a window can be asked about.
func baseDir(t *testing.T) string {
	t.Helper()
	cgMu.Lock()
	defer cgMu.Unlock()
	if cgManager == nil {
		t.Fatal("no cgroup manager")
	}
	return cgManager.Base()
}

// countDirs counts the cgroups directly under dir.
func countDirs(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// alive reports whether a pid exists. Signal 0 rather than a /proc stat because
// this is asked in a loop.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// procsOf is the pids in a cgroup, and whether the directory still exists. A read
// that fails for any reason other than the directory being gone is fatal rather
// than reported as "empty": the difference between those two is the assertion.
func procsOf(t *testing.T, dir string) (pids []string, exists bool) {
	t.Helper()
	b, err := os.ReadFile(dir + "/cgroup.procs")
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read cgroup.procs in %s: %v", dir, err)
	}
	return strings.Fields(string(b)), true
}

// dirOfCgroup is the window's own cgroup path, or "" if it has none.
func dirOfCgroup(w *Window) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.cg == nil {
		return ""
	}
	return w.cg.Dir()
}

// A window whose shell exits on its own must not leave its cgroup behind. Nothing
// else removes it: GC skips any base whose daemon is still live, which includes our
// own, so without the release at EOF the directory survived until shutdown — one
// per window that was never explicitly killed, for the life of the daemon.
func TestANaturalExitReleasesTheCgroup(t *testing.T) {
	initCgroupsOrSkip(t)
	// The shell blocks so this window's own directory can be named before it exits.
	// Counting the directories under the base instead would not be an assertion
	// about THIS window: the base is shared, so the count comes back whenever any
	// sibling goes away, and a test running alongside can satisfy it while this
	// window's cgroup is still sitting there.
	w, err := NewWindow(WindowOpts{Name: "natural", Command: []string{"/bin/sh", "-c", "read x; exit 0"},
		Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	dir := dirOfCgroup(w)
	if dir == "" {
		t.Fatal("the cgroup arm reported itself active and this window has no cgroup")
	}
	if err := w.SendKeys("", true, nil); err != nil { // let the shell exit on its own
		t.Fatalf("release the shell: %v", err)
	}
	<-w.Done()
	// The window reports not-alive before the release runs, so waiting on the
	// window is not enough. Wait for this window's own directory, by name, which
	// also makes this say the release is prompt rather than merely eventual.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(dir)
		return errors.Is(err, os.ErrNotExist)
	}, "the window's cgroup outlived a natural exit: "+dir)
}

// A job that outlives its shell keeps running, and its cgroup is left in place. The
// release at EOF removes an EMPTY directory and nothing else: it must never be what
// kills a process nobody asked to kill. That is the whole difference between it and
// reapCgroup, and it is the policy cgroup.GC and cgroup.Manager.Close also hold to.
//
// The survivor is placed by this test rather than started by the window's shell,
// because a background job of that shell does not survive it: the shell is the
// session leader for the pty, so its exit has the kernel send SIGHUP to the
// foreground process group, and every arrangement of setsid and redirection tried
// here still left the cgroup empty — measured by scanning /proc for live processes
// naming the cgroup, not only by reading cgroup.procs. Writing a pid this test owns
// into cgroup.procs states the premise as an assertion instead of hoping a shell
// produces it.
func TestAJobThatOutlivesItsShellIsLeftRunning(t *testing.T) {
	initCgroupsOrSkip(t)
	// The shell BLOCKS until this test releases it. An `exit 0` shell races the
	// placement below: it can reach EOF first, the release then finds an empty
	// cgroup and removes it, and the placement fails — a failure to construct the
	// premise, which would read as "cannot test" rather than as a defect.
	w, err := NewWindow(WindowOpts{Name: "survivor", Command: []string{"/bin/sh", "-c", "read x; exit 0"},
		Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	dir := dirOfCgroup(w)
	if dir == "" {
		t.Fatal("the cgroup arm reported itself active and this window has no cgroup")
	}

	sleeper := exec.Command("/bin/sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := sleeper.Process.Pid
	t.Cleanup(func() { _ = sleeper.Process.Kill() })
	if err := os.WriteFile(dir+"/cgroup.procs", []byte(strconv.Itoa(pid)), 0); err != nil {
		t.Fatalf("move pid %d into %s: %v", pid, dir, err)
	}
	if got, _ := procsOf(t, dir); !slices.Contains(got, strconv.Itoa(pid)) {
		t.Fatalf("pid %d did not land in %s (holds %v), so there is no survivor to protect",
			pid, dir, got)
	}
	// An empty nested cgroup, which is how this test knows the release RAN rather
	// than merely that it has not damaged anything yet. Release is depth-first and
	// removes what is empty, so it takes this child and leaves the populated parent:
	// the child's disappearance is a positive signal, produced by the code under
	// test, that the decision below was actually reached. Waiting a fixed 200ms
	// instead is what a release that killed the survivor a second later would pass.
	witness := dir + "/probe"
	if err := os.Mkdir(witness, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", witness, err)
	}

	if err := w.SendKeys("", true, nil); err != nil { // let the shell go
		t.Fatalf("release the shell: %v", err)
	}
	<-w.Done()
	waitFor(t, 5*time.Second, func() bool { return !w.IsAlive() }, "the terminal never closed")
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(witness)
		return errors.Is(err, os.ErrNotExist)
	}, "the release never ran: "+witness+" is still there")

	// The survivor has to be alive and STAY alive, not merely be alive at the
	// instant the witness fires. The witness is emitted from inside Release, so
	// anything the release does after Release RETURNS happens after it -- and a kill
	// there was measured to survive a single sampling of liveness 1 run in 5. A held
	// window is what makes the assertion the absence of a kill rather than its
	// absence so far; it is short because it only has to cover the rest of one
	// function, which the witness has already shown was entered.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		pids, exists := procsOf(t, dir)
		switch {
		case !exists:
			t.Fatalf("the release removed %s while pid %d was still in it", dir, pid)
		case !slices.Contains(pids, strconv.Itoa(pid)):
			t.Fatalf("pid %d is no longer in %s (holds %v), so the release moved or killed it",
				pid, dir, pids)
		case !alive(pid):
			t.Fatalf("the release killed pid %d", pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// And the cgroup is still the window's, so an explicit Kill keeps the one arm
	// that reaches a process the /proc union cannot see. This survivor is such a
	// process by construction: it is a child of the test binary, not of the shell.
	if got := dirOfCgroup(w); got != dir {
		t.Fatalf("the window gave up its cgroup (%q) with pid %d still in it", got, pid)
	}
	w.Kill()
	if _, exists := procsOf(t, dir); exists {
		t.Errorf("Kill left %s behind", dir)
	}
	// Reaped before being judged: a SIGKILLed child of this process stays a zombie
	// until it is waited for, and kill(pid, 0) succeeds on a zombie, so `alive`
	// cannot tell "dead" from "not yet reaped".
	st, err := sleeper.Process.Wait()
	if err != nil {
		t.Fatalf("wait for pid %d: %v", pid, err)
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("pid %d ended as %v, want killed by SIGKILL — Kill did not reach it through "+
			"the cgroup, which is the only arm that could", pid, st)
	}
}
