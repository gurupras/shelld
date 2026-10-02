//go:build linux

package shelld

import (
	"os/signal"
	"syscall"
	"testing"
	"unsafe"
)

// kernelSigaction is the kernel's struct sigaction, which is not libc's. The field order is
// handler, flags, restorer, mask, and sa_mask is the kernel's 64-bit sigset_t rather than
// glibc's 1024-bit one -- which is why rt_sigaction takes its size as an argument and why this
// cannot be syscall.Sigaction (there is no such type for Linux in the standard library).
type kernelSigaction struct {
	handler  uintptr
	flags    uintptr
	restorer uintptr
	mask     uint64
}

// kernelIgnoredNow is kernelIgnoredMask for one signal, read with the rt_sigaction syscall
// instead of by parsing /proc/self/status. It exists only for tests, and only because of how
// much faster it is.
//
// The thing being detected is a window a few microseconds wide between signal.Notify clearing
// the runtime's ignored bit and setsig replacing SIG_IGN in the kernel. A detector inside that
// window is racing it, so the detector's own cost comes straight off the measurement: reading
// /proc/self/status means an open, a read and a parse of the whole dump, 24-61us here, and by
// the time it answers the window has usually closed. One rt_sigaction is about 1us.
//
// Measured on the single-signal arm of TestTheCheckAndTheNotificationAreOneStep with the lock
// removed, same tree and same mutant, with both probes read back to back in the same iteration so
// the comparison is paired: this reports 99-111 leaks of 640 and a /proc/self/status parse 13-24,
// with 86-89 caught by this alone and none at all caught by the parse alone. The difference is not
// a difference in the defect, it is the probe getting out of the way, and the containment in one
// direction and not the other is what says so -- two separate runs, which is what this comment
// used to quote, cannot tell a more sensitive probe from a differently-aimed one. With the lock
// restored both read 0 of 640, which is the control that says the faster probe is not inventing
// leaks.
//
// A failed syscall comes back unknown rather than false. False would read as "the kernel is not
// ignoring it", which is the answer that says there was no leak.
func kernelIgnoredNow(sig syscall.Signal) (ignored, known bool) {
	var old kernelSigaction
	_, _, errno := syscall.Syscall6(syscall.SYS_RT_SIGACTION,
		uintptr(sig), 0, uintptr(unsafe.Pointer(&old)), sigsetsize, 0, 0)
	if errno != 0 {
		return false, false
	}
	return old.handler == sigIgn, true
}

// rt_sigaction's fourth argument is sigsetsize, and the kernel rejects anything but its own
// sigset_t size with EINVAL. That is 8 on the Linux architectures whose _NSIG is 65, which is
// every one Go supports except mips, mipsle, mips64 and mips64le: those have _NSIG 129 and a
// 16-byte sigset_t. Nothing here is built for them today, and the two callers say what they do
// when the syscall refuses rather than assuming it cannot.
const sigsetsize = 8

// The two handler values that are not addresses.
const (
	sigDfl = 0
	sigIgn = 1
)

// rawIgnore installs SIG_IGN for sig with the rt_sigaction syscall and returns what was there,
// so a caller can put it back.
//
// Raw rather than signal.Ignore, because setting the kernel's disposition without telling
// os/signal is the whole point: signal.Ignore drives the runtime's bookkeeping as well, and the
// bookkeeping is the variable the test below holds still.
func rawIgnore(t *testing.T, sig syscall.Signal) kernelSigaction {
	t.Helper()
	var old kernelSigaction
	sa := kernelSigaction{handler: sigIgn}
	if _, _, errno := syscall.Syscall6(syscall.SYS_RT_SIGACTION,
		uintptr(sig), uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&old)), sigsetsize, 0, 0); errno != 0 {
		if errno == syscall.EINVAL {
			// The size argument is the one thing here that is not portable across
			// Linux ports, and a host that rejects it cannot be put in the condition
			// at all. That is a missing instrument, not a failing subject.
			t.Skipf("rt_sigaction rejected a sigsetsize of %d for signal %d (%v), so the disposition this needs cannot be installed here", sigsetsize, int(sig), errno)
		}
		t.Fatalf("rt_sigaction could not ignore signal %d (%v), and installing the disposition is the fixture", int(sig), errno)
	}
	return old
}

// rawRestore puts a disposition back exactly as it was read, flags and restorer included -- which
// is why it takes the struct rt_sigaction handed out rather than reinstalling SIG_DFL.
func rawRestore(t *testing.T, sig syscall.Signal, sa kernelSigaction) {
	t.Helper()
	if _, _, errno := syscall.Syscall6(syscall.SYS_RT_SIGACTION,
		uintptr(sig), uintptr(unsafe.Pointer(&sa)), 0, sigsetsize, 0, 0); errno != 0 {
		t.Errorf("rt_sigaction could not put signal %d's disposition back (%v), so a later test in this binary inherits this one's fixture", int(sig), errno)
	}
}

// TestAChildFollowsHandlingSigAndNotTheDisposition pins the claim dontPassOnIgnoredSignals'
// own comment rests on: what a forked child holds is decided by the runtime's handlingSig for
// the signal, and not by the disposition its parent holds at the moment of the fork. The fix
// works by asking to be notified, which sets handlingSig -- if the disposition decided, asking
// to be notified would not be a lever at all.
//
// One signal, one disposition, two children. Each arm puts the kernel in the same state --
// SIG_IGN, asserted in both -- and differs only in whether os/signal is handling the signal,
// and the children come out opposite. Holding the signal constant is what makes that a
// measurement rather than a comparison between two signals that might differ for some other
// reason.
//
// Both handlingSig states are established here rather than inherited, which matters because
// this binary runs every other test in the package first and several of them leave handlingSig
// set. signal.Ignore reaches runtime.sigignore, which stores 0 unconditionally and installs
// SIG_IGN; signal.Notify reaches sigenable, which CASes it to 1 and installs a handler. So
// neither arm depends on what ran before it.
//
// The second arm asserts that the handler was really there, by the value its raw rt_sigaction
// displaced. Without that, a Notify that quietly stopped installing anything would leave the
// arm asserting the first arm's setup twice and agreeing with it.
func TestAChildFollowsHandlingSigAndNotTheDisposition(t *testing.T) {
	// SIGHUP and SIGTSTP because the runtime treats them differently at startup -- it installs
	// a handler for SIGHUP and declines SIGTSTP, which is _SigDefault -- and the property has
	// to hold either way. Both are signals this package clears.
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTSTP} {
		t.Run(sig.String(), func(t *testing.T) {
			// Leaves the package in the state every other test here wants: a handler and
			// an empty ignored set.
			t.Cleanup(dontPassOnIgnoredSignals)

			// Nothing handling it. signal.Ignore is also exactly how a host arrives at
			// this state, so this arm is the hazard itself and not a model of it.
			signal.Ignore(sig)
			if ig, known := kernelIgnoredNow(sig); !known || !ig {
				t.Fatalf("setup: the kernel does not ignore %v after signal.Ignore (known=%v), so this arm is not about a parent that ignores it", sig, known)
			}
			if !childIgnores(t, sig) {
				t.Fatalf("this process ignores %v and a child forked from it does not inherit that, so there is no disposition here for the fix to be about", sig)
			}

			// The same disposition, with os/signal handling the signal underneath it.
			signal.Notify(sigSink, sig)
			t.Cleanup(func() { signal.Reset(sig) })
			old := rawIgnore(t, sig)
			t.Cleanup(func() { rawRestore(t, sig, old) })
			// A handler's address and not handlingSig itself, which is not readable from
			// here. The proxy is exact only for a signal no C library handles: under cgo,
			// glibc's own handlers for 32 and 33 would satisfy it with handlingSig at 0.
			// Neither signal here is one of those, and each arm sets handlingSig itself
			// rather than inferring it from this, so what this catches is narrower and
			// worth catching -- a Notify that installed nothing, which would make this arm
			// a copy of the one above.
			if old.handler == sigDfl || old.handler == sigIgn {
				t.Fatalf("setup: signal.Notify left %#x installed for %v rather than a handler, so it installed nothing and this arm is a copy of the one above", old.handler, sig)
			}
			if ig, known := kernelIgnoredNow(sig); !known || !ig {
				t.Fatalf("setup: the kernel does not ignore %v after a raw rt_sigaction installed SIG_IGN (known=%v), so the two arms no longer share a disposition", sig, known)
			}

			if childIgnores(t, sig) {
				t.Errorf("a child inherits SIG_IGN for %v from a parent that has os/signal handling it, exactly as it did from the parent above that did not -- so the disposition decided and not handlingSig, and asking to be notified is not what keeps a window shell interruptible", sig)
			}
		})
	}
}
