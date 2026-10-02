//go:build unix

package main

import "testing"

// TestTermSizeFallsBackTo80x24 pins the default on the platforms where it is the
// answer. On unix a terminal is a descriptor and nothing else, so once stdout and
// stdin are both pipes termSize has asked everything there is to ask -- consoleSize
// has nothing to add here -- and 80 x 24 is what attach sends as its first resize
// frame. That is the size a daemon lays every window of the session out for when
// shellctl's output is redirected, so the number is visible in the frames.
//
// Windows has one more place to look and answers with the real console instead;
// the untagged test asserts only that both answers are usable.
func TestTermSizeFallsBackTo80x24(t *testing.T) {
	withoutTerminals(t)
	if c, r := termSize(); c != 80 || r != 24 {
		t.Errorf("termSize() = %d x %d with no terminal on either descriptor, want 80 x 24", c, r)
	}
}

// TestConsoleSizeHasNothingToAddOnUnix pins the stub rather than its absence.
// termSize's third question exists for Windows, where a console is an object the
// process holds independently of its descriptors; a unix implementation that
// started answering -- by opening /dev/tty, say -- would change what a redirected
// shellctl lays out for on every unix platform at once, and would do it without
// any test noticing unless this one is here.
func TestConsoleSizeHasNothingToAddOnUnix(t *testing.T) {
	if c, r, ok := consoleSize(); ok || c != 0 || r != 0 {
		t.Errorf("consoleSize() = %d, %d, %v; want 0, 0, false", c, r, ok)
	}
}
