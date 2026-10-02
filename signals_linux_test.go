//go:build linux

package shelld

import "testing"

// TestIgnoredMaskInRefusesADumpItCannotRead pins the one answer this must never give.
//
// A dump it cannot read has to come back unknown, not zero. Zero is a valid mask meaning
// "nothing is ignored", and dontPassOnIgnoredSignals reads that as having nothing to do --
// so a parse failure reported as zero would skip the fix and hand the window shell exactly
// the disposition the call exists to clear, with nothing anywhere saying why.
func TestIgnoredMaskInRefusesADumpItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		what   string
		status string
		mask   uint64
		known  bool
	}{
		{"a mask in a full dump", "Name:\tsh\nSigIgn:\t0000000000001000\nSigCgt:\t0000000180000000\n", 0x1000, true},
		{"SIGHUP, which is bit 0", "SigIgn:\t0000000000000001\n", 1, true},
		{"nothing ignored, which is a real answer and not a failure", "SigIgn:\t0000000000000000\n", 0, true},
		{"a dump with no trailing newline", "SigIgn:\t0000000000000002", 2, true},
		// SigCgt is the caught set and is a different question; matching it would report
		// every handled signal as ignored.
		{"a dump with no SigIgn line", "Name:\tsh\nSigCgt:\t0000000180000000\n", 0, false},
		{"an empty dump", "", 0, false},
		{"a mask that is not hexadecimal", "SigIgn:\tnot-a-mask\n", 0, false},
		{"an empty mask", "SigIgn:\t\n", 0, false},
		{"a mask too wide for 64 bits", "SigIgn:\t10000000000000000\n", 0, false},
	} {
		mask, known := ignoredMaskIn(tc.status)
		if known != tc.known {
			t.Errorf("%s: ignoredMaskIn reported known = %v, want %v", tc.what, known, tc.known)
			continue
		}
		if known && mask != tc.mask {
			t.Errorf("%s: ignoredMaskIn = %#x, want %#x", tc.what, mask, tc.mask)
		}
	}
}

// TestKernelIgnoredMaskCanBeReadHere is the premise every test that asks the kernel rests on,
// asserted rather than skipped past: on Linux the answer is available, and a change that made
// it unavailable would otherwise turn those tests into silent skips.
func TestKernelIgnoredMaskCanBeReadHere(t *testing.T) {
	if _, known := kernelIgnoredMask(); !known {
		t.Error("kernelIgnoredMask cannot answer on Linux, so the fix is running on its fallback here and SIGTSTP is not being cleared")
	}
}
