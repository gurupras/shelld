package daemon

// ListenOptions configures Listen.
type ListenOptions struct {
	// SharedAccess lets other users reach the daemon. On Windows the pipe
	// then admits every authenticated user and, for an elevated daemon, any
	// integrity level; by default only the daemon's own user can open it,
	// and a non-elevated process cannot reach an elevated daemon. On Unix the
	// socket becomes mode 0666 instead of owner-only. Go clients of another user
	// still refuse the daemon, since they check that the socket or pipe is
	// owned by, or served by, their own user; shared access serves clients
	// that don't check. It is a command-line flag only,
	// never read from the environment, so a .env file cannot set it.
	SharedAccess bool
}

// pipeSDDL is the daemon pipe's security descriptor: owned by the daemon's
// user (so clients can check the owner), full access for that user and
// SYSTEM only, and for an elevated daemon a high mandatory label that keeps
// medium-integrity processes of the same user from writing to it. Shared
// access adds read and write for authenticated users (0x12019b:
// FILE_GENERIC_READ|FILE_GENERIC_WRITE without FILE_CREATE_PIPE_INSTANCE, so
// they cannot add server instances of the pipe) and drops the label.
func pipeSDDL(sid string, elevated, shared bool) string {
	sd := "O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
	switch {
	case shared:
		sd += "(A;;0x12019b;;;AU)"
	case elevated:
		sd += "S:(ML;;NW;;;HI)"
	}
	return sd
}
