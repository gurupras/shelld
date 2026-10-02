//go:build linux

package cgroup

// The directory layer: the names a base carries, the one file the readers read,
// and the paths that report a refusal rather than doing anything.
//
// The tests in cgroup_linux_test.go are about placement and killing, and every
// one of them needs a real cgroup and real processes. What is left uncovered by
// them is the other half of this package: deciding from a directory's NAME which
// daemon owns it, deciding from cgroup.events whether a directory may be
// removed, and the refusals — a window cgroup that already holds a process, a
// wait that runs out, a base that will not clear. Those decisions are what stand
// between a GC and removing a live daemon's base, and none of them needs a
// process to be wrong.
//
// Two kinds of fixture are used and they are not interchangeable:
//
//   - A REAL cgroup, from Probe, for everything whose outcome is a removal. A
//     fabricated directory cannot stand in: rmdir of a real cgroup succeeds with
//     its interface files still in it, and rmdir of an ordinary directory holding
//     a file fails with ENOTEMPTY.
//   - A FABRICATED directory, holding nothing but a cgroup.events this test
//     wrote, for the arms a real cgroup cannot be made to take: a cgroup.events
//     with no populated line, and an rmdir that fails. That divergence is the
//     reason the fixture exists and the reason it is kept away from the rest.
//
// Nothing here writes cgroup.kill. The tests that kill are the ones with a
// process to kill, and they live next door.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/proctree"
)

// fakeCgroup makes a directory that answers this package's readers: the only
// file any of them opens is cgroup.events. See the note above for what it is not.
func fakeCgroup(t *testing.T, dir string, pop bool) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	n := "0"
	if pop {
		n = "1"
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte("populated "+n+"\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestParseBaseNameRoundTrip pins the pair of functions against each other over
// the values a real name is built from, the ends of the ranges included.
func TestParseBaseNameRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		pid   int
		start uint64
	}{
		{1, 0},
		{2, 1},
		{4194304, 12345678}, // pid_max's default ceiling
		{99999, 1 << 63},
		{7, 1<<64 - 1},
	} {
		name := baseName(tc.pid, tc.start)
		pid, start, ok := parseBaseName(name)
		if !ok || pid != tc.pid || start != tc.start {
			t.Errorf("parseBaseName(%q) = %d, %d, %v, want %d, %d, true",
				name, pid, start, ok, tc.pid, tc.start)
		}
	}
}

// TestParseBaseNameRejectsWhatItCannotName is the GC's safety argument in table
// form. GC decides a directory is stale by comparing the pid and start time in
// its name against the process holding that pid now, so a name read wrong is a
// GC answering about the wrong process — and the removal it then does is of a
// live daemon's base. Every row here has to come back not-ok, because "I cannot
// name this" is what makes GC skip the directory instead.
//
// Two of the checks in parseBaseName cannot be distinguished by any input and
// are here as belt-and-braces rather than as behaviour, which mutation testing
// showed rather than argument: splitting on the LAST '-' instead of the first
// changes nothing, because a second separator leaves one half of every split
// unparseable either way, and the i <= 0 / i == len(rest)-1 bounds are already
// Atoi's and ParseUint's refusal of an empty string. The pid <= 0 guard is not
// in that class — without it "shelld-0-5" names pid 0.
func TestParseBaseNameRejectsWhatItCannotName(t *testing.T) {
	for _, name := range []string{
		"",
		// The prefix is what makes a directory ours, and these two are the rows
		// that make that load-bearing: a name shaped exactly like the part after
		// the prefix must not parse. Without the check, a sibling cgroup named
		// "<number>-<number>" — which nothing stops a user or another tool from
		// creating under the same parent — would be attributed to a daemon and
		// removed by GC as soon as no live process held that pid.
		"1-2",
		"123-456",
		"shelld",                        // the prefix itself is "shelld-"
		"shelld-",                       // nothing after it
		"Shelld-1-2",                    // the prefix is not matched case-insensitively
		"xshelld-1-2",                   // nor anywhere but at the start
		"shelld-123",                    // no separator before a start time
		"shelld-123-",                   // nothing after the separator
		"shelld--1",                     // the separator is the first byte of the rest
		"shelld--5-5",                   // a negative pid
		"shelld-0-5",                    // pid 0 names no process
		"shelld-abc-5",                  // not a pid
		"shelld- 1-5",                   // Atoi refuses the space, and so must this
		"shelld-1.5-5",                  // nor a float
		"shelld-1-abc",                  // not a start time
		"shelld-1- 5",                   // ParseUint refuses the space too
		"shelld-1-1e3",                  // nor an exponent
		"shelld-1--5",                   // nor a negative one
		"shelld-1-5.0",                  // nor a fraction
		"shelld-1-2-3",                  // the split is on the LAST '-', so the pid here is "1-2"
		"shelld-1-18446744073709551616", // one past the largest start time
		"win-1",                         // a window cgroup, which lives under a base
		"probe",                         // Probe's own child
		"user@1000.service",             // the sort of sibling a systemd host has
	} {
		if pid, start, ok := parseBaseName(name); ok {
			t.Errorf("parseBaseName(%q) = %d, %d, true, want it refused", name, pid, start)
		}
	}
}

// TestPopulatedReadsTheEventsFile covers the file every removal decision is made
// from. A misread here is not a wrong number: reading "populated 1" as empty
// makes removeIfEmpty rmdir a cgroup holding a process, and reading "populated 0"
// as busy makes a base outlive every daemon that ever ran.
func TestPopulatedReadsTheEventsFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
		errLike string
	}{
		{"empty", "populated 0\nfrozen 0\n", false, ""},
		{"populated", "populated 1\nfrozen 0\n", true, ""},
		{"a count rather than a flag", "populated 2\n", true, ""},
		{"populated last", "frozen 0\npopulated 1\n", true, ""},
		{"no trailing newline", "populated 0", false, ""},
		{"padded", "populated  0 \n", false, ""},
		{"frozen alone", "frozen 0\n", false, "no populated line"},
		{"empty file", "", false, "no populated line"},
		// "populated" is only the flag when it is the field name: a prefix match
		// without the space would read this as populated.
		{"another field starting with the name", "populated_something 1\n", false, "no populated line"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := populated(dir)
		switch {
		case tc.errLike != "":
			if err == nil || !strings.Contains(err.Error(), tc.errLike) {
				t.Errorf("%s: populated = %v, %v, want an error naming %q", tc.name, got, err, tc.errLike)
			}
		case err != nil:
			t.Errorf("%s: populated = %v", tc.name, err)
		case got != tc.want:
			t.Errorf("%s: populated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPopulatedOnAMissingCgroupIsNotExist pins the error KIND, not its text.
// removeIfEmpty tells a cgroup that vanished under it from a cgroup it cannot
// read by asking errors.Is for fs.ErrNotExist; if os.ReadFile's error were
// wrapped on the way out, that check would stop matching and a teardown race
// would be reported as a fault.
func TestPopulatedOnAMissingCgroupIsNotExist(t *testing.T) {
	_, err := populated(filepath.Join(t.TempDir(), "gone"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("populated on a missing directory: %v, want it to match fs.ErrNotExist", err)
	}
}

// TestRemoveIfEmptyLeavesWhatIsPopulated covers the counting, which is the only
// thing a caller branches on, and the one arm in this package that reports a
// failed rmdir. A fabricated tree is used deliberately: an ordinary directory
// holding a file cannot be rmdir'd, which is the failure a real cgroup will not
// produce on demand.
func TestRemoveIfEmptyLeavesWhatIsPopulated(t *testing.T) {
	t.Run("a populated child is counted and nothing is removed", func(t *testing.T) {
		root := t.TempDir()
		base := fakeCgroup(t, filepath.Join(root, "base"), false)
		child := fakeCgroup(t, filepath.Join(base, "win-1"), true)
		// The base is empty itself, so its own rmdir would be attempted were the
		// child not counted — and on this fixture that rmdir fails, which is how
		// a lost count would show up as an error instead of a number.
		left, err := removeIfEmpty(base)
		if err != nil {
			t.Fatalf("removeIfEmpty: %v", err)
		}
		if left != 2 {
			t.Errorf("left %d, want 2: the populated child and the base above it", left)
		}
		for _, d := range []string{base, child} {
			if _, err := os.Stat(d); err != nil {
				t.Errorf("%s was removed: %v", d, err)
			}
		}
	})

	t.Run("a failed rmdir is reported with its path", func(t *testing.T) {
		dir := fakeCgroup(t, filepath.Join(t.TempDir(), "base"), false)
		left, err := removeIfEmpty(dir)
		if err == nil {
			t.Fatalf("removeIfEmpty removed %s, which this fixture cannot allow", dir)
		}
		if !strings.Contains(err.Error(), "rmdir") || !strings.Contains(err.Error(), dir) {
			t.Errorf("error %v, want it to name the rmdir and %s", err, dir)
		}
		if left != 1 {
			t.Errorf("left %d, want 1: a directory that could not be removed is left behind", left)
		}
	})

	t.Run("an error from below stops the removal above", func(t *testing.T) {
		root := t.TempDir()
		base := fakeCgroup(t, filepath.Join(root, "base"), false)
		fakeCgroup(t, filepath.Join(base, "win-1"), false)
		_, err := removeIfEmpty(base)
		if err == nil {
			t.Fatal("removeIfEmpty reported no error although no directory could be removed")
		}
		if _, err := os.Stat(base); err != nil {
			t.Errorf("the base was removed even though its child failed: %v", err)
		}
	})

	t.Run("files are neither recursed into nor counted", func(t *testing.T) {
		root := t.TempDir()
		base := fakeCgroup(t, filepath.Join(root, "base"), true)
		if err := os.WriteFile(filepath.Join(base, "cgroup.procs"), []byte("\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		left, err := removeIfEmpty(base)
		if err != nil || left != 1 {
			t.Errorf("removeIfEmpty = %d, %v, want 1 and no error: the interface files are not cgroups", left, err)
		}
	})

	t.Run("a directory that is not there is nothing to do", func(t *testing.T) {
		left, err := removeIfEmpty(filepath.Join(t.TempDir(), "gone"))
		if left != 0 || err != nil {
			t.Errorf("removeIfEmpty on a missing directory = %d, %v, want 0 and no error", left, err)
		}
	})
}

// TestRemoveIfEmptyReportsAnUnreadableDirectory covers the other ReadDir arm: a
// directory that is there and cannot be listed. Reading it as absent — the way
// a missing directory is read — would report a clean removal of a cgroup whose
// contents were never looked at.
//
// One arm of removeIfEmpty is left uncovered and named here rather than left to
// be noticed: the ENOENT from populated, which happens when a cgroup is removed
// between this function listing its parent and reading its events file. It needs
// a removal to land inside a window of a few microseconds, and a test that
// waited for one would pass by not reproducing it.
func TestRemoveIfEmptyReportsAnUnreadableDirectory(t *testing.T) {
	dir := fakeCgroup(t, filepath.Join(t.TempDir(), "base"), false)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this process can list a mode 000 directory; it is probably root")
	}
	left, err := removeIfEmpty(dir)
	if err == nil {
		t.Fatalf("removeIfEmpty = %d, nil on a directory it cannot list", left)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("removeIfEmpty: %v, want the error to name %s", err, dir)
	}
}

// TestRemoveReportsWhatItCannotRemove covers Remove's other error arm, the one
// that is not a live process: the removal itself failing. Remove is called after
// Kill and WaitEmpty, so this is the path that turns a stuck teardown into a
// reported one instead of a silent leak.
func TestRemoveReportsWhatItCannotRemove(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, baseName(os.Getpid(), 1))
	dir := fakeCgroup(t, filepath.Join(base, "win-1"), false)
	c := &Cgroup{base: base, dir: dir}
	err := c.Remove()
	if err == nil {
		t.Fatal("Remove reported success although the directory is still there")
	}
	if !strings.Contains(err.Error(), "rmdir") {
		t.Errorf("Remove: %v, want the rmdir failure", err)
	}
	if _, serr := os.Stat(dir); serr != nil {
		t.Errorf("%s is gone after a Remove that failed: %v", dir, serr)
	}
}

// TestGCReportsWhatItCannotRemove covers the joined errors. A GC that dropped
// them would report "nothing to collect" for a base it failed to remove, and the
// next GC would report the same.
func TestGCReportsWhatItCannotRemove(t *testing.T) {
	root := t.TempDir()
	selfStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's own start time")
	}
	stale := fakeCgroup(t, filepath.Join(root, baseName(os.Getpid(), selfStart+1)), false)
	m := &Manager{parent: root, base: filepath.Join(root, baseName(os.Getpid(), selfStart))}
	removed, busy, err := m.GC()
	if err == nil {
		t.Fatalf("GC = %d, %d, nil although %s could not be removed", removed, busy, stale)
	}
	if !strings.Contains(err.Error(), stale) {
		t.Errorf("GC: %v, want the error to name %s", err, stale)
	}
	if removed != 0 || busy != 0 {
		t.Errorf("GC = %d removed, %d busy, want 0 and 0: a failure is neither", removed, busy)
	}
}

// TestCloseReportsWhatItCannotRemove covers Close's other error arm. The
// populated case has a test next door with a real process in it; this is the one
// where the removal itself fails, and a Close that swallowed it would report a
// clean shutdown having left the base behind.
func TestCloseReportsWhatItCannotRemove(t *testing.T) {
	m := &Manager{base: fakeCgroup(t, filepath.Join(t.TempDir(), baseName(os.Getpid(), 1)), false)}
	err := m.Close()
	if err == nil {
		t.Fatal("Close reported success although the base is still there")
	}
	if !strings.Contains(err.Error(), "rmdir") {
		t.Errorf("Close: %v, want the rmdir failure", err)
	}
	if _, serr := os.Stat(m.base); serr != nil {
		t.Errorf("the base is gone after a Close that failed: %v", serr)
	}
}

// TestGCSkipsWhatIsNotAStaleBase is the other half of the GC's safety argument.
// What it must not touch is anything it cannot attribute to a daemon that is
// gone: a sibling belonging to systemd, a file, a name it cannot parse, and —
// the row that matters most — a directory whose pid is live AND whose start time
// matches, which includes our own base.
//
// Every stale directory here is populated, so GC counts them busy and removes
// nothing at all: this test is about what GC leaves, and a fabricated tree
// cannot have anything removed from it anyway. The removal itself is covered
// next door against real cgroups.
func TestGCSkipsWhatIsNotAStaleBase(t *testing.T) {
	root := t.TempDir()
	selfStart, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's own start time")
	}

	// Left alone, each for a different reason.
	keep := []string{
		baseName(os.Getpid(), selfStart), // live pid, matching start time: this daemon
		"user@1000.service",              // a sibling that is not ours
		"init.scope",
		"shelld-bad-name",
		"shelld-0-1", // a name parseBaseName refuses
		"win-1",
	}
	for _, name := range keep {
		fakeCgroup(t, filepath.Join(root, name), false)
	}
	// A file with a name that would parse, to check the IsDir test: GC reading it
	// as a base would try to remove a file.
	if err := os.WriteFile(filepath.Join(root, baseName(999999, 1)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale, and populated so that GC counts them rather than removing them. The
	// first is the case a pid alone cannot decide: the pid is ALIVE — it is us —
	// and the start time says the daemon that made the directory is not.
	stale := []string{
		baseName(os.Getpid(), selfStart+1),
		baseName(os.Getpid(), selfStart-1),
	}
	for _, name := range stale {
		fakeCgroup(t, filepath.Join(root, name), true)
	}

	m := &Manager{parent: root, base: filepath.Join(root, keep[0])}
	removed, busy, err := m.GC()
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 0 {
		t.Errorf("GC removed %d, want 0: every stale base here holds a process", removed)
	}
	if busy != len(stale) {
		t.Errorf("GC reported %d busy, want %d", busy, len(stale))
	}
	for _, name := range append(keep, stale...) {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("GC removed %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, baseName(999999, 1))); err != nil {
		t.Errorf("GC removed the file named like a base: %v", err)
	}
}

// TestGCReportsAnUnreadableParent covers the one failure GC returns rather than
// counting. The parent is the cgroup the daemon itself is in; if it cannot be
// read there is nothing to iterate, and reporting zero would read as "nothing to
// collect".
func TestGCReportsAnUnreadableParent(t *testing.T) {
	m := &Manager{parent: filepath.Join(t.TempDir(), "gone")}
	removed, busy, err := m.GC()
	if err == nil {
		t.Fatalf("GC on a missing parent = %d, %d, nil", removed, busy)
	}
	if !strings.Contains(err.Error(), m.parent) {
		t.Errorf("GC: %v, want the error to name %s", err, m.parent)
	}
}

// TestWaitEmptyReportsAnUnreadableCgroup covers the arm that is not a timeout: a
// cgroup removed under the wait. It returns immediately rather than spinning to
// the deadline, which is what makes a teardown race readable.
func TestWaitEmptyReportsAnUnreadableCgroup(t *testing.T) {
	c := &Cgroup{dir: filepath.Join(t.TempDir(), "gone")}
	start := time.Now()
	err := c.WaitEmpty(10 * time.Second)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("WaitEmpty on a missing cgroup: %v, want fs.ErrNotExist", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("WaitEmpty took %v, want it to give up at once rather than poll to the deadline", d)
	}
}

// TestOpenReportsAMissingCgroup covers Open's error arm. Open is called once per
// spawn, so this is the error a window spawn reports when its cgroup was removed
// underneath it.
func TestOpenReportsAMissingCgroup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	f, err := (&Cgroup{dir: dir}).Open()
	if err == nil {
		f.Close()
		t.Fatalf("Open on %s succeeded", dir)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("Open: %v, want the error to name %s", err, dir)
	}
}

// TestNewIsIdempotentAndRefusesAPopulatedCgroup is on real cgroups because both
// outcomes are about a directory this package created: New reuses an empty one
// and refuses one with a process in it.
//
// The refusal is the load-bearing half. Adopting a populated cgroup would make
// the new window responsible for killing processes it never started, and the
// window that would kill them is one a user closes.
func TestNewIsIdempotentAndRefusesAPopulatedCgroup(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(7)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	again, err := m.New(7)
	if err != nil {
		t.Fatalf("New on an empty existing cgroup: %v, want it reused", err)
	}
	if again.Dir() != c.Dir() {
		t.Fatalf("New returned %s and then %s for the same id", c.Dir(), again.Dir())
	}
	if _, err := os.Stat(c.Dir()); err != nil {
		t.Fatalf("the reused cgroup is not there: %v", err)
	}

	proc := startInto(t, c, "sleep 300")
	if !waitFor(2*time.Second, func() bool { pop, _ := populated(c.Dir()); return pop }) {
		t.Fatalf("%s never reported a member after spawning %d", c.Dir(), proc.Pid)
	}
	if got, err := m.New(7); err == nil {
		t.Errorf("New adopted %s while a process was in it", got.Dir())
	} else if !strings.Contains(err.Error(), "holds a process") {
		t.Errorf("New: %v, want it to say the cgroup holds a process", err)
	}
	// And the refusal left it alone: the process is still there and the cgroup
	// was not recreated under it.
	if got := members(t, c.Dir()); len(got) == 0 {
		t.Errorf("%s is empty after the refusal, want the process still in it", c.Dir())
	}

	// Torn down here rather than in a cleanup, so the base is empty by the time
	// probeOrSkip's Close runs.
	if err := c.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := c.WaitEmpty(5 * time.Second); err != nil {
		t.Fatalf("WaitEmpty: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

// TestRemoveAndWaitEmptyRefuseWhileAProcessIsInside covers the two refusals that
// need a member: Remove after a Kill that did not happen, and a WaitEmpty that
// runs out. Both are faults worth reporting rather than states to work around,
// and both are silent if the read of cgroup.events is wrong.
func TestRemoveAndWaitEmptyRefuseWhileAProcessIsInside(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(9)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc := startInto(t, c, "sleep 300")
	if !waitFor(2*time.Second, func() bool { pop, _ := populated(c.Dir()); return pop }) {
		t.Fatalf("%s never reported a member after spawning %d", c.Dir(), proc.Pid)
	}

	start := time.Now()
	if err := c.WaitEmpty(20 * time.Millisecond); err == nil {
		t.Error("WaitEmpty returned no error with a process in the cgroup")
	} else if !strings.Contains(err.Error(), "still populated") {
		t.Errorf("WaitEmpty: %v, want it to say the cgroup is still populated", err)
	}
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Errorf("WaitEmpty gave up after %v, before its own deadline", d)
	}

	if err := c.Remove(); err == nil {
		t.Error("Remove deleted a cgroup holding a process")
	} else if !strings.Contains(err.Error(), "still holds a process") {
		t.Errorf("Remove: %v, want it to say the cgroup still holds a process", err)
	}
	if _, err := os.Stat(c.Dir()); err != nil {
		t.Errorf("%s is gone after a Remove that refused: %v", c.Dir(), err)
	}

	if err := c.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := c.WaitEmpty(5 * time.Second); err != nil {
		t.Fatalf("WaitEmpty after Kill: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove after Kill: %v", err)
	}
}

// TestRemoveDescendsIntoANestedCgroup covers the depth-first order on the
// fixture that can show it is load-bearing: a window whose shell made a cgroup
// of its own. rmdir of the parent is EBUSY while the child exists, which the
// assertion below checks before relying on it — otherwise the test would pass on
// a kernel where the order made no difference.
func TestRemoveDescendsIntoANestedCgroup(t *testing.T) {
	m := probeOrSkip(t)
	c, err := m.New(11)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	nested := filepath.Join(c.Dir(), "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", nested, err)
	}
	deeper := filepath.Join(nested, "deeper")
	if err := os.Mkdir(deeper, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", deeper, err)
	}
	if err := os.Remove(c.Dir()); err == nil {
		t.Fatalf("%s was removable with a child cgroup in it: the depth-first order is "+
			"not being exercised here", c.Dir())
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, d := range []string{deeper, nested, c.Dir()} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived Remove: %v", d, err)
		}
	}
}

// TestGuardRefusesAnUnnamedBase is the guard read from the other direction: the
// prefix check is on the BASE's name, and a Cgroup whose base is not ours is
// refused before any file is written. The dirs named here do not exist, which is
// the point — a guard that let the call through would be writing cgroup.kill to
// a path this test chose.
func TestGuardRefusesAnUnnamedBase(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		c       *Cgroup
		errLike string
	}{
		{"a base without the prefix",
			&Cgroup{base: filepath.Join(root, "someone-else"), dir: filepath.Join(root, "someone-else", "win-1")},
			"is not a shelld base"},
		{"a dir outside the base",
			&Cgroup{base: filepath.Join(root, baseName(1, 1)), dir: filepath.Join(root, "elsewhere", "win-1")},
			"is not under"},
		{"the base itself",
			&Cgroup{base: filepath.Join(root, baseName(1, 1)), dir: filepath.Join(root, baseName(1, 1))},
			"is not under"},
		{"a sibling whose path starts with the base",
			&Cgroup{base: filepath.Join(root, baseName(1, 1)), dir: filepath.Join(root, baseName(1, 1)+"-evil", "win-1")},
			"is not under"},
	}
	for _, tc := range cases {
		for _, call := range []struct {
			what string
			fn   func() error
		}{
			{"Kill", tc.c.Kill},
			{"Remove", tc.c.Remove},
		} {
			err := call.fn()
			if err == nil {
				t.Errorf("%s: %s succeeded on %s", tc.name, call.what, tc.c.dir)
				continue
			}
			if !strings.Contains(err.Error(), tc.errLike) {
				t.Errorf("%s: %s = %v, want %q", tc.name, call.what, err, tc.errLike)
			}
			// And it refused before touching the filesystem.
			if _, serr := os.Stat(filepath.Join(tc.c.dir, "cgroup.kill")); serr == nil {
				t.Errorf("%s: %s created something under %s", tc.name, call.what, tc.c.dir)
			}
		}
	}
}

// TestLookTrueFindsSomethingThatExits covers the probe's subject. Probe's last
// step is a real spawn, and it needs a program that exits 0 and reads nothing;
// there is no test for what it returns otherwise, because the list it searches is
// not injectable and both entries are present on this box.
func TestLookTrueFindsSomethingThatExits(t *testing.T) {
	exe, err := lookTrue()
	if err != nil {
		t.Skipf("no /bin/true on this host: %v", err)
	}
	st, err := os.Stat(exe)
	if err != nil || st.IsDir() {
		t.Fatalf("lookTrue returned %s: %v", exe, err)
	}
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{})
	if err != nil {
		t.Fatalf("spawning %s: %v", exe, err)
	}
	state, err := proc.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exited() || state.ExitCode() != 0 {
		t.Errorf("%s ended as %v, want exit 0: Probe reads a non-zero exit as no cgroup support", exe, state)
	}
}

// TestOwnCgroupIsTheV2Line checks the path every base hangs off against the
// other place the kernel publishes it. The two are read from the same file by
// different means: this one takes the line with no controller name, and the
// oracle takes the whole file and looks for our base under whatever it named.
func TestOwnCgroupIsTheV2Line(t *testing.T) {
	own, err := ownCgroup()
	if err != nil {
		t.Skipf("no cgroup v2 path for this process: %v", err)
	}
	if !strings.HasPrefix(own, "/") {
		t.Errorf("ownCgroup = %q, want an absolute cgroup path", own)
	}
	dir := filepath.Join(mountPoint, own)
	if _, err := os.Stat(filepath.Join(dir, "cgroup.events")); err != nil {
		t.Errorf("%s is not a cgroup directory: %v", dir, err)
	}
	// It is the cgroup this process is IN, which is a stronger statement than the
	// path existing: our pid has to be listed in it.
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		t.Skipf("cannot read %s/cgroup.procs: %v", dir, err)
	}
	self := strconv.Itoa(os.Getpid())
	if !slices.Contains(strings.Fields(string(b)), self) {
		t.Errorf("%s does not list this process (%s)", dir, self)
	}
}
