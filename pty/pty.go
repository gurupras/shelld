// Package pty runs a program attached to a pseudo-terminal: openpty +
// setsid + controlling tty on Unix, ConPTY on Windows.
package pty

import (
	"errors"
	"os"
)

// Size is a terminal size in character cells.
type Size struct {
	// Cols and Rows are the width and height in cells. Both are what the child
	// reads back from TIOCGWINSZ, so a zero means "unknown" to the program, not
	// "empty"; a caller should send a real size. Pixel dimensions are not
	// carried: nothing here has them, and a program that wants them asks the
	// terminal directly.
	Cols, Rows uint16
}

// Command describes the process to start.
type Command struct {
	// Path is the program to execute. A name with a path separator is taken
	// relative to the effective working directory; a bare name is looked up
	// only in the child's PATH, never in the working directory (on Windows
	// with PATHEXT). See resolve (Unix) and winexec.LookPath (Windows).
	Path string
	// Args is the full argv; Args[0] is the name the program sees.
	Args []string
	// Dir is the working directory; empty or not a directory means $HOME.
	Dir string
	// Env is the complete environment as KEY=value entries.
	Env []string
	// PlaceIn puts the child into a process container at the moment it is
	// created rather than moving it there afterwards. On Linux it is an open
	// directory handle for a cgroup v2 cgroup, passed as SysProcAttr.CgroupFD;
	// nil means the child is created wherever the daemon already is.
	//
	// "At the moment it is created" is the whole point, not a refinement of
	// "immediately after". A child spawned first and written into a cgroup
	// second exists, briefly, outside it, and in that window it can fork: the
	// fork lands in the old cgroup and nothing later moves it, because
	// migration by pid moves one process and leaves its children where they
	// are. A container assembled after the fact therefore has no moment at
	// which it is known to hold the whole tree — which is the one property a
	// kill needs from it.
	//
	// Start neither closes the handle nor keeps it: the descriptor has to be
	// open across the spawn and is the caller's to close afterwards. Open it
	// per spawn rather than caching it, because a handle for a cgroup that has
	// since been removed is still accepted by open and fails only at clone3,
	// which turns a teardown race into a spawn failure.
	//
	// Where a platform cannot honour it, Start REFUSES rather than spawning
	// the child unplaced: silently dropping it would produce exactly the
	// defect the field exists to close, a caller that believes it can kill a
	// tree and a tree that is not in the container about to be killed.
	PlaceIn *os.File
}

// ProgramError is Start's error for a spawn refused because of the program the
// Command names: a name that resolves to nothing, a file that is not executable
// or not a binary the system can load, an argv the exec will not take. The same
// Command cannot succeed on a retry. Every other Start error — opening the
// terminal, setting up the child, a fork the host has no room for — is the
// host's, and may.
//
// The split is by errno where the failure comes back from the exec, so it
// follows the table shelld-rs uses for the same route (ShellError::is_client_fault
// in shelld-core's window.rs), and the daemon can answer 400 for one and 500 for
// the other.
type ProgramError struct {
	// Err is the spawn's own error, text unchanged.
	Err error
}

func (e *ProgramError) Error() string { return e.Err.Error() }

// Unwrap returns Err, so errors.Is and errors.As reach through to it.
func (e *ProgramError) Unwrap() error { return e.Err }

// ExitStatus is how a child process ended.
type ExitStatus struct {
	// Code is the exit code, or -1 when the process was killed by a signal.
	Code int
	// Signal names the signal that killed the process (Unix), as "signal N"
	// when the platform has no name for it; empty when it exited normally.
	Signal string
}

// ErrUnsupported is returned on platforms without PTY support.
var ErrUnsupported = errors.New("pty: unsupported platform")
