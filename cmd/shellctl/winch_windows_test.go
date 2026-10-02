//go:build windows

package main

// Written on a Linux machine and vetted for windows/amd64 and windows/arm64
// there; executed on Windows on 2026-09-25, where all eight tests the windows
// build of this package has passed with no skips. Anything changed here is back
// to untested until someone runs it again.
//
// HOW TO RUN IT: the process needs a console. It no longer needs stdout to BE
// that console -- termSize asks CONOUT$ when stdout is not a console, so
// redirecting the test binary's output (`*> log`, `> log`, a CI step that captures
// output) reports the real size and the resize case still works; that is how the
// passing run above was done, over ssh with output sent to a file. It was the
// other way round when this file was written: the first real-hardware run failed
// after five seconds with the resize case's own message, which is what found the
// defect. A test binary with no console at all still skips.
//
// The resize case needs a console it is allowed to resize, so it skips where
// there is none. It shrinks the window by four columns and puts the size back,
// which is visible as a flicker to anyone watching the terminal it runs in.

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys/windows does not wrap SetConsoleWindowInfo, and a resize is the only way
// to make the poll loop report anything, so the test declares it.
//
// syscall.SyscallN rather than LazyProc.Call: the uintptr(unsafe.Pointer(&r))
// conversion is only guaranteed to keep the rect alive across the call when it
// appears in the argument list of a //go:uintptrescapes function, and SyscallN is
// the one that carries the annotation.
var procSetConsoleWindowInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleWindowInfo")

func setConsoleWindow(h windows.Handle, r windows.SmallRect) error {
	if err := procSetConsoleWindowInfo.Find(); err != nil {
		return err
	}
	const absolute = 1
	ret, _, errno := syscall.SyscallN(procSetConsoleWindowInfo.Addr(), uintptr(h), absolute, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		if errno != 0 {
			return errno
		}
		return errors.New("SetConsoleWindowInfo failed without reporting why")
	}
	return nil
}

// TestWatchResizeStaysQuietWhileNothingResizes is the half of the poll loop that
// needs no resize: whatever termSize answers with neither stdout nor stdin a
// terminal -- 80 x 24 with no console, the console's own size with one -- it
// answers the same thing every time it is asked while nothing changes. The
// comparison against the last size is the whole reason this is a poll and not a
// timer: a callback that ran on every tick would send the daemon four resize
// frames a second for the life of every attached session, and each one reflows the
// window.
//
// It is also the one test here that would catch a consoleSize that answered
// differently on successive calls -- it opens CONOUT$ fresh each time -- since
// four unequal answers in a row would fire the callback.
func TestWatchResizeStaysQuietWhileNothingResizes(t *testing.T) {
	withoutTerminals(t)
	var calls atomic.Int64
	stop := watchResize(func() { calls.Add(1) })
	time.Sleep(1100 * time.Millisecond) // four ticks at 250ms
	stop()
	if n := calls.Load(); n != 0 {
		t.Errorf("the callback ran %d times in a second with the size unchanged", n)
	}
}

// TestWatchResizeNoticesAConsoleResizeAndNotOneAfterStop drives the loop with a
// real change. Windows has no SIGWINCH, so this poll is the only thing that ever
// tells the daemon the terminal got smaller; if it stops noticing, the session
// renders for the old size until it is reattached.
func TestWatchResizeNoticesAConsoleResizeAndNotOneAfterStop(t *testing.T) {
	h, _ := consoleForTest(t)

	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
		t.Fatal(err)
	}
	was := info.Window
	if was.Right-was.Left < 8 {
		t.Skipf("the console is %d columns wide, too narrow to shrink and still be a console", was.Right-was.Left+1)
	}
	t.Cleanup(func() { _ = setConsoleWindow(h, was) })

	fired := make(chan struct{}, 8)
	var calls atomic.Int64
	stop := watchResize(func() {
		calls.Add(1)
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	smaller := was
	smaller.Right -= 4
	if err := setConsoleWindow(h, smaller); err != nil {
		stop()
		t.Skipf("this console will not take a window size change: %v", err)
	}
	// Succeeding and having an effect are two different things here, and only the
	// second one leaves the poll something to find. A pseudo-console -- which is
	// what an ssh session gets, and what any remote or CI Windows runner gets --
	// can return non-zero from SetConsoleWindowInfo and leave the rect exactly
	// where it was. The wait below then runs its full 5 s, 20 polls at 250 ms, and
	// reports that the poll never noticed a resize; the one thing it cannot be
	// evidence of is a broken poll, because no resize happened. That is a false
	// red, and it fired 4 times in 14 runs on one Windows host -- often enough to
	// distrust the package and rare enough to look like contention.
	//
	// So read the rect back and skip on a no-op. A skip here says the host cannot
	// pose the question, which is true and is the honest outcome; entering the wait
	// asserts something about watchResize that the host has not set up.
	var moved windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &moved); err != nil {
		stop()
		t.Fatalf("reading the console back after resizing it: %v", err)
	}
	if moved.Window == was {
		stop()
		t.Skipf("this console accepted a window size change and made none: still L=%d T=%d R=%d B=%d",
			was.Left, was.Top, was.Right, was.Bottom)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("a console resize was never reported: the poll is the only notice Windows gives")
	}

	stop()
	seen := calls.Load()
	if err := setConsoleWindow(h, was); err != nil {
		t.Fatalf("putting the console size back: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // more than two ticks
	if now := calls.Load(); now != seen {
		t.Errorf("the callback ran %d more times after stop: attach's callback writes a resize frame to a connection it is already closing", now-seen)
	}
}
