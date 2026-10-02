//go:build shelld_trace && linux

package tracing

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func threadID() uint32 { return uint32(unix.Gettid()) }

func cpuID() uint16 {
	var cpu uint32
	_, _, _ = unix.RawSyscall(unix.SYS_GETCPU, uintptr(unsafe.Pointer(&cpu)), 0, 0)
	return uint16(cpu)
}
