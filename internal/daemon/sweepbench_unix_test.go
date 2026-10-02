//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkReleaseKeptSweep measures the sweep at Release with n descriptors
// parked for an inode this process still holds -- the case where the sweep does
// no work and must still decide that, which is what a daemon serving a second
// socket pays on every Release. The parking is done directly rather than through
// the swap race, because the race's rate is not the quantity under test.
func BenchmarkReleaseKeptSweep(b *testing.B) {
	dir := b.TempDir()
	socket := filepath.Join(dir, "s.sock")
	l, err := AcquireInstance(socket)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Release()

	const n = 2000
	heldLocks.Lock()
	for range n {
		f, err := os.OpenFile(lockPath(socket), os.O_RDWR, 0o600)
		if err != nil {
			heldLocks.Unlock()
			b.Fatal(err)
		}
		st, err := f.Stat()
		if err != nil {
			heldLocks.Unlock()
			b.Fatal(err)
		}
		id, ok := lockIDOf(st)
		if !ok {
			heldLocks.Unlock()
			b.Fatal("no inode for the parked descriptor")
		}
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f, id: id})
	}
	if len(keptLockFiles) != n {
		heldLocks.Unlock()
		b.Fatalf("parked %d, want %d", len(keptLockFiles), n)
	}
	heldLocks.Unlock()
	// Cleanup runs after the deferred Release, so by then nothing holds the inode
	// and these are ordinary descriptors to close. Closing them matters: n of them
	// per run would reach the descriptor limit under -count.
	b.Cleanup(func() {
		heldLocks.Lock()
		for _, k := range keptLockFiles {
			k.f.Close()
		}
		keptLockFiles = nil
		heldLocks.Unlock()
	})

	heldLocks.Lock()
	defer heldLocks.Unlock()
	b.ResetTimer()
	for b.Loop() {
		releaseKeptLocked()
	}
	b.StopTimer()
	// A sweep that dropped the entries would be measuring an empty slice from the
	// second iteration on, which is the one way this benchmark could read fast for
	// the wrong reason.
	if len(keptLockFiles) != n {
		b.Fatalf("after the sweep %d descriptors are parked, want all %d kept: the benchmark measured an emptied slice", len(keptLockFiles), n)
	}
}
