//go:build unix

package main

import (
	"os"
	"syscall"
)

// enableVTOutput does nothing: Unix terminals interpret VT sequences.
func enableVTOutput() (restore func()) { return func() {} }

// consoleSize has nothing to add on Unix. There is no console object apart from
// the descriptors termSize has already asked: a terminal here IS a descriptor,
// and when neither stdout nor stdin is one there is nothing further to ask.
func consoleSize() (cols, rows int, ok bool) { return 0, 0, false }

// stopSignals are the signals attach turns into a clean detach, which restores
// the terminal. SIGTERM, SIGHUP and SIGQUIT end the process by default, which
// leaves a raw terminal behind for whoever sent them -- timeout, a supervisor, a
// closed window. They are taken only when stdin is a terminal, as shelld-rs's
// StopSignals takes them (shellctl/src/main.rs:899-941): with nothing to
// restore, their default action is the right one.
func stopSignals(tty bool) []os.Signal {
	if !tty {
		return []os.Signal{os.Interrupt}
	}
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}
