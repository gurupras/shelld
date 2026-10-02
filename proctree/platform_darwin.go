//go:build darwin

package proctree

import "golang.org/x/sys/unix"

// platformStartSupported reports that platformStart below is a real reader rather
// than the stub, which is what StartTimeSupported needs and what a (0, false) from
// StartTicks cannot say for itself.
const platformStartSupported = true

// The two ids this package's oracle needs that darwin does not keep in a file.
// Both were dead here before: StartTicks read /proc/<pid>/stat, which darwin has
// no equivalent of, so it answered false for every pid — and since KillTree
// refuses a root whose start time it cannot match, KillTree signalled NOTHING on
// darwin while reporting the empty set and true. Measured before the fix, on
// macOS 26.4 arm64: StartTicks(self) and StartTicks(a live child) both false,
// while Descendants worked and returned all three of [self, sh, sleep]. So it was
// only the identity guard that was missing, not the enumeration; reapChildTree
// fell through to p.Kill, which is os.Process.Kill — SIGKILL to the one pid, not
// the group — and a grandchild that had setsid'd away survived, which is the exact
// case the package exists for.

// platformStart is StartTicks' darwin half: the process start time out of
// kinfo_proc, as microseconds since the epoch rather than ticks since boot.
//
// The unit differs from Linux's, and what makes that safe is a build tag rather
// than a property of the value: the one thing in this module that puts a start time
// somewhere durable is internal/cgroup, which makes it a component of a cgroup
// directory name and compares it against a live read across restarts — and that file
// is //go:build linux. Being an internal package, it is also the only candidate the
// module can have. So the bound is "no publisher is compiled into a darwin build",
// which this file can check, and not "no darwin code publishes it", which it cannot.
//
// That distinction is why the clauses that stood here were replaced. They said
// "StartTicks is the only producer, KillTree's equality test the only consumer" and
// "never serialized" — true of what this file can see, but stated as facts about
// StartTicks, which is a cross-platform function: a platform file ruling on a
// tree-wide invariant it only has half of in view. See StartTicks itself, which
// carries the real bound.
//
// What the value has to be here is stable for one process and different for two,
// which is what was measured: pid 1 at
// 1787512457.244338 against three processes started within one second of each
// other at .144672, .910499 and .237540, all distinct at microsecond resolution.
//
// Note this is wall-clock, so unlike Linux's boot-relative ticks it is not
// invalidated by a reboot. The old text called that irrelevant "because nothing
// survives the daemon that holds it", which is correct for the darwin value and
// stays correct — it is only the Linux value that outlives its daemon, in a cgroup
// directory name. What the wall-clock basis does mean either way is that the value
// cannot be mistaken for an uptime.
func platformStart(pid int) (uint64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}
	return foldStart(kp.Proc.P_starttime), true
}

// foldStart is the one place the timeval becomes the scalar KillTree compares, so
// that platformStart and platformIDs cannot fold it two different ways — they are
// read at different moments and compared to each other, so a disagreement between
// them would read as a recycled pid and refuse a live root.
func foldStart(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1e6 + uint64(tv.Usec)
}

// platformIDs is the whole id snapshot, from one sysctl plus a getsid per process.
//
// It replaces the `ps -axo pid=,ppid=` fork that the non-Linux path used, and the
// reason is measured rather than aesthetic. On a 952-process box:
//
//	ps fork (psParentMap)      13.4-18.7 ms
//	kern.proc.all, one sysctl       0.14 ms
//	getsid loop, all 952 pids       0.14 ms
//
// So the fork was ~99% of the cost of asking. Measured end to end across the
// change, same box: idMap 15.7 ms -> 0.26 ms, and Descendants — the call the
// window-close path actually makes — 14.5 ms -> 0.39 ms, about 37x.
//
// It was not a visible stall before: Descendants runs once per window close and 15
// ms is inside the 33 ms bar. What the change buys is that the cost stops being the
// same order of magnitude as the whole budget, and that a KILL path no longer forks
// and execs. That second part is worth more than the milliseconds, because
// exec.Command("ps") resolves through PATH — the set of pids to SIGKILL was being
// decided by whichever `ps` the daemon's environment happened to point at.
//
// The session id is getsid(2) and NOT kinfo_proc's Eproc.Sess, which is a
// correctness matter rather than a style one. Eproc.Sess is a kernel POINTER to
// the session struct, not an id, and it does not survive the copy out to
// userspace: measured over every process on the machine, 935 of them, it held
// exactly ONE distinct value and 934 of those were 0x0. Building the session set
// from it — "same Sess as root" — would therefore have matched nearly every
// process on the box, and since that set goes straight to SIGKILL the result would
// have been a kill of the whole uid. That is also why `ps -o sess=` is useless
// here and was right to be distrusted: ps prints the same unpopulated field, 0 for
// all 999 lines, including login shells that certainly lead their own sessions.
//
// getsid(2) answers properly: 580 distinct session ids over those same 935
// processes, 1 for launchd and the leader's own pid for a test's session. A pid
// whose sid cannot be read keeps a zero sid rather than a guessed one, which drops
// it from the session term instead of widening it — no live process has pid 0, so a
// zero sid matches no root.
//
// False on an error rather than a half-filled map, so that idMap falls back to the
// ps path instead of running the oracle over a snapshot missing arbitrary
// processes: a parent absent from the map ends the closure early, and that is a
// tree left alive, not a tree over-killed.
func platformIDs() (map[int]procIDs, bool) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil || len(all) == 0 {
		return nil, false
	}
	m := make(map[int]procIDs, len(all))
	for i := range all {
		pid := int(all[i].Proc.P_pid)
		if pid <= 0 {
			continue
		}
		id := procIDs{
			ppid:  int(all[i].Eproc.Ppid),
			pgid:  int(all[i].Eproc.Pgid),
			start: foldStart(all[i].Proc.P_starttime),
		}
		// Not from this snapshot, for the reason above.
		if sid, err := unix.Getsid(pid); err == nil && sid > 0 {
			id.sid = sid
		}
		m[pid] = id
	}
	return m, true
}
