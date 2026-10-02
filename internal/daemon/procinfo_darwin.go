package daemon

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// processInfo reads pid's creation time and command name (at most 16
// bytes). alive is false when the process does not exist or is a zombie.
func processInfo(pid int) (info procInfo, alive bool, err error) {
	// SysctlRaw rather than SysctlKinfoProc, because absence has to be told apart
	// from failure and SysctlKinfoProc cannot express it. A pid that is gone --
	// reaped, or never existing at all -- makes this sysctl *succeed* and return
	// zero bytes; it does not report ESRCH. SysctlKinfoProc turns that short read
	// into EIO, indistinguishable from a real error, so every caller that has to
	// separate "not there" from "cannot tell" got "cannot tell" for every dead
	// process, and the ESRCH branch below never ran. Measured on macOS 26.4 arm64:
	// alive and zombie both yield 648 bytes (= SizeofKinfoProc) with a nil error,
	// reaped and above-pid_max both yield 0 bytes with a nil error.
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		// Not the path darwin takes, but absence is absence however it is spelled.
		if errors.Is(err, unix.ESRCH) {
			return procInfo{}, false, nil
		}
		return procInfo{}, false, err
	}
	if len(raw) == 0 {
		return procInfo{}, false, nil
	}
	var kp unix.KinfoProc
	if len(raw) < int(unsafe.Sizeof(kp)) {
		return procInfo{}, false, fmt.Errorf("kern.proc.pid %d: %d bytes, want %d", pid, len(raw), unsafe.Sizeof(kp))
	}
	// Copy rather than cast the slice, so the struct is read at its own alignment
	// instead of wherever the byte slice happened to land. The layout is the
	// kernel's, which is what unix.KinfoProc is generated from.
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&kp)), unsafe.Sizeof(kp)), raw)

	if kp.Proc.P_pid != int32(pid) {
		return procInfo{}, false, nil
	}
	const szomb = 5 // SZOMB in sys/proc.h
	if kp.Proc.P_stat == szomb {
		return procInfo{}, false, nil
	}
	st := kp.Proc.P_starttime
	info.created = time.Unix(int64(st.Sec), int64(st.Usec)*1000)
	comm := kp.Proc.P_comm[:]
	for i, c := range comm {
		if c == 0 {
			comm = comm[:i]
			break
		}
	}
	info.image = string(comm)
	return info, true, nil
}
