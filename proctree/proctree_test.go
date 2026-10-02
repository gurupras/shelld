//go:build linux

package proctree

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestSelfInParentMap(t *testing.T) {
	m := parentMap()
	ppid, ok := m[os.Getpid()]
	if !ok {
		t.Fatal("own pid missing from /proc map")
	}
	if ppid != os.Getppid() {
		t.Fatalf("parsed ppid %d, getppid %d", ppid, os.Getppid())
	}
}

// TestKillTreeZeroIsNoop runs against the killProc recorder, and that is not
// belt-and-braces. As written it called KillTree(0) for real, and the only thing
// standing between it and syscall.Kill(pid, SIGKILL) for every process on the
// machine was the guard it was checking — root 0's parent closure is the whole box,
// because init's ppid in /proc is 0. A test whose failure mode is "running the suite
// kills the machine" cannot be the witness for that guard. guard_test.go covers the
// negative roots the same way; this one is kept because it is the case the package
// shipped with a name for.
func TestKillTreeZeroIsNoop(t *testing.T) {
	var pids []int
	var ok bool
	signalled := withRecordedKills(t, func() { pids, ok = KillTree(0, 0) })
	if !ok || len(pids) != 0 {
		t.Fatalf("KillTree(0) = %v, %v", first(pids, 8), ok)
	}
	if len(signalled) != 0 {
		t.Fatalf("KillTree(0) signalled %d pids: %v", len(signalled), first(signalled, 8))
	}
}

func TestKillTreeReapsGrandchildren(t *testing.T) {
	// startPgroupTree, rather than a bare exec.Command, for two reasons. It puts
	// the shell in its own process group so cleanup can killpg it: as written this
	// test had no cleanup at all, so its "expected root plus two sleeps" Fatalf
	// leaked three processes for the life of the machine. And it waits on a
	// /proc-derived process-group count instead of on Descendants, so the wait no
	// longer asks the code under test whether the fixture is ready.
	root, cmd := startPgroupTree(t, "sleep 300 & sleep 300 & wait", 3)
	before := Descendants(root)
	if len(before) < 3 {
		t.Fatalf("expected root plus two sleeps, got %v", before)
	}
	killed, _ := killTreeNow(t, root)
	// Not decoration: the loop below counts survivors among `killed`, so a KillTree
	// that named nothing at all leaves alive == 0 and this test passes vacuously —
	// it asserts a property of the code's own answer against that same answer. Three
	// processes are what Descendants counted a moment ago and none of them can exit
	// on their own, so the answer must not have shrunk. Found by a mutant that made
	// KillTree refuse every root: it reddened four of the five kill tests and this
	// one stayed green.
	if len(killed) < len(before) {
		t.Fatalf("KillTree named %d pids, fewer than the %d Descendants saw a moment "+
			"earlier: %v vs %v", len(killed), len(before), killed, before)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for {
		alive := 0
		for _, pid := range killed {
			if pid == root {
				continue
			}
			if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
				alive++
			}
		}
		if alive == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d descendants survived KillTree", alive)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
