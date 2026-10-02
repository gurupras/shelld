//go:build linux

package shelld

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// reapCgroup takes the cgroup off the window before killing it, so that a second
// Kill finds nothing to do. That is right once the kill has happened — and a kill
// that FAILED has not happened. Dropping the handle there retired, for the window's
// whole life, the one arm that reaches a process which has both setsid'd away and
// lost its parent: a retried Window.Kill became a silent no-op over a tree still
// running, and the directory stayed under the base until the daemon exited.
//
// The failure is produced rather than simulated. chmod 0400 on a real cgroup.kill is
// accepted by kernfs and makes the write EACCES while the cgroup still exists and
// still holds the shell, which is the state the bug needs: a live container the
// window has forgotten. Nothing here is stubbed, so what the test exercises is the
// same code path a full disk or a revoked delegation would take.
//
// TestKillTwiceIsQuiet is the control for the same mutant and must stay green: it is
// the successful kill, where dropping the handle is correct and the directory goes.
// The second subtest below repeats that inline so the pairing is visible in one
// place.
func TestAFailedCgroupKillKeepsTheHandle(t *testing.T) {
	initCgroupsOrSkip(t)

	t.Run("the kill fails, so the window keeps its cgroup", func(t *testing.T) {
		// The shell holds a SETSID'D ORPHAN as well as itself, and that is what makes
		// this subtest's premise hold instead of race. An ordinary shell is reached by
		// Kill's /proc union whether or not the cgroup write succeeded, so the cgroup
		// empties, the read loop reaches EOF, and releaseCgroup removes the directory
		// — which the retained handle is precisely what enables. The directory then
		// vanishes underneath the steps below: observed 2026-09-29 as ENOENT from the
		// chmod that restores cgroup.kill, on a run whose assertions had all passed.
		//
		// The orphan is also the case the cgroup arm exists for at all — a process
		// that both setsid'd away and lost its parent — so the fixture that stops the
		// race is the same one that reaches the arm. With it in the cgroup the failed
		// kill leaves a member the union cannot touch, releaseCgroup reports it as
		// left running and keeps the handle, and the directory stays put because it
		// is genuinely still populated.
		w, err := NewWindow(WindowOpts{Name: "killfail",
			Command: []string{"/bin/sh", "-c", "(setsid sleep 300 >/dev/null 2>&1 &) ; exec sleep 300"},
			Cols:    80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		pid := w.Pid()
		dir := dirOfCgroup(w)
		if dir == "" {
			t.Fatal("the arm reported itself active and the window still has no cgroup")
		}
		killFile := filepath.Join(dir, "cgroup.kill")

		// The fixture is checked rather than assumed. Every assertion below still
		// passes on a lucky interleaving without the orphan, so a setsid that did not
		// run — missing binary, a /bin/sh that parsed the line differently — would
		// leave this subtest reporting a green it had not earned, which is the state
		// it was in before.
		orphanDeadline := time.Now().Add(5 * time.Second)
		for {
			pids, exists := procsOf(t, dir)
			if !exists {
				t.Fatalf("%s went away while waiting for the orphan to join it", dir)
			}
			if len(pids) >= 2 {
				break
			}
			if !time.Now().Before(orphanDeadline) {
				t.Fatalf("%s holds %d pid(s) after 5s, want the shell and a setsid'd "+
					"orphan: the fixture never reached the case this subtest is about, "+
					"so its result would say nothing about the cgroup arm", dir, len(pids))
			}
			time.Sleep(5 * time.Millisecond)
		}

		// Registered before the chmod, and it kills the pid directly as well: if an
		// assertion below t.Fatals with the file still read-only, w.Kill cannot
		// remove the directory and the shell would outlive the test binary.
		//
		// The chmod comes first because of the orphan: it is in the cgroup but not in
		// the window's process tree, so cgroup.kill is the only thing here that
		// reaches it — the direct kill below is the shell's pid alone. If the chmod
		// itself fails the orphan survives, bounded by its own sleep rather than by
		// anything this test does, which is why it is a sleep and not a loop.
		t.Cleanup(func() {
			_ = os.Chmod(killFile, 0o200)
			w.Kill()
			// Guarded, because the argument is not a constant: Pid() answers 0 for a
			// window whose process is already gone, and kill(0, SIGKILL) is this test
			// binary's own process group.
			if pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
		if err := os.Chmod(killFile, 0o400); err != nil {
			t.Skipf("cannot make %s unwritable, so a failing kill cannot be staged: %v", killFile, err)
		}

		w.Kill()

		// The directory surviving is what says the kill really failed: reapCgroup
		// returns before WaitEmpty and Remove on that arm, so a Kill that got past
		// c.Kill() would have taken it. Without this the assertion below could pass
		// on a Kill that never reached the cgroup at all.
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("stat %s after a kill that should have failed: %v — the kill "+
				"did not fail, so this test is not measuring the failure path", dir, err)
		}
		if got := dirOfCgroup(w); got != dir {
			t.Errorf("the window holds cgroup %q after a failed kill, want %q: the "+
				"handle was dropped, so the cgroup arm is retired for this window's "+
				"life while %s is still there holding its tree", got, dir, dir)
		}

		// The consequence, and the reason the field matters: with the handle back,
		// the retry that a caller would make is the one that works. Under the bug
		// this second Kill is a no-op and the directory is still there afterwards.
		if err := os.Chmod(killFile, 0o200); err != nil {
			t.Fatalf("restore %s: %v", killFile, err)
		}
		w.Kill()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("stat %s after the retried kill: %v, want it gone: the retained "+
				"handle did not make the retry effective", dir, err)
		}
	})

	t.Run("the kill succeeds, so the window lets it go", func(t *testing.T) {
		w, err := NewWindow(WindowOpts{Name: "killok",
			Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Kill)
		dir := dirOfCgroup(w)
		if dir == "" {
			t.Fatal("the arm reported itself active and the window still has no cgroup")
		}
		w.Kill()
		if got := dirOfCgroup(w); got != "" {
			t.Errorf("the window still holds cgroup %q after a kill that succeeded", got)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("stat %s after a kill that succeeded: %v, want it gone", dir, err)
		}
	})
}

// The other way reapCgroup's teardown stops early, and the one where dropping the
// handle costs a removal that would have worked. WaitEmpty returns the error from
// READING cgroup.events as well as its own timeout, so an unreadable cgroup.events
// ends the teardown with the members already dead and the directory removable. With
// the handle gone nothing in this daemon's life revisits it: releaseCgroup at EOF
// finds w.cg nil and returns, a retried Kill is a no-op, and GC skips any base whose
// daemon is still live. The directory survives to shutdown.
//
// What separates this from the test above is WHERE the failure is: cgroup.kill stays
// writable and only cgroup.events is made unreadable, so the kill succeeds and the
// drain check is what fails. The shell therefore dies, which is why the assertion is
// on the DIRECTORY and not on the handle — the read loop reaches EOF and
// releaseCgroup may clear the handle concurrently, which is the fix working rather
// than a violation. Not leaking the directory is the property both paths agree on.
//
// The poll is generous on purpose. Under the bug no amount of waiting removes the
// directory, so the bound buys a slow red, never a false green.
func TestADrainCheckThatCannotReadKeepsTheCgroupCollectable(t *testing.T) {
	initCgroupsOrSkip(t)

	w, err := NewWindow(WindowOpts{Name: "drainfail",
		Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pid := w.Pid()
	dir := dirOfCgroup(w)
	if dir == "" {
		t.Fatal("the arm reported itself active and the window still has no cgroup")
	}
	events := filepath.Join(dir, "cgroup.events")

	// Registered before the chmod, for the same reason as the test above: an
	// assertion that t.Fatals with cgroup.events still unreadable would otherwise
	// leave the drain check failing for every later attempt on this directory.
	t.Cleanup(func() {
		_ = os.Chmod(events, 0o444)
		w.Kill()
		// Guarded: Pid() answers 0 for a window whose process is already gone, and
		// kill(0, SIGKILL) is this test binary's own process group.
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := os.Chmod(events, 0o200); err != nil {
		t.Skipf("cannot make %s unreadable, so a failing drain check cannot be staged: %v", events, err)
	}

	w.Kill()

	// Says the failure really landed in the drain check rather than in the kill: a
	// kill that got through leaves nothing alive in there, so cgroup.procs is empty
	// while the directory is still present. Without this the assertion below could
	// pass on a Kill that never reached the cgroup at all.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stat %s after a kill whose drain check should have failed: %v — the "+
			"directory went, so this test is not measuring the drain-check path", dir, err)
	}

	// The restore models the transient that the unreadable file stands in for: what
	// the fix has to deliver is that the removal still happens once reading works
	// again, by either of the two paths that hold the handle.
	if err := os.Chmod(events, 0o444); err != nil {
		t.Fatalf("restore %s: %v", events, err)
	}
	w.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s is still there 10s after a kill whose drain check failed: the "+
				"handle was dropped, so neither the EOF release nor a retried Kill can "+
				"remove it and it survives until this daemon exits", dir)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
