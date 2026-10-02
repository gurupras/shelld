//go:build unix

package proctree

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// KillTree SIGKILLs root and every descendant. It returns the targeted pids
// and true; individual failures (already-dead pids) are ignored.
//
// start is root's start time in clock ticks since boot, from StartTicks, read while
// root was still the caller's unreaped child. It is not optional and there is no
// sentinel for "unknown", because a pid on its own does not name a process: pids
// wrap (about 11 days of steady forking at this box's pid_max), and the whole of
// this package's safety argument is about which processes are root's — a claim that
// quantifies over processes while the code holds an integer. If the platform no
// longer knows root, or knows it with a different start time, the pid has been
// reaped or reused and KillTree signals NOTHING; it reports the empty set and true,
// because a refusal is a successful kill of a tree that no longer exists.
//
// That empty-set-and-true is why StartTicks failing wholesale is not a small bug,
// and it is what darwin used to do: with no /proc, StartTicks answered false for
// every pid, so this refused every root and killed nothing on that platform while
// reporting success. Window.reapChildTree survives it — it falls back on an empty
// answer rather than only on ok == false, so the direct child still dies — but the
// fallback is os.Process.Kill, one pid and not a group, so the setsid'd grandchild
// in the package comment lived. See platform_darwin.go for the ids that fixed it.
//
// The reach that costs, stated rather than hidden: once root is reaped, its F3
// orphans are unreachable by this oracle even though they may still be alive, since
// the session sweep's key is root's pid and a live stranger holding that pid would
// lead a session of its own with the same id. There is no /proc evidence that
// separates the two cases, so the sweep is refused rather than guessed. Reaching
// those orphans needs the cgroup membership set (see cgroups.go, reapCgroup), which
// survives both the reap and the reparent.
//
// A root that is not a positive pid kills NOTHING, and the check is on the sign
// rather than on zero alone because syscall.Kill reads both non-positive values as
// a broadcast: 0 is "every process in our own process group" — the daemon and its
// siblings — and -1 is "every process we may signal", i.e. the whole uid. A
// negative root is not merely a broadcast either: Descendants(-1) is [-1], so
// nothing in the oracle bounds it, and Kill(-N, SIGKILL) SIGKILLs process group N
// whether or not this daemon created it. Reproduced against a group the reviewer
// created: KillTree(-P) returned [-P], true and took the group from 3 members to
// 1. Descendants carries the same guard, so this one is the second of two.
//
// "Descendant" is Descendants' answer, which is wider than the parent-link
// closure: if root leads a session, every member of that session is killed, on
// the ground that a session can only be entered by fork from a member (see
// unionDescendants). So root must be a process this daemon started as a session
// leader — pty.Start does that for every window shell — and NOT some pid
// belonging to a session the caller did not create. Against a root that leads no
// session the answer degrades to the parent closure, which is safe but loses an
// orphaned subtree.
func KillTree(root int, start uint64) ([]int, bool) {
	if root <= 0 {
		return []int{}, true
	}
	// Checked before the enumeration, so a stale root never reaches the session
	// sweep; and the enumeration re-reads root's own stat anyway, so a REAP between
	// the two only ever shrinks the answer: root goes absent from the id map and
	// unionDescendants returns the parent closure with the session term skipped.
	//
	// The other landing in the same window is not bounded by that clause. A reap plus a
	// reuse between this read and the enumeration leaves a stranger's row under root's
	// pid, and if that stranger leads its own session unionDescendants splices in that
	// whole session — a widening, not a shrink. The doc above rules on reuse before the
	// call, which this read catches; it does not rule on reuse after it.
	//
	// An earlier version of this comment claimed the reverse of that clause: that "a
	// caller that reaps root before calling has no protection from this test at all, and
	// nothing in this package can stop one being written." That is wrong, and this read is
	// the protection it denied. A root reaped before the call reaches here in one of two
	// states, and both refuse: with the pid still free there is no row, so startTicks
	// answers !ok; with the pid already reissued the stranger's row carries its own start
	// time, which is not the caller's. What is true of a pre-call reap is only what is
	// true of every equality test here — the tick collision bounded below, and a start
	// argument of 0, neither of which is specific to reaping before the call.
	//
	// That case is checked rather than argued about, immediately after the enumeration
	// below: root's row in the snapshot the answer is actually derived from, against the
	// start this call was given. Two notes on why the check sits there.
	//
	// It costs no syscall. The id map already carries every process's start time
	// (procIDs.start, filled by parseIDs), root's included, so the OBSERVED value was
	// always in hand; what was missing was the EXPECTED one, since unionDescendants
	// takes (root, ids) and nothing hands it this call's argument.
	//
	// And that is why the check did not move INTO the oracle, which is the tidier-looking
	// place for it. "Is this still the root I was given" is KillTree's question.
	// Descendants(root) is a public entry point with no start to supply and no business
	// acquiring one — asking for a process tree is not asserting an identity. Widening
	// the oracle's signature, or giving it an unchecked mode, remain the things not to do.
	//
	// What made this a residual rather than a live bug is in the caller, and it is worth
	// keeping because it is now the second line of defence rather than the only one:
	// root's zombie can only be reaped by an explicit wait, which is PTY.Wait
	// (PTY.Wait, pty/pty_unix.go), and Window.Kill calls reapChildTree before its <-w.done,
	// so Kill cannot enter the window it opens. There is no kernel auto-reap to route
	// around that: an inherited SIGCHLD=SIG_IGN would do it, but the Go runtime clears
	// one — measured on go1.26.1 linux/amd64, SigIgn bit 16 survives execve into a
	// non-Go binary and reads 0 in a Go one — and nothing in this tree sets SIGCHLD in
	// non-test code. waitLoop is concurrent, though, so a shell exiting on its own can
	// still land inside the window, which is the case the check below exists for.
	//
	// What the check does NOT cover, stated without a rate for the same reason as before.
	// Every test here is equality on an opaque start time, so a stranger passes by
	// holding root's exact value. On Linux that is clock ticks at 100 Hz, and ticks
	// collide freely in general — 1123 processes on the development box carried 709
	// distinct values, 67 of them sharing tick 27 in the boot storm — but for a RECYCLED
	// pid the collision has to be with the tick root itself started in, so the reap and
	// the pid wrap both have to land inside one 10 ms window. That is a phase
	// coincidence, not a rate, and nothing in this package bounds it. A rate is what a
	// reader would use to dismiss it, against a consequence that is SIGKILL to a
	// stranger's whole session.
	//
	// The reachable residual is the opposite end. A start time of 0 means "no identity
	// available", in two different directions: as this call's ARGUMENT it makes every
	// test here refuse, so KillTree returns the empty set and TRUE; as a snapshot ROW it
	// makes the check below skip, so that pid is signalled on the strength of an integer.
	// The first is platform_other's behaviour by decision, and on Linux it is what a
	// failed read at spawn produces (window.go, `w.pidStart, startOK = StartTicks`).
	//
	// What that refusal COSTS is not stated here, deliberately. reapChildTree's doc
	// (window.go) owns it and already answers the tempting loud reading — that the
	// fallback fires on an empty answer and not only on ok == false, and why the weakest
	// case is a unix with no implementation in proctree. Do not restate that argument
	// here: a comment in proctree asserting how Window.Kill sequences its teardown is a
	// claim whose scope is wider than the file making it, in the package that is imported
	// rather than the one importing, so this is the copy that would drift.
	//
	// What that doc does not cover is the CAUSE, which is the half worth carrying here. It
	// frames the residual per-platform, and a failed read at spawn puts the same residual
	// on a platform that HAS an implementation, for one window, permanently, out of one
	// transient /proc failure. Nothing HERE can tell that apart from "this is the wrong
	// root", since the argument is all it has — so only the spawn site can name the cause,
	// which is why the ok is kept there rather than logged at kill time.
	//
	// 0 works as that sentinel for a measured reason rather than an assumed one: no live
	// process on Linux reports start tick 0 (0 of those same 1123, and pid 1 is at tick
	// 17), and darwin's value is microseconds since the epoch, where 0 is 1970.
	if live, ok := startTicksOf(root); !ok || live != start {
		return []int{}, true
	}
	// The enumeration's own snapshot, kept rather than discarded, because the loop
	// below re-checks every pid it answers against the start time it was read with.
	// What that costs is one open+read+close per answered pid, against an
	// enumeration that has already walked all of /proc; the reasoning is at the loop
	// itself rather than here, where the only decision is to keep the map.
	ids := readIDMap()
	// The check the gate above prescribed: root's identity against the snapshot
	// the answer is actually derived from, rather than against a /proc read taken
	// before it. That closes the window named there — a reap plus a reuse between the
	// StartTicks gate and this enumeration — and it is the one comparison the per-pid
	// loop below cannot make, because that loop tests each pid against the start the
	// enumeration itself read for that same pid, so a stranger under root's pid is
	// compared against itself, matches, and passes, taking its whole spliced-in
	// session with it. Only KillTree's own argument distinguishes them, and only for
	// root, once.
	//
	// rec.start != 0 is not defensive padding, and it is why this is not the two-line
	// version: idMap's last fallback is psParentMap, which fills ppid alone and leaves
	// every start at zero, so without the guard every row mismatches and KillTree
	// refuses every tree it is asked to kill — reporting each refusal as a successful
	// kill, which is a total silent leak.
	//
	// The platform that reaches it is darwin, and only darwin. It takes the bulk
	// SysctlKinfoProcSlice in platformIDs failing while the per-pid SysctlKinfoProc in
	// platformStart succeeds: different syscalls with independent failures, which is
	// what puts a startless snapshot underneath a StartTicks gate that passed. On the
	// other BSDs this line is unreachable rather than load-bearing — platformStart is
	// the (0, false) stub there, so the gate above returns on !ok first, and refusing
	// every root is that platform's documented behaviour anyway (platform_other's
	// header comment). The loop below carries the same guard, where the reachable
	// population is the same one.
	if rec, ok := ids[root]; ok && rec.start != 0 && rec.start != start {
		return []int{}, true
	}
	pids := unionDescendants(root, ids)
	// Every pid the enumeration answered is re-checked against the start time it was
	// read with, because the identity test above pins ROOT and nothing pinned the
	// rest. A pid reissued while this loop is still walking — a small pid_max, a
	// container's pid namespace, a wrap — would otherwise be SIGKILLed on the strength
	// of an integer. That is the one thing this package promises not to do (see
	// procIDs.start, and unionDescendants on holding an integer while quantifying over
	// processes).
	//
	// Signalling root first is what OPENS that window rather than what closes it, and
	// the mechanism is the reaping, not the reparenting. SIGKILLing root does not free
	// its descendants' pids: they are still running, and reparenting only changes a
	// ppid. A pid is released by exit PLUS reap — so while root is alive its dead
	// children stay zombies under it and their numbers cannot be reissued at all,
	// whereas once root is gone they are orphaned onto init, which reaps them
	// promptly, and the number is free while this loop still has it in hand. Killing
	// descendants first would therefore NARROW this hazard, at the price of leaving
	// root alive and forking for the length of the loop. Root first is kept for that
	// price alone, not because the order is safe.
	//
	// And the price is smaller than "the tree cannot grow", which neither order buys:
	// pids is a fixed slice taken before anything is signalled, so a descendant the
	// loop has not reached yet can fork freely under either order and what it forks
	// is not in that slice to begin with. What root-first actually stops is ROOT
	// forking, which matters because root is the one process in the tree the caller
	// knows the identity of — everything else is here by inference from a snapshot.
	//
	// Only a MISMATCH skips. An unreadable pid does not: /proc/<pid>/stat is
	// unreadable here for a process that is gone, and signalling a gone pid is
	// ESRCH, whereas refusing one would leak exactly the process this package
	// exists to kill. Skipping on a failed read is the inverted polarity that has
	// already cost elsewhere in this tree.
	//
	// A skipped pid is left OUT of the answer, which is why this builds a second
	// slice rather than returning the enumeration. The doc's "targeted pids" is what
	// reapChildTree keys on: its fallback fires on an empty answer, so reporting a pid
	// the loop had just refused to signal would suppress the fallback on the strength
	// of that refusal — the one shape this file's own doc rules out when it says a
	// refusal is not silently a no-op.
	//
	// The residual the loop cannot close, since every other one in this function is
	// named: between the live read below and killProc, that pid can be reaped and
	// reissued, and the signal lands on the stranger. It is irreducible without an
	// atomic check-and-signal (no pidfd here, which would be the fix on Linux alone),
	// and it is bounded far more tightly than the window the root check closes: one
	// pid per iteration, with no session splice, because unionDescendants has already
	// run against a root whose identity was verified.
	killed := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pid == root {
			// root gets its own arm because the snapshot is not where its expected
			// identity comes from: the argument is, and the argument is available
			// whether or not root has a row. Without this arm an ABSENT row takes
			// the permissive default below and root is signalled with no kill-time
			// check at all — and absent is reachable for root and root alone, since
			// unionDescendants seeds out with []int{root} unconditionally while
			// every other element it appends is a key of ids.
			//
			// Absent also means something sharper than unknown here. root missing
			// from a whole-/proc snapshot is root fully reaped, which is exactly
			// when its pid is free to be reissued — so the one case the default
			// treats as "nothing known, signal it" is the case where the number is
			// most likely to have moved to a stranger. The window is snapshot-to-kill
			// and it is WIDER than the gate-to-snapshot one the check above closes,
			// because idMap walks all of /proc first.
			//
			// The gate above is not this check. It ran before readIDMap, so it says
			// root was the caller's process then, not now, and the residual note
			// further up is about a zero row rather than a missing one.
			if live, ok := startTicksOf(root); ok && live != start {
				continue
			}
		} else if rec, known := ids[pid]; known && rec.start != 0 {
			if live, ok := startTicksOf(pid); ok && live != rec.start {
				continue
			}
		}
		_ = killProc(pid, syscall.SIGKILL)
		killed = append(killed, pid)
	}
	return killed, true
}

// killProc is syscall.Kill, replaced by tests with a recorder.
//
// The seam is here because the only interesting property of KillTree is WHICH pids
// it signals, and without the seam there is no safe way for a test to ask. A test
// that calls KillTree with a bad root and inspects the aftermath has already sent
// the broadcast if the guard it is testing is absent — so the test would be the
// bug. Against a recorder the guards above and in unionDescendants can each be
// removed and the answer read off, with nothing signalled either way.
var killProc = syscall.Kill

// startTicksOf is StartTicks, replaced by tests that need KillTree's two live reads
// to disagree with each other.
//
// The seam is needed for ROOT's arm and only root's. A descendant's expected identity
// comes from the snapshot, so readIDMap alone builds the mismatch — tamper with the
// row and the live /proc read disagrees with it (killedset_test.go). Root's expected
// identity comes from the ARGUMENT, which no seam is needed to choose, and its live
// read is real /proc; so on a live root the gate's read and the loop's read are the
// same field microseconds apart and always agree, leaving that arm's skip unreachable
// by any fixture. Producing the state for real means racing a pid wrap. Without this
// seam the arm that refuses to signal a stranger under root's number could be deleted
// with the suite still green, which is the same class of gap as a test whose fixture
// never reaches its arm.
//
// Keep it out of the gate's argument: a test that seams this must decide what the
// gate sees too, and the interleaving under test is precisely that the gate passed
// and the later read did not. That makes call ORDER load-bearing in such a fixture,
// which is why the one test that uses it says so in those words.
var startTicksOf = StartTicks

// readIDMap is idMap, replaced by tests that need the enumeration's snapshot to
// disagree with a live /proc read.
//
// The seam exists because the two identity checks in KillTree cannot be told apart
// without it. The gate reads /proc for root, the check below it reads root's row out
// of this map, and on a healthy box both come from the same file microseconds apart
// and therefore always agree — so no fixture can make the second one fire, and the
// three lines that close the reuse window would be carried by argument alone. Only
// KillTree is routed through it, deliberately: Descendants is pure and its callers
// are not asserting an identity, so widening the seam would add a mode without
// adding a question anyone asks.
//
// It also buys the one case no CI leg here can reach. idMap's last fallback fills
// ppid and leaves every start at zero, which is what darwin does when the bulk
// sysctl fails; substituting such a map is how a Linux test can pin that KillTree
// still kills there rather than refusing every tree.
var readIDMap = idMap

func parentMap() map[int]int {
	if runtime.GOOS == "linux" {
		return procParentMap()
	}
	return psParentMap()
}

// idMap is parentMap plus the session and group ids, for unionDescendants. On
// Linux all three come from the same /proc/<pid>/stat read the parent snapshot
// already does, so the session set costs no extra syscall.
//
// Off Linux a platform may publish the whole snapshot itself, and darwin does —
// see platformIDs, which reads every id from one sysctl and so gets the session
// set that this path used to leave switched off. An earlier version of this comment
// said `ps -o sess=` "exists on the BSDs but is untested here" and left the set off
// for that reason; it has now been tested, and it is not merely untrusted but
// unusable, along with the other obvious source. platform_darwin.go has the
// numbers and why either would have SIGKILLed most of the machine.
//
// The `ps` fallback below is what is left for a unix with neither /proc nor an
// implementation here: parent links only, so unionDescendants degrades to the
// parent closure. sid stays zero and is not guessed at — no live process has pid 0,
// so a zero sid matches no root — and pgid stays unset because the oracle does not
// use it (see procIDs) and only the Linux fixtures assert premises with it.
func idMap() map[int]procIDs {
	if runtime.GOOS == "linux" {
		return procIDMap()
	}
	if m, ok := platformIDs(); ok {
		return m
	}
	m := make(map[int]procIDs)
	for pid, ppid := range psParentMap() {
		m[pid] = procIDs{ppid: ppid}
	}
	return m
}

// fastDescendants walks Linux's per-task children lists down from root, in
// breadth-first order, and reports false when it cannot: only /proc carries
// the links, and only with CONFIG_PROC_CHILDREN. Cost is O(descendants),
// against O(processes on the machine) for a parent snapshot.
//
// NOT ON ANY PRODUCTION PATH, and not safe to put on one. Descendants does not
// call it; it is kept because the measurement below is the reason, and because
// the differential tests hold it against the snapshot. The 12 ms it was meant to
// save is paid once per window close, where it is invisible, so there is nothing
// on the other side of the trade.
//
// The kernel documents these lists as
// best-effort — "doesn't guarantee to provide precise results and some children
// might be skipped" — and the skip is not rare. Measured against a cgroup
// oracle: 18 skips in 174,589 observations of a 3-child parent, 68 in 465,283
// at 8 children, 211 in 3,803,024 at 64 (replicated: 22, 49, 222). The
// snapshot path skipped 0 in 127,845. One skip is not one lost pid: the missed
// child's whole subtree goes with it, 15 live pids over 40,000 rounds, worst
// single round 5.
//
// The mechanism, from fs/proc/array.c get_children_pid: seq_file iteration
// drops tasklist_lock between entries, and the fast continuation is valid only
// while the previously emitted child still satisfies pid_task(pid_prev) &&
// task->real_parent == start && !list_empty(&task->sibling). A reap
// (release_task -> __unhash_process) breaks all three; a reparent breaks the
// second. The kernel then falls back to a POSITIONAL search over a list that
// has shrunk, and skips the live child after the removed one. So the trigger is
// a reap or a reparent, not a fork (list_add_tail appends below any cursor) and
// not a bare exit (a zombie stays on the sibling list).
//
// This race is the walk's alone. proc_pid_readdir resumes via find_ge_pid,
// which is pid-keyed rather than positional, and readPPID is an independent
// per-pid read, so snapshotDescendants has no cursor to invalidate and is
// structurally immune. An earlier version of this comment claimed the snapshot
// "runs the same race over a far wider window"; that is wrong, and the 12 ms
// against 0.7 ms it cited are whole-call times, not race windows. The windows
// that decide a single node are 173-187 us for the walk against 5.3-7.2 ms for
// the snapshot — the walk is 30-40x narrower, and it still loses, because the
// snapshot's window is not exposed to this failure at all.
func fastDescendants(root int) ([]int, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	kids, ok := childrenOf(root)
	if !ok {
		// Not one of root's threads had a children list: either the kernel
		// lacks the feature or root has already gone. The snapshot answers
		// both correctly, so let it.
		return nil, false
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; ; {
		for _, kid := range kids {
			if !seen[kid] {
				seen[kid] = true
				out = append(out, kid)
			}
		}
		if i++; i >= len(out) {
			return out, true
		}
		// A descendant that exits before we read it has no list of its own. Its
		// pid is still returned, and killing it is harmless. Its children are
		// not: they reparent to the nearest live ancestor thread, or to the
		// subreaper, and this walk has already passed the level they land on, so
		// it never sees them. The snapshot does not reach the same outcome — it
		// re-reads every ppid and follows the new link. This is the second half
		// of the gap in the header comment, and it is why the missing subtree is
		// the failure to fix, not the missing pid.
		kids, _ = childrenOf(out[i])
	}
}

// childrenOf returns pid's first-level children. Children are listed per
// thread, and a threaded parent can have a child attributed to any of its
// threads, so every task is read. The bool is false when no task's list could
// be read at all, which does not distinguish an unsupported kernel from a
// process that has already exited; callers treat both as "use the snapshot".
// procRoot is "/proc" outside tests. A test overrides it to point childrenOf at
// a fabricated tree, which is the only way to reach the states real /proc never
// produces: a task directory whose children file cannot be read, and a children
// file holding a value that is not a usable pid.
var procRoot = "/proc"

func childrenOf(pid int) ([]int, bool) {
	dir := procRoot + "/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var out []int
	read := false
	for _, t := range tasks {
		b, err := os.ReadFile(dir + "/" + t.Name() + "/children")
		if err != nil {
			continue
		}
		read = true
		for _, f := range strings.Fields(string(b)) {
			// c > 0 is not defensive padding: these pids go straight to
			// syscall.Kill, where 0 means "every process in our own process
			// group" — the daemon and its siblings — and -1 means "every
			// process we may signal". A kernel children file is always
			// well-formed positive decimal, so this is unreachable today; the
			// check is here because the failure mode if it ever is reachable is
			// unbounded.
			if c, err := strconv.Atoi(f); err == nil && c > 0 {
				out = append(out, c)
			}
		}
	}
	return out, read
}

// procIDMap reads /proc/<pid>/stat for every process.
//
// One buffer for the whole walk: this runs once per window kill over every process on
// the machine, so a per-entry allocation here is ~1000 of them on a path whose cost is
// already the largest single latency in a window close. See readIDsBuf for what the
// reuse is worth and for the equivalence check against the allocating reader.
func procIDMap() map[int]procIDs {
	m := make(map[int]procIDs)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return m
	}
	buf := make([]byte, statBufSize)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if id, ok := readIDsBuf(pid, buf); ok {
			m[pid] = id
		}
	}
	return m
}

// procParentMap is procIDMap reduced to the parent links, for the parent-closure
// path the differential tests compare against.
func procParentMap() map[int]int {
	ids := procIDMap()
	m := make(map[int]int, len(ids))
	for pid, id := range ids {
		m[pid] = id.ppid
	}
	return m
}

// statBufSize is one page, and the guard in readIDsBuf is what makes that a bound
// rather than an assumption.
//
// Measured over all 1082 readable processes on this box: 52 fields in every one of
// them, line length mean 247 and max 368 bytes, and field 22 ending within 141 bytes
// of the start. The margin is therefore ~11x on the observed worst case, and the
// arithmetic worst case is not much tighter: 51 numeric fields of at most 21 bytes
// (a 20-digit u64 or a signed long) plus a comm of at most 64, with separators, is
// ~1200 bytes, so a page is still ~3x.
//
// Neither of the two things that make those numbers big is the thing you would
// guess, which is why they are written down rather than reasoned about. The widest
// numeric field is not an address — on this 47-bit-VA box addresses reach 15 digits
// — it is rsslim, field 25, at the full 20 digits of 18446744073709551615 on pid 1.
// And comm is NOT capped at the 15 usable characters of TASK_COMM_LEN: for kernel
// workqueue workers the kernel prints the worker's description rather than
// task->comm, into a 64-byte buffer, so this box has comms of 37 bytes such as
// kworker/R-amdgpu_dm_hpd_rx_offload_wq. An earlier version of this comment asserted
// both of those the wrong way round; the bound was sound anyway, but only by luck,
// and a bound whose stated reason is false is one a later reader may tighten.
const statBufSize = 4096

// readIDs reads /proc/<pid>/stat and hands it to parseIDs. It supplies the buffer
// readIDsBuf wants, because its callers read a single pid and have nothing to reuse.
//
// "Supplies" and not "allocates": the make below does not reach the heap. The compiler
// reports `make([]byte, 4096) does not escape` for this line and for the inlined copy
// in StartTicks, because readIDsBuf only reads into the buffer and parses out of it, so
// the page is a stack frame. That is what makes a per-pid caller fine, and it is worth
// stating because the obvious reading of a 4096-byte make inside a loop is the wrong
// one: a benchmark against pid 1 reports the same 1120 B and 3 allocs/op whether the
// buffer is made here or hoisted and passed in (5440 vs 5391 ns/op), so threading one
// through KillTree's loop would buy nothing. Re-derive with go build -gcflags=-m
// before acting on a contrary intuition; what would invalidate it is readIDsBuf or
// parseIDs starting to retain any part of the buffer.
//
// How often it runs is therefore not a performance question, but the old count here
// was wrong and a later reader may rely on it: StartTicks runs once per window create,
// once at KillTree's gate, and then once more per pid KillTree enumerated — root plus
// every descendant whose snapshot row carries a start time. So a window close is
// O(tree) of these, three syscalls each, not one.
//
// It delegates rather than keeping an os.ReadFile of its own so that there is ONE read
// path in this file. Leaving this on os.ReadFile and giving procIDMap a separate
// reader would mean the dozen-plus tests that go through readIDs no longer exercise
// the path the whole-box walk actually takes.
func readIDs(pid int) (procIDs, bool) {
	return readIDsBuf(pid, make([]byte, statBufSize))
}

// readIDsBuf is readIDs against a caller-owned buffer, for procIDMap's loop over every
// process on the machine.
//
// open+read+close rather than os.ReadFile because os.ReadFile is FIVE syscalls per
// entry plus THREE allocations — openat, fstat, read, a second read to see EOF, close
// — and across ~1000 processes that per-entry cost is most of the walk.
//
// Three and not one, and the third is the one worth writing down because it is there
// only on procfs: the *os.File, the initial 512-byte buffer, and a slices.Grow before
// the final read. procfs reports st_size=0, so os.ReadFile has no size to size the
// buffer from and starts at 512; by the time it goes back for the EOF read it has 212
// bytes of capacity left against its own 512-byte minimum read, so it grows the buffer
// 512 -> 896 to make a read it already knows will return nothing. The same file on a
// regular filesystem costs two allocations and one read.
//
// Measured over 12 reps at 902-923 processes, in two windows of six, all three arms built
// from this commit, the order rotating by rep so that whichever went first cannot be the
// result, and each rep's ratio taken INSIDE the rep before any median:
//
//	                          median    per-rep ratio against this arm
//	os.ReadFile              11.3 ms    1.52x  (1.40-1.63)   procIDMap as it was
//	os.Open + reused buffer   9.3 ms    1.29x  (1.17-1.32)   never in the tree
//	open/read/close, reused   7.3 ms    --                   <- this
//
// Compare the ratio column and not the millisecond column. They disagree: 11.3/7.3 is
// 1.55 and the paired figure is 1.52, because dividing two medians is not the median of
// the divisions. The paired one is the number, and the ms column is here for the LEVEL,
// which is the reason any of this matters — an 11 ms walk against a 33 ms budget for a
// whole window close. The ranges are here because a single quoted ratio reads as far more
// settled than 12 reps support: the two windows gave 1.55 and 1.50 for the same arms,
// back to back on the same box minutes apart, which is the width of the thing being
// measured rather than a difference between two occasions.
//
// So dropping the fstat, the EOF read and the per-entry allocations is worth 34% of the
// walk (28-39% across reps). Routing it through os.Open instead of raw hands back about
// HALF of that saving — 51% paired, 40-64% across reps, and 51% then 53% in the two
// windows independently — to an *os.File allocation and its finalizer. An earlier
// revision of this comment said a quarter, from arithmetic over three numbers that had
// never been measured in one window; the middle arm exists to make that row a measurement
// rather than an inference, which is also why it is worth the paragraph. That is why this
// is the only FILE READ in
// the package that does not go through os. Not the only raw syscall — killProc above
// is syscall.Kill and is the package's whole purpose, and platform_darwin.go reaches
// for x/sys/unix three times. Every other file access here does go through os or exec:
// the task-children walk, procIDMap's ReadDir, and the ps fork.
//
// Equivalence was measured before the swap rather than argued, against the os.ReadFile
// reader this commit deletes: over 200 rounds the two readers agreed on all 195,785
// pids present in both, zero field disagreements, the only differences being processes
// that started or exited between the two walks.
//
// That figure is a record and not something a reader here can re-run, which is worth
// saying plainly rather than letting the number imply otherwise. The harness is not in
// the tree, and the other half of it was the allocating reader that readIDs above no
// longer contains, so reproducing this means re-adding deleted code. Re-deriving it is
// the job of whoever next changes parseIDs or this function, and the way to do it is to
// run both readers over /proc in the same loop and diff the structs, not to trust this
// paragraph.
//
// A read that fills the buffer returns false instead of parsing a possibly truncated
// line. statBufSize makes that unreachable; the point is that the failure mode is a
// refusal and not a plausible wrong number. A truncated field parses as a smaller
// integer, and KillTree compares start times for equality, so a wrong start time reads
// as "this pid was recycled".
//
// Where that leads is worth stating exactly rather than as "it declines to kill", which
// is true of this package and not of the system it sits in, because the caller has a
// fallback. Refusing on root makes KillTree return no pids AND supported, and
// reapChildTree then falls back to pty.PTY.Kill: the shell still dies, through its
// os.Process handle rather than by pid, so no recycled pid can be hit — but a
// descendant that setsid'd away outlives it. Refusing on any other pid drops that pid
// from procIDMap's map silently, losing it and the whole subtree hanging off it, while
// the rest of the answer is killed as usual.
//
// Nothing rescues a dropped pid, and which mechanism fails to is worth knowing. The
// union's second term sweeps the map for members of root's session, so a pid absent
// from the map is absent from BOTH terms at once: not in the parent closure, and
// invisible to the session sweep that would otherwise have caught it after a reparent.
// For root itself it is worse than that — unionDescendants reads ids[root] to learn
// root's own sid, and on a miss it returns the bare parent closure, skipping the session
// term entirely and so losing precisely the reparented orphans that term was added to
// reach. Where root leads no session the term is skipped anyway, which is the standing
// state on any unix that is neither Linux nor darwin, and there one dropped mid-tree pid
// costs its whole subtree.
//
// So the cost of a refusal is always a MISSED kill and never a wrong one, and that is
// the half worth having. A window with a cgroup loses nothing either way, because that
// arm kills by container membership and never consults this map.
//
// The one behavioural difference from the reader this replaces is worth naming, because
// it is an assumption and not a measurement: os.ReadFile loops until EOF, and this does
// ONE read. A short read would therefore be parsed rather than retried, and where the
// cut lands decides what happens — three regimes, all of them safe, which is narrower
// and better than "a truncated field parses as a smaller integer" suggests on its own.
// Cut before field 22 is complete and strings.Fields yields fewer than 20, which
// parseIDs refuses outright. Cut inside field 22's own digits and the start time does
// parse as a smaller integer, failing KillTree's equality check and reading as a
// recycled pid, so it declines. Cut anywhere after field 22 and all four values are
// intact, so the answer is simply correct. Only the middle regime is the smaller-integer
// case, and it is the narrowest of the three.
//
// That third regime is correct because of parseIDs and not because of this read, and the
// difference matters to whoever edits next. The highest index parseIDs touches is
// fields[19], so everything past field 22 is ignored and a line cut there is
// indistinguishable from a whole one — n < len(buf) either way, so the guard below cannot
// fire, and there is nothing else to notice with. Add a fifth value read from a field
// beyond 22 and regime 3 stops being safe and starts being a wrong answer with no
// refusal: the tie is to the field-22 ceiling, so widening the ceiling means the single
// read has to become a loop.
//
// It is also not a shape procfs produces: /proc/<pid>/stat is a seq_file record generated
// in one shot, so a buffer at or above the record size takes all of it. The code does not
// rely on that, which is the point of saying it out loud rather than adding a loop.
//
// O_CLOEXEC is not optional and is the one thing os.ReadFile was doing for us that is
// easy to drop on the way past: os.Open sets it on every fd Go opens, and the daemon
// starts pty shells on other goroutines while this walk runs, so without it a fork in
// that window carries a /proc fd into a window's shell. Go's ForkExec does not close
// fds it was not told about. The alternative to the flag is holding syscall.ForkLock
// across the open, which would serialize the walk against every spawn.
func readIDsBuf(pid int, buf []byte) (procIDs, bool) {
	fd, err := syscall.Open("/proc/"+strconv.Itoa(pid)+"/stat", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return procIDs{}, false
	}
	n, err := syscall.Read(fd, buf)
	_ = syscall.Close(fd)
	if err != nil || n <= 0 || n == len(buf) {
		return procIDs{}, false
	}
	return parseIDs(buf[:n])
}

// parseIDs pulls ppid, pgid, sid and start time out of a /proc/<pid>/stat line —
// fields 4, 5, 6 and 22, which are the second, third, fourth and twentieth after
// the comm field, so fields[1], fields[2], fields[3] and fields[19] below. The
// ordinals and the indexes are both written out because they differ by one and the
// old text said "nineteenth" for field 22: that number matches the correct index,
// so checking fields[19] against the prose confirmed it — and would have confirmed
// fields[19] just as readily if starttime were field 21.
//
// comm may contain spaces AND parentheses, so fields are read after
// the LAST ')': a process can be named ") 1 2 3 (" and a shell can put it there.
//
// Split out from readIDs so that shape can be tested. It is the one branch here
// with a plausible wrong answer rather than an error — bytes.IndexByte in place of
// LastIndexByte parses the comm's own contents as ids and returns ok — and there is
// no way to feed a fabricated stat line through /proc.
//
// All of them or none: a stat line short of the start-time field is a shape this
// kernel does not produce, and half an answer here would be a sid or a start time
// of 0 taken as real. Neither can match anything today — no live process has pid 0,
// and KillTree compares start times for equality — but the reason it is safe should
// be that the value was never invented, not that the comparison happens to fail.
func parseIDs(stat []byte) (procIDs, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return procIDs{}, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	// 20, not 4: index 19 is field 22, the start time. proc(5) gives 52 fields for
	// every process on every kernel that has this file, so a line this short is
	// unparseable rather than old.
	if len(fields) < 20 {
		return procIDs{}, false
	}
	ppid, err1 := strconv.Atoi(fields[1])
	pgid, err2 := strconv.Atoi(fields[2])
	sid, err3 := strconv.Atoi(fields[3])
	start, err4 := strconv.ParseUint(fields[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return procIDs{}, false
	}
	return procIDs{ppid: ppid, pgid: pgid, sid: sid, start: start}, true
}

// StartTicks is a process's start time, or false if it cannot be read. Callers
// record it while the process is still their unreaped child — a pid cannot be
// recycled before its parent reaps it, so a value read at any point before the
// reap identifies the process for as long as the caller holds the zombie — and
// pass it back to KillTree.
//
// The value is OPAQUE and its unit is the platform's: clock ticks since boot on
// Linux (stat field 22), microseconds since the epoch on darwin (kinfo_proc). The
// two are allowed to disagree, but not for the reason this paragraph used to give.
// It said nobody reads the value, that KillTree's equality test is its only
// consumer, and that it is not serialized. All three were false as written:
// internal/cgroup puts this daemon's own start time into a directory NAME
// (cgroup_linux.go, baseName, "shelld-<pid>-<start>") and its GC compares a live
// StartTicks against a value parsed back out of a name a PREVIOUS daemon wrote —
// a second consumer, a serialized form, and a comparison across restarts.
//
// What licenses the platform difference is narrower, and it is a build tag rather
// than a property of the value: the publishing consumer is cgroup_linux.go, so only
// Linux ticks ever reach a filename, and ticks stay comparable there for exactly as
// long as the names do (cgroup_linux.go carries that premise, being the site that
// depends on it). The bound is firmer than a convention, too: internal/cgroup is an
// internal package, so no code outside this module can become a second publisher.
//
// The old paragraph's closing sentence — "treat a change that publishes it as a
// change of contract" — was doing its job and is kept in spirit here. What failed
// was the description of the present tense around it: it said nothing published the
// value while something already did, so a reader had a live stop sign guarding a
// gate that was already open. The unit may differ per platform only while one
// platform publishes it; a darwin analogue reusing baseName is exactly the change
// to weigh, and weighing it means reading cgroup_linux.go, not this paragraph.
//
// False on the unixes that are neither, which disables KillTree there rather than
// weakening it; see platform_other.go.
func StartTicks(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	if runtime.GOOS == "linux" {
		id, ok := readIDs(pid)
		if !ok {
			return 0, false
		}
		return id.start, true
	}
	return platformStart(pid)
}

// StartTimeSupported reports whether this build can read a start time at all.
//
// It exists because (0, false) out of StartTicks has two causes a caller cannot
// tell apart from the value: on Linux and darwin it is a failed read of one pid,
// and on every other unix it is the platform having no implementation, where it is
// false for every pid for the life of the build (platform_other.go). KillTree does
// not need the distinction — it refuses either way, which is the under-reach
// direction on purpose — so this is not a gate on any kill. It is for the spawn
// site, which is the only place that can name the cause (see StartTicks' callers
// in window.go) and must not report a transient failure on a platform where the
// answer was never going to be anything else.
func StartTimeSupported() bool { return runtime.GOOS == "linux" || platformStartSupported }

// psParentMap takes one `ps` snapshot on systems without /proc.
func psParentMap() map[int]int {
	m := make(map[int]int)
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return m
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			m[pid] = ppid
		}
	}
	return m
}
