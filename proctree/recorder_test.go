//go:build linux || darwin

package proctree

import (
	"syscall"
	"testing"
)

// withRecordedKills swaps killProc for a recorder and returns the pids KillTree
// asked to signal. Restored via t.Cleanup rather than defer, so a t.Fatalf inside
// the body cannot leave the package's kill path pointed at a test's closure for
// every later test in the binary.
//
// It is here rather than beside its first caller because both platforms' tests
// need it: guard_test.go is Linux-only and about /proc-shaped inputs, while
// orphan_reach_test.go runs on darwin as well, and a second copy of a kill
// recorder is the last thing this package should have two of.
func withRecordedKills(t *testing.T, body func()) []int {
	t.Helper()
	orig := killProc
	var got []int
	killProc = func(pid int, sig syscall.Signal) error {
		got = append(got, pid)
		return nil
	}
	t.Cleanup(func() { killProc = orig })
	body()
	return got
}
