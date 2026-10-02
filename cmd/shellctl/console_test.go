package main

// console_windows.go and winch_windows.go had no test on any platform, and
// neither did the one-line unix stub each is paired with. The contract is split
// across four files by what it takes to observe it:
//
//   - here: what holds on every platform, with neither stdout nor stdin a
//     terminal -- which is the shape `go test` itself runs in, and also the
//     shape `shellctl attach >file` runs in;
//   - console_linux_test.go: what needs a terminal on a descriptor a test can
//     own, driven through /dev/ptmx;
//   - winch_unix_test.go: what needs a SIGWINCH;
//   - console_windows_test.go and winch_windows_test.go: what needs a real
//     console, driven through CONOUT$. Those two have never been executed.
//
// enableVTOutput and termSize both read os.Stdout directly, so the variable is
// the only seam a test has. Nothing here changes product code to widen it.

import (
	"os"
	"testing"
)

// withoutTerminals points os.Stdout and os.Stdin at a pipe for the duration of
// the test. The code under test asks whether those two are terminals, and the
// answer would otherwise depend on how the binary was started: `go test` hands
// it pipes, but the same binary run straight from a shell has a terminal on
// both, and the size of that terminal is whoever's window it is.
func withoutTerminals(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	restore := swapStd(w, r)
	t.Cleanup(func() {
		restore()
		w.Close()
		r.Close()
	})
}

// swapStd installs stdout and stdin, returning a function that puts back what
// was there. Either argument may be nil to leave that one alone.
func swapStd(out, in *os.File) (restore func()) {
	outWas, inWas := os.Stdout, os.Stdin
	if out != nil {
		os.Stdout = out
	}
	if in != nil {
		os.Stdin = in
	}
	return func() { os.Stdout, os.Stdin = outWas, inWas }
}

// TestEnableVTOutputAlwaysReturnsSomethingWorthDeferring pins the one thing
// attach needs from it unconditionally. The call site is
//
//	defer enableVTOutput()()
//
// so a nil return is not a no-op: it panics, and it panics on the way out of
// attach, after the connection is already up and the terminal is already in raw
// mode. Every early return in the Windows implementation -- stdout is not a
// console, the console refuses both mode requests -- has to hand back a
// callable function, and so does the unix stub.
func TestEnableVTOutputAlwaysReturnsSomethingWorthDeferring(t *testing.T) {
	withoutTerminals(t)
	restore := enableVTOutput()
	if restore == nil {
		t.Fatal("enableVTOutput returned nil: `defer enableVTOutput()()` in attach would panic after the session is already attached")
	}
	restore() // must not panic either; on unix and on a non-console stdout it does nothing
}

// TestTermSizeFallsBackToAUsableSizeRatherThanZero pins the last resort. The
// first thing attach sends is resizeFrame(termSize()), and the daemon acts on
// it: a zero-column window is not a smaller window, it is a window nothing can
// be laid out in. So when neither descriptor is a terminal -- redirected output,
// a cron job, a CI runner -- the answer still has to be a size somebody can use.
//
// Which size that is belongs next to the platform, because the platforms have a
// different number of places to look. On unix a terminal IS a descriptor, so once
// stdout and stdin are pipes there is nothing left to ask and the answer is the
// 80 x 24 default (console_unix_test.go). On Windows the process can still be
// attached to a console that neither descriptor points at, and termSize asks it
// through CONOUT$ (console_windows_test.go).
func TestTermSizeFallsBackToAUsableSizeRatherThanZero(t *testing.T) {
	withoutTerminals(t)
	c, r := termSize()
	if c <= 0 || r <= 0 {
		t.Errorf("termSize() = %d x %d, want a size a session can be laid out in: attach sends this as its first resize frame", c, r)
	}
}
