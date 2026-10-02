//go:build unix && !linux

package shelld

import "syscall"

// kernelIgnoredNow cannot be answered here, for the same reason kernelIgnoredMask cannot:
// rt_sigaction is Linux's, and in this module's supported set that leaves darwin.
//
// Unknown rather than false. The only caller is TestTheCheckAndTheNotificationAreOneStep, which
// skips when the kernel's disposition cannot be read at all -- a detector that answered false
// would instead report every rep as clean and pass.
func kernelIgnoredNow(syscall.Signal) (ignored, known bool) { return false, false }
