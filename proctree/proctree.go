// Package proctree kills a process together with all of its descendants.
//
// Killing only a PTY's direct child leaks grandchildren that setsid() into
// their own session — e.g. a launcher that starts `claude`, where the real `claude`
// becomes a session leader, survives a SIGKILL of the shell, and reparents
// to init. KillTree discovers the whole set while the parent links are still
// intact, then SIGKILLs it, so a later reparent cannot hide a process.
//
// Parent links alone are not enough, and the gap needs no race to reach. When a
// process exits, forget_original_parent splices its children onto
// find_new_reaper's answer — an alive thread of its own group, else the nearest
// ancestor with PR_SET_CHILD_SUBREAPER, else init. Nothing here sets that flag,
// so a launcher that exits between the window's shell and the process that
// matters sends that process to `systemd --user`, and it is then a descendant of
// root by no relation /proc can express. Measured on this box: root forks an
// intermediate, the intermediate forks `sleep 300` and exits, root reaps it —
// the sleep is alive, its ppid is systemd, and root's children file is empty.
//
// So the oracle is the closure union root's session, rather than the closure
// alone; see unionDescendants, which also records why there is no process-group
// term.
//
// Linux and darwin both get the whole oracle, from different sources: /proc for
// one, `ps` plus getsid(2) for the other (see idMap, platform_darwin.go). Windows
// gets none of it and says so — KillTree reports false there. Any other unix gets
// the parent closure and a KillTree that refuses, which is under-reach rather than
// a wider signal; platform_other.go says why that is where it stops.
package proctree

import "slices"

// procIDs are the ids a kill oracle reads about a process. The oracle itself
// uses ppid and sid; pgid is parsed because it is the adjacent field in the same
// line and the fixtures assert their premises with it (a process group is what
// separates a job-control orphan from a plain one). On Linux all of them come from
// one /proc/<pid>/stat read; on darwin ppid comes from `ps` and sid from getsid(2),
// and pgid is left unset because nothing reads it there; on any other unix only
// ppid is available and sid is zero, which disables the session set — no live
// process has pid 0. See idMap.
//
// start is the process's start time, in a unit the platform picks and nothing
// prints (see StartTicks). It is not part of the oracle's answer; it is what makes
// a pid mean one process rather than an integer. A pid on its own is reusable —
// this box wraps at /proc/sys/kernel/pid_max, about 11 days of steady forking — so
// any decision made about a pid recorded earlier has to be re-checked against the
// start time before a signal is sent. See KillTree.
type procIDs struct {
	ppid  int
	pgid  int
	sid   int
	start uint64
}

// Descendants returns root followed by every process that belongs to root's
// tree, root first. A root that is not a positive pid has no tree and gets an
// empty answer; see unionDescendants for why that is a guard and not a formality.
func Descendants(root int) []int {
	return unionDescendants(root, idMap())
}

// unionDescendants is the oracle: the transitive closure of parent links from
// root, union the members of root's session when root is the session leader.
//
// Why a union at all, rather than the parent closure the package started with:
// the closure loses an orphaned subtree outright (see the package comment), and
// the id that survives the reparent is already in the file the closure parses.
// Measured on the F3 fixture: the orphaned sleep's ppid becomes systemd's while
// its sid remains root's pid. Field 6 of /proc/<pid>/stat, no prctl, no shim,
// and read at kill time rather than promised at spawn time.
//
// The session set is used ONLY when root leads it. That is the safety premise,
// not a micro-optimisation: `same session as root` sweeps in every process of
// that session, so against a root that is not the session leader it would name
// the daemon and every other window. Window shells are started with
// SysProcAttr{Setsid: true} (pty.Start), which makes root a session leader, so
// the test is `sid == root` — the leader's own pid — and cannot match outside
// the session root itself created.
//
// The session set adds no false positives, and this is the whole of the safety
// argument: a process cannot join an existing session, because setsid always
// creates a new one. Session membership therefore only ever arrives by fork from
// a member, so every pid the set adds is something root forked, transitively,
// whatever its parent link says now.
//
// That argument quantifies over processes; this function holds an integer, and
// closing the gap is a precondition on the caller rather than a property of the
// code. A session id IS a pid — the leader's — and pid_max wraps, so `sid == root`
// can be satisfied by a session that some unrelated process created after root
// died and its number was handed out again. Nothing the kernel will tell us
// distinguishes that from the real thing, so Descendants does not try: it answers
// truthfully about whoever holds the number it was given. Pinning identity is
// KillTree's job, which is why KillTree takes root's start time and refuses a root
// whose start time no longer matches. A caller that reads a pid, lets it be
// reaped, and only then asks for its descendants is asking a different question
// than it thinks it is.
//
// There is no process-GROUP term, and the reason is worth keeping. An earlier
// version had one, with the same closed-under-fork argument attached, and the
// argument is false for groups: setpgid(child, sibling) is legal for a parent
// onto its own child whenever the target group is in the same session, which is
// how a shell builds every pipeline — the group's leader is one member and the
// rest are its siblings, forked by the shell. Against such a leader a group test
// names processes it never forked, and KillTree SIGKILLs them
// (TestDescendantsRefusesASiblingInRootsProcessGroup). The term was unsafe
// exactly where it was load-bearing: when root leads its session, every member
// of root's group is in root's session as well, since setpgid cannot cross a
// session — so the group could only ever add a pid the session had not already
// added when root did NOT lead its session, which is the one configuration in
// which the pid it adds may be a stranger's.
//
// The consequence is stated rather than papered over: against a root that leads
// no session, this is the parent closure and nothing more, so an F3 orphan of
// such a root is out of reach for every /proc oracle. That is a case for the
// cgroup membership set, not for a wider id test.
//
// The other residual, unchanged: a descendant that calls setsid leaves root's
// session, so the set does not hold it, and if it is ALSO orphaned the parent
// closure does not either. Also a cgroup case — cgroup membership is inherited
// on fork and unaffected by both setsid and reparenting.
//
// Sibling order is sorted rather than left to map iteration, so a caller (and a
// test) sees the same list twice for the same tree.
//
// A non-positive root is refused here rather than only at the kill, because the
// oracle's answer for one is not a small set — it is unbounded in both directions.
// root == 0: pid 1's ppid is 0, so children[0] holds init and the closure is every
// process on the machine (measured: 869 pids on an 868-process box). root < 0: no
// pid is negative, so the closure is [root] itself and the oracle bounds nothing —
// the damage is then done by syscall.Kill reading -N as "process group N". Neither
// is a tree, so neither gets an answer. KillTree repeats the check; two guards for
// one hazard is deliberate, since Descendants is exported and its caller need not
// be KillTree.
func unionDescendants(root int, ids map[int]procIDs) []int {
	if root <= 0 {
		return []int{}
	}
	children := make(map[int][]int, len(ids))
	for pid, id := range ids {
		children[id.ppid] = append(children[id.ppid], pid)
	}
	for _, kids := range children {
		slices.Sort(kids)
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	// expand walks parent links breadth-first from out[from:], appending as it
	// goes. It is called again after the session and group members are spliced
	// on, because their own children may have left the session (a setsid'd child
	// of an orphan is in neither set) and are reachable only from them.
	expand := func(from int) {
		for i := from; i < len(out); i++ {
			for _, kid := range children[out[i]] {
				if !seen[kid] {
					seen[kid] = true
					out = append(out, kid)
				}
			}
		}
	}
	expand(0)
	self, ok := ids[root]
	if !ok {
		// No stat for root: it has exited, or this platform has no ids. Either
		// way the closure above is all there is.
		return out
	}
	if self.sid != root {
		// root leads no session, so no id here separates root's own descendants
		// from the daemon's. The closure is the whole answer.
		return out
	}
	var extra []int
	for pid, id := range ids {
		if seen[pid] {
			continue
		}
		if id.sid == root {
			extra = append(extra, pid)
		}
	}
	slices.Sort(extra)
	first := len(out)
	for _, pid := range extra {
		seen[pid] = true
		out = append(out, pid)
	}
	expand(first)
	return out
}

// snapshotDescendants is the parent-link closure on its own: one snapshot of
// every process's ppid, then breadth-first from root. Not what Descendants
// answers any more — it is the term of the union that loses an orphaned subtree
// — and kept because the differential tests compare the walk against it. It carries
// unionDescendants' non-positive guard so the differential comparison stays a
// comparison: without it the two terms disagree on root == 0 by the whole machine.
func snapshotDescendants(root int) []int {
	if root <= 0 {
		return []int{}
	}
	children := make(map[int][]int)
	for pid, ppid := range parentMap() {
		children[ppid] = append(children[ppid], pid)
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, kid := range children[out[i]] {
			if !seen[kid] {
				seen[kid] = true
				out = append(out, kid)
			}
		}
	}
	return out
}
