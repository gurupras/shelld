package shelld

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func countOpenDescriptors(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatalf("open /proc/self/fd: %v", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(names) // includes this directory's own descriptor, consistently
}

// reservableSlots is the largest size reserveDescriptorTable accepts on this host:
// descriptorTableSlots, or the soft RLIMIT_NOFILE where that is smaller. The tests
// that reserve for reasons of their own ask for this and not for
// descriptorTableSlots, because a host whose limit is already below the full
// reservation is a host the clamp exists for and not a host where reserving fails:
// asking for 4096 under a limit of 256 made two tests in this file FAIL there rather
// than exercise what they are about.
//
// Computed from the limit rather than read from descriptorTableTarget, so that a
// target which gets it wrong is a failure in the test above and not a premise these
// tests quietly adopt.
func reservableSlots(t *testing.T) int {
	t.Helper()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("reading RLIMIT_NOFILE: %v", err)
	}
	if lim.Cur < uint64(descriptorTableSlots) {
		return int(lim.Cur) // in range: the branch bounds it below 4096
	}
	return descriptorTableSlots
}

// The point of the reservation is the table, not the descriptor: parking one in
// the top slot and closing it must leave the process's descriptor set exactly as
// it was, or the daemon leaks one per call.
func TestReserveDescriptorTableLeavesNoDescriptorBehind(t *testing.T) {
	n := reservableSlots(t)
	before := countOpenDescriptors(t)
	for i := range 3 {
		if err := reserveDescriptorTable(n); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if got := countOpenDescriptors(t); got != before {
			t.Fatalf("call %d left %d descriptors open, want %d", i, got, before)
		}
	}
	// Nothing above observes that a descriptor was parked at all, so this test alone
	// cannot tell three reservations from three no-ops — that is
	// TestReserveDescriptorTableParksPastTheTableEnd's job, and it is what makes the
	// three iterations here mean "three parks, three releases".
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(n-1), syscall.F_GETFD, 0); errno != syscall.EBADF {
		t.Fatalf("slot %d is still open after the reservation (fcntl: %v)", n-1, errno)
	}
}

// The untimed gate for the expansion: after the call the table holds at least the
// size asked for. Without it every other untimed test here passes under a
// reserveDescriptorTable that parks nothing and returns nil — the leak count is
// unchanged (nothing was opened), the occupied slot is unclobbered (nothing was
// duped), and the two refusal tests exercise only the guards that run before the
// park. A reviewer demonstrated exactly that: the body after the F_GETFD probe
// replaced with `return nil`, every other untimed test green.
//
// It reads FDSize rather than watching the park through a hook, which is what this
// test used to do. The claim worth gating is the table's size, and FDSize is that
// number; a hook that fires says a descriptor was placed somewhere. The two come
// apart in both directions, and neither favours the hook:
//
//   - Parking one slot low, or halfway up, leaves the table exactly as large
//     wherever the size asked for is a power of two, because a table covering slot
//     s is rounded up to the next power of two above s. So a hook asserting
//     `slot == n-1` kills mutants with no observable consequence.
//   - Where the size asked for is NOT a power of two, one slot low is one power of
//     two short, and that is a real shortfall. Hence the limit below.
//
// So the size reserved here is not descriptorTableSlots but growProbeSize's 2^k+1,
// which makes the shortfall a whole power of two and visible. It needs no lowered
// limit, because it is derived from whatever this host allows.
//
// A child, for the reason TestInitDescriptorTableActsOnTheTarget gives: a
// reservation is permanent and process-wide, so in-process the table would already
// be large by the time this ran, and the reading needs a process of its own.
//
// Calling twice is TestReserveDescriptorTableLeavesNoDescriptorBehind's job — it
// reserves three times and fails on any error, which is what a second call must not
// produce. FDSize cannot add to that: the table does not shrink, so a second call
// that did nothing at all reads the same as one that worked.
func TestReserveDescriptorTableGrowsTheTable(t *testing.T) {
	if arm := os.Getenv(fdStallArmEnv); arm != "" {
		fdGrowArm(t)
		return
	}
	// A size the table would have reached on its own buys nothing: the daemon grows
	// past it at runtime and pays the grace period there, which is the defect. 1024
	// is a floor rather than the value chosen — a window costs a handful of
	// descriptors, so a daemon holding a hundred of them stays well inside it and
	// never crosses again. Checked in the parent because it is about the constant and
	// holds on every host, whatever the child was able to do.
	if descriptorTableSlots < 1024 {
		t.Errorf("descriptorTableSlots is %d, small enough that a daemon in ordinary use grows the table past it and pays the grace period anyway", descriptorTableSlots)
	}

	line := runArm(t, "^TestReserveDescriptorTableGrowsTheTable$", "grow")
	t.Log(line)
	fields := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	if s := fields["skip"]; s != "" {
		t.Skipf("child arm declined to run: %s", s)
	}
	asked := mustAtoi(t, fields, "asked")
	before := mustAtoi(t, fields, "before")
	if before >= asked {
		t.Fatalf("premise broken: the child's table already held %d slots against a reservation of %d, so there was nothing to grow and a call that parks nothing would read the same", before, asked)
	}
	if after := mustAtoi(t, fields, "after"); after < asked {
		t.Errorf("after reserving %d descriptors the table holds %d: the reservation must place a descriptor past the table's end, which is what makes the kernel grow it to cover the size asked for", asked, after)
	}
}

// growProbeSize is the largest size of the form 2^k+1 that this host will let
// reserveDescriptorTable have. A table that covers one slot fewer than 2^k+1 covers
// 2^k, a whole power of two short, so FDSize distinguishes the two — which it cannot
// do at a power-of-two size, where every slot from 2^(k-1)+1 upwards yields the same
// table.
func growProbeSize(t *testing.T) int {
	t.Helper()
	n := reservableSlots(t)
	size := 1
	for size*2+1 <= n {
		size *= 2
	}
	return size + 1
}

func fdGrowArm(t *testing.T) {
	asked := growProbeSize(t)
	before, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	if before >= asked {
		fmt.Printf("RESULT skip=table-already-%d-asked-%d\n", before, asked)
		return
	}
	// Fatal rather than reported: growProbeSize is within the soft limit by
	// construction, so a refusal here is a broken arm and not a result to parse, and
	// runArm prints the child's whole output when it exits non-zero.
	if err := reserveDescriptorTable(asked); err != nil {
		t.Fatalf("reserving %d descriptors: %v", asked, err)
	}
	after, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	fmt.Printf("RESULT asked=%d before=%d after=%d\n", asked, before, after)
}

// The hazard in the implementation: dup3 onto an occupied slot closes what is
// there without a word. A caller that passes a size covering a live descriptor
// must get an error, and the live descriptor must still be live afterwards.
func TestReserveDescriptorTableRefusesAnOccupiedSlot(t *testing.T) {
	path := t.TempDir() + "/witness"
	if err := os.WriteFile(path, []byte("still here"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	slot := int(f.Fd())

	err = reserveDescriptorTable(slot + 1) // n-1 == slot, which is f's own
	if err == nil {
		t.Fatalf("reserving %d descriptors targeted slot %d, which was open, and reported success", slot+1, slot)
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error does not say the slot was in use: %v", err)
	}

	// The assertion that matters: the refusal happened BEFORE the dup, so the
	// descriptor still refers to the same file. A clobbered slot would read as
	// /dev/null, which is empty rather than an error.
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n == 0 {
		t.Fatalf("witness descriptor unusable after the refusal: %v", err)
	}
	if got := string(buf[:n]); got != "still here" {
		t.Fatalf("witness descriptor now reads %q: the slot was clobbered", got)
	}
}

func TestReserveDescriptorTableRefusesWhatItCannotDo(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Cur > uint64(1)<<40 {
		t.Skipf("RLIMIT_NOFILE is %d, too large to exceed in an int", lim.Cur)
	}
	// Each case asserts the REASON and not just the failure, because for these two
	// guards the reason is the whole product: dup3 rejects a negative slot and a
	// slot above the limit on its own, so removing either guard still returns an
	// error, just an opaque EBADF. InitDescriptorTable logs what comes back, so a
	// reader of that log is who the guards are for.
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"zero", 0, "no slot to park in"},
		{"negative", -1, "no slot to park in"},
		{"above the soft limit", int(lim.Cur) + 1, "RLIMIT_NOFILE is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := countOpenDescriptors(t)
			err := reserveDescriptorTable(tc.n)
			if err == nil {
				t.Fatalf("reserve %d reported success", tc.n)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("reserve %d said %q, which does not name the reason (%q)", tc.n, err, tc.want)
			}
			if got := countOpenDescriptors(t); got != before {
				t.Fatalf("the refused call left %d descriptors open, want %d", got, before)
			}
		})
	}
}

const (
	fdStallArmEnv    = "SHELLD_FD_STALL_ARM"
	fdStallTimingEnv = "SHELLD_FD_STALL_TIMING"
	fdStallAllocs    = 300 // enough to cross 64, 128 and 256 from a test binary's handful
	fdStallSlowMs    = 5.0
	// fdInitialTableSlots is the size a process's descriptor table starts at:
	// NR_OPEN_DEFAULT, which is BITS_PER_LONG, 64 on a 64-bit kernel. It matters
	// because a power of two BELOW it is not a boundary — the first 63 descriptors
	// fit in the table the process was born with, and only 64, 128 and 256 make the
	// kernel allocate a new one. The plain arm checks this assumption rather than
	// resting on it: it asserts that the powers of two below this size are fast.
	fdInitialTableSlots = 64
)

// splitAtTableStart divides the powers of two an arm's descriptors landed on into
// the ones that force an expansion and the ones inside the table the process
// started with.
func splitAtTableStart(fds []int) (boundaries, insideTable []int) {
	for _, fd := range fds {
		if fd >= fdInitialTableSlots {
			boundaries = append(boundaries, fd)
		} else {
			insideTable = append(insideTable, fd)
		}
	}
	return boundaries, insideTable
}

// TestDescriptorTableExpansionRemovesTheStall is the measurement the change
// exists for: without the reservation, the allocation that crosses a power of two
// blocks for an RCU grace period; with it, no allocation in the run does.
//
// Two properties force the shape of this test. The expansion is irreversible
// within a process, so the two arms cannot share one; and a fixed order would
// hand the second arm every warmed cache in the run, which is the arm the change
// wants to win. So each arm runs in its own child of this binary and the order
// alternates between rounds.
//
// It is gated because what it measures is a kernel grace period, whose duration
// belongs to the machine and not to this code: it is a measurement to be run, not
// a gate to be trusted. Run it with SHELLD_FD_STALL_TIMING=1.
func TestDescriptorTableExpansionRemovesTheStall(t *testing.T) {
	if arm := os.Getenv(fdStallArmEnv); arm != "" {
		fdStallArm(t, arm)
		return
	}
	if os.Getenv(fdStallTimingEnv) == "" {
		t.Skipf("set %s=1: this times a kernel grace period, so it is a measurement and not a gate", fdStallTimingEnv)
	}
	for round, order := range [][]string{{"plain", "reserved"}, {"reserved", "plain"}} {
		for _, arm := range order {
			line := runFDStallArm(t, arm)
			t.Logf("round %d %-8s %s", round, arm, line)
			r := parseFDStall(t, line)
			// The premise the old version of this test did not check. It counted slow
			// allocations and asked for three, which a pre-expanded plain arm plus any
			// 6 ms hiccup satisfies at descriptors 3, 6 and 7 — a reviewer produced
			// exactly that PASS with zero boundaries crossed. What the two arms have to
			// agree on is the DESCRIPTORS: both reach the same powers of two, and the
			// durations are compared at each one.
			boundaries, insideTable := splitAtTableStart(r.fds)
			if len(boundaries) < 3 {
				t.Fatalf("the arm started at %d descriptors and reached the powers of two %v, of which only %v are past the table's initial %d slots; %d allocations should reach 64, 128 and 256, and below three of them neither arm says anything about a boundary",
					r.base, r.fds, boundaries, fdInitialTableSlots, fdStallAllocs)
			}
			switch arm {
			case "plain":
				// Per boundary, not in aggregate: one fast crossing means the table was
				// already that large, which is the plain arm not being the plain arm.
				for _, fd := range boundaries {
					if r.cross[fd] <= fdStallSlowMs {
						t.Errorf("plain arm obtained descriptor %d in %.3f ms, want more than %.1f: that allocation is the one that grows the table past %d, so it is where the grace period lands",
							fd, r.cross[fd], fdStallSlowMs, fd)
					}
				}
				// And the assumption behind `boundaries` checks itself here: a power of
				// two below the initial size is not a crossing, so if one of those is
				// slow the table did not start where fdInitialTableSlots says and the set
				// asserted above is the wrong set.
				for _, fd := range insideTable {
					if r.cross[fd] > fdStallSlowMs {
						t.Errorf("plain arm took %.3f ms to obtain descriptor %d, which is inside the table's initial %d slots: the initial size is not what this test assumes, so the boundaries it checks are wrong",
							r.cross[fd], fd, fdInitialTableSlots)
					}
				}
				if r.slow != len(boundaries) {
					// Not a failure: an unrelated hiccup on a loaded box is not this code's,
					// and the per-boundary assertions above are what carry the claim.
					// Recorded rather than tolerated silently, because a count that nobody
					// looks at is how the old version passed.
					t.Logf("  note: %d slow allocations at %s, against %d boundaries %v — the extras are not asserted on",
						r.slow, r.at, len(boundaries), boundaries)
				}
			case "reserved":
				// Every power of two, not just the boundaries: after the reservation none
				// of them is a boundary, which is the whole claim.
				for _, fd := range r.fds {
					if r.cross[fd] > fdStallSlowMs {
						t.Errorf("reserved arm obtained descriptor %d in %.3f ms: the reservation already grew the table past %d, so this allocation had nothing to wait for",
							fd, r.cross[fd], fd)
					}
				}
				if r.slow != 0 {
					t.Errorf("reserved arm had %d slow allocations at %s: the table was already expanded, so nothing should have waited",
						r.slow, r.at)
				}
			}
		}
	}
}

// A size equal to the soft limit is the largest legal one: the highest descriptor
// a process may hold is RLIMIT_NOFILE-1, which is that size's top slot. Testing it
// needs a lowered limit, and lowering this binary's would apply to every other
// test in the package, so it runs in a child.
func TestReserveDescriptorTableAtExactlyTheSoftLimit(t *testing.T) {
	if arm := os.Getenv(fdStallArmEnv); arm != "" {
		fdLimitArm(t)
		return
	}
	line := runArm(t, "^TestReserveDescriptorTableAtExactlyTheSoftLimit$", "limit")
	t.Log(line)
	fields := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	if s := fields["skip"]; s != "" {
		t.Skipf("child arm declined to run: %s", s)
	}
	if fields["atlimit"] != "ok" {
		t.Errorf("a size equal to the soft limit was refused, and its top slot is legal: atlimit=%s", fields["atlimit"])
	}
	if fields["above"] != "err" {
		t.Errorf("a size one past the soft limit was accepted: above=%s", fields["above"])
	}
}

func fdLimitArm(t *testing.T) {
	const limit = 512
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Cur < limit {
		fmt.Printf("RESULT skip=soft-limit-already-%d\n", lim.Cur)
		return
	}
	lowered := lim
	lowered.Cur = limit
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatalf("lowering RLIMIT_NOFILE: %v", err)
	}
	report := func(err error) string {
		if err != nil {
			return "err"
		}
		return "ok"
	}
	atLimit := reserveDescriptorTable(limit)
	above := reserveDescriptorTable(limit + 1)
	fmt.Printf("RESULT atlimit=%s above=%s atlimiterr=%v\n", report(atLimit), report(above), atLimit)
}

func runFDStallArm(t *testing.T, arm string) string {
	return runArm(t, "^TestDescriptorTableExpansionRemovesTheStall$", arm)
}

func runArm(t *testing.T, test, arm string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", test, "-test.count=1")
	cmd.Env = append(os.Environ(), fdStallArmEnv+"="+arm)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s arm: %v\n%s", arm, err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "RESULT ") {
			return strings.TrimPrefix(line, "RESULT ")
		}
	}
	t.Fatalf("%s arm printed no RESULT line:\n%s", arm, out)
	return ""
}

// fdStallResult is one arm's report. fds and cross are the discriminating part:
// the descriptors that are powers of two above where the arm started, and how long
// the allocation that obtained each one took.
type fdStallResult struct {
	base, slow int
	at         string
	fds        []int
	cross      map[int]float64
}

func parseFDStall(t *testing.T, line string) fdStallResult {
	t.Helper()
	fields := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	r := fdStallResult{at: fields["at"], cross: map[int]float64{}}
	if _, err := fmt.Sscan(fields["base"], &r.base); err != nil {
		t.Fatalf("no base in %q: %v", line, err)
	}
	if _, err := fmt.Sscan(fields["slow"], &r.slow); err != nil {
		t.Fatalf("no slow count in %q: %v", line, err)
	}
	// Parsed strictly: a pow2 field this function silently skipped would look like an
	// arm that crossed nothing, which the caller treats as a premise failure but
	// would in fact be this parser's bug.
	for _, pair := range strings.Split(fields["pow2"], ",") {
		if pair == "" {
			continue
		}
		fdText, msText, ok := strings.Cut(pair, ":")
		if !ok {
			t.Fatalf("pow2 entry %q in %q is not fd:ms", pair, line)
		}
		var fd int
		var ms float64
		if _, err := fmt.Sscan(fdText, &fd); err != nil {
			t.Fatalf("pow2 entry %q in %q: %v", pair, line, err)
		}
		if _, err := fmt.Sscan(msText, &ms); err != nil {
			t.Fatalf("pow2 entry %q in %q: %v", pair, line, err)
		}
		if fd <= r.base || fd&(fd-1) != 0 {
			t.Fatalf("pow2 reports descriptor %d, which is not a power of two above the arm's starting %d", fd, r.base)
		}
		r.fds = append(r.fds, fd)
		r.cross[fd] = ms
	}
	return r
}

// fdStallArm runs in a child process. It allocates descriptors one at a time and
// times each allocation, which is the syscall the grace period lands on: in a
// window create it is the pipe os/exec opens, here it is the open itself, and the
// kernel path they share is expand_files.
func fdStallArm(t *testing.T, arm string) {
	base := countOpenDescriptors(t)
	if arm == "reserved" {
		if err := reserveDescriptorTable(descriptorTableSlots); err != nil {
			fmt.Printf("RESULT err=%v\n", err)
			t.Fatalf("reserve: %v", err)
		}
	}
	held := make([]*os.File, 0, fdStallAllocs)
	defer func() {
		for _, f := range held {
			_ = f.Close()
		}
	}()
	samples := make([]float64, 0, fdStallAllocs)
	slowAt := []int{}
	// Keyed by descriptor, for the powers of two only. The plain arm expands its
	// table on the allocation that OBTAINS such a descriptor, so this is the
	// per-crossing cost, reported for both arms at the same descriptors: in the
	// reserved arm those numbers are the same allocations no longer crossing
	// anything, which is the comparison the test makes.
	crossings := map[int]float64{}
	for range fdStallAllocs {
		start := time.Now()
		f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		d := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		held = append(held, f)
		samples = append(samples, d)
		if d > fdStallSlowMs {
			slowAt = append(slowAt, int(f.Fd()))
		}
		if fd := int(f.Fd()); fd > base && fd&(fd-1) == 0 {
			crossings[fd] = d
		}
	}
	fds := make([]int, 0, len(crossings))
	for fd := range crossings {
		fds = append(fds, fd)
	}
	sort.Ints(fds)
	parts := make([]string, 0, len(fds))
	for _, fd := range fds {
		parts = append(parts, fmt.Sprintf("%d:%.3f", fd, crossings[fd]))
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	// Comma-separated and not %v: the parser splits the line on whitespace, so a
	// `[3 6 7]` reaches an error message as `[3` and reports three slow allocations
	// as one.
	slowText := make([]string, 0, len(slowAt))
	for _, fd := range slowAt {
		slowText = append(slowText, strconv.Itoa(fd))
	}
	fmt.Printf("RESULT base=%d slow=%d max=%.3f p50=%.3f at=%s pow2=%s\n",
		base, len(slowAt), sorted[len(sorted)-1], sorted[len(sorted)/2],
		strings.Join(slowText, ","), strings.Join(parts, ","))
}

// fdTableSlots is the descriptor table's capacity, read from FDSize in
// /proc/self/status. It is the one userspace view of the thing this file changes:
// the expansion has no other observable, and the table never shrinks, so a reading
// taken after a reservation still reports it.
func fdTableSlots() (int, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "FDSize:"); ok {
			return strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return 0, fmt.Errorf("no FDSize line in /proc/self/status")
}

// Where the soft limit has room, the target is the full reservation and nothing is
// clamped. That is the arm that must NOT change: clamping when there is room would
// tie the table to whatever limit the host happened to set.
//
// Which arm this checks is decided by the host rather than skipped on, because the
// two lowered-limit tests below both need a limit they can lower FROM and skip
// together on a host whose limit is already small — a container with
// LimitNOFILE=256, which is exactly the kind of host the clamp exists for. There
// the clamped arm is the live one, and it is checkable in-process with no child.
func TestDescriptorTableTargetTracksTheSoftLimit(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	wantN, wantClamped := descriptorTableSlots, false
	if lim.Cur < uint64(descriptorTableSlots) {
		wantN, wantClamped = int(lim.Cur), true
	}
	n, clamped, err := descriptorTableTarget()
	if err != nil {
		t.Fatalf("descriptorTableTarget: %v", err)
	}
	if n != wantN || clamped != wantClamped {
		t.Errorf("target with a soft limit of %d is (%d, clamped=%t), want (%d, %t)", lim.Cur, n, clamped, wantN, wantClamped)
	}
}

// Computing the target is worth nothing unless InitDescriptorTable acts on it, and
// on a host whose soft limit is already below the full reservation nothing else here
// checks that. The two tests that lower a limit skip where there is no limit to lower
// from, and the test above exercises descriptorTableTarget in isolation, which a
// correct helper passes however Init treats the answer. So on the LimitNOFILE=256
// container this clamp exists for, an InitDescriptorTable that reserved
// descriptorTableSlots directly — the one-line simplification somebody will
// eventually propose — is refused, declines, leaves the table at NR_OPEN_DEFAULT, and
// passes every other test in this file.
//
// It lowers nothing, so it runs on every host, and where the limit has room it is
// still not vacuous: an Init that reserves nothing leaves the table at 64 and reds
// here. What it cannot see there is the wiring above, which under a limit of 4096 or
// more asks for the same size either way.
//
// It runs in a child for the table's sake rather than the limit's. Every reservation
// in this file is process-wide and permanent, so by the time this test ran in-process
// the table would already cover the target and the reading would be of an expansion
// some earlier test performed — the premise below would skip, and under -run or
// -shuffle it would sometimes skip and sometimes not. A child starts at
// NR_OPEN_DEFAULT whatever else the package has done.
func TestInitDescriptorTableActsOnTheTarget(t *testing.T) {
	if arm := os.Getenv(fdStallArmEnv); arm != "" {
		fdActsArm(t)
		return
	}
	line := runArm(t, "^TestInitDescriptorTableActsOnTheTarget$", "acts")
	t.Log(line)
	fields := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	if s := fields["skip"]; s != "" {
		t.Skipf("child arm declined to run: %s", s)
	}
	target := mustAtoi(t, fields, "target")
	before := mustAtoi(t, fields, "before")
	if fields["init"] != "true" {
		t.Errorf("InitDescriptorTable declined with a target of %d slots and a table of %d (init=%s)", target, before, fields["init"])
	}
	if after := mustAtoi(t, fields, "after"); after < target {
		t.Errorf("InitDescriptorTable left the table holding %d slots against a target of %d: the target was computed and then not acted on", after, target)
	}
}

func fdActsArm(t *testing.T) {
	target, _, err := descriptorTableTarget()
	if err != nil {
		t.Fatalf("descriptorTableTarget: %v", err)
	}
	before, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	// Reported rather than assumed: with the table already covering the target there is
	// nothing to expand, and an Init that did nothing at all would read the same.
	if before >= target {
		fmt.Printf("RESULT skip=table-already-%d-target-%d\n", before, target)
		return
	}
	ok := InitDescriptorTable()
	after, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	fmt.Printf("RESULT target=%d before=%d init=%t after=%d\n", target, before, ok, after)
}

// The reservation must not become a no-op on the hosts it exists for. systemd's
// default soft RLIMIT_NOFILE for a service is 1024, which is below
// descriptorTableSlots, and reserveDescriptorTable refuses a size above the soft
// limit rather than clamping it — a size that high has no legal top slot to park
// in. So InitDescriptorTable has to do the clamping, and if it does not, the whole
// change is a no-op wherever it would matter most: the table stays at
// NR_OPEN_DEFAULT and the daemon pays the grace period at 64, 128, 256 and 512.
//
// This box cannot see that. Its soft limit is over a million, so the refusal is
// unreachable here and every other test in this file passes either way. The lowered
// limit is the point of the test.
//
// What it checks is that the table ends up covering the limit, which is what the
// daemon actually gets, rather than that some descriptor was placed somewhere.
//
// It runs in a child: lowering this binary's limit would apply to every other test
// in the package.
func TestInitDescriptorTableClampsToTheSoftLimit(t *testing.T) {
	if arm := os.Getenv(fdStallArmEnv); arm != "" {
		fdClampArm(t)
		return
	}
	line := runArm(t, "^TestInitDescriptorTableClampsToTheSoftLimit$", "clamp")
	t.Log(line)
	fields := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	if s := fields["skip"]; s != "" {
		t.Skipf("child arm declined to run: %s", s)
	}
	soft := mustAtoi(t, fields, "soft")
	before := mustAtoi(t, fields, "before")

	// Two premises, asserted rather than assumed, because either one failing makes
	// the assertions below pass for a reason that has nothing to do with the clamp.
	if fields["unclamped"] != "err" {
		t.Fatalf("premise broken: asking for the full %d slots under a soft limit of %d was accepted (unclamped=%s), so there is no refusal here for a clamp to avoid", descriptorTableSlots, soft, fields["unclamped"])
	}
	if before >= soft {
		t.Fatalf("premise broken: the child's table already held %d slots at a soft limit of %d, so there was nothing left to expand and a broken clamp would look identical", before, soft)
	}

	if fields["init"] != "true" {
		t.Errorf("InitDescriptorTable declined under a soft limit of %d (init=%s); systemd's default is exactly this limit, so declining here is declining in production", soft, fields["init"])
	}
	// The invariant worth having, and stronger than "it grew": the table covers every
	// descriptor the process is permitted to hold, so no crossing is reachable at all.
	// Observed exactly equal to the limit at 1024, 512 and 256 on this kernel.
	if after := mustAtoi(t, fields, "after"); after < soft {
		t.Errorf("after the reservation the table holds %d slots under a soft limit of %d; it must cover the whole legal descriptor range, or a crossing between %d and %d still pays the grace period", after, soft, after, soft)
	}
}

func mustAtoi(t *testing.T, fields map[string]string, key string) int {
	t.Helper()
	v, err := strconv.Atoi(fields[key])
	if err != nil {
		t.Fatalf("child arm reported %s=%q, which is not a number: %v", key, fields[key], err)
	}
	return v
}

func fdClampArm(t *testing.T) {
	// Not an arbitrary small number: 1024 is systemd's default soft RLIMIT_NOFILE
	// for a service, so it is the limit the daemon will actually meet.
	const limit = 1024
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Cur < limit {
		fmt.Printf("RESULT skip=soft-limit-already-%d\n", lim.Cur)
		return
	}
	lowered := lim
	lowered.Cur = limit
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatalf("lowering RLIMIT_NOFILE: %v", err)
	}
	before, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	// Taken before the clamped call, and safe there: reserveDescriptorTable checks
	// the size against the limit before it opens or duplicates anything, so a refusal
	// cannot be what expands the table.
	unclamped := "ok"
	if reserveDescriptorTable(descriptorTableSlots) != nil {
		unclamped = "err"
	}
	ok := InitDescriptorTable()
	after, err := fdTableSlots()
	if err != nil {
		t.Fatalf("reading FDSize: %v", err)
	}
	fmt.Printf("RESULT soft=%d before=%d unclamped=%s init=%t after=%d\n", limit, before, unclamped, ok, after)
}
