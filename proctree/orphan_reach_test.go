//go:build linux || darwin

package proctree

// The one end-to-end check that the oracle reaches the process the package was
// written for: a grandchild whose parent exited, so that no parent link leads to
// root any more, and which is found only by the session it never left.
//
// This is deliberately not linux-only, unlike every other test here. The whole
// oracle was dead on darwin — StartTicks read /proc/<pid>/stat, so it answered
// false for every pid and KillTree refused every root while reporting success —
// and no test in this package could see that, because none of them ran there. A
// test that only runs where the thing already worked cannot report that it stopped
// working somewhere else. Running the same assertions on both platforms is what
// makes the darwin ids (platform_darwin.go) checked rather than merely written.
//
// It signals nothing through the code under test: the reach assertions use
// Descendants, which is pure, and the kill assertion uses the killProc recorder.
// The fixture's own processes are cleaned up by explicit signals to pids this test
// started, which is the only thing it is entitled to signal.

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startOrphanFixture builds leader -> (intermediate, exits) -> grandchild and
// returns both live pids. leader is a session leader made the way pty.Start makes
// one, SysProcAttr{Setsid: true}, because that is the premise the session term
// rests on and a fixture that got its session some other way would be testing a
// shape production never has.
//
// setsid(1) is not used to build it. That binary is util-linux and does not exist
// on macOS, which is how the root package's own grandchild test came to skip there;
// the syscall is available on both.
func startOrphanFixture(t *testing.T) (leader, grandchild int) {
	t.Helper()
	pidFile := t.TempDir() + "/gc.pid"
	// The inner sh backgrounds the grandchild, records it, and exits, which is what
	// orphans it. The outer sh then blocks, so root stays alive to be asked about.
	script := "sh -c 'sleep 300 & echo $! > " + pidFile + "' ; exec sleep 300"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the fixture: %v", err)
	}
	leader = cmd.Process.Pid
	t.Cleanup(func() {
		// Explicit, because the assertions below run against a recorder and so
		// signal nothing themselves. Both pids were started by this test.
		for _, pid := range []int{grandchild, leader} {
			if pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_, _ = cmd.Process.Wait()
	})

	// Wait for the pid file rather than sleeping a fixed time: the orphaning is a
	// race against this test, and a fixed sleep would make the fixture's absence
	// look like the oracle's failure.
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				grandchild = pid
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fixture never recorded a grandchild pid in %s", pidFile)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And wait for the reparent, which is the state under test. Until the
	// intermediate is reaped the grandchild is still in root's parent closure, and
	// the test would pass without the session term ever being consulted.
	for {
		if ppid := idMap()[grandchild].ppid; ppid != 0 && ppid != leader {
			return leader, grandchild
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still has leader %d in its parent chain, so it was "+
				"never orphaned and this fixture cannot test the session term",
				grandchild, leader)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartTicksAnswersForALiveProcess is the narrow regression guard, and it is
// the one that failed on darwin before platform_darwin.go existed. It is separate
// from the reach test because its failure has a different cause and a different
// fix: KillTree refuses a root it cannot identify, so a StartTicks that answers
// false for everything turns the whole package into a no-op that reports success,
// no matter how well the enumeration works.
func TestStartTicksAnswersForALiveProcess(t *testing.T) {
	leader, grandchild := startOrphanFixture(t)
	for _, pid := range []int{os.Getpid(), leader, grandchild} {
		start, ok := StartTicks(pid)
		if !ok {
			t.Errorf("StartTicks(%d) = _, false for a process that is running: "+
				"KillTree refuses a root it cannot identify, so this makes it kill nothing", pid)
			continue
		}
		if start == 0 {
			t.Errorf("StartTicks(%d) = 0, true: zero is what an unset field reads as, and "+
				"KillTree compares for equality, so a start time every process shares "+
				"identifies none of them", pid)
		}
	}
	// Stability, which is the property the guard actually rests on, and NOT
	// distinctness between processes. An earlier version of this test asserted the
	// latter and failed on Linux: start times there are USER_HZ ticks since boot, so
	// the leader and its grandchild, forked 2 ms apart, both read 12774341. That
	// costs nothing, because the comparison is only ever made between two readings
	// of ONE pid — a collision would need a full pid_max wrap inside a single 10 ms
	// tick. Asserting distinctness would have been a bound the design does not claim.
	first, _ := StartTicks(leader)
	again, ok := StartTicks(leader)
	if !ok || first != again {
		t.Errorf("StartTicks(%d) returned %d then %d, ok=%v: KillTree compares a stored "+
			"reading against a fresh one, so a value that moves on its own makes it "+
			"refuse a root that is still there", leader, first, again, ok)
	}
}

// TestDescendantsReachesAnOrphanedGrandchild is the reach itself. The parent
// closure cannot find this process — the fixture asserts that its parent link no
// longer leads to root before getting here — so an answer that holds it came from
// the session term, and an answer that does not means the session term is off on
// this platform.
func TestDescendantsReachesAnOrphanedGrandchild(t *testing.T) {
	leader, grandchild := startOrphanFixture(t)

	ids := idMap()
	if got := ids[leader].sid; got != leader {
		t.Fatalf("leader %d reports sid %d, so it does not lead its session and the "+
			"session term is not even consulted; the fixture, not the oracle, is wrong",
			leader, got)
	}
	if got := ids[grandchild].sid; got != leader {
		t.Fatalf("orphaned grandchild %d reports sid %d, want the leader's pid %d: "+
			"a reparent does not change a session, so either this platform has no "+
			"session ids or it is reporting the wrong one", grandchild, got, leader)
	}
	if got := ids[grandchild].ppid; got == leader {
		t.Fatalf("grandchild %d still has ppid %d, so it is in the parent closure and "+
			"this test proves nothing about the session term", grandchild, got)
	}

	got := Descendants(leader)
	if !contains(got, grandchild) {
		t.Errorf("Descendants(%d) = %v, which does not hold the orphaned grandchild %d: "+
			"this is the process the package exists to kill, and the window's shell "+
			"would die leaving it running", leader, got, grandchild)
	}
	if !contains(got, leader) {
		t.Errorf("Descendants(%d) = %v, which does not hold root itself", leader, got)
	}
}

// TestKillTreeNamesAnOrphanedGrandchild closes the loop: the reach above is only
// worth something if the pids reach killProc. Against the recorder, so the fixture
// survives the assertion and nothing is signalled by the code under test.
func TestKillTreeNamesAnOrphanedGrandchild(t *testing.T) {
	leader, grandchild := startOrphanFixture(t)
	var ok bool
	var killed []int
	got := withRecordedKills(t, func() {
		start, sok := StartTicks(leader)
		if !sok {
			t.Fatalf("StartTicks(%d) failed, so KillTree would refuse this root for a "+
				"reason that has nothing to do with the tree", leader)
		}
		killed, ok = KillTree(leader, start)
	})
	if !ok {
		t.Fatalf("KillTree(%d) reported unsupported on a platform whose oracle answered", leader)
	}
	if !contains(killed, grandchild) {
		t.Errorf("KillTree(%d) returned %v, which omits the orphaned grandchild %d", leader, killed, grandchild)
	}
	if !contains(got, grandchild) {
		t.Errorf("KillTree(%d) signalled %v, which omits the orphaned grandchild %d: it "+
			"was named in the answer but never sent a signal", leader, got, grandchild)
	}
	if !contains(got, leader) {
		t.Errorf("KillTree(%d) signalled %v, which omits root itself", leader, got)
	}
}

func contains(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
