package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the clock-tick unit of /proc/<pid>/stat, fixed at 100 in
// Linux's userspace ABI.
const userHZ = 100

// processInfo reads pid's creation time and executable. alive is false when
// the process does not exist or is a zombie.
func processInfo(pid int) (info procInfo, alive bool, err error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return procInfo{}, false, nil
	}
	if err != nil {
		return procInfo{}, false, err
	}
	open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return procInfo{}, false, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is field 3 (state); starttime is field 22.
	if len(fields) < 20 {
		return procInfo{}, false, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return procInfo{}, false, nil
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procInfo{}, false, fmt.Errorf("parse /proc/%d/stat starttime: %w", pid, err)
	}
	boot, err := bootTime()
	if err != nil {
		return procInfo{}, false, err
	}
	// The parentheses are load-bearing. Multiplying by time.Second first builds a
	// nanosecond product that overflows int64 once ticks passes ~9.22e9 — about 2.92
	// years of uptime at 100 Hz — and boot.Add then lands far in the PAST. Both
	// creation-time guards in sameDaemon compare against that value, so both pass for
	// any pid, leaving a pid-recycle check that is present in the source and absent in
	// effect: a lockfile pid reused by a DIFFERENT shelld reads as verified and gets
	// SIGTERM then SIGKILL. Dividing first is exact here because userHZ divides a
	// second evenly, and it moves the overflow out to ~292 years.
	info.created = boot.Add(time.Duration(ticks) * (time.Second / userHZ))
	if exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		info.image = exe
	} else {
		info.image = string(stat[open+1 : end]) // comm, when exe is unreadable
	}
	return info, true, nil
}

// bootTime is /proc/stat's btime. It is derived from the current wall clock,
// so a clock step since boot moves every creation time computed from it.
func bootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}
