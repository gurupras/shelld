//go:build linux

package proctree

// These tests need the one-line `var procRoot = "/proc"` seam in
// proctree_unix.go. They reach the two states real /proc never produces on this
// box (verified: 0 of 937 live pids have a readable task directory with no
// readable children file), and they are the only way to kill mutants M-E2, M-L
// and M-M.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc builds a /proc-shaped tree: tasks[pid] maps a tid to the contents of
// its children file, and a nil value means the children file is absent.
func fakeProc(t *testing.T, tasks map[int]map[int]*string) string {
	t.Helper()
	root := t.TempDir()
	for pid, tids := range tasks {
		for tid, content := range tids {
			d := filepath.Join(root, strconv.Itoa(pid), "task", strconv.Itoa(tid))
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if content != nil {
				if err := os.WriteFile(filepath.Join(d, "children"), []byte(*content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	return root
}

func str(s string) *string { return &s }

// TestChildrenOfUnreadableChildrenFileIsNotRead kills mutants "M-E2"
// (read := true instead of false), "M-M" (read = true set before the ReadFile
// error check) and "M-L" (returning nil,false on a per-task read error instead
// of continuing). The task directory exists and lists a tid, but that tid has no
// children file. childrenOf must report false so Descendants falls back to the
// snapshot; reporting true makes fastDescendants answer "a tree of one" and
// KillTree then leaks every descendant.
func TestChildrenOfUnreadableChildrenFileIsNotRead(t *testing.T) {
	fakeProc(t, map[int]map[int]*string{
		100: {100: nil}, // sole task, no children file
	})
	kids, ok := childrenOf(100)
	if ok {
		t.Errorf("childrenOf(100) = %v, true; the sole task has no children file, want false "+
			"so Descendants falls back to the snapshot", kids)
	}
	if _, ok := fastDescendants(100); ok {
		t.Error("fastDescendants(100) reported true with no readable children file; " +
			"it must decline so the snapshot answers")
	}
}

// TestChildrenOfSkipsOneUnreadableTask is the other half of the M-L contract: a
// single unreadable task must not discard the tasks that did read. M-L returns
// nil,false here and loses two real children.
func TestChildrenOfSkipsOneUnreadableTask(t *testing.T) {
	fakeProc(t, map[int]map[int]*string{
		100: {
			100: nil,           // no children file
			101: str("201 \n"), // this one has children
			102: str("202\n"),
		},
	})
	kids, ok := childrenOf(100)
	if !ok {
		t.Fatalf("childrenOf(100) = %v, false; two of three tasks are readable, want true", kids)
	}
	got := setOf(kids)
	for _, want := range []int{201, 202} {
		if !got[want] {
			t.Errorf("child %d missing from childrenOf(100) = %v: one unreadable task "+
				"must not discard the readable ones", want, kids)
		}
	}
}

// TestDescendantsNeverReturnsANonPositivePid is not a mutant kill but a gap:
// KillTree calls syscall.Kill on whatever Descendants returns, and Kill(0)
// signals the caller's own process group while Kill(-n) signals group n. Nothing
// in the package filters the parsed values, so a parse-tolerance change — mutant
// "M-J", ignoring strconv.Atoi's error, which is unkillable against real /proc
// because the children file is always well-formed decimal — escalates from a
// missed pid to killing the daemon and everything beside it.
func TestDescendantsNeverReturnsANonPositivePid(t *testing.T) {
	fakeProc(t, map[int]map[int]*string{
		100: {100: str("0 -1 201 abc\n")},
		201: {201: str("")},
	})
	pids, ok := fastDescendants(100)
	if !ok {
		t.Fatal("fastDescendants(100) declined on a readable children file")
	}
	for _, pid := range pids {
		if pid <= 0 {
			t.Errorf("fastDescendants returned pid %d in %v; KillTree would signal a whole "+
				"process group, not one process", pid, pids)
		}
	}
	if !setOf(pids)[201] {
		t.Errorf("the one real child 201 is missing from %v", pids)
	}
}
