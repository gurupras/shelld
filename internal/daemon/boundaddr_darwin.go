//go:build darwin

package daemon

// addressHeld cannot be answered on darwin, so it says so rather than guessing.
//
// There is no /proc/net/unix and no sysctl that lists bound Unix sockets. The only
// way to find the descriptor holding an address is to walk every process's open files
// through libproc, which needs privileges over processes this daemon does not own --
// and a daemon that needed them to start would be a worse thing than the bug.
//
// What that costs is stated at isBound: darwin cannot tell a socket file whose
// process is gone from an address a live descriptor still holds, because its kernel
// answers ECONNREFUSED to both. That is where every platform was before this, so the
// behaviour there is unchanged rather than newly wrong. Linux can tell them apart.
func addressHeld(string) (held, known bool) { return false, false }
