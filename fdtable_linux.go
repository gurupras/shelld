package shelld

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// descriptorTableSlots is the size InitDescriptorTable expands to. Each window
// holds a pty master for its whole life and each attached client a connection, so
// a daemon in ordinary use walks up through 64, 128, 256 and beyond; 4096 is one
// allocation past every boundary a session is likely to cross, and costs 32 KiB
// of kernel memory. It is not a limit on anything: crossing 4096 later behaves
// exactly as crossing 64 does today.
const descriptorTableSlots = 4096

// InitDescriptorTable expands this process's descriptor table once, up front, so
// that no later window create pays for the expansion.
//
// Linux grows the table to the next power of two when a descriptor lands past its
// end, and expand_fdtable waits a full RCU grace period whenever the files_struct
// is shared. A Go program's always is, because every thread of the runtime shares
// it, so the wait is not avoidable per-call: measured on this kernel it is 12-25
// ms, and it lands on whichever syscall happened to ask for the crossing
// descriptor. In a create that is the pipe os/exec opens to report exec failures,
// which is why one create in twenty-one took 20 ms against a median of 0.3 ms
// while every other phase of the same create stayed in microseconds. A
// single-threaded server crossing the same boundaries pays 7 microseconds.
//
// The table never shrinks, so one expansion covers the process's whole life.
//
// Like InitCgroups, this is called by a program and not by the library, because
// resizing a process-wide table is a decision for a program; and like it, every
// failure is a DECLINE and not a fatal error. Declining costs the occasional
// 20 ms create and nothing else, so it must not be fatal — but it must not be
// silent either, so the reason is logged.
func InitDescriptorTable() bool {
	n, clamped, err := descriptorTableTarget()
	if err == nil {
		err = reserveDescriptorTable(n)
	}
	if err != nil {
		slog.Info("descriptor table not expanded up front; an occasional window create will pay an RCU grace period",
			"err", err)
		return false
	}
	if clamped {
		slog.Info("descriptor table expanded to the soft descriptor limit, which is below the full reservation",
			"slots", n, "wanted", descriptorTableSlots)
	}
	return true
}

// descriptorTableTarget is how many slots to ask for: descriptorTableSlots where
// the soft RLIMIT_NOFILE has room for that many, and the limit itself where it
// does not.
//
// Clamping here rather than declining is the whole of this function's reason to
// exist. reserveDescriptorTable is strict on purpose — a size above the soft limit
// has no legal top slot to park in, so it refuses — and the systemd default soft
// limit for a service is 1024, well below descriptorTableSlots. Asking for the
// full reservation there gets a refusal and leaves the table at NR_OPEN_DEFAULT,
// so the daemon pays the grace period at 64, 128, 256 and 512: every boundary this
// change was measured against, and the pipe2 crossing the commit names among them.
// Clamped to 1024 it pays none of them.
//
// Nothing is given up by not reserving past the limit. The kernel will not hand
// out a descriptor at or above RLIMIT_NOFILE, so once the table covers
// min(descriptorTableSlots, soft limit) there is no reachable descriptor left that
// could force a further expansion — a raise of the soft limit afterwards can, and
// that is the same position a process is in today above 4096.
func descriptorTableTarget() (n int, clamped bool, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false, fmt.Errorf("reading RLIMIT_NOFILE: %w", err)
	}
	if lim.Cur < uint64(descriptorTableSlots) {
		return int(lim.Cur), true, nil // in range: the branch bounds it below 4096
	}
	return descriptorTableSlots, false, nil
}

// reserveDescriptorTable makes this process's descriptor table hold at least n
// descriptors, by parking one in the top slot and closing it again.
//
// dup3 onto an occupied slot closes what is there, silently, so the slot has to
// be known free. It is: the kernel hands out the lowest free descriptor, so while
// the process holds far fewer than n descriptors no concurrent open can be given
// slot n-1, and nothing in shelld asks for a descriptor by number. The F_GETFD
// probe checks that rather than assume it, and refuses instead of closing a live
// descriptor out from under its owner.
func reserveDescriptorTable(n int) error {
	if n < 1 {
		return fmt.Errorf("reserve %d descriptors: no slot to park in", n)
	}
	slot := n - 1
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return fmt.Errorf("reserve %d descriptors: reading RLIMIT_NOFILE: %w", n, err)
	}
	// dup3 rejects a descriptor at or above the soft limit, so report the limit
	// rather than an opaque EBADF from the dup.
	if uint64(n) > lim.Cur {
		return fmt.Errorf("reserve %d descriptors: RLIMIT_NOFILE is %d", n, lim.Cur)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(slot), syscall.F_GETFD, 0); errno != syscall.EBADF {
		return fmt.Errorf("reserve %d descriptors: slot %d is in use (fcntl: %v)", n, slot, errno)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("reserve %d descriptors: %w", n, err)
	}
	defer f.Close()
	// O_CLOEXEC because the slot is briefly a real descriptor: a create racing this
	// call would otherwise hand slot 4095 to somebody's shell. Nothing forks this
	// early, and the flag means the code does not depend on that staying true.
	if err := syscall.Dup3(int(f.Fd()), slot, syscall.O_CLOEXEC); err != nil {
		return fmt.Errorf("reserve %d descriptors: dup3 to slot %d: %w", n, slot, err)
	}
	// The expansion, not the descriptor, is what was wanted: the table keeps its
	// new size after the slot is closed.
	if err := syscall.Close(slot); err != nil {
		return fmt.Errorf("reserve %d descriptors: closing slot %d: %w", n, slot, err)
	}
	return nil
}
