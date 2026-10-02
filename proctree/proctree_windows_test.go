//go:build windows

package proctree

import "testing"

// This package reported no test files at all on Windows, which is the shape that
// hid a KillTree that killed nothing on darwin: a package with no tests produces a
// run indistinguishable from one that passes. The Windows contract is that the
// oracle is absent and says so, and that is worth a test of its own, because the
// hazard is not a wrong answer -- it is a stub that starts returning a confident
// one.

// KillTree must report false rather than claim a kill it did not perform. false is
// what sends the caller to pty.PTY.Kill, which terminates the kill-on-close job
// the window's child is assigned to; a true here would skip that and kill nothing.
func TestKillTreeReportsUnsupportedOnWindows(t *testing.T) {
	pids, ok := KillTree(1234, 0)
	if ok {
		t.Fatalf("KillTree reported supported on windows (pids %v); callers would skip the job-object kill", pids)
	}
	if len(pids) != 0 {
		t.Fatalf("KillTree returned %v alongside ok=false, want no pids", pids)
	}
}

// StartTicks has no Windows equivalent, so it must not invent a value a caller
// could mistake for an identity -- least of all 0 with ok=true, which every pid
// would then share.
func TestStartTicksReportsUnsupportedOnWindows(t *testing.T) {
	for _, pid := range []int{1, 4, 1234, 0, -1} {
		if start, ok := StartTicks(pid); ok {
			t.Fatalf("StartTicks(%d) = (%d, true), want ok=false on windows", pid, start)
		}
	}
}

// Descendants answers root alone: the enumeration is what Windows lacks, and an
// empty idMap makes the union degenerate to exactly that. A positive root is
// echoed back; a non-positive one is still refused, since that guard is in the
// shared code and not the platform file.
func TestDescendantsIsRootAloneOnWindows(t *testing.T) {
	if got := Descendants(1234); len(got) != 1 || got[0] != 1234 {
		t.Fatalf("Descendants(1234) = %v, want [1234]", got)
	}
	for _, root := range []int{0, -1} {
		if got := Descendants(root); len(got) != 0 {
			t.Fatalf("Descendants(%d) = %v, want empty", root, got)
		}
	}
}
