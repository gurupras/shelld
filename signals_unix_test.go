//go:build unix

package shelld

import (
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestAWindowShellDoesNotInheritAnIgnoredSignal asserts the disposition of the shell
// itself, which is the only place the defect exists.
//
// It is the shell's mask and not this process's because Go's bookkeeping and the kernel
// disposition disagree here by design: signal.Ignored answers for what the signal package
// is keeping, execve copies what the kernel holds, and a child is the only place the two
// are the same question. Asserting on signal.Ignored would pass with the fix deleted.
//
// No daemon appears anywhere in it. That is the point: cmd/shelld has its own end-to-end
// test for a daemon launched under an ignored SIGHUP, and this covers a Go program using
// the package as a library, which is the entry point doc.go leads with and the one a
// daemon-level fix does not reach.
//
// The signals run as separate subtests rather than together so that each one names what it
// costs, and so that a fix covering one of them cannot be read as covering the rest.
//
// Each arm arranges the disposition with an in-process signal.Ignore, which is a faithful
// stand-in for an inherited one only for SIGHUP and SIGINT -- see the subprocess test below,
// which is what establishes that and what covers SIGTSTP in the condition that really
// produces it.
func TestAWindowShellDoesNotInheritAnIgnoredSignal(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skipf("cannot read a process's ignored-signal mask here (%v), and the mask is the whole assertion", err)
	}

	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		cost string
	}{
		{"hangup", syscall.SIGHUP, "when nothing gets to run Window.Kill -- the daemon itself died -- hanging the session up is all that is left, so a shell that ignores SIGHUP outlives its window and its daemon"},
		{"interrupt", syscall.SIGINT, "Ctrl-C in the window does nothing, and a shell that starts a background job without job control ignores SIGINT in it, so no nohup is needed to get here"},
		{"suspend", syscall.SIGTSTP, "a command exec'd as the window's argv with no shell in between cannot be suspended with Ctrl-Z; through a shell the inherited ignore is undetectable, since an interactive bash and dash both ignore SIGTSTP for themselves anyway and restore SIG_DFL in every job they fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The cleanup runs the fix rather than restoring what was there, because
			// there is no call that restores SIG_DFL from here: signal.Reset installs
			// SIG_IGN. sigenable overwrote fwdSig with the SIG_IGN signal.Ignore had
			// just installed, and sigdisable puts fwdSig back, so a Reset here would
			// hand the hazard to every test in the package that runs after this one --
			// most of which open a window. Measured: a window created afterwards had
			// the bit set, and signal.Ignored answered false, so the fix could never
			// re-arm. What the fix leaves is a handler and an empty ignored set, which
			// is the state every other test in the package wants, and running it in
			// the cleanup covers the paths where this subtest fails before it spawns.
			//
			// Not a hand-rolled signal.Ignore followed by signal.Notify either, which does
			// clear the same state -- Ignore goes through cancel(), driving os/signal's
			// reference count to 0, so the Notify after it reaches sigenable and the CAS
			// succeeds, measured including on a raw SIG_IGN installed under a held
			// reference. It is rejected for what it would become rather than for not
			// working: a second copy of dontPassOnIgnoredSignals, in a cleanup, drifting
			// from it silently.
			t.Cleanup(dontPassOnIgnoredSignals)
			signal.Ignore(tc.sig)
			if !signalIgnoredIn(t, readStatus(t, "/proc/self/status"), tc.sig) {
				t.Fatalf("setup: %v is not ignored after signal.Ignore, so nothing here is about an inherited disposition", tc.sig)
			}

			if got := windowShellIgnoredMask(t); signalIgnoredIn(t, got, tc.sig) {
				t.Errorf("the window's shell ignores %v (%s): %s", tc.sig, strings.TrimSpace(got), tc.cost)
			}
		})
	}
}

// windowShellIgnoredMask opens a window whose program reports its own ignored-signal mask,
// and returns it in the form /proc uses. The report goes through a file rather than the pty
// so that the assertion does not depend on reading or decoding terminal output.
func windowShellIgnoredMask(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/sigign"
	w, err := NewWindow(WindowOpts{
		Name:    "signals",
		Cwd:     dir,
		Command: []string{"/bin/sh", "-c", "awk '/^SigIgn:/{print $2}' /proc/self/status > " + path},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()

	// Written by a process that has already exec'd, so there is nothing to wait for but
	// the write; a deadline rather than a fixed sleep keeps it quick and still bounded.
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, readErr := os.ReadFile(path)
		if s := strings.TrimSpace(string(b)); readErr == nil && s != "" {
			return "SigIgn:\t" + s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the window's shell never reported its ignored-signal mask (last read: %v), so this test cannot say what it inherited", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readStatus(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// signalIgnoredIn reads the SigIgn line of a /proc status dump. Signal n is bit n-1.
func signalIgnoredIn(t *testing.T, status string, sig syscall.Signal) bool {
	t.Helper()
	for line := range strings.SplitSeq(status, "\n") {
		hex, ok := strings.CutPrefix(line, "SigIgn:")
		if !ok {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			t.Fatalf("SigIgn %q: %v", strings.TrimSpace(hex), err)
		}
		return bits&(1<<(uint(sig)-1)) != 0
	}
	t.Fatal("no SigIgn line in the status dump, so the mask cannot be read")
	return false
}

// The environment variable that tells a re-exec'd copy of this test binary it is the child
// half of TestAnInheritedIgnoredSignalIsClearedExactlyWhereItCosts.
const inheritedSignalHelperEnv = "SHELLD_TEST_INHERITED_SIGNAL_HELPER"

// leakingSignals is the set of signals whose SIG_IGN survives into a child of a Go program.
// clearedWhenIgnored is the subset dontPassOnIgnoredSignals acts on, and the subprocess test
// below is what holds the two apart.
//
// Which signals reach that far is decided by whether the runtime's initsig installs a handler
// over the inherited disposition, and it installs one for everything except three groups: the
// signals with no flags at all (SIGKILL and SIGSTOP, neither of which can be ignored anyway),
// the _SigDefault signals (SIGCONT, SIGTSTP, SIGTTIN, SIGTTOU), and the _SigSetStack signals
// (32, 33, 34) less 33 -- which stays out of the set in a cgo and a non-cgo build alike, by two
// different mechanisms; dontPassOnIgnoredSignals records which, and why the difference matters
// for cmd/shelld. SIGHUP and SIGINT are the exception that makes this a bug at all: the runtime
// does install handlers for those two, but respects an inherited SIG_IGN and leaves it in place.
//
// This list is not what establishes the set, and must not be read as if it were -- a hardcoded
// list cannot discover a member it does not already have.
// TestAnInheritedIgnoredSignalIsClearedExactlyWhereItCosts sweeps every signal its launcher can
// ignore and requires the survivors to be exactly this list intersected with the set the
// launcher managed to set, so a signal that starts leaking fails there rather than going
// unnoticed here.
var leakingSignals = []syscall.Signal{
	syscall.SIGHUP, syscall.SIGINT, syscall.SIGCONT,
	syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU,
	// glibc's SIGCANCEL and SIGRTMIN, which have no names in syscall. 34 is an ordinary
	// application signal and a plain `trap "" 34` reaches it; 32 is reserved by glibc, whose
	// sigaction refuses it, so only the raw rt_sigaction syscall can ignore it and the
	// subprocess test reports it as unreachable rather than pretending to cover it.
	syscall.Signal(32), syscall.Signal(34),
}

// visibleToOsSignalWhenInherited is the measured subset of leakingSignals that signal.Ignored
// can report when the disposition was inherited across an exec rather than installed
// in-process.
//
// It is four of the eight, and that is exactly why visibility is not the rule for deciding what
// to clear: 32 and 34 are visible here and are deliberately left alone, while SIGTSTP is
// invisible and is cleared. A rule phrased as "clear what os/signal can see" would get both of
// those backwards. What kernelIgnoredMask buys is the four _SigDefault signals, and SIGTSTP is
// the one of those whose inheritance costs a user anything.
//
// Pinned rather than described, so that a future Go widening what signal.Ignored reports fails
// here and says which signal changed instead of quietly making the comment wrong.
var visibleToOsSignalWhenInherited = []syscall.Signal{
	syscall.SIGHUP, syscall.SIGINT, syscall.Signal(32), syscall.Signal(34),
}

// signalsIn names the signals a /proc mask has bits for, for a failure message that says which
// signal moved rather than printing two hex numbers and leaving the reader to decode them.
func signalsIn(mask uint64) []int {
	var out []int
	for n := 1; n <= 64; n++ {
		if mask&(1<<(uint(n)-1)) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// maskAfterPrefix reads the hex mask out of a line the launcher or child printed, which carries
// a /proc SigIgn line behind a marker word.
func maskAfterPrefix(line, prefix string) (uint64, bool) {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, false
	}
	mask, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
	return mask, err == nil
}

// TestAnInheritedIgnoredSignalIsClearedExactlyWhereItCosts is the justification for
// clearedWhenIgnored having three entries and not eight, measured in the condition that
// produces the bug rather than in a stand-in for it.
//
// signal.Ignore is a faithful stand-in for an inherited SIG_IGN on SIGHUP and SIGINT and on
// nothing else: an in-process signal.Ignore(SIGTSTP) is visible to signal.Ignored where the same
// disposition inherited across an exec is not, and signal.Ignore(32) does not reach the kernel
// at all. Only a process that really inherited one can tell the cases apart, so this re-execs
// the test binary under a shell that has ignored everything it could.
//
// What it pins, for every signal the launcher managed to ignore:
//
//   - whether the kernel reports the inherited disposition, which is what the fix reads;
//   - whether signal.Ignored reports it, which is what the fix must not read, and the reason;
//   - whether a window shell spawned afterwards still has it.
//
// The third is the behaviour and the first two are why the behaviour is reachable. Keeping all
// three means a regression says which layer moved: the kernel column failing is a broken
// predicate, the visible column failing is os/signal having changed underneath the design, and
// the shell column failing on its own is the fix itself.
//
// It also pins the leak set itself, which is the part a per-signal table cannot do. The launcher
// sweeps all 64 signals rather than the ones expected to leak, reports the set it achieved, and
// the child reports the set that survived; requiring the second to equal the first intersected
// with leakingSignals is what makes the population measured instead of assumed. Without that, a
// signal a future Go stops handling would start reaching window shells and every test here would
// go on passing, because each one only ever asks about signals already on the list.
func TestAnInheritedIgnoredSignalIsClearedExactlyWhereItCosts(t *testing.T) {
	if os.Getenv(inheritedSignalHelperEnv) != "" {
		reportInheritedSignals(t)
		return
	}
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skipf("cannot read a process's ignored-signal mask here (%v), and the masks are the whole assertion", err)
	}

	// Every signal, not only the ones expected to leak, so the survivors are measured out of
	// the whole population rather than checked against a list that already assumes the answer.
	// The shell cannot set all of them -- 9 and 19 cannot be ignored at all, and glibc reserves
	// 32 and 33 -- and `trap` reports success either way, so the launcher prints the set it
	// actually achieved and every assertion below is made against that instead of against what
	// was asked for.
	//
	// `exec` so the trapping shell is replaced by the test binary: exec preserves the ignored
	// set, and using it means the process that inherits is the one under test rather than a
	// descendant of it.
	var trap strings.Builder
	for n := 1; n <= 64; n++ {
		fmt.Fprintf(&trap, " %d", n)
	}
	script := `trap ""` + trap.String() + `
printf 'LAUNCHER '
grep SigIgn: /proc/self/status
exec "$1" -test.run='^` + t.Name() + `$'`
	cmd := exec.Command("/bin/sh", "-c", script, "sh", os.Args[0])
	cmd.Env = append(os.Environ(), inheritedSignalHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child half of this test failed (%v); its output was:\n%s", err, out)
	}

	type answers struct{ kernel, visible, inShell bool }
	seen := map[syscall.Signal]answers{}
	var launcherMask, childMask uint64
	var haveLauncher, haveChild bool
	for line := range strings.SplitSeq(string(out), "\n") {
		var num int
		var a answers
		switch {
		case strings.HasPrefix(line, "LAUNCHER "):
			launcherMask, haveLauncher = maskAfterPrefix(line, "LAUNCHER SigIgn:")
		case strings.HasPrefix(line, "CHILD "):
			childMask, haveChild = maskAfterPrefix(line, "CHILD SigIgn:")
		default:
			if n, _ := fmt.Sscanf(line, "INHERITED %d kernel=%t visible=%t in-window-shell=%t",
				&num, &a.kernel, &a.visible, &a.inShell); n == 4 {
				seen[syscall.Signal(num)] = a
			}
		}
	}
	if !haveLauncher || !haveChild {
		t.Fatalf("the launcher and the child have to report their own ignored-signal masks and one of them did not (launcher=%t child=%t); the output was:\n%s", haveLauncher, haveChild, out)
	}

	var expected uint64
	for _, sig := range leakingSignals {
		expected |= 1 << (uint(sig) - 1)
	}

	// The floor, asserted before anything else is asserted against launcherMask.
	//
	// Every assertion below intersects with the set the launcher achieved. That is the right
	// shape -- the launcher cannot ignore SIGKILL and it would be dishonest to claim otherwise
	// -- and it is also the shape that turns into nothing at all if the launcher achieves
	// nothing. Demonstrated: replacing this test's `trap ""` with `: ""` leaves launcherMask
	// zero, and then launcherMask&expected is zero, childMask is zero, the population assertion
	// holds, and all eight per-signal arms log and continue. The test passes having established
	// nothing, and the log lines it explains itself with are not printed by a passing gate.
	//
	// So require the fixture to have achieved the condition first. Two floors, because they buy
	// different things.
	//
	// What the first floor demands comes from two sources, unioned. Every signal the fix claims
	// to clear, because clearedWhenIgnored is dontPassOnIgnoredSignals' own subject and a test
	// that silently skipped one of those would assert nothing about the thing it is for. And the
	// rest of the leak set below signal 32, which is what the per-signal arms need: they are the
	// three columns, and a silently absent signal deletes one.
	//
	// 32 is the line because that is where a libc stops following the ABI and starts keeping
	// numbers for itself. Signals 1 to 31 are named by the kernel's ABI and no libc may
	// repurpose one, so a launcher that cannot ignore one of them is a broken fixture and this
	// has to say so. The realtime numbers above them are the libc's to carve up, and how many it
	// takes is its own answer: measured here, glibc's sigaction refuses 32 and 33 while a plain
	// `trap "" 34` succeeds, which is why 34 is in leakingSignals at all. A libc that kept one
	// more would make this Fatalf about its own reservation, and a reservation is not a fixture
	// failure -- which is exactly what the earlier spelling did, exempting the literal 32 and
	// nothing else, so it read this host's answer as a property of the signals.
	//
	// Below the line is not automatically reachable either, and that is why the line is the right
	// shape for this rather than a longer exemption list: /bin/dash will not ignore SIGSEGV where
	// busybox sh will, measured. SIGSEGV is not in the leak set so it never reaches this floor,
	// but a leak set that held a signal some shell refuses for its own reasons should Fatalf
	// here, because then the fixture really cannot put the test in the condition it is about.
	//
	// The popcount is what the *population* assertion needs, and that it needs anything is easy
	// to miss: its power to discover a signal nobody listed comes entirely from the launcher
	// having swept signals outside leakingSignals. A launcher that ignored exactly the eight
	// listed ones would satisfy the floor above and could never find a ninth. 56 of 64 is a floor
	// and not an exact figure because the achievable set is shell-dependent, measured here:
	// /bin/sh and /bin/dash reach 58, /bin/bash 59 (it also ignores SIGCHLD). Four can never be
	// reached on this host -- the kernel refuses 9 and 19, glibc reserves 32 and 33 -- so 60 is
	// the ceiling here, and 56 leaves a shell four of its own without letting the sweep collapse
	// to the list it is supposed to be auditing. That margin is also what keeps the floor
	// portable: a libc that reserves more of the realtime numbers lowers the ceiling by one
	// apiece, and the floor has to sit far enough below it that a reservation does not fail this.
	const libcMayReserve = 32
	need := expected & ((uint64(1) << (libcMayReserve - 1)) - 1)
	for _, sig := range clearedWhenIgnored {
		need |= 1 << (uint(sig.(syscall.Signal)) - 1)
	}
	if missing := need &^ launcherMask; missing != 0 {
		t.Fatalf("the launcher did not manage to ignore %v, and every assertion below intersects with what it achieved, so those signals would go unasserted rather than tested; the output was:\n%s", signalsIn(missing), out)
	}
	if n := bits.OnesCount64(launcherMask); n < 56 {
		t.Fatalf("the launcher ignored %d of 64 signals (%v), below the floor of 56: the population assertion can only discover a signal outside leakingSignals if the launcher swept past it, and a narrow sweep makes that assertion agree with whatever the list already says; the output was:\n%s", n, signalsIn(launcherMask), out)
	}

	// The population assertion, and the reason this test can discover a signal nobody listed:
	// the child inherited whatever the launcher achieved, the runtime took back every signal it
	// installs a handler for, and what is left has to be exactly the leak set. A signal that
	// starts surviving execve shows up here as a bit leakingSignals does not have.
	if want := launcherMask & expected; childMask != want {
		t.Errorf("the launcher ignored %v and %v survived into the Go child, but leakingSignals says %v should have:\n"+
			"  leaked and unaccounted for: %v\n"+
			"  listed as leaking and did not: %v\n"+
			"a signal in the first list reaches window shells with nothing having decided that it should",
			signalsIn(launcherMask), signalsIn(childMask), signalsIn(want),
			signalsIn(childMask&^want), signalsIn(want&^childMask))
	}

	cleared := map[syscall.Signal]bool{}
	for _, sig := range clearedWhenIgnored {
		cleared[sig.(syscall.Signal)] = true
	}
	visibleToOsSignal := map[syscall.Signal]bool{}
	for _, sig := range visibleToOsSignalWhenInherited {
		visibleToOsSignal[sig] = true
	}

	for _, sig := range leakingSignals {
		if launcherMask&(1<<(uint(sig)-1)) == 0 {
			// Named rather than skipped in silence: glibc refuses to ignore 32, so no
			// /bin/sh launcher can put this test in the condition it is about for that
			// signal. The population assertion above already accounts for the absence -- it
			// intersects with what the launcher achieved -- and 32 keeps its entry in
			// leakingSignals because it does leak, measured out of tree with a raw
			// rt_sigaction launcher.
			//
			// This is where any realtime number the running libc keeps for itself arrives,
			// which is what the floor's line at 32 leaves to this arm: 34 is reachable on
			// glibc and this says so in a sentence if some other libc's is not.
			t.Logf("signal %d is in leakingSignals and /bin/sh could not ignore it here, so nothing below is asserted about it", int(sig))
			continue
		}
		got, reported := seen[sig]
		if !reported {
			t.Errorf("the launcher ignored signal %d and the child said nothing about it, so this cannot say whether it reaches a window shell; the output was:\n%s", int(sig), out)
			continue
		}

		// The predicate's own reading. Every signal reaching here is inherited ignored, so a
		// false means the fix could not act on it even if it wanted to.
		if !got.kernel {
			t.Errorf("the kernel does not report an inherited ignored %v, so kernelIgnoredMask cannot be the predicate for it and nothing else here is about a disposition the fix could act on", sig)
		}

		// os/signal's reading, which is the reason the kernel is asked instead.
		if got.visible != visibleToOsSignal[sig] {
			t.Errorf("signal.Ignored reports an inherited ignored %v as %v, want %v: visibleToOsSignalWhenInherited is the measured set and the comment on dontPassOnIgnoredSignals explains the design from it, so one of the two is now wrong",
				sig, got.visible, visibleToOsSignal[sig])
		}

		// The behaviour. Both directions are asserted: a signal in the list must be gone
		// from the shell, and one left out must still be there -- because "still ignored"
		// is what the comment claims for SIGCONT, SIGTTIN and SIGTTOU, and a silent
		// improvement should make the claim fail rather than rot into a false one.
		switch {
		case cleared[sig] && got.inShell:
			t.Errorf("%v is in clearedWhenIgnored and the window's shell still ignores it", sig)
		case !cleared[sig] && !got.inShell:
			t.Errorf("an inherited ignored %v no longer reaches the window's shell, but it is deliberately left out of clearedWhenIgnored, so either something else is clearing it or the reason given for leaving it out no longer holds", sig)
		}
	}
}

// reportInheritedSignals is the child half: it runs in a process that inherited SIG_IGN for
// every signal in leakingSignals, and prints one line per signal for the parent to check.
func reportInheritedSignals(t *testing.T) {
	status := readStatus(t, "/proc/self/status")

	// The whole inherited mask, which is what lets the parent measure the leak set out of every
	// signal the launcher could ignore instead of reading it off leakingSignals.
	fmt.Printf("CHILD %s\n", sigIgnLine(status))

	// Both readings are taken BEFORE the window, because opening one runs the fix and the fix
	// is what clears three of these: asking afterwards reports them as never having been
	// ignored and the test blames the wrong thing.
	mask, known := kernelIgnoredMask()
	if !known {
		t.Fatal("setup: kernelIgnoredMask could not answer, and it is the predicate this test is about")
	}
	visible := map[syscall.Signal]bool{}
	for _, sig := range leakingSignals {
		visible[sig] = signal.Ignored(sig)
	}

	// One window, read once: dontPassOnIgnoredSignals runs at the spawn, so a single window's
	// mask answers for every signal at the same point in the process's life.
	//
	// Only the signals actually inherited are reported. A signal the launcher could not ignore
	// is not a setup failure to fatal on -- glibc refuses 32 and there is no shell that can --
	// and the parent reconciles the silence against the mask the launcher printed.
	shell := windowShellIgnoredMask(t)
	for _, sig := range leakingSignals {
		if !signalIgnoredIn(t, status, sig) {
			continue
		}
		fmt.Printf("INHERITED %d kernel=%t visible=%t in-window-shell=%t\n",
			int(sig), ignoredNow(sig, mask, true), visible[sig], signalIgnoredIn(t, shell, sig))
	}
}

func sigIgnLine(status string) string {
	for line := range strings.SplitSeq(status, "\n") {
		if strings.HasPrefix(line, "SigIgn:") {
			return line
		}
	}
	return "(none)"
}

// kernelIgnoresNow is the kernel's answer for one signal, for tests that are about the
// disposition the fix reads rather than about a window shell.
func kernelIgnoresNow(t *testing.T, sig syscall.Signal) bool {
	t.Helper()
	mask, known := kernelIgnoredMask()
	if !known {
		t.Skipf("the kernel's ignored-signal mask cannot be read here, and it is the whole assertion")
	}
	return ignoredNow(sig, mask, true)
}

// TestTheCheckAndTheNotificationAreOneStep is the regression for a hole that a single-threaded
// test cannot see: two windows opened at once, where the first one's notification is in
// progress and the second one's check is told the signal is already dealt with.
//
// signal.Notify clears the runtime's ignored word before it installs the handler, so there is
// a window in which the predicate answers "not ignored" and the kernel still holds SIG_IGN. A
// caller that returns inside that window goes on to fork, and its shell inherits the
// disposition. Measured against the unlocked version: a large fraction of concurrent callers
// return inside it, and how large depends on the machine -- see the rates below and what they
// are worth.
//
// It runs on the fallback rather than the kernel path deliberately. Asking the kernel happens
// to close this window by itself -- a caller that reads SIG_IGN calls signal.Notify, which
// blocks on os/signal's own lock until the first caller's setsig has landed -- so the kernel
// path cannot demonstrate what the lock is for. The fallback is the production path everywhere
// except Linux, it takes no lock of its own, and it is where removing this lock reintroduces
// the bug.
//
// No windows are opened. The property is about dontPassOnIgnoredSignals having returned while
// the kernel still ignores the signal, which is the instant window.go forks; spawning 640
// shells to observe the same thing would be slower and no more conclusive.
//
// Both arms exist because without the lock the loop protects itself unevenly, and which signal
// is exposed depends on where it sits in clearedWhenIgnored. A loser that skips signal k goes on
// to call signal.Notify for signal k+1, which blocks on os/signal's lock until the winner has
// finished -- and by then the winner's setsig for k has landed. So every entry is covered by the
// entry after it and only the last is open. One arm ignores a single signal, where nothing
// follows it; the other ignores all three, where the leak moves to the end of the list. A
// one-signal test would pass with the lock removed if that signal were not last, which is a
// worse trap than the race: it makes the order of a var declaration load-bearing with nothing
// saying so.
//
// The two arms have very different power, and the first is the one that catches a regression.
// With the lock removed, the single-signal arm leaks several times what the three-signal arm
// does, every time it has been measured -- but the absolute rates are a property of the machine
// as much as of the defect, so they are given with the conditions they were taken under rather
// than as figures to compare against: one session gave 99-111 of 640 on the single-signal arm
// against 1 of 640 on the three-signal one, conditions not recorded, and another gave 173-228
// against 12-59 at loadavg 4-8.
//
// (99-111 is also the figure kernelIgnoredNow quotes, for a different claim -- the only number
// in these files with two homes, because each claim needs its own second operand and neither
// restates the other. What a re-measurement has to replace is not that number in two places: it
// is both operands at each site, because each pair was read in one run, and the same-run pairing
// is what the sensitivity claim rests on. Updating the shared one alone would leave every
// sentence here reading correctly and the claim underneath one of them no longer supported.)
//
// What the pair is for is the spread itself: the single-signal rate moves by about 2x with the
// machine, so a maintainer who runs the mutant and counts 3 can tell that from a quiet box
// rather than reading it as the defect being mostly closed. Only that arm moves by about 2x --
// the three-signal arm moved by more than ten times as much between the same two sessions, which
// is the same fact as its failing to fire at all on a quiet machine. The ordering is the part
// that reproduces. Read the second arm as evidence for the mechanism and not as the detector.
// Neither arm can flake,
// because the count only ever produces an error: a zero proves nothing and is reported as
// nothing.
//
// Those rates belong to the probe as much as to the defect, so the probe is named. They are
// rt_sigaction's, and a /proc/self/status parse of the same arm reads several times lower. The
// paired measurement that establishes that -- both probes read back to back in one iteration,
// with the containment result and the lock-restored control -- is on kernelIgnoredNow in
// sigprobe_linux_test.go, which is the one place it is written down. It used to be written in
// three, and a correction to it reached two.
//
// How much of an upper bound, measured rather than left as a caveat. In the same fixture, a
// goroutine that forks a child immediately after the guard returns and reads the *child's*
// disposition counted 7 of 640 in one run and 0 of 640 in the other eight -- 7 of 5760, 0.12%,
// against the ~19% the probe above reports. An arm that forked with no probe read at all
// counted 0 of 640 three times, and with the lock restored every arm counted 0. The gap is the
// deciding variable being read in the child after the fork: on Linux the child shares the
// parent's address space until the exec (CLONE_VM|CLONE_VFORK), so a winner's CAS that lands
// after the fork still saves that child.
//
// So the harm is about 150x rarer than its own precondition, and the assertion is still the
// precondition. Moving it to the child would be the faithful thing and would delete the
// detector: a gate that fires on one run in nine is indistinguishable from green, and the
// regression it exists for would land.
func TestTheCheckAndTheNotificationAreOneStep(t *testing.T) {
	// Gated on the probe this test actually uses, not on the one the production code uses:
	// they are different syscalls and a host can have one without the other.
	if _, known := kernelIgnoredNow(syscall.SIGHUP); !known {
		t.Skip("the kernel's disposition cannot be read with rt_sigaction here, and it is how a leak is detected")
	}

	// The fallback, which is what signal.Ignored answers and where the lock is load-bearing.
	//
	// Written under passOnMu, and restored under it, because dontPassOnIgnoredSignals reads
	// ignoredMaskSource while holding it. No test here spawns from a background goroutine, so
	// a bare write races nothing today -- taking the lock is what keeps that true of the next
	// test rather than something a reader has to re-derive.
	passOnMu.Lock()
	restore := ignoredMaskSource
	ignoredMaskSource = func() (uint64, bool) { return 0, false }
	passOnMu.Unlock()
	t.Cleanup(func() {
		passOnMu.Lock()
		ignoredMaskSource = restore
		passOnMu.Unlock()
		dontPassOnIgnoredSignals()
	})

	// Derived from clearedWhenIgnored rather than spelled out, because the second arm's name is a
	// claim about that list: the exposed entry is whichever one is last in it, so a list that
	// gained a fourth signal has to move this arm's exposure with it. A literal here would go on
	// ignoring three of four and the arm would quietly stop being the arm it says it is.
	allCleared := make([]syscall.Signal, len(clearedWhenIgnored))
	for i, sig := range clearedWhenIgnored {
		allCleared[i] = sig.(syscall.Signal)
	}

	for _, tc := range []struct {
		name string
		host []syscall.Signal
	}{
		{"one signal ignored, so nothing in the loop follows it", []syscall.Signal{syscall.SIGHUP}},
		{"every cleared signal ignored, which leaves only the last exposed", allCleared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const reps, concurrent = 40, 16
			var mu sync.Mutex
			leaked := map[syscall.Signal]int{}
			for range reps {
				// Re-arm the precondition each rep: the window only exists around the
				// first notification, and every rep needs a first one.
				for _, sig := range tc.host {
					signal.Ignore(sig)
				}
				var wg sync.WaitGroup
				for range concurrent {
					wg.Add(1)
					go func() {
						defer wg.Done()
						// Allocated before the call and not after it, so that the
						// allocation is not inside the window being measured. Sized from
						// the arm rather than fixed at 3, which would panic on a
						// clearedWhenIgnored that grew -- the same coupling the arm
						// above removed.
						ignored := make([]bool, len(tc.host))
						dontPassOnIgnoredSignals()
						// window.go forks here.
						//
						// Read with rt_sigaction and not kernelIgnoredMask, because the
						// probe is racing the window it is measuring and a slow one
						// closes it -- see kernelIgnoredNow. Each read costs about 1us,
						// so with more than one signal the later ones are sampled later
						// and read slightly low. That is the safe direction: the
						// assertion below only ever fires on a nonzero count.
						for i, sig := range tc.host {
							ig, known := kernelIgnoredNow(sig)
							if !known {
								return
							}
							ignored[i] = ig
						}
						mu.Lock()
						defer mu.Unlock()
						for i, sig := range tc.host {
							if ignored[i] {
								leaked[sig]++
							}
						}
					}()
				}
				wg.Wait()
			}
			for _, sig := range tc.host {
				if n := leaked[sig]; n > 0 {
					t.Errorf("%d of %d concurrent callers returned from dontPassOnIgnoredSignals with %v still SIG_IGN in the kernel. That is the precondition for a spawn in that instant to hand the disposition to its shell, and an upper bound on how many would rather than a count of them: the child's own clearSignalHandlers fires on handlingSig, which sigenable sets before setsig, so a caller that returned in between reads as SIG_IGN here and would still have forked a protected child",
						n, reps*concurrent, sig)
				}
			}
		})
	}
}

// TestAHostsSignalResetCannotDisableTheFix covers the one way a host can put the hazard back
// without doing anything unusual, and the reason the predicate cannot be signal.Ignored.
//
// A host that ignores SIGHUP around some critical section and ends it with signal.Reset has
// done nothing wrong and nothing to do with shelld. What it leaves behind is SIG_IGN in the
// kernel -- sigenable captured the disposition signal.Ignore had installed, and sigdisable
// puts that back -- while signal.Ignored answers false. A fix that believed signal.Ignored
// would skip every spawn from then on, silently and permanently, and every window after that
// point would get an uninterruptible shell.
//
// All three cleared signals, because Reset does not do the same thing to all three and a test
// of SIGHUP alone let the comment on dontPassOnIgnoredSignals generalise from one measurement.
// The restores column is runtime.sigdisable's answer rather than anything about the signal:
// sigdisable puts fwdSig back only when !sigInstallGoHandler(sig), and sigInstallGoHandler
// respects an inherited SIG_IGN for SIGHUP and SIGINT by name. SIGTSTP is neither of those and
// is not _SigSetStack, so it gets a Go handler and keeps it across a Reset -- which means the
// hazard cannot be put back for SIGTSTP at all, and the fix stays armed for it.
//
// The two directions of a surprise are not symmetric, so they are not reported the same way.
// A signal that stops being restorable makes this test's state unreachable and the hazard
// gone -- skipped. A signal that starts being restorable is a new way to disable the fix --
// failed.
func TestAHostsSignalResetCannotDisableTheFix(t *testing.T) {
	for _, tc := range []struct {
		sig      syscall.Signal
		restores bool
	}{
		{syscall.SIGHUP, true},
		{syscall.SIGINT, true},
		{syscall.SIGTSTP, false},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			t.Cleanup(dontPassOnIgnoredSignals)

			signal.Ignore(tc.sig)
			if !kernelIgnoresNow(t, tc.sig) {
				t.Fatalf("setup: %v is not ignored after signal.Ignore, so nothing here is about clearing it", tc.sig)
			}
			dontPassOnIgnoredSignals()
			if kernelIgnoresNow(t, tc.sig) {
				t.Fatalf("setup: the first call did not clear %v, so this cannot be about surviving a Reset", tc.sig)
			}

			// The ordinary host action.
			signal.Reset(tc.sig)

			switch back := kernelIgnoresNow(t, tc.sig); {
			case back && !tc.restores:
				t.Fatalf("signal.Reset put SIG_IGN back for %v, which sigdisable's gate says it cannot: that is a new way for an ordinary host action to re-arm the hazard, and the comment on dontPassOnIgnoredSignals says this signal is immune to it", tc.sig)
			case !back && tc.restores:
				t.Skipf("signal.Reset no longer restores the SIG_IGN it was handed for %v, so the state this test is about cannot be reached and the hazard it guards against is gone", tc.sig)
			case !back:
				// Nothing was put back, so there is nothing for the next spawn to
				// re-clear. The Go handler is still installed and handlingSig with it,
				// so a fork from here clears the child regardless -- which is the whole
				// claim for this arm.
				if signal.Ignored(tc.sig) {
					t.Errorf("the kernel does not ignore %v after a Reset but signal.Ignored says it does, so the two disagree in the direction that would make a predicate clear a signal that is already clear", tc.sig)
				}
				if childIgnores(t, tc.sig) {
					t.Errorf("a child forked after a Reset that restored no SIG_IGN for %v inherits SIG_IGN anyway, so this arm's claim -- that the handler left behind protects every spawn from here -- is not what a window's shell gets", tc.sig)
				}
				return
			}

			// The disagreement is what makes signal.Ignored unusable as the predicate, and this
			// is where it is recorded rather than where it gates anything: the assertion below
			// is that the *kernel's* SIG_IGN gets cleared, and the fix reads the kernel, so it
			// holds whether or not os/signal happens to agree today.
			//
			// Today it never agrees, and this branch is unreachable: reaching it needs the
			// step above to have cleared the kernel, that clearing is a signal.Notify, and
			// signal_enable clears the ignored bit signal_disable never puts back. So
			// signal.Ignored is false on every path that gets here -- run -v and no log line
			// appears. It is written as a log and not a skip for the Go that changes that:
			// the day the bit comes back, a skip here would take the live assertion below out
			// with it, and the arm this test exists for would stop being checked while the
			// package stayed green. Forcing the branch and running the notify-once mutant is
			// how that was checked; the branch does not fire on its own.
			if signal.Ignored(tc.sig) {
				t.Logf("signal.Ignored now agrees with the kernel after a Reset for %v, so the disagreement this test also documents is gone; the re-arm below is still the assertion", tc.sig)
			}

			// What the Reset costs is what a spawn hands on, and that is the child's mask and
			// not this process's -- read before the fix runs, because the fix is what removes it.
			if !childIgnores(t, tc.sig) {
				t.Errorf("signal.Reset left SIG_IGN for %v in this process and a child forked from here does not inherit it, so the state below is one that costs a window's shell nothing and the comment on dontPassOnIgnoredSignals says otherwise", tc.sig)
			}

			dontPassOnIgnoredSignals()
			if kernelIgnoresNow(t, tc.sig) {
				t.Errorf("after a host's signal.Reset the kernel ignores %v again and the next spawn did not clear it, so the fix is disabled for the life of the process and every window from here on inherits it", tc.sig)
			}
		})
	}
}

// childIgnores reports whether a child forked from this process inherits SIG_IGN for sig.
//
// This process's own mask cannot answer that, and the difference is the whole of what
// signal.Reset does: execve carries the kernel's disposition, while Go's clearSignalHandlers
// runs in the child for every signal handlingSig covers and puts SIG_DFL back. So a parent
// holding SIG_IGN whose child holds SIG_IGN, and a parent holding a handler whose child holds
// SIG_DFL, are two different outcomes of the same Reset -- and it is the child's that a window's
// shell gets. A witness that ran NewWindow instead would be reading the fix's own output.
func childIgnores(t *testing.T, sig syscall.Signal) bool {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "grep SigIgn: /proc/self/status").CombinedOutput()
	if err != nil {
		t.Fatalf("the child that reports its inherited ignored-signal mask failed (%v); its output was:\n%s", err, out)
	}
	return signalIgnoredIn(t, string(out), sig)
}

// TestAHostThatIgnoresASignalAfterItsFirstWindowIsStillCovered pins the property that makes
// this per spawn rather than once.
//
// It is the reason the lock is a sync.Mutex: a sync.Once would also close the race that
// TestTheCheckAndTheNotificationAreOneStep is about, and would silently give this up.
//
// Three cycles and not one, and the third is the one that earns its place. A sync.Once around
// the whole function is already caught by the second window, because the window before it is a
// call with nothing to do and the Once fires on that. What only the third window catches is a
// fix that notifies each signal at most once -- `if _, loaded := notified.LoadOrStore(sig, true);
// loaded { continue }`, which someone might write believing os/signal's registration is
// permanent. That one clears the second window correctly and hands the third one the disposition:
// measured, it passes this test bounded to a single cycle and fails it at window 3.
//
// Both re-arm routes, because
// the re-arm works for a reason about os/signal's reference count rather than about either
// route: signal.Ignore installs SIG_IGN outright, signal.Reset restores the fwdSig that the
// previous window's Notify captured, and both drive the count to zero so the next Notify
// installs a handler again. A regression in one route would not show in the other.
//
// Each cycle asserts the hazard is back before the window that is supposed to clear it. Without
// that, a re-arm route that quietly stopped working would leave every window after the first
// with nothing to do, and a test that only checks the signal is clear afterwards would pass on
// all three.
func TestAHostThatIgnoresASignalAfterItsFirstWindowIsStillCovered(t *testing.T) {
	for _, rearm := range []struct {
		what string
		put  func(os.Signal)
	}{
		{"signal.Ignore", func(sig os.Signal) { signal.Ignore(sig) }},
		{"signal.Reset", func(sig os.Signal) { signal.Reset(sig) }},
	} {
		t.Run(rearm.what, func(t *testing.T) {
			t.Cleanup(dontPassOnIgnoredSignals)
			const sig = syscall.SIGHUP

			// The first window, with nothing to do.
			dontPassOnIgnoredSignals()
			if kernelIgnoresNow(t, sig) {
				t.Fatalf("setup: %v is still ignored after a call with nothing to clear, so the first-window half of this is not in the state it names", sig)
			}

			// The host ignores it afterwards, for its own reasons. The first re-arm is
			// Ignore on both arms, because Reset can only restore a SIG_IGN some earlier
			// Notify captured and there has not been one yet.
			signal.Ignore(sig)

			for window := 2; window <= 4; window++ {
				if !kernelIgnoresNow(t, sig) {
					t.Fatalf("before window %d the kernel does not ignore %v, so %s did not put the hazard back and this cycle would assert nothing", window, sig, rearm.what)
				}
				dontPassOnIgnoredSignals()
				if kernelIgnoresNow(t, sig) {
					t.Errorf("a host that ignored %v after window %d still hands the disposition to window %d's shell, so the fix ran a bounded number of times instead of per spawn", sig, window-1, window)
					return
				}
				rearm.put(sig)
			}
		})
	}
}

// TestIgnoredNowReadsTheBitTheSignalNumberNames covers the arithmetic, because an off-by-one
// in it would clear a different signal than the one asked about and every test above would
// still pass -- they ask about SIGHUP, which is bit 0, the one value an off-by-one leaves
// alone in one direction.
func TestIgnoredNowReadsTheBitTheSignalNumberNames(t *testing.T) {
	for _, tc := range []struct {
		what string
		sig  syscall.Signal
		mask uint64
		want bool
	}{
		{"SIGHUP is bit 0", syscall.SIGHUP, 1 << 0, true},
		{"SIGHUP against the next signal's bit", syscall.SIGHUP, 1 << 1, false},
		{"SIGTSTP is bit 19", syscall.SIGTSTP, 1 << (syscall.SIGTSTP - 1), true},
		{"SIGTSTP against the bit below it", syscall.SIGTSTP, 1 << (syscall.SIGTSTP - 2), false},
		{"SIGTSTP against the bit above it", syscall.SIGTSTP, 1 << syscall.SIGTSTP, false},
		{"an empty mask", syscall.SIGHUP, 0, false},
		{"a full mask", syscall.SIGTTOU, ^uint64(0), true},
		// Out of range in both directions: a mask has 64 bits and signal 0 names none.
		{"signal 0, which names no bit", syscall.Signal(0), ^uint64(0), false},
		{"a signal past the width of the mask", syscall.Signal(65), ^uint64(0), false},
	} {
		if got := ignoredNow(tc.sig, tc.mask, true); got != tc.want {
			t.Errorf("%s: ignoredNow(%d, %#x) = %v, want %v", tc.what, tc.sig, tc.mask, got, tc.want)
		}
	}
}

// TestIgnoredNowFallsBackToTheSignalPackage covers the branch that runs on every platform
// except Linux, where there is no mask to read.
func TestIgnoredNowFallsBackToTheSignalPackage(t *testing.T) {
	t.Cleanup(dontPassOnIgnoredSignals)

	// A mask that says nothing is ignored, so an answer of true can only have come from
	// signal.Ignored.
	signal.Ignore(syscall.SIGHUP)
	if !ignoredNow(syscall.SIGHUP, 0, false) {
		t.Error("with no kernel mask available, ignoredNow did not fall back to signal.Ignored, so the fix does nothing at all on a platform that cannot answer")
	}
	if ignoredNow(syscall.SIGHUP, 1<<0, true) != true {
		t.Error("with a mask available, ignoredNow did not read it")
	}
	dontPassOnIgnoredSignals()
	if ignoredNow(syscall.SIGHUP, 0, false) {
		t.Error("signal.Ignored still reports SIGHUP as ignored after the fix ran, so the fallback cannot tell a cleared signal from an ignored one")
	}
}

// TestEveryLeakingSignalIsAccountedForWithItsReason pins the partition itself, which the
// table-driven test above cannot.
//
// That test asserts the consequences of membership -- a signal in clearedWhenIgnored is gone
// from the window shell, one outside it is still there -- and so it describes whatever the set
// happens to contain. Remove SIGTSTP from the list and the signal simply moves to the other
// branch, is found still ignored in the shell, and the test passes: measured, that mutation
// survived it. So the membership is asserted here, with the reason that puts each signal on its
// side of the line, and the two sides are required to cover leakingSignals exactly.
//
// The coverage half is the part that earns its keep, and it is the second of two nets. The
// subprocess test measures what leaks and fails if leakingSignals is missing a member; this one
// fails if a member of leakingSignals has no decision recorded about it. Together they mean a
// signal that starts surviving execve cannot reach a window shell without somebody having
// written down which side of the line it belongs on and why.
func TestEveryLeakingSignalIsAccountedForWithItsReason(t *testing.T) {
	clearedBecause := map[syscall.Signal]string{
		syscall.SIGHUP:  "a shell that ignores it outlives its daemon: once nothing is left to run Window.Kill, hanging the session up is the only thing that ends the shell",
		syscall.SIGINT:  "Ctrl-C in the window does nothing",
		syscall.SIGTSTP: "a directly exec'd window command cannot be suspended with Ctrl-Z, the argv being client-supplied and not necessarily a shell; through a shell the inherited ignore is undetectable at every hop",
	}
	leftAloneBecause := map[syscall.Signal]string{
		syscall.SIGCONT:    "an ignored SIGCONT still resumes a stopped process, because the kernel resumes whatever the disposition, so there is nothing to fix",
		syscall.SIGTTIN:    "tmux deliberately leaves it ignored in its panes, so passing it on is the behaviour to have",
		syscall.SIGTTOU:    "tmux deliberately leaves it ignored in its panes, so passing it on is the behaviour to have",
		syscall.Signal(32): "glibc's SIGCANCEL, which it uses to cancel its own threads: an interactive shell never sends it with an expectation, so an inherited one costs nothing, and notifying on it would put a Go handler over a signal that is glibc's",
		syscall.Signal(34): "glibc's SIGRTMIN: nothing a shell session does sends it with an expectation, so an inherited one costs nothing, while notifying would take a real-time signal away from the host process for the rest of its life",
	}

	inList := map[syscall.Signal]bool{}
	for _, sig := range clearedWhenIgnored {
		n, ok := sig.(syscall.Signal)
		if !ok {
			t.Errorf("clearedWhenIgnored holds %v, which is not a syscall.Signal, so ignoredNow can never match it against a mask", sig)
			continue
		}
		inList[n] = true
		if _, want := clearedBecause[n]; !want {
			t.Errorf("%v is cleared but no reason here says what its inheritance costs a user; either give one or leave it as it arrives", n)
		}
	}
	for sig, cost := range clearedBecause {
		if !inList[sig] {
			t.Errorf("%v is not in clearedWhenIgnored, so a window shell inherits it ignored, and that costs: %s", sig, cost)
		}
	}
	for sig, why := range leftAloneBecause {
		if inList[sig] {
			t.Errorf("%v is in clearedWhenIgnored, but the reason recorded for leaving it alone is: %s", sig, why)
		}
	}

	// Both sides together have to be the whole measured leak set, in both directions.
	accountedFor := map[syscall.Signal]bool{}
	for sig := range clearedBecause {
		accountedFor[sig] = true
	}
	for sig := range leftAloneBecause {
		accountedFor[sig] = true
	}
	leaks := map[syscall.Signal]bool{}
	for _, sig := range leakingSignals {
		leaks[sig] = true
		if !accountedFor[sig] {
			t.Errorf("%v survives execve as ignored and neither side here says what to do about it, so it reaches window shells with nothing having decided that", sig)
		}
	}
	for sig := range accountedFor {
		if !leaks[sig] {
			t.Errorf("%v has a reason recorded here but is not in leakingSignals, so the reason is about a signal that does not survive execve and one of the two is wrong", sig)
		}
	}
}
