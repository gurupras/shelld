//go:build linux

package main

// termSize asks three questions in order -- is stdout a terminal with a real
// size, else is stdin, else give up -- and the untagged tests can only reach the
// third. Reaching the first two needs a descriptor that is a terminal and whose
// size the test decides. A pty master is both: TIOCGWINSZ and TIOCSWINSZ work on
// it without a slave ever being opened, so there is no child process and nothing
// to clean up but a file.
//
// Linux only: darwin's ptmx needs a different sequence to become usable, and the
// same coverage there would be a second implementation of the helper rather than
// a second test.

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// ptyMaster returns a terminal of the given size. Cols 0 means leave the size
// unset, which is how the kernel starts one.
func ptyMaster(t *testing.T, cols, rows uint16) *os.File {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx, so there is no terminal to test against: %v", err)
	}
	f := os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { f.Close() })
	if cols != 0 {
		if err := resizePTY(f, cols, rows); err != nil {
			t.Fatalf("setting the pty size: %v", err)
		}
	}
	return f
}

func resizePTY(f *os.File, cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}

// TestTermSizeUsesStdoutWhenItIsATerminal is the case that matters in practice:
// the frames go to stdout, so stdout's size is the size the daemon must lay out
// for.
func TestTermSizeUsesStdoutWhenItIsATerminal(t *testing.T) {
	out := ptyMaster(t, 120, 40)
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	defer swapStd(out, pipeR)()

	if c, r := termSize(); c != 120 || r != 40 {
		t.Errorf("termSize() = %d x %d, want 120 x 40 from stdout", c, r)
	}
}

// TestTermSizePrefersStdoutWhenBothAreTerminals pins the order rather than just
// the two branches. With one terminal on each descriptor the two branches are
// indistinguishable, and this is the case where the wrong one is a plausible
// mistake: under `shellctl attach >/dev/pts/5` or a wrapper that hands the client
// a control terminal on stdin, the two sizes are different and only stdout's is
// the one the frames have to fit.
func TestTermSizePrefersStdoutWhenBothAreTerminals(t *testing.T) {
	out, in := ptyMaster(t, 120, 40), ptyMaster(t, 100, 30)
	defer swapStd(out, in)()

	if c, r := termSize(); c != 120 || r != 40 {
		t.Errorf("termSize() = %d x %d with 120 x 40 on stdout and 100 x 30 on stdin, want stdout's", c, r)
	}
}

// TestTermSizeFallsBackToStdinWhenStdoutIsNotATerminal pins the second question.
// It is not a redundant one: `shellctl attach | tee log` and `shellctl attach
// >frames` both leave stdin as the only descriptor that knows how wide the
// window is, and 80 x 24 for a session being driven from a 200-column terminal
// is a wrong answer the daemon cannot tell from a right one.
func TestTermSizeFallsBackToStdinWhenStdoutIsNotATerminal(t *testing.T) {
	in := ptyMaster(t, 100, 30)
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	defer swapStd(pipeW, in)()

	if c, r := termSize(); c != 100 || r != 30 {
		t.Errorf("termSize() = %d x %d, want 100 x 30 from stdin", c, r)
	}
}

// TestTermSizeIgnoresATerminalThatReportsNoSize pins why termSize checks c > 0
// and not just the error. A terminal whose size has never been set answers
// successfully with 0 x 0 -- that is the state every pty starts in, and it is
// what a terminal emulator that has not finished starting can report. Trusting
// it would make attach's first frame ask the daemon for a zero-column window.
func TestTermSizeIgnoresATerminalThatReportsNoSize(t *testing.T) {
	out := ptyMaster(t, 0, 0) // never sized
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	defer swapStd(out, pipeR)()

	// Precondition: the kernel really does answer this way, or the test would be
	// pinning the guard against a case that cannot happen.
	if ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ); err != nil || ws.Col != 0 {
		t.Skipf("an unsized pty master reports %+v, err %v: nothing here to guard against", ws, err)
	}
	if c, r := termSize(); c != 80 || r != 24 {
		t.Errorf("termSize() = %d x %d from a terminal reporting 0 x 0, want the 80 x 24 fallback", c, r)
	}
}
