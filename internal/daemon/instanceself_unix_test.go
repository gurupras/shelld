//go:build linux || darwin

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/testthreads"
)

// childCanLock reports whether another process can take socket's instance lock.
// The observer has to be another process: POSIX record locks are per-process, so
// this one is never in conflict with itself and an in-process F_SETLK would be
// granted whether or not the lock is still held.
func childCanLock(t *testing.T, socket string) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildTakesTheInstanceLock$")
	cmd.Env = append(os.Environ(), "SHELLD_TEST_LOCK_SOCKET="+socket)
	out, err := cmd.CombinedOutput()
	t.Logf("child: err=%v out=%s", err, out)
	return err == nil
}

// TestChildTakesTheInstanceLock is childCanLock's other half and is skipped
// unless it is running as that child.
func TestChildTakesTheInstanceLock(t *testing.T) {
	socket := os.Getenv("SHELLD_TEST_LOCK_SOCKET")
	if socket == "" {
		t.Skip("only meaningful as childCanLock's child process")
	}
	l, err := AcquireInstance(socket)
	if err != nil {
		t.Fatalf("the lock is held, as it should be: %v", err)
	}
	l.Release()
}

// TestInspectingTheLockDoesNotReleaseIt covers the two ways this process can
// touch a lock file it already holds. Closing any descriptor for a file drops
// every record lock the closing process holds on it, so a helper that opens the
// lock file to read its holder, or a second acquire whose eventual Release
// closes a second descriptor, would leave the socket unguarded while this daemon
// is still serving it — and the next daemon to start would bind over the top.
//
// Each half is bracketed by the same question put to a child process, so a
// release is observed rather than inferred: the child must be refused before the
// call, and must still be refused after it.
func TestInspectingTheLockDoesNotReleaseIt(t *testing.T) {
	socket := filepath.Join(shortDir(t), "s.sock")
	l, err := AcquireInstance(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if childCanLock(t, socket) {
		t.Fatal("setup: a child took the lock while this process holds it, so nothing here could observe a release")
	}

	// The lock is reported, not merely seen as free, and reading it costs nothing.
	holder, err := InstanceHolder(socket)
	if err != nil || holder != os.Getpid() {
		t.Errorf("InstanceHolder names pid %d (err %v) while this process (pid %d) holds the lock", holder, err, os.Getpid())
	}
	if childCanLock(t, socket) {
		t.Fatalf("after InstanceHolder(%s) a child took the instance lock this process still holds: reading the holder released it", socket)
	}

	// A second acquire is refused, so there is no second descriptor for a later
	// Release to close.
	second, err := AcquireInstance(socket)
	if err == nil {
		second.Release()
		t.Fatalf("AcquireInstance granted a second lock on %s to the process that already holds it; the first Release would then free the lock this one believes it has", socket)
	}
	if !errors.Is(err, ErrInstanceHeld) {
		t.Errorf("a second acquire failed with %v, which does not wrap ErrInstanceHeld, so a caller cannot tell it apart from a broken lock file", err)
	}
	if childCanLock(t, socket) {
		t.Fatalf("after a refused second acquire a child took the instance lock this process still holds: the refused attempt released it")
	}
}

// TestConcurrentAcquiresGrantExactlyOne is the race the registry has to win, and
// seeing it needs a barrier. Two acquires run one after the other are refused
// correctly even when the check and the lock are separate steps, because the first
// has finished registering before the second looks. Only overlapping acquires
// reach the state where both read the registry before either writes it, and the
// kernel then grants both, because a process never conflicts with itself.
//
// One grant is not a statistic to compare against a threshold. A second grant is
// one lock that two callers believe is theirs, and whichever releases first frees
// it for the other, so the bound is one; reps and racers only say how hard the
// overlap is looked for. The all-refused case is fatal rather than passing,
// because a rep where nothing was granted cannot have granted anything twice.
func TestConcurrentAcquiresGrantExactlyOne(t *testing.T) {
	const reps = 20
	const racers = 16

	for i := range reps {
		socket := filepath.Join(shortDir(t), "s.sock")
		granted := make([]*InstanceLock, racers)
		errs := make([]error, racers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for r := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				granted[r], errs[r] = AcquireInstance(socket)
			}()
		}
		close(start)
		wg.Wait()

		var held []*InstanceLock
		for r := range racers {
			switch {
			case errs[r] == nil:
				held = append(held, granted[r])
			case !errors.Is(errs[r], ErrInstanceHeld):
				t.Errorf("rep %d racer %d was refused with %v, which does not wrap ErrInstanceHeld, so a caller cannot tell a served socket from a broken lock file", i, r, errs[r])
			}
		}
		if len(held) == 0 {
			t.Fatalf("rep %d: every one of the %d racers was refused, so this rep could not have granted the lock twice and proves nothing", i, racers)
		}
		if len(held) > 1 {
			t.Errorf("rep %d: %d of %d overlapping acquires were each granted %s's instance lock; the first Release frees it for all of them and leaves the socket unguarded", i, len(held), racers, socket)
		}
		for _, l := range held {
			l.Release()
		}
	}
}

// TestInstanceHolderDoesNotDropAConcurrentAcquiresLock is the third place this
// package touches a lock file, and the one no sequential test reaches. Naming
// another process's holder means asking the kernel, asking needs a descriptor, and
// closing that descriptor drops every record lock this process holds on the file.
// So an acquire that registers between InstanceHolder deciding this process is not
// the holder and closing that descriptor is a lock the kernel granted and the close
// then takes away, leaving the socket unguarded while the acquirer still believes
// it holds it.
//
// The two sleeps are what make the window reachable rather than merely present.
// The first lets the readers get as far as their open. The second matters more: once
// the acquire has registered, every later reader is answered by the registry and
// opens nothing, so without it the readers would usually be torn down before one
// already past its check had reached its close.
//
// A child is the observer because the kernel reports this process's own lock as
// free, and one loss is the result rather than a rate: a single dropped lock is a
// socket a second daemon can bind over.
func TestInstanceHolderDoesNotDropAConcurrentAcquiresLock(t *testing.T) {
	socket := filepath.Join(shortDir(t), "s.sock")
	// The lock file has to exist, or InstanceHolder returns on ErrNotExist without
	// ever reaching the open whose close is the hazard.
	l0, err := AcquireInstance(socket)
	if err != nil {
		t.Fatal(err)
	}
	l0.Release()
	if !childCanLock(t, socket) {
		t.Fatal("setup: a child cannot take a lock nobody holds, so a child failing below would not mean the lock survived")
	}

	const reps = 10
	const readers = 8
	for i := range reps {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						InstanceHolder(socket)
					}
				}
			}()
		}
		time.Sleep(200 * time.Microsecond)
		l, err := AcquireInstance(socket)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("rep %d: a lock no other process holds was refused, so nothing here held a lock to lose: %v", i, err)
		}
		time.Sleep(500 * time.Microsecond)
		close(stop)
		wg.Wait()

		took := childCanLock(t, socket)
		l.Release()
		if took {
			t.Fatalf("rep %d: a child took %s's instance lock while this process held it; a concurrent InstanceHolder closed its descriptor and dropped the lock", i, socket)
		}
	}
}

// swapLockFileUnderneath alternates the lock file at victim between a fresh inode
// nobody holds and a hard link to held's, until stop closes. It takes no descriptor
// on either lock file — link, rename and unlink do not open anything — so no lock
// it disturbs can have been released by the swapper itself.
func swapLockFileUnderneath(t *testing.T, held, victim string, stop <-chan struct{}, wg *sync.WaitGroup) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		dir := filepath.Dir(victim)
		linked := filepath.Join(dir, "linked")
		fresh := filepath.Join(dir, "fresh")
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.Remove(linked)
			if os.Link(lockPath(held), linked) == nil {
				os.Rename(linked, lockPath(victim))
			}
			os.Remove(fresh)
			if os.WriteFile(fresh, nil, 0o600) == nil {
				os.Rename(fresh, lockPath(victim))
			}
		}
	}()
}

// keptDescriptors reports how many descriptors this package has had to keep.
func keptDescriptors() int {
	heldLocks.Lock()
	defer heldLocks.Unlock()
	return len(keptLockFiles)
}

// TestAnUnidentifiedKeptDescriptorSurvivesTheSweep covers the entry the sweep has to
// keep without being able to justify it: one parked when its inode could not be read
// at all. The sweep decides from the inode recorded at park time, and the zero id is
// not a held one, so treating it as an ordinary id would close exactly the descriptor
// nothing knows to be safe.
//
// The entry is parked directly, because the only thing that produces one is an fstat
// failing on a descriptor this process has just opened successfully, which a test
// cannot arrange. That is not a shortcut: what is under test is the sweep's treatment
// of such an entry, not the route that creates it.
func TestAnUnidentifiedKeptDescriptorSurvivesTheSweep(t *testing.T) {
	dir := shortDir(t)
	kept := filepath.Join(dir, "k.sock")
	released := filepath.Join(dir, "r.sock")
	k, err := AcquireInstance(kept)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Release()
	r, err := AcquireInstance(released)
	if err != nil {
		t.Fatal(err)
	}

	// A second descriptor for the lock file this process holds, parked with no
	// recorded inode. Closing it would drop the lock held through the first one.
	f, err := os.OpenFile(lockPath(kept), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	heldLocks.Lock()
	keptLockFiles = append(keptLockFiles, keptLockFile{f: f})
	heldLocks.Unlock()
	t.Cleanup(func() {
		heldLocks.Lock()
		keptLockFiles = nil
		heldLocks.Unlock()
	})
	if childCanLock(t, kept) {
		t.Fatal("setup: a child took a lock this process holds, so nothing below could observe a release")
	}

	// Releasing the other lock runs the sweep, which is its only caller.
	r.Release()

	if n := keptDescriptors(); n != 1 {
		t.Errorf("the sweep kept %d of 1 descriptors whose inode it could not read; such an entry must be kept, since the sweep cannot prove closing it is safe", n)
	}
	if childCanLock(t, kept) {
		t.Errorf("a child took %s's instance lock, which this process still holds: the sweep closed a second descriptor for it, and closing any descriptor for a file drops every record lock this process holds on it", kept)
	}
}

// TestASwappedLockPathDoesNotDropAHeldLock covers the half of reading a holder that
// the registry cannot serialise, because that half is a path rather than a lock.
// holderHeld names the path once to ask the registry and once to open it, and
// between the two the lock file can be replaced by a link to a lock file this
// process does hold — under any socket, not only the one being asked about. The
// close that ends the read then drops that lock, so a daemon serving an unrelated
// socket loses its guard because somebody asked about this one.
//
// Nothing about the directory prevents the swap: the lock arbitrates between
// processes of the same user, so same-uid ownership excludes none of them, and a
// socket named explicitly outside DefaultDir is only warned about. So the fix is to
// identify the file from the descriptor, which no rename can change, and to keep a
// descriptor that turns out to name a held file instead of closing it.
//
// The observer is a child process because the kernel reports this process's own
// lock as free. One loss is the result rather than a rate: a single dropped lock is
// a socket a second daemon can bind over. The kept count is asserted too, because
// a rep that never swapped at the wrong moment never reached the branch under test,
// and a run of those would pass without exercising anything.
func TestASwappedLockPathDoesNotDropAHeldLock(t *testing.T) {
	// The swap has to land between holderHeld's registry check and its open, so the
	// swapper and a reader have to be on a thread each at that instant. Sharing one
	// thread they reach it only when a preemption falls exactly there: measured over
	// six runs of 20 reps, the kept count below is 2 to 4 under GOMAXPROCS=1 against
	// roughly 1000 at GOMAXPROCS=2 and 1500 at 24. Two to four is not a margin — the
	// one full-package GOMAXPROCS=1 run that failed here failed on the kept check,
	// because co-tenants competing for the single P took it to zero.
	testthreads.Need(t, 2)

	dir := shortDir(t)
	held := filepath.Join(dir, "h.sock")
	victim := filepath.Join(dir, "v.sock")
	// victim's lock file has to exist, or holderHeld returns on ErrNotExist without
	// reaching the open whose close is the hazard.
	if err := os.WriteFile(lockPath(victim), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Positive control first, while nobody holds held's lock: every other assertion
	// here is that a child is refused, and a child that cannot run at all is refused
	// for the wrong reason. Written rather than created, because an *os.File this
	// test drops without closing is closed by its finalizer instead, which would
	// release the very lock the test goes on to assert survives.
	if err := os.WriteFile(lockPath(held), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !childCanLock(t, held) {
		t.Fatal("setup: a child cannot take a lock nobody holds, so a child being refused below would not mean the lock survived")
	}

	l, err := AcquireInstance(held)
	if err != nil {
		t.Fatal(err)
	}
	if childCanLock(t, held) {
		t.Fatal("setup: a child took the lock this process just acquired, so nothing below could observe a release")
	}

	const reps = 20
	const readers = 4
	kept := 0
	for i := range reps {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		swapLockFileUnderneath(t, held, victim, stop, &wg)
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						InstanceHolder(victim)
					}
				}
			}()
		}
		time.Sleep(25 * time.Millisecond)
		close(stop)
		wg.Wait()

		if n := keptDescriptors(); n > kept {
			kept = n
		}
		if childCanLock(t, held) {
			l.Release()
			t.Fatalf("rep %d: a child took %s's instance lock while this process held it, after %d reads of a different socket (%s) whose lock file was being swapped: the open named the held inode and the close released it", i, held, i+1, victim)
		}
	}
	if kept == 0 {
		t.Fatalf("no descriptor was ever kept across %d reps, so the swap never landed between the registry check and the open and this run never reached the branch it is testing", reps)
	}

	// And the kept descriptors do not outlive the lock that made them unclosable.
	l.Release()
	if n := keptDescriptors(); n != 0 {
		t.Errorf("%d descriptors are still kept after the lock they belonged to was released; they can be closed once nothing holds the inode, and holding them leaks one per swap for the life of the process", n)
	}
}

// TestASwappedLockPathIsNotAcquiredTwice is the same path-swap window in the other
// operation that opens a lock file. acquireHeld checks the path's inode against the
// registry and then opens that path, and a lock file replaced in between by a link
// to one this process holds gets past the check: F_SETLK is then granted, because a
// process never conflicts with itself, so a second InstanceLock comes back for an
// inode already locked and whichever holder releases first frees it for the other.
//
// The direct witness is the granted lock's own inode, since a grant whose id is the
// held socket's is that defect however the test goes on to behave. A child is asked
// as well, after releasing the grants, because a lock handed out twice and released
// once is a socket another daemon can take while this process still serves it — the
// consequence the id comparison only predicts.
//
// A rep where nothing was granted exercised nothing, so grants are counted and a run
// with none is fatal rather than passing; likewise the refusals, which are what the
// swap has to have reached for the branch under test to have run at all.
func TestASwappedLockPathIsNotAcquiredTwice(t *testing.T) {
	// The swap has to land between one acquire's check of the path and its open of
	// it, so the swapper and an acquirer must be on a thread each at the same
	// instant. On one thread they never are, and the run reaches neither the grant
	// nor the refusal: 0 of 10 runs passed under GOMAXPROCS=1 against 10 of 10 at
	// GOMAXPROCS=2 and above. The premise checks at the end are what turned that
	// into a failure instead of a vacuous pass, and they stay.
	testthreads.Need(t, 2)
	dir := shortDir(t)
	held := filepath.Join(dir, "h.sock")
	victim := filepath.Join(dir, "v.sock")
	for _, p := range []string{lockPath(held), lockPath(victim)} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := AcquireInstance(held)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if childCanLock(t, held) {
		t.Fatal("setup: a child took the lock this process just acquired, so nothing below could observe a release")
	}
	heldID := l.id

	const reps = 20
	const racers = 4
	grants, kept := 0, 0
	for rep := range reps {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		swapLockFileUnderneath(t, held, victim, stop, &wg)

		var mu sync.Mutex
		var got []*InstanceLock
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if l, err := AcquireInstance(victim); err == nil {
						mu.Lock()
						got = append(got, l)
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(25 * time.Millisecond)
		close(stop)
		wg.Wait()

		bad := 0
		for _, g := range got {
			if g.id == heldID {
				bad++
			}
			g.Release()
		}
		grants += len(got)
		if n := keptDescriptors(); n > kept {
			kept = n
		}
		if bad > 0 {
			t.Fatalf("rep %d: %d of %d acquires of %s were granted a lock on the inode this process already holds for %s, because the lock file was swapped between the check of the path and the open of it", rep, bad, len(got), victim, held)
		}
		if childCanLock(t, held) {
			t.Fatalf("rep %d: a child took %s's instance lock while this process held it, after acquiring and releasing a different socket (%s) whose lock file was being swapped: the second lock was on the same inode and releasing it freed the first", rep, held, victim)
		}
	}
	if grants == 0 {
		t.Fatalf("no acquire of %s was ever granted across %d reps, so no Release ran and this run could not have observed one dropping another lock", victim, reps)
	}
	if kept == 0 {
		// Not reachable on darwin, and skipped there rather than dropped: everything
		// above ran — the id of every grant was compared, and a child was asked 20
		// times whether it could take a lock this process holds — and only the proof
		// that the refusal branch was reached is missing.
		//
		// Measured, because "it is slower there" would have been a guess. Same 0.5s,
		// same 4 racers: linux 12623 grants and 15 kept descriptors, darwin 15554
		// grants and 0. Giving darwin 20x the time raised it to 15554 grants in 10s
		// and still 0 kept, so it is a rate near zero and not a shortfall of tries.
		// Darwin lands the swap perfectly well when there are enough opens to land it
		// in: TestASwappedLockPathDoesNotDropAHeldLock, whose readers open the lock
		// file on every iteration rather than only when the path shows an inode this
		// process does not already hold, keeps 55 descriptors in the same 0.5s there.
		// This path offers about one open per swap cycle, and the check-to-open window
		// inside it is CPU-bound while the swap around it is filesystem-bound, so the
		// ratio that decides the hit rate moves against it on the slower filesystem.
		if runtime.GOOS != "linux" {
			t.Skipf("the window between the check of the path and the open of it was never hit on %s; see the comment here", runtime.GOOS)
		}
		t.Fatalf("no descriptor was ever kept across %d reps, so the swap never landed between the check and the open and this run never reached the refusal it is testing", reps)
	}
}

// TestReclaimingKeptDescriptorsSparesAStillHeldLock is the else side of the
// reclamation's guard, which a process holding one lock cannot reach. Releasing a
// lock makes the descriptors kept for it closable, but the kept set is shared, so a
// reclaim that closes everything would release a lock the process still holds —
// turning a leak fix back into the loss it was meant to stop. A daemon serving two
// sockets is the ordinary case where the two differ.
//
// The descriptor kept here belongs to the socket that is NOT released, because the
// swapper links the victim's lock file to that one.
func TestReclaimingKeptDescriptorsSparesAStillHeldLock(t *testing.T) {
	// The same premise as the two swap tests above, and it fails the same way: the
	// loop below needs the swap to land between a reader's registry check and its
	// open, which wants the swapper and a reader on a thread each at that instant. On
	// one thread that needs a preemption to fall exactly there -- often enough to
	// look sound, not often enough to be. Measured at GOMAXPROCS=1, 6 of 60 runs
	// exhausted all 20 rounds without keeping a descriptor and died on the premise,
	// against 0 of 60 with a second thread. It is the same 10% in the full package,
	// where it was the only test still failing a GOMAXPROCS=1 sweep.
	testthreads.Need(t, 2)
	dir := shortDir(t)
	released := filepath.Join(dir, "r.sock")
	retained := filepath.Join(dir, "k.sock")
	victim := filepath.Join(dir, "v.sock")
	if err := os.WriteFile(lockPath(victim), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := AcquireInstance(released)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := AcquireInstance(retained)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if childCanLock(t, retained) {
		t.Fatal("setup: a child took a lock this process holds, so nothing below could observe a release")
	}

	// Read the victim against the swapper until a descriptor for retained's lock
	// file has had to be kept, which is the state the reclaim then has to respect.
	for i := range 20 {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		swapLockFileUnderneath(t, retained, victim, stop, &wg)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						InstanceHolder(victim)
					}
				}
			}()
		}
		time.Sleep(25 * time.Millisecond)
		close(stop)
		wg.Wait()
		if keptDescriptors() > 0 {
			break
		}
		if i == 19 {
			t.Fatal("no descriptor was ever kept, so releasing a lock below cannot reclaim one and this run reaches nothing")
		}
	}

	first.Release()
	if childCanLock(t, retained) {
		t.Errorf("releasing %s's lock let a child take %s's, which this process still holds: reclaiming the kept descriptors closed one belonging to a lock that is still held", released, retained)
	}
}

// TestReleaseLetsAnotherProcessIn is the half that has to keep working: the
// registry must not outlive the lock, or a daemon's successor would be refused a
// socket nobody serves.
func TestReleaseLetsAnotherProcessIn(t *testing.T) {
	socket := filepath.Join(shortDir(t), "s.sock")
	l, err := AcquireInstance(socket)
	if err != nil {
		t.Fatal(err)
	}
	if childCanLock(t, socket) {
		t.Fatal("setup: the lock was never held, so releasing it proves nothing")
	}
	l.Release()

	if !childCanLock(t, socket) {
		t.Errorf("after Release a child still cannot take %s's instance lock", socket)
	}
	if holder, err := InstanceHolder(socket); err != nil || holder != 0 {
		t.Errorf("after Release InstanceHolder still names pid %d (err %v)", holder, err)
	}
	// And this process can take it again, which a registry entry left behind by
	// Release would refuse.
	again, err := AcquireInstance(socket)
	if err != nil {
		t.Fatalf("this process cannot retake a lock it released: %v", err)
	}
	again.Release()
}
