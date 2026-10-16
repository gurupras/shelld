package shelld

import (
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurupras/shelld/internal/cgroup"
)

// The cgroup arm of window teardown, wired at the process level rather than
// through WindowOpts.
//
// It is process-level because a cgroup base belongs to a daemon, not to a
// window or a session: there is one base per running shelld, named for its pid
// and start time, and every window's cgroup is a child of it. Threading a
// manager through every NewWindow caller would ask each of them to answer a
// question none of them has an opinion about.
//
// It is OPT-IN, and that is what keeps this change invisible to anything that
// embeds this package: the manager is nil until InitCgroups is called, a nil
// manager hands out nil cgroups, and a window with a nil cgroup behaves exactly
// as it did before — the /proc union is the whole of its teardown. Only the
// daemon binary calls InitCgroups.
//
// What the arm buys is the one process the union cannot reach: a process that
// has BOTH setsid'd away and been orphaned is in neither the parent closure nor
// the shell's session, so no walk of /proc finds it from the window's pid. It is
// still a member of the window's cgroup, because membership is inherited at fork
// and survives setsid, exit of the parent, and reparenting to pid 1.
//
// The two arms are CO-PRIMARY rather than one being a fallback for the other.
// The union's anchor is a pid, which names a process only while that process is
// unreaped; the cgroup's anchor is a directory, which nothing can invalidate.
// But a process can leave a cgroup — one write to an ancestor's cgroup.procs
// moves it, and moves only it, leaving its children behind — and the escapee is
// then reachable by the union and not by the cgroup. Neither set contains the
// other.

var (
	cgMu      sync.Mutex
	cgManager *cgroup.Manager
	// cgNextID numbers window cgroups. It is a counter and not the window's name
	// because a window name is user input and this becomes a filesystem path;
	// it is process-wide and never reused, so a window's directory cannot
	// collide with one a previous window has not finished tearing down.
	cgNextID atomic.Uint64
)

// InitCgroups probes for a usable cgroup v2 setup and, if there is one, takes a
// base directory for this daemon and collects the bases left by daemons that are
// gone. It reports whether the arm is active.
//
// Every failure here is a DECLINE and not a fatal error: the /proc union works
// on its own, and a host without cgroup v2, or one where the daemon's own cgroup
// is not writable, is a host shelld runs on with one less way to find an escaped
// process. What it must not do is decline silently, so the reason is logged.
func InitCgroups() bool {
	cgMu.Lock()
	defer cgMu.Unlock()
	if cgManager != nil {
		return true
	}
	m, err := cgroup.Probe()
	if err != nil {
		slog.Info("cgroup window containment unavailable; window kill will use /proc only",
			"err", err)
		return false
	}
	cgManager = m
	// GC kills nothing. A stale base whose windows are still running holds
	// either processes a crashed daemon left behind or a job someone
	// deliberately detached, and a directory name cannot tell those apart, so
	// they are counted and left for whichever daemon starts after they exit.
	removed, busy, err := m.GC()
	if err != nil {
		slog.Warn("collecting stale cgroup bases", "removed", removed, "busy", busy, "err", err)
	} else if removed > 0 || busy > 0 {
		slog.Info("collected stale cgroup bases", "removed", removed, "stillRunning", busy)
	}
	slog.Info("cgroup window containment active", "base", m.Base())
	return true
}

// CloseCgroups releases the daemon's base at shutdown. Like GC it kills nothing:
// a window cgroup still populated here holds either a window nobody closed or a
// job that outlived its shell, and shutdown is not the place to decide about
// either. The directory is left for the next daemon's GC and the error says so.
func CloseCgroups() {
	cgMu.Lock()
	defer cgMu.Unlock()
	if cgManager == nil {
		return
	}
	// Windows killed in hangup mode hand their cgroup to a reaper that removes it
	// once it empties; give the ones killed just before shutdown the same bound a
	// default Kill gives its own cgroup. What is still populated after that holds a
	// process the hangup did not end, and is left like any other populated cgroup.
	if left := hupReaper.drain(cgroupDrainTimeout); left > 0 {
		slog.Info("hung-up window cgroups still hold a process at shutdown; left for the next daemon's GC",
			"cgroups", left)
	}
	if err := cgManager.Close(); err != nil {
		slog.Warn("releasing the cgroup base", "err", err)
	}
	cgManager = nil
}

// newWindowCgroup makes the cgroup for one window, or returns nil when there is
// no cgroup arm.
//
// A failure returns nil rather than an error, and the window is spawned with the
// /proc union alone. That is deliberate: refusing to open a window because a
// mkdir under a cgroup directory failed would trade a working feature for a
// stricter one, and the arm this failure loses is the additional one. It is
// logged at warn, because a daemon whose probe succeeded and whose per-window
// mkdir fails is in a state worth seeing.
func newWindowCgroup() *cgroup.Cgroup {
	cgMu.Lock()
	m := cgManager
	cgMu.Unlock()
	if m == nil {
		return nil
	}
	c, err := m.New(cgNextID.Add(1))
	if err != nil {
		slog.Warn("no cgroup for this window; its kill will use /proc only", "err", err)
		return nil
	}
	return c
}

// openCgroup returns the handle pty.Command.PlaceIn wants, or nil for no cgroup.
//
// Unlike the two failures above, a failure HERE is fatal to the spawn, and the
// asymmetry is the point. Not having a cgroup is a state the window can be in
// honestly: w.cg is nil, Kill uses the union alone, and nothing believes
// otherwise. But a cgroup that exists and could not be opened would leave a
// window holding a directory its shell was never placed in — a container the
// teardown would faithfully kill and find empty, reporting success over a tree it
// never contained. Failing the spawn is how the window avoids being wrong about
// itself.
func openCgroup(c *cgroup.Cgroup) (*os.File, error) {
	if c == nil {
		return nil, nil
	}
	f, err := c.Open()
	if err != nil {
		slog.Warn("opening the window cgroup", "dir", c.Dir(), "err", err)
		discardCgroup(c)
		return nil, err
	}
	return f, nil
}

// discardCgroup removes a cgroup nothing was ever started in. It is separate from
// reapCgroup because it does not kill: on this path there has been no spawn, so
// an empty rmdir is the whole job, and a Kill would be a write to cgroup.kill
// with no member to receive it.
func discardCgroup(c *cgroup.Cgroup) {
	if c == nil {
		return
	}
	if err := c.Remove(); err != nil {
		slog.Warn("removing an unused window cgroup", "dir", c.Dir(), "err", err)
	}
}

// reapCgroup kills the window's cgroup, waits for it to empty, and removes it. It
// is called once per window and is safe to call again: the cgroup is taken out of
// the window first, so a second Kill finds nothing to do — except when the kill
// itself failed, which puts it back, because then there is still something to do.
//
// The wait is not optional and is not a courtesy to slow machines. An rmdir 0.1 ms
// after the kill was measured EBUSY on this box while cgroup.events already read
// populated 0 by 0.3 ms: the processes are dead either way, and what the wait is
// for is the directory. Without it every window would leave one behind.
func (w *Window) reapCgroup() {
	w.mu.Lock()
	c := w.cg
	w.cg = nil
	w.mu.Unlock()
	if c == nil {
		return
	}
	// Kill first and unconditionally, even though the union has already run: this
	// is the one call that reaches a process which both setsid'd away and lost its
	// parent, and cgroup.kill is atomic with respect to forks, so a member cannot
	// outrun it the way it can outrun a pid-by-pid sweep.
	//
	// Atomic with respect to FORKS, which is the narrower claim it sounds like and
	// not a statement that a member cannot escape at all. The other way out is
	// MIGRATION: a process writes its own pid to another cgroup.procs it can open,
	// and this call then does not reach it because it is no longer a member. That is
	// not an adversarial setup. Window cgroups are created relative to the daemon's
	// own path (internal/cgroup, ownCgroup) and so sit inside the user's delegated
	// subtree, where the window's shell -- same uid as the daemon -- may write any
	// sibling cgroup.procs.
	//
	// Reproduced rather than reasoned, and by hand rather than by a test, so the
	// recipe is here instead of a citation -- it wants two cgroups and a live process,
	// which is not something to leave a package test doing:
	//
	//	R=/sys/fs/cgroup$(awk -F: '/^0::/{print $3}' /proc/self/cgroup)
	//	mkdir $R/a $R/b; sleep 600 & P=$!
	//	echo $P > $R/a/cgroup.procs      # a holds it
	//	echo $P > $R/b/cgroup.procs      # the escape: same uid, sibling directory
	//	echo 1  > $R/a/cgroup.kill       # a is empty, so this reaches nothing
	//	kill -0 $P                       # still alive
	//
	// The migration is the single write to b, and it needs no privilege the window's
	// shell does not already have. Run on the development box 2026-09-26: after the
	// second write a/cgroup.procs was empty, b held the pid, and the kill left it
	// running.
	//
	// So this call and reapChildTree's /proc union are not one strong arm and one
	// redundant arm; each reaches a case the other misses, which is the argument for
	// "unconditionally" above and the reason the union is not skipped when w.cg is
	// non-nil. A migrated escapee keeps its parent link and its session, which is
	// exactly what the union matches on.
	if err := c.Kill(); err != nil {
		// Put the handle back on the window. Taking it off first is what makes a
		// SECOND Kill find nothing to do, and that is right once the kill has
		// happened — but a kill that failed has not happened, and this is the one
		// arm that reaches a process which both setsid'd away and lost its parent.
		// Dropping it here retired that arm for the window's whole life and made a
		// retried Window.Kill a silent no-op over a tree still running. Safe to
		// write: reapCgroup is only reached from Kill, which holds killMu, and the
		// only other mutator (releaseCgroup) takes killMu too.
		w.mu.Lock()
		w.cg = c
		w.mu.Unlock()
		slog.Warn("killing the window cgroup", "dir", c.Dir(), "err", err)
		return
	}
	if err := c.WaitEmpty(cgroupDrainTimeout); err != nil {
		// The directory is left in place, and the handle is put back — for a
		// different reason than the arm above. The kill HAPPENED here, so a retried
		// Window.Kill has nothing to redo and none of that arm's reasoning carries
		// over. What the handle is still worth is the RMDIR: releaseCgroup runs at
		// the read loop's EOF and its Release is removeIfEmpty, so with the handle
		// in place the directory can still go during this daemon's life, and with it
		// dropped the earliest opportunity is a GC at some later daemon's start.
		// Safe to write for the reason given above.
		//
		// The two ways this arm is reached do not share a prognosis, and the reason
		// not to drop the handle is in the second one. A TIMEOUT means a member
		// survived SIGKILL — on Linux, stuck in an uninterruptible wait — and then
		// the rmdir really is EBUSY until it clears. But WaitEmpty also returns the
		// error from READING cgroup.events, and an unreadable cgroup.events says
		// nothing about whether anything is in there: on that path the members are
		// dead and the rmdir would succeed. Dropping the handle forfeited a removal
		// that works, for a directory that nothing else in this daemon revisits.
		w.mu.Lock()
		w.cg = c
		w.mu.Unlock()
		slog.Warn("window cgroup did not empty after being killed", "dir", c.Dir(), "err", err)
		return
	}
	if err := c.Remove(); err != nil {
		slog.Warn("removing the window cgroup", "dir", c.Dir(), "err", err)
	}
}

// releaseCgroup removes the window's cgroup once its terminal has closed, and
// KILLS NOTHING. It is the teardown for a window that ended on its own, where
// reapCgroup is the teardown for one a caller asked to die, and the difference
// between them is the whole point: a user who closes a window has named the tree
// that dies with it, whereas a shell that exited said nothing about the job it left
// running. Killing here would SIGKILL a deliberately detached process on the
// strength of its shell having finished, which is the decision cgroup.GC and
// cgroup.Manager.Close both refuse for the same reason.
//
// Without this the directory outlived every window that was never explicitly
// killed, one per exit, for the life of the daemon: nothing else removes it, since
// GC skips a base whose daemon is still running — which includes our own — and only
// shutdown swept them up.
//
// A survivor keeps the cgroup ON the window rather than releasing it, so a later
// Kill still has its cgroup arm. That arm is the only one that reaches a process
// which has both setsid'd away and been orphaned, and a process that outlived the
// shell is exactly the kind that has.
//
// That leaves one directory behind on purpose, and it is worth naming rather than
// implying: once the survivor exits, nothing runs this again, so the now-empty
// cgroup stays until the window is explicitly killed or the daemon shuts down.
// This is far narrower than the leak the release closes — it needs a job that
// outlived its shell, not merely a window that exited — and the alternatives are
// worse than an empty directory: polling would put a timer on every exited window,
// and removing it at the moment the last member leaves would mean watching
// cgroup.events for a window nobody is attached to any more.
//
// It takes killMu, which Kill holds for the whole of its body, because the two run
// concurrently on the ordinary Kill path: Kill's tree kill ends the shell, that
// gives the read loop its EOF, and the read loop arrives here while Kill is still
// working. Serialising them is what makes either order correct — whichever runs
// first tears the cgroup down, and the other finds w.cg already nil and does
// nothing. Without it the two interleave on one directory, and a release that won
// the rmdir would leave Kill writing cgroup.kill to a path that no longer exists.
// Kill never waits on the read loop, so this cannot deadlock against it.
func (w *Window) releaseCgroup() {
	w.killMu.Lock()
	defer w.killMu.Unlock()
	w.mu.Lock()
	c := w.cg
	w.mu.Unlock()
	if c == nil {
		return
	}
	left, err := c.Release()
	if err != nil {
		slog.Warn("releasing the window cgroup at EOF", "dir", c.Dir(), "err", err)
		return
	}
	if left > 0 {
		slog.Info("window cgroup outlived its shell and was left running",
			"window", w.Name(), "dir", c.Dir(), "cgroups", left)
		return
	}
	w.mu.Lock()
	w.cg = nil
	w.mu.Unlock()
}

// cgroupDrainTimeout bounds the wait for a killed cgroup to empty. The measured
// figure is under a millisecond; this is three orders of magnitude above it,
// because the only thing that runs out the clock is a process in an
// uninterruptible wait, and the answer to that is to give up and log rather than
// to keep a window's Kill blocked.
const cgroupDrainTimeout = 2 * time.Second
