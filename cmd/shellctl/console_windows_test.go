//go:build windows

package main

// Written on a Linux machine, vetted for windows/amd64 and windows/arm64 there,
// and executed on Windows on 2026-09-25 -- twice, before and after the CONOUT$
// fallback existed. Both runs were on windows/amd64 on a real console, output
// redirected to a file, which is the shape the fallback is for. Anything changed
// here is back to untested until someone runs it again.
//
// What those runs establish, beyond "it passes":
//
//   - The console was 120 x 30, so the size the fallback reports is not the
//     80 x 24 default, and TestTermSizeUsesTheConsoleWhenNeitherDescriptorIsOne
//     can tell them apart. With consoleSize stubbed out to return false it fails
//     with exactly its own message, "termSize() = 80 x 24 with the console at
//     120 x 30".
//   - That console's screen buffer was taller than its window, since
//     TestConsoleSizeReportsTheWindowNotTheBuffer did not log the admission that
//     it could not discriminate. Reading info.Size there would have failed.
//   - All eight tests the windows build of this package has passed, none skipped.
//
// It needs a console attached to run at all: a test binary launched by a service
// or a build agent with no console skips, because CONOUT$ cannot be opened.
// Running it under a plain `go test` in a terminal is what exercises it. While it
// runs it changes that terminal's output mode and puts it back.
//
// NO WINDOWS COUNTERPART TO console_linux_test.go's STDIN FALLBACK, because on
// Windows there is no such fallback to reach. termSize asks term.GetSize, which on
// Windows is GetConsoleScreenBufferInfo, and that answers only for a console
// OUTPUT handle. The handle on stdin is a console INPUT handle, which it rejects
// -- so the second branch of termSize is dead on Windows whatever the caller
// does, not merely unhelpful when stdout is redirected. The only handle that
// could satisfy it is a console output handle on descriptor 0, which nothing
// arranges.
//
// Measured on both OSes with one probe mirroring termSize's chain, 2026-09-25:
//
//	Linux,   stdout -> file      stdout ENOTTY          stdin 137x41 ok    => 137x41
//	Windows, stdout = console    stdout 120x30 ok       stdin invalid      => 120x30
//	Windows, stdout -> file      stdout invalid         stdin invalid      => 80x24
//
// The middle row is what settles it: the stdin branch fails there too, with
// everything else working. Note also that term.IsTerminal(stdin) returns true on
// Windows for the same handle GetSize rejects, so a check written in terms of
// IsTerminal disagrees with what GetSize will do -- which is why the fix below is
// not an IsTerminal guard. A guard would reproduce the third row exactly.
//
// The third row WAS the defect: `shellctl attach >frames` laid every window of
// the session out for 80 x 24 on Windows while Linux used the real terminal.
// termSize now asks a third question that only Windows can answer -- consoleSize,
// which opens CONOUT$ -- so a redirected stdout reports the console like the
// second row. The two termSize/consoleSize tests below are the only place that is
// pinned, on any platform.

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// consoleForTest opens this process's console output buffer, makes it os.Stdout
// -- the only descriptor the code under test looks at -- and restores both the
// variable and the console's mode afterwards.
func consoleForTest(t *testing.T) (h windows.Handle, mode uint32) {
	t.Helper()
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		t.Fatal(err)
	}
	h, err = windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Skipf("no console output buffer to test against (CONOUT$: %v)", err)
	}
	f := os.NewFile(uintptr(h), "CONOUT$")
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		f.Close()
		t.Skipf("CONOUT$ here is not a console: %v", err)
	}
	restoreStd := swapStd(f, nil)
	t.Cleanup(func() {
		restoreStd()
		_ = windows.SetConsoleMode(h, mode) // before the handle is closed
		f.Close()
	})
	return h, mode
}

// TestEnableVTOutputAddsVTAndPutsTheModeBack pins the three things attach needs
// from the console mode, all of which are invisible until a user is looking at a
// terminal that got them wrong.
func TestEnableVTOutputAddsVTAndPutsTheModeBack(t *testing.T) {
	h, _ := consoleForTest(t)

	// A deliberate starting mode: VT off, so there is something to turn on, and
	// one unrelated flag on, so there is something to preserve.
	// ENABLE_WRAP_AT_EOL_OUTPUT is the right choice for that second job -- a
	// console without it truncates every line that reaches the last column
	// instead of wrapping, which is every full-width row of a frame.
	const baseline = windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_WRAP_AT_EOL_OUTPUT
	if err := windows.SetConsoleMode(h, baseline); err != nil {
		t.Skipf("this console will not take the baseline mode %#x: %v", uint32(baseline), err)
	}

	restore := enableVTOutput()
	if restore == nil {
		t.Fatal("enableVTOutput returned nil on a console")
	}

	var got uint32
	if err := windows.GetConsoleMode(h, &got); err != nil {
		t.Fatal(err)
	}
	if got&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Errorf("mode %#x: VT processing is off, so the classic console host prints the frame's escape sequences literally", got)
	}
	if got&windows.ENABLE_WRAP_AT_EOL_OUTPUT == 0 {
		t.Errorf("mode %#x: the flags the console already had were replaced instead of added to", got)
	}
	if got&windows.DISABLE_NEWLINE_AUTO_RETURN == 0 {
		// Allowed: that is the documented fallback for a build that rejects the
		// flag, and the check above is what must still hold when it happens.
		t.Logf("mode %#x: this console rejected DISABLE_NEWLINE_AUTO_RETURN, so the fallback ran; rows ending in the last column will scroll twice", got)
	}

	restore()
	var after uint32
	if err := windows.GetConsoleMode(h, &after); err != nil {
		t.Fatal(err)
	}
	if after != baseline {
		t.Errorf("mode after restore = %#x, want the %#x it started as: restore has to put back the mode it found rather than clear the flags it set, because shellctl exits back into the shell that ran it", after, baseline)
	}
}

// TestTermSizeUsesTheConsoleWhenNeitherDescriptorIsOne is the regression test for
// the third measured row above. It is the Windows answer to
// console_linux_test.go's stdin fallback: same intent, a different question,
// because the one Linux asks cannot be answered here.
func TestTermSizeUsesTheConsoleWhenNeitherDescriptorIsOne(t *testing.T) {
	// Through a handle of the test's own, so the expectation does not come from
	// the code under test. consoleForTest also makes CONOUT$ os.Stdout, which the
	// next line takes away again -- cleanups run in reverse, so both are undone.
	h, _ := consoleForTest(t)
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
		t.Fatal(err)
	}
	wantC := int(info.Window.Right - info.Window.Left + 1)
	wantR := int(info.Window.Bottom - info.Window.Top + 1)

	// What `shellctl attach >frames` does to stdout, and what a piped stdin does
	// to the descriptor GetSize cannot answer for anyway.
	withoutTerminals(t)

	c, r := termSize()
	if c == 80 && r == 24 && (wantC != 80 || wantR != 24) {
		t.Fatalf("termSize() = 80 x 24 with the console at %d x %d: the fallback did not fire, so a redirected attach lays the session out for a size nobody is looking at", wantC, wantR)
	}
	if c != wantC || r != wantR {
		t.Errorf("termSize() = %d x %d, want the console's %d x %d", c, r, wantC, wantR)
	}
}

// TestConsoleSizeReportsTheWindowNotTheBuffer pins the one piece of arithmetic in
// consoleSize. A console's screen buffer is normally far taller than its visible
// window -- that extra height IS the scrollback -- so reading dwSize instead of
// srWindow would ask the daemon to lay a session out for hundreds of rows the
// user cannot see, and the frames would be mostly off screen.
func TestConsoleSizeReportsTheWindowNotTheBuffer(t *testing.T) {
	h, _ := consoleForTest(t)
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
		t.Fatal(err)
	}
	wantC := int(info.Window.Right - info.Window.Left + 1)
	wantR := int(info.Window.Bottom - info.Window.Top + 1)

	c, r, ok := consoleSize()
	if !ok {
		t.Fatal("consoleSize declined on a console it had just been shown to have")
	}
	if c != wantC || r != wantR {
		t.Errorf("consoleSize() = %d x %d, want the visible window's %d x %d (the buffer is %d x %d)",
			c, r, wantC, wantR, info.Size.X, info.Size.Y)
	}
	if int(info.Size.Y) == wantR {
		t.Logf("this console's buffer is exactly as tall as its window (%d rows), so this run cannot tell buffer from window; a console with scrollback can", wantR)
	}
}
