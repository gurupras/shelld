//go:build !windows

package shelld

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// clearedWhenIgnored are the signals shelld refuses to pass on to a window shell as
// ignored. See dontPassOnIgnoredSignals for why these three, and why the other five that
// leak are deliberately left alone.
var clearedWhenIgnored = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTSTP}

// passOnMu serialises the whole of dontPassOnIgnoredSignals. See the comment there: the
// check and the notification are not separable.
var passOnMu sync.Mutex

// sigSink receives the signals this file asks to be notified about. Nothing reads it, and
// nothing needs to -- being notified is the lever, not the notification.
//
// One channel for the package rather than a fresh one per call, because signal.Notify keeps a
// permanent reference to every channel it is handed: a fresh channel each time adds an entry
// to os/signal's map that nothing ever removes, and delivery walks that map under a lock for
// every signal the process receives. Measured: 15us of delivery latency at one registration
// and 225us at 31,001, with ~182 bytes retained each. A host that re-ignores between spawns
// accumulates them, and that is precisely the host per-spawn re-arming exists for. Sharing
// one channel is deduplicated by os/signal's own want check, so both stay flat, and it
// re-arms identically: want and ref move together, so a host's signal.Reset clears this
// channel's want bit too and the next Notify installs again. Semantics are otherwise
// unchanged -- a full buffer drops, which is already the fate of every signal delivered here.
var sigSink = make(chan os.Signal, 1)

// ignoredMaskSource is kernelIgnoredMask, replaced by tests with one that reports it cannot
// answer.
//
// The seam is here because the fallback that answer selects is the production path on every
// platform except Linux, and no test on Linux can reach it otherwise -- /proc/self/status is
// always readable here. Without it the branch that runs on darwin and the BSDs would be the
// only part of this file nothing ever executes, and it is also the branch where the lock is
// load-bearing.
var ignoredMaskSource = kernelIgnoredMask

// dontPassOnIgnoredSignals keeps an inherited SIG_IGN from reaching the window shell about
// to be spawned, without changing whether those signals can end this process.
//
// execve resets a handled signal to SIG_DFL but *preserves* SIG_IGN, so a signal this
// process inherited as ignored is inherited by every window shell it spawns, through any
// number of fork+exec generations.
//
// Which signals reach that far is decided by whether the runtime's initsig installs a handler
// over the inherited disposition, and it installs one for all but three groups: the signals
// with no flags at all (SIGKILL and SIGSTOP, neither of which can be ignored anyway), the
// _SigDefault signals (SIGCONT, SIGTSTP, SIGTTIN, SIGTTOU), and the _SigSetStack signals (32,
// 33, 34) less 33.
//
// 33 is the exception inside that last group, and the reason it does not leak changes with the
// build while the outcome does not. In a non-cgo binary sigInstallGoHandler answers true for it
// ahead of the _SigSetStack test, because the runtime uses SIGSETXID for its own per-thread
// syscalls when glibc is not there to. That branch is gated on !iscgo, so in a cgo binary it
// does not fire -- but glibc's own SIGSETXID handler is installed before initsig reads the
// disposition, so fwdSig is that handler rather than the inherited SIG_IGN and initsig keeps it
// with setsigstack. Measured both ways, same program built twice: the child's mask was identical,
// 32 and 34 inherited and 33 caught and not. The distinction is worth recording because
// cmd/shelld links runtime/cgo and this package's own test binary need not, so a claim resting on
// !iscgo alone would be true of the test binary and not of the daemon.
//
// Reaching 33 at all takes a raw rt_sigaction launcher. glibc's sigaction refuses both 32 and 33,
// so a `trap ""` launcher silently fails to ignore them -- exit status 0, bit clear in SigIgn --
// and a shell-based measurement says nothing about either one.
//
// SIGHUP and SIGINT are the exception that makes any of this a bug: the runtime installs
// handlers for those two, but respects an inherited SIG_IGN and leaves it in place. Measured
// here by ignoring every ignorable signal in a launcher, exec'ing a Go program, and forking a
// /bin/sh from it, the set that survives both hops is eight:
//
//	SIGHUP(1) SIGINT(2) SIGCONT(18) SIGTSTP(20) SIGTTIN(21) SIGTTOU(22) 32 34
//
// What decides which of the eight to clear is what an ignored one costs the user, and not
// whether it can be detected. Those are different lines and it matters which one is written
// down: signal.Ignored sees SIGHUP, SIGINT, 32 and 34, so a rule phrased as "clear what
// os/signal can see" would pull 32 and 34 in and still miss SIGTSTP, and now that the kernel is
// the predicate, "clear what can be detected" would pull in all eight.
//
// Three of the eight cost the user something, and those are cleared:
//
//	SIGHUP   Not for Window.Kill, which SIGKILLs the shell's whole tree and no disposition
//	         refuses that. It is for the case where nothing gets to run Kill: when the
//	         daemon itself dies, its descriptors close, and hanging the session up is the
//	         only thing left that ends the shell. A shell that ignores SIGHUP survives
//	         that -- pty gone, controlling terminal reading as none, sitting in state S,
//	         outliving the daemon and everything above it. `nohup shelld daemonize`, or any
//	         launcher that ignores SIGHUP, is enough to produce it.
//	SIGINT   The command running in the window cannot be interrupted with Ctrl-C: the ignore
//	         reaches a shell's foreground job, measured against an unarmed control in both
//	         bash and dash. dash also keeps it for its own prompt; bash installs its handler
//	         over it, so Ctrl-C at a bash prompt still works. This one needs no nohup: a
//	         shell starting a background job without job control ignores SIGINT in it, as
//	         POSIX requires, so `shelld ... &` from any /bin/sh script is enough, and was
//	         measured to be.
//	SIGTSTP  Not for a window running a shell, where an inherited ignore is undetectable.
//	         Measured against an unarmed control at three hops: an interactive bash and dash
//	         both set SIGTSTP to SIG_IGN for themselves whether or not one arrives -- the
//	         armed and unarmed legs read an identical bit at the shell -- and both restore
//	         SIG_DFL in every job they fork, so Ctrl-Z suspends the foreground job normally
//	         either way. It is for a command with no shell between it and this process: a
//	         window's command is client-supplied, and a directly exec'd one does hold the
//	         ignore, which is the positive control that makes the rest of this a measurement
//	         rather than an apparatus failure. Being hurt takes all three -- exec'd directly,
//	         depends on SIGTSTP, installs no handler of its own -- so the class is narrow.
//	         It is not enumerated, and "nothing like that exists" is not the claim: that
//	         would be an absence over an unbounded population.
//
// The other five are left as they arrive, for reasons particular to each. An ignored
// SIGCONT still resumes a stopped process, because the kernel resumes on SIGCONT whatever
// the disposition, so there is nothing to fix. SIGTTIN and SIGTTOU tmux deliberately leaves
// ignored in its panes, so passing them on is the behaviour to have and clearing them would
// be the deviation. 32 and 34 are glibc's SIGCANCEL and SIGRTMIN: nothing an interactive
// shell does sends either with an expectation, so an inherited one costs nothing, while
// notifying would take a real-time signal away from the host for the rest of its life and,
// for 32, would put a Go handler over the signal glibc uses to cancel its own threads.
//
// Asking to be notified is the lever, and what carries it into the child is the fork rather
// than the exec. signal.Notify sets the runtime's handlingSig for the signal -- by CAS, inside
// sigenable -- and installs a handler over the inherited SIG_IGN. Every Go fork then runs
// clearSignalHandlers in the child before it execs (runtime.syscall_runtime_AfterForkInChild,
// which walks every signal and forces SIG_DFL for each one whose handlingSig is set), so the
// child reaches execve already holding SIG_DFL. execve resetting a handled signal is a second
// mechanism arriving at the same place, but handlingSig is the deciding variable and the kernel
// disposition at the moment of the fork is not: two forks with identical dispositions and
// different handlingSig give opposite children, measured. That is also how to read the
// concurrency number in signals_unix_test.go: handlingSig is set before setsig, so a child
// forked in between is already protected and the count there is an upper bound.
//
// signal.Ignore is not a substitute: it installs a real SIG_IGN,
// which is exactly what gets inherited. Nothing ever reads the channel and nothing needs to
// -- the signal package drops a signal it cannot deliver rather than blocking or killing the
// process -- so a daemon started under nohup goes on surviving the hangup nohup was asked to
// protect it from, and one started as a background job goes on surviving Ctrl-C.
//
// The kernel is asked, not signal.Ignored, because signal.Ignored answers from a word the
// runtime keeps and that word disagrees with the kernel in three directions, all measured here.
// Two of them can end with a window shell holding the disposition this is supposed to clear,
// and one sub-case of the second cannot, for the reason the paragraph above gives -- it is
// called out where it arises:
//
//   - It cannot see the four _SigDefault signals at all when they were *inherited*. In a
//     process that inherited SIG_IGN for all eight, signal.Ignored answered true for SIGHUP,
//     SIGINT, 32 and 34 and false for SIGCONT, SIGTSTP, SIGTTIN and SIGTTOU -- so SIGTSTP
//     could not be cleared while that was the predicate. An in-process signal.Ignore is not a
//     stand-in for this: it sets the word, and then signal.Ignored answers true.
//
//   - After the first notification it answers false permanently, and for SIGHUP and SIGINT a
//     host can put SIG_IGN back underneath it. signal.Reset restores the disposition sigenable
//     captured, which is the inherited SIG_IGN, while signal.Ignored goes on answering false --
//     so a host that ignores one of those two, opens a window, and then ends its own critical
//     section with an ordinary signal.Reset leaves the kernel ignoring the signal and leaves
//     this function unable to tell. Every window after that point gets it.
//
//     SIGHUP and SIGINT and not SIGTSTP, and the asymmetry is runtime.sigdisable's: it restores
//     fwdSig only when !sigInstallGoHandler(sig), and sigInstallGoHandler answers false for an
//     inherited SIG_IGN on SIGHUP or SIGINT by name while SIGTSTP, being neither of those nor
//     _SigSetStack, gets true. So a host's Reset leaves a Go handler on SIGTSTP with handlingSig
//     still set: the hazard cannot be put back for it, and the fix is permanently armed for it
//     after its first notification. Measured for all three, with the forked child reporting its
//     own SigIgn: hangup and interrupt come back SIG_IGN and the child inherits it, stopped
//     comes back a handler and the child does not. TestAHostsSignalResetCannotDisableTheFix is
//     that measurement, and it has to read the child: this process's own mask after a Reset does
//     not say what execve carries. Inverting either expectation fails all three arms.
//
//     Asking the kernel each time is what makes the fix re-armable for the two that can be put
//     back, and it is why the fix must not be a sync.Once. The re-arm works because signal.Reset
//     and signal.Ignore both drive os/signal's reference count for the signal to zero, so the
//     next Notify installs a handler again. A host that puts SIG_IGN back *without* going through
//     os/signal -- a raw rt_sigaction, or cgo signal(SIGHUP, SIG_IGN) in a C library -- leaves
//     that count held, and Notify then returns having installed nothing while the kernel goes on
//     ignoring the signal: this function reports nothing to do, on that spawn and every one
//     after. Measured, and cgo is not what it takes to get there: syscall.Syscall6 on
//     SYS_RT_SIGACTION is the route and it is pure Go, measured at CGO_ENABLED=0 in a binary
//     with no x_cgo_init in it -- Notify, then a raw SIG_IGN, and the kernel's SigIgn bit is set
//     while signal.Ignored answers false and a second Notify installs nothing over it. It is a
//     limit of the lever rather than of the predicate, since no choice of predicate installs a
//     handler Notify declined to install.
//
//     That sub-case alone does not reach the window's shell, and saying it did was this
//     comment's own paragraph above being ignored: for a _SigNotify signal, which all three of
//     these are, a held reference count implies handlingSig is set -- os/signal's add calls
//     enableSignal only at ref 0, and handlingSig returns to 0 only once ref has been driven to
//     0 under handlers.Lock, in cancel() or in Stop. (Not for a signal without _SigNotify:
//     sigenable returns before touching handlingSig for 32, 33 and 34, so a Notify for one of
//     those holds a reference over a handlingSig that is still 0 -- measured.) So the
//     fork still clears the disposition in the child. What it costs is this function's own
//     postcondition -- it returns reporting nothing to do while the kernel has SIG_IGN -- and
//     any host fork that does not go through syscall.forkAndExecInChild1, a cgo fork(3) being the
//     case that matters. That is a narrower host class than the sub-case itself needs, and the two
//     were once written as the same one: reaching the sub-case takes only a raw rt_sigaction,
//     which is pure Go, while being harmed by it also takes a fork this package does not perform.
//
//   - It over-reports in the other direction, for the _SigSetStack signals. signal.Ignore(32)
//     and signal.Ignore(34) set the runtime's word without ever reaching setsig -- measured,
//     the kernel's SigIgn does not change -- and signal.Ignored then answers true for a signal
//     the kernel is not ignoring. This one costs nothing today, because 32 and 34 are not
//     cleared, but it is the same defect as the other two and it is why the word is not worth
//     trusting for a question the kernel can be asked directly.
//
// Where the kernel cannot be asked, this falls back to signal.Ignored and inherits both gaps:
// SIGTSTP is then never cleared, and a host's signal.Reset disables SIGHUP and SIGINT for the
// life of the process. That is a stated limitation and not a measured one.
//
// It is not only darwin's. kernelIgnoredMask is implemented for Linux alone, so in this module's
// supported set darwin always takes the fallback -- and there is no darwin host here to measure
// it on. But the Linux implementation reads /proc/self/status, and a Linux process that cannot
// is on the fallback too: /proc not mounted, as in a distroless container or a chroot; openat
// denied by seccomp. Not a proc mounted with subset=pid, which is the obvious third and does not
// belong here: it hides the top-level entries that do not belong to a task and keeps each task's
// own directory, so it is a route to an unreadable /proc/net/unix -- internal/daemon's
// addressHeld, which is where that mistake belongs -- and not to an unreadable
// /proc/self/status. So the set of hosts running with these gaps is larger than the platform list
// suggests, and nothing here records which side a given process is on.
//
// The lock covers the check and the notification together, because they are not separable.
// signal.Notify clears the runtime's ignored bit *before* it installs the handler:
// signal_enable stores the cleared bit and only then calls sigenable, which hands the
// signal-mask goroutine one channel round trip -- enableSigChan <- sig, then <-maskUpdatedChan --
// before setsig replaces SIG_IGN in the kernel.
// A concurrent second spawn that looks inside that window is told the signal is no longer
// ignored while the kernel still has SIG_IGN, and it forks. Measured without the race detector
// at all, against signal.Ignored as the predicate, and detected by reading the kernel's
// disposition with a raw rt_sigaction immediately after this function returns: a large fraction
// of 640 concurrent first spawns returned from here with SIGHUP still SIG_IGN in the kernel.
// A fraction and not a count, because how large depends on the machine and the clause this
// supports is about what holding the lock fixes rather than about how often -- the rates, with
// the conditions they were taken under, are in TestTheCheckAndTheNotificationAreOneStep. Holding
// the lock across the notification makes a later caller wait for setsig, so its answer is true
// whichever source it came from.
//
// Naming the detector is not pedantry, because the window is a few microseconds wide and the
// probe's own cost is subtracted from it. A /proc/self/status parse takes 24-61us and reads the
// disposition after most of the window has already closed; rt_sigaction takes about 1us, and
// catches strictly more -- the paired measurement that establishes that lives with the probe it
// is about, on kernelIgnoredNow in sigprobe_linux_test.go, and is not repeated here. It was
// written out in three places once, and the correction to it landed in two of them. So a rate
// quoted here without its instrument, and the load it was taken under, is not comparable to any
// other rate quoted here.
//
// Where the lock is load-bearing is the fallback. Asking the kernel happens to close the same
// window on its own: a caller that reads SIG_IGN mid-window goes on to call signal.Notify,
// which takes the lock inside os/signal that the first caller is still holding, and so returns
// only after setsig. The fallback has no such luck -- it returns early, taking no lock at all
// -- and that is the path this lock exists for. It is kept on both paths rather than only the
// one because an invariant that holds for a local reason is worth more than one that holds
// because of where another package happens to lock.
//
// What the host can observe is therefore not nothing, and the honest statement of it is:
// whether these signals end this process is unchanged, since both SIG_IGN and a handler
// nobody reads drop them; what does change is that signal.Ignored answers false for them
// afterwards, and that children the host spawns afterwards get SIG_DFL where they used to get
// SIG_IGN. That second effect is the cost, and it is not free -- the lever is process-global,
// so a host that had ignored SIGHUP for its own workers loses that for every worker it starts
// after its first window, and there is no way to ignore a signal for one child and not
// another. What a host gets for it is a window it can interrupt, suspend, and end by closing
// its pty. A host that would rather keep the inherited disposition can: end windows with
// Window.Kill, which SIGKILLs and depends on none of this.
//
// Taking something process-global is normally a decision for a program and not for a package
// -- the rule main.go states twice, for InitCgroups and InitDescriptorTable -- and the rule
// does not reach this case. Those two acquire a resource the host then shares: a cgroup
// directory, a larger descriptor table. This one acquires nothing; it removes a hazard from
// the processes this package is about to create.
//
// It is called from NewWindow, at the spawn, because the spawn is where the disposition is
// inherited and NewWindow is the only place in this module that spawns a window shell. The
// one other spawn that outlives its parent, daemon.SpawnDetached, deliberately does not call
// this: a daemon is *meant* to survive a hangup, which is what nohup was asked for, and
// `shelld stop` reaches it with SIGTERM and then SIGKILL, neither of which a disposition
// refuses. Placing this where a daemon starts serving would cover a daemon and nothing else:
// not a Go program using this package as a library, which is the entry point doc.go leads
// with and the surface README.md calls the library, and not one embedding daemon.Handler
// behind its own http.Server. Per spawn rather than once also covers a host that ignores one
// of these after its first window.
func dontPassOnIgnoredSignals() {
	passOnMu.Lock()
	defer passOnMu.Unlock()

	mask, fromKernel := ignoredMaskSource()
	for _, sig := range clearedWhenIgnored {
		if !ignoredNow(sig, mask, fromKernel) {
			continue
		}
		signal.Notify(sigSink, sig)
	}
}

// ignoredNow reports whether sig is ignored, from the kernel's mask where there is one and
// from the signal package otherwise.
func ignoredNow(sig os.Signal, mask uint64, fromKernel bool) bool {
	if !fromKernel {
		return signal.Ignored(sig)
	}
	n, ok := sig.(syscall.Signal)
	// Signal n is bit n-1 of a 64-bit field; anything outside that cannot be asked about.
	if !ok || n < 1 || n > 64 {
		return false
	}
	return mask&(1<<(uint(n)-1)) != 0
}
