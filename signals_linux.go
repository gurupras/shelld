//go:build linux

package shelld

import (
	"os"
	"strconv"
	"strings"
)

// kernelIgnoredMask is the set of signals this process currently ignores, as the kernel
// holds it, and whether the kernel could be asked at all. Signal n is bit n-1, which is
// the layout /proc uses.
//
// It exists because signal.Ignored does not answer this question. That function answers
// from a word inside the runtime, and the word disagrees with the kernel in both
// directions -- see dontPassOnIgnoredSignals for the two disagreements that matter and
// what each one costs. The kernel's own answer has no such gap, and it is the disposition
// execve copies, so it is the one worth asking.
//
// One read covers every signal, which is why this returns a mask rather than answering
// about one: the file is opened once per spawn, next to a fork and an exec that cost
// orders of magnitude more.
func kernelIgnoredMask() (mask uint64, known bool) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	return ignoredMaskIn(string(raw))
}

// ignoredMaskIn is kernelIgnoredMask over an already-read status dump, so the parsing can
// be tested without having to arrange a process that ignores a particular signal.
//
// A dump with no SigIgn line, or one this cannot parse, comes back unknown rather than
// zero. Zero would read as "nothing is ignored", which is the answer that makes the caller
// skip the fix and hand a window shell the disposition it was supposed to clear.
func ignoredMaskIn(status string) (mask uint64, known bool) {
	for line := range strings.SplitSeq(status, "\n") {
		hex, ok := strings.CutPrefix(line, "SigIgn:")
		if !ok {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			return 0, false
		}
		return bits, true
	}
	return 0, false
}
