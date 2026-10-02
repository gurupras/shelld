//go:build unix && !darwin

package proctree

// The stubs for every unix that is not darwin. Linux compiles these and never
// calls them — it has /proc, and both ids come out of the single
// /proc/<pid>/stat read the parent snapshot already does, so routing it through
// here would cost a second read per process for nothing.
//
// What is left is the other BSDs and the like, and they keep the behaviour they
// had: no start time, so KillTree refuses every root and reapChildTree falls back
// to killing the direct child; no session id, so Descendants is the parent closure
// alone. Both are the safe direction — under-reach, never a wider signal — and
// neither is guessed at. kinfo_proc is the obvious source for a FreeBSD
// implementation, but its layout is not darwin's and none of it has been run here,
// and a wrong session id does not produce a wrong report, it produces a list of
// pids to SIGKILL.

func platformStart(int) (uint64, bool) { return 0, false }

// platformStartSupported is StartTimeSupported's half of the same split. False
// here covers Linux too, which compiles this file and answers the question in
// StartTimeSupported's own runtime.GOOS test instead, the way StartTicks does.
const platformStartSupported = false

// False rather than an empty map, so idMap takes the `ps` path instead of running
// the oracle over a snapshot with no processes in it.
func platformIDs() (map[int]procIDs, bool) { return nil, false }
