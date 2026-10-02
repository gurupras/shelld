//go:build windows

package shelld

// dontPassOnIgnoredSignals has nothing to do on Windows.
//
// The hazard it exists for is execve preserving SIG_IGN across a spawn, and Windows has
// neither: CreateProcess does not carry a signal disposition into the child, because there
// are no POSIX signal dispositions to carry. The Go signal names exist on Windows as
// constants, but SIGHUP and SIGTSTP are never delivered there at all, so there is no
// inherited state for a window's shell to be handed.
//
// It is a no-op rather than absent because NewWindow calls it unconditionally, and a spawn
// site that has to ask which platform it is on is the worse shape.
func dontPassOnIgnoredSignals() {}
