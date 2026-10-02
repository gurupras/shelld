//go:build linux

package proctree

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// setOf is a pid set, so the two discovery paths can be compared regardless of
// the order they visit siblings in.
func setOf(pids []int) map[int]bool {
	m := make(map[int]bool, len(pids))
	for _, p := range pids {
		m[p] = true
	}
	return m
}

func diff(a, b map[int]bool) []int {
	var out []int
	for p := range a {
		if !b[p] {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// sessionOf reads a pid's session id, /proc/<pid>/stat's fourth field after the
// comm (state, ppid, pgrp, session). Go's syscall package has no Getsid.
func sessionOf(pid int) (int, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 4 {
		return 0, false
	}
	sid, err := strconv.Atoi(f[3])
	return sid, err == nil
}

// The walk-vs-snapshot comparison lives in bracket_test.go, not here. A single
// snapshot taken after the walk cannot support the comparison: the walk runs in
// 0.05 ms and the snapshot in ~15 ms, so anything forked in between is reported as
// "walk missed N pids", which is both a false positive (one run in ~430) and an
// assertion that cannot tell a lost subtree from a concurrent fork. Bracketing
// with a snapshot on each side rules on exactly the pids that were present
// throughout.
//
// Tree fixtures come from startPgroupTree in adv_walk_test.go, whose cleanup
// kills the process group directly instead of calling KillTree. A fixture that
// cleans up by invoking the code under test leaks precisely what a discovery bug
// leaks, which is when the cleanup matters most.

// TestKillTreeReapsSetsidEscapee covers the package's stated reason to exist: a
// grandchild that setsid'd into its own session, which a killpg of the PTY's
// session would leave running. Its parent link is intact at discovery time,
// which is what makes it findable at all.
func TestKillTreeReapsSetsidEscapee(t *testing.T) {
	// want is 2, not 3: the escapee setsid's out of root's process group, so it
	// is not one of the processes countPgroup can wait for. Waiting for it is
	// the escapee search below.
	root, _ := startPgroupTree(t, "setsid sleep 300 & sleep 300 & wait", 2)

	rootSid, ok := sessionOf(root)
	if !ok {
		t.Fatalf("cannot read root %d's session", root)
	}
	// The escapee is the descendant in a different session from root's. Poll for
	// it: setsid(1) forks and execs, so it is not in a new session the instant
	// the shell has spawned two children.
	var escapee int
	deadline := time.Now().Add(5 * time.Second)
	for escapee == 0 && time.Now().Before(deadline) {
		for _, pid := range Descendants(root) {
			if pid == root {
				continue
			}
			if sid, ok := sessionOf(pid); ok && sid != rootSid {
				escapee = pid
				break
			}
		}
		if escapee == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if escapee == 0 {
		t.Fatalf("no setsid'd descendant among %v; the test cannot see the defect it covers", Descendants(root))
	}
	// No cleanup for the escapee here on purpose. startPgroupTree enumerates the
	// tree with its own /proc walk before killing anything, so it reaches a
	// descendant that has left root's process group, and asserts it died. Killing
	// the escapee from this test would run first (cleanups are LIFO) and hide
	// whether the fixture can do it.

	killed, ok := killTreeNow(t, root)
	if !ok {
		t.Fatal("KillTree reported unsupported on linux")
	}
	if !setOf(killed)[escapee] {
		t.Fatalf("escapee %d not among the killed pids %v", escapee, killed)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(escapee)); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("setsid'd escapee %d survived KillTree", escapee)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// forked is a child and the task id of the thread that forked it.
type forked struct {
	cmd *exec.Cmd
	tid int
}

// forkedFromLockedThreads starts n sleeping children, each forked from a
// goroutine on a thread of its own, and kills them when the test ends. It is the
// fixture for everything here that depends on the kernel attributing a child to
// the forking thread: a threaded parent — the daemon is one — has its children
// spread over its tasks, and no tree built from shells can produce that, since
// /bin/sh has one thread.
//
// Every forker holds its thread until all of them do, and that barrier is what
// makes the spread structural. A locked thread runs nothing but its own
// goroutine, so the runtime cannot hand it on to the next forker the way it hands
// on an unlocked one, and has to find each a thread of its own. Locking, forking
// and unlocking one at a time instead leaves the spread to the scheduler:
// measured over 200 runs of each shape with n=8, the barrier gives 8 distinct
// tasks 200 times out of 200 at GOMAXPROCS=1 and above, while without it the same
// fixture gives 8 distinct tasks 0 times out of 200 at GOMAXPROCS=1 — every child
// forked from the one thread — and only 14 times out of 200 at GOMAXPROCS=2, with
// as few as 4.
//
// That n tasks came out distinct is checked here, once, rather than left to each
// caller to phrase as its own premise. Fatal and not Skip: the barrier makes it
// structural, so a run that got fewer means that stopped being true, and a skip
// would quietly stop testing the per-task read for good.
func forkedFromLockedThreads(t *testing.T, n int) []forked {
	t.Helper()
	ch := make(chan forked, n)
	var locked sync.WaitGroup
	locked.Add(n)
	fork := make(chan struct{})
	for range n {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			locked.Done()
			<-fork
			cmd := exec.Command("sleep", "300")
			if err := cmd.Start(); err != nil {
				ch <- forked{}
				return
			}
			ch <- forked{cmd, syscall.Gettid()}
		}()
	}
	locked.Wait()
	close(fork)
	var kids []forked
	tids := map[int]bool{}
	for range n {
		k := <-ch
		if k.cmd == nil {
			continue
		}
		kids = append(kids, k)
		tids[k.tid] = true
		t.Cleanup(func() {
			_ = k.cmd.Process.Kill()
			_, _ = k.cmd.Process.Wait()
		})
	}
	if len(kids) < n {
		t.Fatalf("only %d of %d children started", len(kids), n)
	}
	if len(tids) < n {
		t.Fatalf("%d children were forked from only %d distinct tasks (%v); one thread each is what this fixture is for, so nothing that rests on it is being exercised", n, len(tids), tids)
	}
	return kids
}

// TestChildrenOfReadsEveryTask exercises the reason childrenOf loops over
// /proc/<pid>/task at all: the kernel attributes a child to the thread that
// forked it, so a threaded parent — the daemon is one — has children spread
// across its tasks, and reading one task's list finds only that task's children.
// The test process forks its own children from locked threads and then requires
// childrenOf to report all of them. A shell-based tree cannot cover this;
// /bin/sh has one thread, which is how a version reading a single task passed
// the rest of this file.
func TestChildrenOfReadsEveryTask(t *testing.T) {
	requireChildrenLists(t)
	// One task per child is what makes reading a single task insufficient, and
	// forkedFromLockedThreads is what makes that a fact rather than a hope.
	kidsStarted := forkedFromLockedThreads(t, 8)
	kids, ok := childrenOf(os.Getpid())
	if !ok {
		t.Fatal("childrenOf on our own pid reported unreadable")
	}
	found := setOf(kids)
	for _, s := range kidsStarted {
		if !found[s.cmd.Process.Pid] {
			t.Errorf("child %d forked from task %d missing from childrenOf(%d) = %v",
				s.cmd.Process.Pid, s.tid, os.Getpid(), kids)
		}
	}
}

// TestChildrenOfReportsUnreadable pins the contract Descendants relies on to
// choose a path: an absent process yields false, so a kernel without the
// children lists and a dead root both fall back to the snapshot rather than
// being reported as a tree of one.
func TestChildrenOfReportsUnreadable(t *testing.T) {
	requireChildrenLists(t)
	// A pid that cannot exist: max_pid is at most 2^22.
	if kids, ok := childrenOf(1 << 23); ok {
		t.Errorf("childrenOf(impossible pid) = %v, true; want false so the caller falls back", kids)
	}
	if _, ok := fastDescendants(1 << 23); ok {
		t.Error("fastDescendants(impossible pid) reported true; a dead root must fall back")
	}
}
