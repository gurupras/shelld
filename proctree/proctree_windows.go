//go:build windows

package proctree

// KillTree is unsupported on Windows and reports false: parent pids there are not
// updated when a parent exits, so a recycled pid could pull an unrelated process
// into the tree.
//
// Reporting false is the whole contract, and unlike darwin's old behaviour it is
// not a hidden no-op: false sends reapChildTree to pty.PTY.Kill, which on Windows
// calls TerminateJobObject on the kill-on-close job every window's child is
// assigned to while still suspended. So the tree still dies, by a mechanism that
// does not need parent links at all -- and one a descendant cannot escape, since
// neither breakaway limit is set. An earlier version of this comment said callers
// "fall back to killing the direct child" and named a Job Object at spawn time as
// the durable fix still to come; the job has been there since pty_windows.go was
// written, and the fallback is a tree kill rather than a single process. See
// newKillOnCloseJob.
//
// What is genuinely missing here is the ENUMERATION, not the kill: nothing on this
// platform can answer which pids the tree contains, so Descendants is [root] and
// callers get no list to log or assert against.
func KillTree(root int, start uint64) ([]int, bool) { return nil, false }

// StartTicks has no Windows equivalent that KillTree could use, and KillTree
// reports false before it is asked, so it reports false too rather than inventing
// a value a caller might treat as an identity.
func StartTicks(pid int) (uint64, bool) { return 0, false }

// StartTimeSupported is false here for the whole build, which is what keeps the
// spawn site from logging a failed read once per window on a platform that was
// never going to answer one. Nothing is lost by it: the kill on this platform does
// not use a start time at all, it uses the job object KillTree's doc describes.
func StartTimeSupported() bool { return false }

func parentMap() map[int]int { return map[int]int{} }

// idMap is empty for the same reason parentMap is: with no trustworthy parent
// links there is nothing for the union to close over, and Windows has no
// sessions or process groups in the POSIX sense to substitute. Descendants
// therefore answers [root] here, and KillTree reports false before it is asked.
func idMap() map[int]procIDs { return map[int]procIDs{} }

// fastDescendants has no Windows equivalent: the children links it walks come
// from /proc.
func fastDescendants(int) ([]int, bool) { return nil, false }
