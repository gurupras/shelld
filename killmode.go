package shelld

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// KillMode selects what Window.Kill does. It is process-wide, like the cgroup
// arm, because it is a policy of the daemon and not of a window: the daemon
// reads it once at start (SHELLD_KILL_MODE) and every window it opens is killed
// the same way.
type KillMode int32

const (
	// KillTree is the default and the stronger guarantee: SIGKILL the shell's
	// process tree and session, wait for the shell to be reaped, kill and remove
	// the window's cgroup, and only then return. Nothing the window started
	// survives, including a process that ignores SIGHUP or called setsid.
	KillTree KillMode = iota
	// KillHangup is tmux's kill-window: close the pty master and return at once.
	// The kernel hangs up the terminal, which SIGHUPs the session; the shell is
	// reaped asynchronously by the window's waitLoop. Processes that ignore
	// SIGHUP, or that left the terminal's session (setsid, a daemon), survive,
	// exactly as they survive tmux kill-window. No /proc scan runs and nothing
	// is SIGKILLed. The window's cgroup is not killed: it is removed by a
	// background reaper as soon as it empties (see reapCgroupWhenEmpty).
	KillHangup
)

// KillModeEnv is the environment variable the daemon reads once at start.
const KillModeEnv = "SHELLD_KILL_MODE"

var killMode atomic.Int32

// SetKillMode sets the process-wide kill mode. The daemon calls it once at
// start; tests may call it and must restore the previous value.
func SetKillMode(m KillMode) KillMode { return KillMode(killMode.Swap(int32(m))) }

// CurrentKillMode reports the process-wide kill mode.
func CurrentKillMode() KillMode { return KillMode(killMode.Load()) }

func (m KillMode) String() string {
	switch m {
	case KillTree:
		return "tree"
	case KillHangup:
		return "hangup"
	}
	return fmt.Sprintf("KillMode(%d)", int32(m))
}

// ParseKillMode reads a SHELLD_KILL_MODE value. Empty, "tree" and "default"
// are the default; "hangup" (alias "tmux") selects KillHangup. Anything else is
// an error, so that a typo cannot silently select the weaker kill or silently
// fail to select it.
func ParseKillMode(s string) (KillMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "tree", "default":
		return KillTree, nil
	case "hangup", "tmux":
		return KillHangup, nil
	}
	return KillTree, fmt.Errorf("%s=%q: want \"tree\" (default) or \"hangup\"", KillModeEnv, s)
}
