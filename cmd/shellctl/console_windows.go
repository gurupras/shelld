//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// enableVTOutput turns on VT processing for stdout when it is a console, so
// frames render in the classic console host as well as Windows Terminal, and
// returns a function that restores the previous mode. Without
// DISABLE_NEWLINE_AUTO_RETURN, writing a row's last cell moves the cursor at
// once and the frame's CR LF adds a line; builds that reject that flag get VT
// processing alone.
func enableVTOutput() (restore func()) {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	vt := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if err := windows.SetConsoleMode(h, vt|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		if err := windows.SetConsoleMode(h, vt); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot enable VT output on this console: %v\n", err)
			return func() {}
		}
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// consoleSize reports the size of this process's active console screen buffer.
// On Windows it is the only question left once stdout is not a console, and
// termSize's stdin fallback is no substitute: term.GetSize is
// GetConsoleScreenBufferInfo, which answers only for a console OUTPUT handle, and
// the handle on stdin is a console INPUT handle it rejects. Nothing arranges a
// console output handle on descriptor 0, so that branch cannot fire here whatever
// the caller does — measured on a real console, where stdout answered and stdin
// was rejected in the same run. Without this, `shellctl attach >frames` laid
// every session out for 80 x 24 on Windows while Linux used the real terminal.
//
// CONOUT$ opens the active screen buffer whatever stdout points at, so it answers
// for redirected output; a process with no console — a service, a build agent —
// cannot open it and falls through to termSize's default. It is opened fresh on
// each call rather than cached because watchResize asks four times a second, and
// a cached handle would keep answering for a screen buffer that is no longer the
// active one.
func consoleSize() (cols, rows int, ok bool) {
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0, 0, false
	}
	// GENERIC_READ|GENERIC_WRITE with both share modes is what CreateFile
	// documents for a console handle.
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(h)
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(h, &info) != nil {
		return 0, 0, false
	}
	// The window, not the buffer. A console's buffer is normally taller than its
	// visible window — that extra height is the scrollback — and laying a session
	// out for the buffer would render rows the user cannot see.
	cols = int(info.Window.Right - info.Window.Left + 1)
	rows = int(info.Window.Bottom - info.Window.Top + 1)
	if cols <= 0 || rows <= 0 {
		return 0, 0, false
	}
	return cols, rows, true
}

// stopSignals is Ctrl-C alone on Windows, as in shelld-rs: a closed console has
// no mode left to restore, and TerminateProcess cannot be handled at all.
func stopSignals(bool) []os.Signal { return []os.Signal{os.Interrupt} }
