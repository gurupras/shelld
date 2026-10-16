package shelld

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurupras/shelld/internal/cgroup"
	"github.com/gurupras/shelld/internal/tracing"
	"github.com/gurupras/shelld/proctree"
	"github.com/gurupras/shelld/pty"
	"github.com/gurupras/shelld/vt"
)

// WindowOpts configures a new window.
type WindowOpts struct {
	// Name identifies the window within its session. A Session rejects a
	// duplicate; a Window on its own does not care.
	Name string
	// Cwd is the working directory; empty means $HOME (%USERPROFILE% on
	// Windows). A Cwd that is not a directory fails NewWindow with a
	// *CwdError, where tmux and shelld-rs start in $HOME instead: an agent
	// must not run silently in the wrong directory.
	//
	// $HOME is the default for a Manager driven as a library. The daemon does
	// not rely on it: it fills this in with the directory it was started in, so
	// that a window over HTTP inherits that the way tmux's does (see
	// daemon.State.DefaultCwd).
	Cwd string
	// Command is the program and its arguments, run as given.
	Command []string
	// Shell is a command line split on whitespace, with ' and " quoting and no
	// escapes (see splitCommandLine) — the form the daemon's HTTP API takes.
	// Used when Command is empty; both empty means the default shell.
	Shell string
	// Cols and Rows are the initial size; zero means 200×50. Above MaxCols
	// or MaxRows, NewWindow fails.
	Cols uint16
	Rows uint16

	// The child's environment is built in order:
	//
	//  1. the base: Environ when it is non-nil, otherwise this process's
	//     environment without the outer terminal's identity variables
	//     (outerTerminalEnv: TMUX, WT_SESSION, TERM_PROGRAM, …), with
	//     TERM_PROGRAM=shelld, TERM_PROGRAM_VERSION=Version and, except on
	//     Windows, SHELL set to the login shell, TERM=xterm-256color and
	//     COLORTERM=truecolor describing shelld's own terminal rather than the
	//     one the daemon was started from, and HISTFILE=/dev/null;
	//  2. Env, each entry setting one variable (later entries win);
	//  3. UnsetEnv, removing variables by name.
	//
	// So every variable step 1 supplies is a default: Env overrides it and
	// UnsetEnv removes it, with no exceptions. With Environ set, the child gets
	// exactly Environ plus Env minus UnsetEnv.
	Environ  []string
	Env      [][2]string
	UnsetEnv []string

	// PasteMode is how Paste brackets text: "auto" (the default), "bracketed"
	// or "raw"; see PasteOpts.Mode.
	PasteMode string

	// InputQueueLimit bounds the bytes queued for the program's input
	// (DefaultInputQueueLimit when 0), and InputBudget the bytes queued
	// across the windows that share it (a process-wide DefaultInputBudget
	// when nil). Input past either fails with ErrInputQueueFull, or with the
	// ErrInputTooLarge that wraps it when the write is larger than the bound
	// itself and so would not fit an empty queue either.
	InputQueueLimit int
	InputBudget     *InputBudget

	// ScrollbackBytes bounds the bytes of raw history the window holds
	// (sbDefaultBudget when 0); the oldest lines are evicted past it. A window
	// running a full-screen program is bounded by this and not by the line
	// capacity, since one of its lines can be 64 KiB. Anything below
	// sbMaxLine+sbMaxChunk (128 KiB) is raised to it: a store that cannot hold
	// the line it just cut would evict that line as it arrived.
	ScrollbackBytes int64

	// StallTimeoutMS bounds how long a queued injection waits to be delivered
	// before it is discarded; see InjectConfig.StallTimeoutMS for why an expiry
	// drops rather than types. Nil means the default 300 000 (five minutes); a
	// pointer to 0 means no deadline, which is the behaviour before this was
	// implemented. It is a pointer for that reason: zero has to stay reachable
	// and cannot double as "unset". A value too large to convert to a duration
	// is clamped to about 292 years rather than wrapped; see stallTimeout.
	StallTimeoutMS *uint64

	// OnExit, when set, is called once in its own goroutine after the
	// process exits and Done is closed.
	OnExit func(ExitStatus)
}

// ErrEmptyCommand is NewWindow's error for a shell or command that names no
// program at all.
var ErrEmptyCommand = errors.New("empty shell command")

// CwdError is NewWindow's error for a working directory that is not a
// directory.
type CwdError struct {
	// Cwd is the path as the caller gave it, before any expansion.
	Cwd string
	// Err is what the stat of that path reported, or an error saying it is not a
	// directory. Test for os.ErrNotExist and friends through errors.Is.
	Err error
}

// Error describes the directory and the underlying failure.
func (e *CwdError) Error() string { return fmt.Sprintf("cwd %q is not a directory: %v", e.Cwd, e.Err) }

// Unwrap returns Err, so errors.Is and errors.As reach through to it.
func (e *CwdError) Unwrap() error { return e.Err }

// ExitStatus is how a window's process ended.
type ExitStatus = pty.ExitStatus

// Minimum effective PTY size — xterm's practical lower bound.
const (
	minEffectiveCols = 20
	minEffectiveRows = 5
)

// Notifier wakes every goroutine currently waiting; notifications with no
// waiter are dropped (tokio's Notify::notify_waiters).
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

// Wait returns a channel closed by the next NotifyWaiters.
func (n *Notifier) Wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// NotifyWaiters wakes everyone holding a channel from Wait, and does nothing if
// nobody is. It is edge-triggered on purpose: a window that produces output
// faster than a client reads it should wake that client once and let it read the
// current state, not queue one wakeup per write.
func (n *Notifier) NotifyWaiters() {
	n.mu.Lock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
	n.mu.Unlock()
}

// Window is a shell on a PTY, with raw scrollback, a VT grid, and an inject
// queue.
//
// Lock order: killMu → attMu → mu → ptyMu → vtMu. Never take a lower lock
// and then reach up. The input queue's lock is a leaf, and no window lock is
// held while writing to the terminal.
type Window struct {
	traceID   uint64
	attMu     sync.Mutex
	attachers []*Attacher
	nextAttID atomic.Uint64
	// sizeMu serializes effective-size recomputation.
	sizeMu sync.Mutex

	mu   sync.RWMutex
	name string
	pid  int
	// pidStart is the shell's start time in clock ticks since boot, read while it
	// was still an unreaped child, so `pid` names one process rather than an integer
	// another process may later hold. proctree.KillTree refuses a root whose start
	// time no longer matches; without it, killing a window whose shell waitLoop has
	// already reaped would sweep whatever now holds that pid — and if that stranger
	// leads a session, its whole session with it.
	//
	// Zero if the read failed, and KillTree refuses a zero start for the life of the
	// window — correctly, since it cannot tell a start time that was never read from
	// one that names the wrong root, the argument being all it has. reapChildTree's doc
	// already rules on what a refusal costs and does not overstate it: the fallback
	// fires on an empty answer, and what outlives it is a descendant that setsid'd
	// away. Read that paragraph rather than re-deriving it here.
	//
	// What it does not cover is this cause. It frames the residual on the platform axis
	// — a unix with no proctree implementation — where a failed read at spawn puts the
	// same residual on a platform that HAS one, for one window, permanently, from a
	// transient /proc failure, and with the cgroup arm as the only thing left covering
	// the tree. Whether to fail window creation instead, or carry an explicit unchecked
	// marker rather than overloading zero, is a policy question and is not decided
	// here. What is decided is that it no longer happens silently.
	pidStart uint64
	// cg is the window's cgroup, or nil when this daemon has no cgroup arm (see
	// cgroups.go). It is the other half of teardown, and the half whose anchor
	// does not expire: `pid` names one process only while that process is
	// unreaped, while a directory cannot come to mean something else. Membership
	// is inherited at fork and survives setsid, the parent's exit and reparenting
	// to pid 1 — which is exactly the process the /proc union cannot see.
	cg        *cgroup.Cgroup
	cols      uint16
	rows      uint16
	pinned    bool
	pinCols   uint16
	pinRows   uint16
	shell     string
	alive     bool
	exit      ExitStatus
	exited    bool
	done      chan struct{}
	drainStop chan struct{}
	stopOnce  sync.Once

	killMu sync.Mutex

	ptyMu sync.Mutex
	pty   *pty.PTY
	// ptyClosed is set once the master is closed or about to be (EOF, Kill):
	// resizes skip the ioctl.
	ptyClosed bool
	closeOnce sync.Once

	input *inputQueue

	vtMu sync.Mutex
	vt   *VtGrid
	// syncTimer ends a synchronized update the program left open (see
	// endExpiredSync). Guarded by vtMu; nil when no timer is armed.
	syncTimer *time.Timer

	scrollback   *Scrollback
	dirty        *Notifier
	injectQueue  *InjectQueue
	inputTracker *InputTracker
	idleDetector *IdleDetector
	stash        *StashState
	injectCfg    InjectConfig
	injectExpiry expiryLog

	// scanStalled is whether the last idle-scan verdict was a refusal with a
	// payload queued behind it, so the log line is written when that changes
	// rather than every 200 ms. Atomic because the drain loop is not the only
	// caller: a test waiting for a window to go idle calls isIdleForInjection
	// from its own goroutine.
	scanStalled atomic.Bool
}

// Attacher is one client's geometry declaration. The effective PTY size is
// max(pin, min over voting attachers), floored at 20×5.
//
// A non-voting attacher (an observer) declares a size for its own cropped view
// only and is left out of that min, so a watcher on a small screen cannot
// demote the grid the program is laid out for.
type Attacher struct {
	id uint64
	// One word for both axes, cols in the high half, so a declaration is stored
	// and loaded whole. Two atomics would each be race-free and still let
	// RecomputeEffectiveSize -- which reads every voting attacher's pair -- pair
	// one client's new width with its old height and size the grid to a
	// declaration nobody made. Read it through Size.
	size   atomic.Uint32
	voting bool // immutable after registration
	window *Window
	once   sync.Once
}

// ID identifies the attacher within its window, for the lifetime of the
// attachment. Ids are not reused.
func (a *Attacher) ID() uint64 { return a.id }

// Size is the geometry this client last declared, both axes from one load, not
// the geometry it got: the effective size is the whole window's, from
// Window.Size. Prefer this to Cols and Rows together.
func (a *Attacher) Size() (cols, rows uint16) {
	v := a.size.Load()
	return uint16(v >> 16), uint16(v)
}

// Cols is the width this client last declared, not the width it got: the
// effective size is the whole window's, from Window.Cols. For both axes use
// Size.
func (a *Attacher) Cols() uint16 { c, _ := a.Size(); return c }

// Rows is the height this client last declared, with the same caveat as Cols.
func (a *Attacher) Rows() uint16 { _, r := a.Size(); return r }

// packSize is the stored form of a declaration, clamped. Rows occupies the low
// half, so an unclamped value cannot bleed into cols.
func packSize(cols, rows uint16) uint32 {
	return uint32(min(cols, MaxCols))<<16 | uint32(min(rows, MaxRows))
}

// Voting reports whether this client's size is counted in the effective size.
// It is fixed at registration.
func (a *Attacher) Voting() bool { return a.voting }

// SetSize updates the declared geometry, clamped to MaxCols×MaxRows, and
// recomputes the effective size. A non-voting attacher changes only its own
// crop, so it skips the recomputation.
func (a *Attacher) SetSize(cols, rows uint16) {
	a.size.Store(packSize(cols, rows))
	if !a.voting {
		return
	}
	_ = a.window.RecomputeEffectiveSize()
}

// Release deregisters the attacher and recomputes the effective size.
// Idempotent.
func (a *Attacher) Release() {
	a.once.Do(func() {
		w := a.window
		w.attMu.Lock()
		for i, x := range w.attachers {
			if x == a {
				w.attachers = append(w.attachers[:i], w.attachers[i+1:]...)
				break
			}
		}
		n := len(w.attachers)
		w.attMu.Unlock()
		if a.voting {
			_ = w.RecomputeEffectiveSize()
		}
		ec, er := w.Size()
		slog.Debug("attacher released", "attacher_id", a.id, "voting", a.voting,
			"effective_cols", ec, "effective_rows", er, "total_attachers", n)
	})
}

func newWindowState(name string, cfg InjectConfig, vtCols, vtRows int, sbBytes int64) *Window {
	return &Window{
		traceID:      tracing.NewID(),
		name:         name,
		done:         make(chan struct{}),
		drainStop:    make(chan struct{}),
		vt:           NewVtGrid(vtCols, vtRows),
		scrollback:   NewScrollbackWithBudget(defaultScrollbackCapacity, sbBytes),
		dirty:        &Notifier{},
		injectQueue:  NewInjectQueue(cfg.MaxQueueSize),
		inputTracker: NewInputTracker(cfg.DebounceMS),
		idleDetector: NewIdleDetector(cfg.IdlePattern, cfg.ScanLines),
		stash:        &StashState{},
		injectCfg:    cfg,
	}
}

// NewWindow spawns the shell and starts the read and inject-drain loops.
func NewWindow(opts WindowOpts) (*Window, error) {
	var span, tid uint64
	if tracing.Enabled {
		tid = tracing.NewID()
		span = tracing.Begin(tracing.KindWindowCreate, tid, 0)
	}
	parts, shell := opts.Command, strings.Join(opts.Command, " ")
	if len(parts) == 0 {
		shell = opts.Shell
		if shell == "" {
			shell, parts = defaultShell()
		} else {
			var err error
			if parts, err = splitCommandLine(shell); err != nil {
				return nil, err
			}
		}
	}
	cols, rows := opts.Cols, opts.Rows
	if cols == 0 {
		cols = 200
	}
	if rows == 0 {
		rows = 50
	}
	if len(parts) == 0 || parts[0] == "" {
		return nil, ErrEmptyCommand
	}
	if err := CheckSize(cols, rows); err != nil {
		return nil, err
	}
	if opts.Cwd != "" {
		if st, err := os.Stat(opts.Cwd); err != nil || !st.IsDir() {
			if err == nil {
				err = errors.New("not a directory")
			}
			return nil, &CwdError{Cwd: opts.Cwd, Err: err}
		}
	}

	cfg := DefaultInjectConfig()
	cfg.IdlePattern = IdlePatternForProgram(parts[0])
	if opts.StallTimeoutMS != nil {
		cfg.StallTimeoutMS = *opts.StallTimeoutMS
	}
	if opts.PasteMode != "" {
		if !ValidPasteMode(opts.PasteMode) {
			return nil, fmt.Errorf("invalid paste mode %q", opts.PasteMode)
		}
		cfg.PasteMode = opts.PasteMode
	}

	// The cgroup is made BEFORE the spawn because the shell is created inside it,
	// not moved into it afterwards: a shell that is placed second exists for a
	// moment outside, and anything it forks in that moment stays outside forever,
	// since migration by pid moves one process and leaves its children behind.
	// nil here is the ordinary case on a host with no cgroup arm.
	cg := newWindowCgroup()
	place, err := openCgroup(cg)
	if err != nil {
		slog.Debug("spawn failed", "shell", shell, "err", err)
		return nil, fmt.Errorf("spawn shell: %w", err)
	}
	// Before the spawn below, because the child clears only what this process is handling at
	// the moment it forks. SIG_IGN is the one disposition an exec keeps, and a window shell
	// that ignores SIGHUP cannot be ended by closing its pty, nor one that ignores SIGINT be
	// interrupted with Ctrl-C. SIGTSTP is cleared for the command, not for the shell: an
	// interactive shell ignores it for itself either way, but this argv need not be a shell
	// at all. signals_unix.go has the measurements.
	dontPassOnIgnoredSignals()
	p, err := pty.Start(pty.Command{
		Path:    parts[0],
		Args:    parts,
		Dir:     opts.Cwd,
		Env:     childEnv(opts.Environ, opts.Env, opts.UnsetEnv),
		PlaceIn: place,
	}, pty.Size{Cols: cols, Rows: rows})
	// Closed on both paths and closed here rather than deferred: the descriptor
	// only has to outlive the clone, and it is this function's to close because
	// pty.Start neither keeps it nor closes it.
	if place != nil {
		place.Close()
	}
	if err != nil {
		slog.Debug("spawn failed", "shell", shell, "err", err)
		// The cgroup is removed rather than left for the GC: nothing was ever
		// created in it, so this is the one teardown that cannot be destroying
		// something a user cares about.
		discardCgroup(cg)
		return nil, fmt.Errorf("spawn shell: %w", err)
	}

	w := newWindowState(opts.Name, cfg, int(cols), int(rows), opts.ScrollbackBytes)
	if tracing.Enabled {
		w.traceID = tid
		tracing.Instant(tracing.KindWindowName, tid, int64(tracing.Intern(opts.Name)), 0)
		tracing.End(tracing.KindWindowCreate, tid, span, int64(p.Pid()), 0)
	}
	w.pid = p.Pid()
	// Read here, not at kill time: the shell is this process's child and has not been
	// reaped, so its pid cannot yet have been reused and the value read now is the
	// one that identifies it. Read at kill time it would be whatever holds the pid
	// then, which is exactly the thing it exists to detect.
	// The ok is not discarded: a failure here is not a failure of this call, it is the
	// window's tree sweep disabled for good (see pidStart), and the only moment at
	// which anything can say so. At kill time the same zero is indistinguishable from
	// a root that was reaped, so the log line there cannot name this cause; here it
	// can. Warn rather than fail: the window is otherwise sound, and on the platform
	// where the cgroup arm exists that arm still covers the tree.
	//
	// Gated on StartTimeSupported because the cause the warn names — a read that
	// failed — is not the only way to arrive here. On Windows and on every unix that
	// is neither Linux nor darwin, StartTicks answers false for every pid by
	// construction, so an ungated warn fires once per window on a platform where
	// nothing went wrong and there is nothing to fix; it would also be the only line
	// in the log claiming a fallback "plus the cgroup" on platforms that have no
	// cgroup arm at all. Those platforms' sweep is disabled for the whole build, not
	// for this window, and proctree documents it there (platform_other.go's header,
	// proctree_windows.go): a per-window line is the wrong place to say it.
	var startOK bool
	if w.pidStart, startOK = proctree.StartTicks(w.pid); !startOK && proctree.StartTimeSupported() {
		slog.Warn("could not read the shell's start time; this window's process-tree "+
			"sweep is disabled for its lifetime and teardown falls back to the direct "+
			"child, plus the cgroup if this window has one",
			"window", opts.Name, "pid", w.pid, "cgroup", cg != nil)
	}
	w.cg = cg
	w.cols, w.rows = cols, rows
	w.shell = shell
	w.alive = true
	w.pty = p
	w.input = newInputQueue(opts.InputQueueLimit, opts.InputBudget)
	// A payload is typed into the input queue, so one past its limit could only
	// be accepted to expire as too-large; the inject cap follows it (H16).
	w.injectQueue.maxBytes = w.input.limit

	go w.writeInput(p)
	go w.drainLoop()
	go w.readLoop(p)
	go w.waitLoop(p, opts.OnExit)
	return w, nil
}

// NewFakeWindow is a test stand-in with no PTY or child: a window whose
// process has, as far as every reader is concerned, already gone.
//
// It has a real 80x24 grid, scrollback and inject queue, so anything that feeds
// bytes in and reads the grid out works on it. What does not work is anything
// that goes through the process: Done is already closed, alive is false, there
// is no pid, and writing input fails with "window has no writer". No goroutines
// are started, so a fake window needs no Kill — though calling one is harmless.
//
// It is exported for the same reason as Session.InsertFakeWindow, and carries
// the same caveat: it is test support, not API to build on.
func NewFakeWindow(name string) *Window {
	w := newWindowState(name, DefaultInjectConfig(), 80, 24, 0)
	w.stopOnce.Do(func() { close(w.drainStop) })
	close(w.done)
	w.input = newInputQueue(0, nil)
	w.input.close(errors.New("window has no writer (fake or closed)"))
	return w
}

// childEnv builds the child's environment as WindowOpts documents. With a nil
// base: this process's environment without outerTerminalEnv, with
// TERM_PROGRAM=shelld, SHELL resolved and HISTFILE disabled, then the caller's
// overrides, then its removals.
func childEnv(base []string, extra [][2]string, unset []string) []string {
	return childEnvFor(runtime.GOOS, base, extra, unset)
}

// outerTerminalEnv names the variables through which a terminal, multiplexer
// or terminal session identifies itself (a trailing * matches a prefix). A
// window's program runs in shelld's terminal, not the one the daemon was
// started from, so inherited values are dropped. Agents act on them: with
// TMUX set, Claude Code and Codex run tmux commands against that outer tmux
// server, and both choose terminal features from TERM_PROGRAM, WT_SESSION and
// the rest. TMUX_TMPDIR configures tmux rather than naming a terminal and is
// kept.
var outerTerminalEnv = []string{
	"TMUX", "TMUX_PANE", "STY", "WINDOW",
	"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID",
	"WT_SESSION", "WT_PROFILE_ID",
	"KITTY_WINDOW_ID", "KITTY_PID", "GHOSTTY_RESOURCES_DIR", "VTE_VERSION",
	"XTERM_VERSION", "TILIX_ID", "TERMINATOR_UUID", "LC_TERMINAL", "LC_TERMINAL_VERSION",
	"ZELLIJ*", "WEZTERM_*", "ITERM_*", "KONSOLE_*", "GNOME_TERMINAL_*", "ALACRITTY_*",
}

func isOuterTerminalVar(key string) bool {
	for _, name := range outerTerminalEnv {
		if prefix, ok := strings.CutSuffix(name, "*"); ok {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		} else if key == name {
			return true
		}
	}
	return false
}

// childEnvFor is childEnv for the operating system goos.
func childEnvFor(goos string, base []string, extra [][2]string, unset []string) []string {
	type entry struct{ key, value string }
	var order []string
	vals := map[string]entry{}
	mapKey := func(k string) string {
		if goos == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	set := func(k, v string) {
		mk := mapKey(k)
		if _, ok := vals[mk]; !ok {
			order = append(order, mk)
		}
		vals[mk] = entry{k, v}
	}
	inherit := base == nil
	if inherit {
		base = os.Environ()
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			if inherit && isOuterTerminalVar(mapKey(k)) {
				continue
			}
			set(k, v)
		}
	}
	if inherit {
		set("TERM_PROGRAM", "shelld")
		set("TERM_PROGRAM_VERSION", Version)
		if goos != "windows" {
			set("SHELL", loginShell())
			// The child runs in shelld's terminal, so TERM has to describe that
			// one and not whatever the daemon was started from: a daemon under
			// screen or tmux otherwise hands its windows screen terminfo, and a
			// daemon started as a service hands them no TERM at all, which is
			// what makes tput fail and Node report one colour. xterm-256color
			// and truecolor are what the emulator implements — 256-colour and
			// 24-bit SGR, and xterm's key encodings, which is the same choice
			// K2's Home/End encoding already commits to.
			//
			// Windows is left alone: console programs do not read TERM, and
			// whether MSYS2 and Cygwin programs under ConPTY are better off
			// with it set is untested here (go-api proposal 4, X2 checklist).
			set("TERM", "xterm-256color")
			set("COLORTERM", "truecolor")
			// A window is usually driven by an agent, whose commands belong in that
			// window rather than in the history of the shell the user types into
			// next. It is a default and not a guarantee: a caller that wants its
			// history kept says so through Env, exactly as it does for TERM, and
			// the program can set HISTFILE for itself whatever this says. No
			// equivalent on Windows, where the shell is not sh.
			set("HISTFILE", "/dev/null")
		}
	}
	for _, kv := range extra {
		set(kv[0], kv[1])
	}
	for _, k := range unset {
		delete(vals, mapKey(k))
	}
	env := make([]string, 0, len(order))
	for _, k := range order {
		if e, ok := vals[k]; ok {
			env = append(env, e.key+"="+e.value)
		}
	}
	return env
}

// loginShell is $SHELL when executable, else the passwd entry's shell, else
// /bin/sh.
func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" && isExecutable(s) {
		return s
	}
	if f, err := os.Open("/etc/passwd"); err == nil {
		defer f.Close()
		uid := strconv.Itoa(os.Getuid())
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Split(sc.Text(), ":")
			if len(fields) >= 7 && fields[2] == uid && isExecutable(fields[6]) {
				return fields[6]
			}
		}
	}
	return "/bin/sh"
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// readLoop feeds PTY output into scrollback and the grid, answers color
// queries, and wakes attached renderers.
// readLoop reads the PTY until it closes, feeding the scrollback and grid.
//
// While a program writes continuously, reads never block, so this goroutine
// would keep its P indefinitely. The Go scheduler then leaves expired timers
// and network readiness for other goroutines — tick loops, WebSocket readers
// — unprocessed until sysmon notices, 10–20 ms later: keystroke echo in
// another window stalled for up to 18 ms beside a flooding one. Yielding
// after at most yieldEvery of continuous reading bounds that delay.
func (w *Window) readLoop(p *pty.PTY) {
	const yieldEvery = time.Millisecond
	lastYield := time.Now()
	buf := make([]byte, 4096)
	var id uint64
	if tracing.Enabled {
		id = w.traceID
	}
	for {
		var span uint64
		if tracing.Enabled {
			span = tracing.Begin(tracing.KindPTYRead, id, 0)
		}
		n, err := p.Read(buf)
		if tracing.Enabled {
			tracing.End(tracing.KindPTYRead, id, span, int64(n), 0)
		}
		if n > 0 {
			data := buf[:n]
			// The grid advances before the chunk reaches the scrollback, so
			// by the time Capture returns text, the modes it set (bracketed
			// paste, DECCKM) are what Paste and SendKeyNames read. Replies are
			// written after releasing vtMu: writing takes ptyMu, which sits
			// above vtMu in the lock order.
			if tracing.Enabled {
				span = tracing.Begin(tracing.KindVTLockWait, id, int64(tracing.LockRead))
			}
			w.vtMu.Lock()
			if tracing.Enabled {
				tracing.End(tracing.KindVTLockWait, id, span, int64(tracing.LockRead), 0)
				span = tracing.Begin(tracing.KindVTAdvance, id, int64(n))
			}
			// A synchronized update past its deadline is ended first, so
			// these bytes land after what it held, as they were written.
			w.vt.EndExpiredSync()
			w.vt.Advance(data)
			w.armSyncTimerLocked()
			reply := w.vt.TakeReplies()
			var gen uint64
			if tracing.Enabled {
				gen = w.vt.Gen()
				tracing.End(tracing.KindVTAdvance, id, span, int64(n), int64(gen))
			}
			w.vtMu.Unlock()
			if tracing.Enabled {
				span = tracing.Begin(tracing.KindScrollback, id, int64(n))
			}
			w.scrollback.Write(data)
			if tracing.Enabled {
				tracing.End(tracing.KindScrollback, id, span, int64(n), 0)
			}
			if reply != nil {
				w.input.pushReply(reply)
			}
			w.dirty.NotifyWaiters()
			if tracing.Enabled {
				tracing.Instant(tracing.KindNotify, id, int64(gen), 0)
			}
			if now := time.Now(); now.Sub(lastYield) >= yieldEvery {
				runtime.Gosched()
				lastYield = now
			}
		}
		if err != nil {
			break
		}
	}
	// Nothing more will be read to end a synchronized update, so apply what
	// it held now rather than at its deadline.
	w.vtMu.Lock()
	w.vt.EndSync()
	w.vtMu.Unlock()
	w.mu.Lock()
	w.alive = false
	w.mu.Unlock()
	w.stopDrain()
	w.closeAtEOF(p)
	// The window ended without anyone asking it to, so its cgroup goes the same
	// way: released rather than killed. Nothing else removes it — GC skips any base
	// whose daemon is still live, which includes our own — so before this a window
	// that was never explicitly killed left its directory behind for the life of
	// the daemon, one per exit. Its position here carries no
	// requirement: reaching this line already means every slave fd is closed, and
	// closing the master has no bearing on cgroup membership either way.
	w.releaseCgroup()
	// Wake attached tick loops so they see the exit promptly.
	w.dirty.NotifyWaiters()
}

// armSyncTimerLocked starts the timer that ends a pending synchronized
// update at its deadline, if one is pending and no timer is armed. A program
// that stops writing inside an update sends nothing that would end it, so
// without the timer the grid stays frozen, and a reply to a query made inside
// the update stays unsent, until more output arrives; a reply that late can
// land after the program stopped waiting for it, as typed input. Caller holds
// vtMu.
func (w *Window) armSyncTimerLocked() {
	if w.syncTimer != nil {
		return
	}
	if d, ok := w.vt.SyncDeadline(); ok {
		w.syncTimer = time.AfterFunc(time.Until(d), w.endExpiredSync)
	}
}

// endExpiredSync is the sync timer's callback. It ends the update if its
// deadline has passed and re-arms for a later one: a BSU inside the update
// moves the deadline, and the read loop may have ended this update and begun
// another while the timer was armed.
func (w *Window) endExpiredSync() {
	w.vtMu.Lock()
	ended := w.vt.EndExpiredSync()
	reply := w.vt.TakeReplies()
	if d, ok := w.vt.SyncDeadline(); ok {
		w.syncTimer.Reset(time.Until(d))
	} else {
		w.syncTimer = nil
	}
	w.vtMu.Unlock()
	// After vtMu, as in readLoop: writing takes ptyMu, above vtMu.
	if reply != nil {
		w.input.pushReply(reply)
	}
	if ended {
		w.dirty.NotifyWaiters()
	}
}

// closeAtEOF stops input once every holder of the terminal has exited:
// queued input is dropped, and on Linux and macOS the master is closed, as
// tmux closes a pane's fd at EOF. That releases a write still pending to the
// exited program, which would otherwise spin: the master reports hangup
// while writes keep failing with EAGAIN. On Windows, PTY.Close also releases
// the process handle waitLoop waits on, so the master is closed only by
// Kill, after the process is reaped.
func (w *Window) closeAtEOF(p *pty.PTY) {
	w.input.close(w.deadErr())
	if !closeMasterAtEOF {
		return
	}
	w.ptyMu.Lock()
	w.ptyClosed = true
	w.ptyMu.Unlock()
	w.closePTY(p)
}

func (w *Window) closePTY(p *pty.PTY) { w.closeOnce.Do(func() { _ = p.Close() }) }

// waitLoop reaps the process when it exits and publishes its status.
func (w *Window) waitLoop(p *pty.PTY, onExit func(ExitStatus)) {
	st, err := p.Wait()
	if err != nil {
		slog.Debug("wait failed", "window", w.Name(), "err", err)
	}
	if tracing.Enabled {
		signaled := int64(0)
		if st.Signal != "" {
			signaled = 1
		}
		tracing.Instant(tracing.KindProcessExit, w.traceID, int64(st.Code), signaled)
	}
	w.mu.Lock()
	w.exit, w.exited = st, true
	w.mu.Unlock()
	// What this window's history cost, so the default byte budget can be revisited
	// from live sessions rather than from a corpus of recordings replayed to
	// saturation, which is all it was set from.
	slog.Info("window scrollback at exit", "window", w.Name(),
		"held_bytes", w.scrollback.HeldBytes(), "budget_bytes", w.scrollback.Budget(),
		"lines_retained", w.scrollback.Len(), "lines_started", w.scrollback.TotalLines(),
		"bytes_written", w.scrollback.TotalBytes())
	close(w.done)
	if onExit != nil {
		go onExit(st)
	}
}

// Done is closed once the window's process has exited. Output it wrote may
// still be arriving, and IsAlive stays true until the terminal itself closes
// (background processes can hold it open).
func (w *Window) Done() <-chan struct{} { return w.done }

// TraceID identifies the window in trace files; it is 0 unless shelld was
// built with the shelld_trace tag.
func (w *Window) TraceID() uint64 { return w.traceID }

// ExitStatus reports how the window's process ended; ok is false before it has
// been reaped. Not "while it is still running": waitLoop records the status
// after p.Wait() returns, so there is an interval in which the process is gone
// and ok is still false. A caller that reads ok=false must treat it as "no
// status yet" and poll, not as "running" — IsAlive answers a different question.
func (w *Window) ExitStatus() (st ExitStatus, ok bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.exit, w.exited
}

// Liveness reports IsAlive and ExitStatus as one snapshot. The two are separate
// facts about separate things — the terminal and the window's own process — and
// every combination of them is reachable, so this exists only to keep a caller
// that publishes both from catching a write between the two reads. It is free:
// both fields already live under w.mu, so this takes the same single RLock the
// two accessors would take one after the other.
//
// It does not, and must not, reconcile them. alive=true with a recorded status
// is the documented state of a window whose process has exited while a
// background process holds the terminal open; measured here that state lasts as
// long as the holder does — 4.988s in TestHolderKeepsTerminalOpenAfterExit, with
// 246 writes accepted and output still reaching the grid throughout — so a
// caller that reads it as a race and suppresses one side reports a live
// terminal as dead.
func (w *Window) Liveness() (alive bool, st ExitStatus, exited bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.alive, w.exit, w.exited
}

func (w *Window) stopDrain() { w.stopOnce.Do(func() { close(w.drainStop) }) }

// drainLoop delivers queued injections once the prompt is idle and no human
// has typed within the debounce window, then waits out the cooldown. A
// payload is typed unchanged and followed by CR, as in shelld-rs: unlike
// Paste it is neither bracketed nor sanitized, so it must be trusted text.
func (w *Window) drainLoop() {
	cooldown := msDuration(w.injectCfg.DeliveryCooldownMS)
	stall := stallTimeout(w.injectCfg.StallTimeoutMS)
	// Whatever is still queued when this goroutine leaves is recorded, on every
	// exit rather than on the one below that noticed a dead window. The other three
	// returns are all drainStop, and drainStop is closed in exactly two places --
	// Kill, and the reader loop after it sets alive false -- so reaching any of them
	// means the same thing the push error means: nothing will be typed into this
	// window again. Without this the usual way a window dies recorded nothing at
	// all, because the usual way is the one where no injection was in flight.
	//
	// It matters beyond the log. The queue outlives this goroutine and
	// /inject/queue reports Pending as `pending`, so a client polling for the 202 it
	// already holds was told its payload was still waiting its turn, for as long as
	// it cared to ask, with no goroutine left that could ever take a turn. Draining
	// is what makes the record true rather than merely present.
	//
	// held is the payload already out of the queue when a push failed: recorded
	// first, so it keeps its place ahead of the ones queued behind it, and under the
	// same timestamp as the rest, because they did not stop being deliverable at
	// different moments -- the window did.
	var held *InjectMessage
	defer func() {
		// Closed, not just drained: this goroutine is the queue's only consumer,
		// and an Enqueue after it has gone would be answered 202 and then sit in
		// Pending forever.
		queued := w.injectQueue.Close()
		if held == nil && len(queued) == 0 {
			return
		}
		now := time.Now().UnixMilli()
		if held != nil {
			w.recordExpiry(now, *held, expiryWindowGone)
		}
		for _, msg := range queued {
			w.recordExpiry(now, msg, expiryWindowGone)
		}
	}()
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-w.drainStop:
			return false
		}
	}
	for {
		select {
		case <-w.injectQueue.Signal():
		case <-w.drainStop:
			return
		}
		for {
			if !sleep(200 * time.Millisecond) {
				return
			}
			if w.injectQueue.Len() == 0 {
				break
			}
			// One scan per poll, feeding both decisions: whether to deliver, and
			// why a payload that ran out of time never was. Taking the reason
			// from this scan rather than from a flag is what makes it accurate
			// for the first expiry after a delivery — the cooldown is longer
			// than a short deadline, so a payload can expire on the first poll
			// after it, having never been scanned against alongside.
			idle, refused := w.idleVerdict()
			// Input still queued for the program counts as typing: an
			// injection would land behind it, and the cooldown would run
			// before anything was written.
			if !idle || !w.inputTracker.IsQuiet() || w.input.pending() {
				// Inside the gate rather than before it: expiring first dropped a
				// payload on the very poll that would have typed it, and dropped
				// it by age where the delivery order is by priority, so a
				// high-priority payload died while a low-priority one was typed.
				// The delivery branch sweeps too, after its write, which is what
				// keeps the deadline a deadline on a window that stays busy.
				reason := expiryNotIdle
				if refused {
					reason = expiryScanRefused
				}
				w.expireStalledInjections(stall, reason)
				// Checked here so that expiring the last payload leaves the loop
				// rather than polling a queue that is already empty.
				if w.injectQueue.Len() == 0 {
					break
				}
				continue
			}
			msg, ok := w.injectQueue.Dequeue()
			if !ok {
				break
			}
			if err := w.input.push(inputChunk{data: append([]byte(msg.Payload), '\r')}); err != nil {
				if errors.Is(err, ErrInputTooLarge) {
					// Requeueing this would not be a retry. Requeue puts the payload
					// back at the front, ahead of others of its priority, and no state
					// of this window's input queue accepts it, so it is a queue head
					// that never moves: it is dequeued, refused and put back every
					// 200 ms for the life of the window, and everything queued at its
					// priority or below waits behind it. Only a strictly
					// higher-priority payload gets past. The deadline
					// would not clear it either: reaching this line means the gate found
					// the window ready, so no poll takes the branch that sweeps, and a
					// window nobody types into stays ready. So it is expired here and
					// now, without waiting out a deadline that cannot make the payload
					// smaller, and the expiry log is where the caller that already has
					// its 202 learns what became of it.
					w.expireInjection(msg, expiryTooLarge)
					continue
				}
				if errors.Is(err, ErrInputQueueFull) {
					w.injectQueue.Requeue(msg)
					// Swept here as well as in the branch above, because this is the
					// one path that reaches a push: getting here means the gate found
					// the window idle and quiet, so the gate's own sweep did not run,
					// and without this one StallTimeoutMS bounds nothing at all for a
					// queue held up by a program that has stopped reading its input --
					// which is a wait with no end, and the case the deadline is for.
					// It bounds this case only when it is set: zero still means wait
					// for ever, here as everywhere. Unlike the branch above, that is
					// the right default, because these bytes would go through the
					// moment the program read its input.
					w.expireStalledInjections(stall, expiryNoRoom)
					continue
				}
				// Returning rather than expiring, and that rests on an invariant
				// rather than on a fact about this line: push has exactly three error
				// returns, and with the two above accounted for the remainder is
				// inputQueue.err, which is only ever set by close. Every call to close
				// is a window that has lost its writer or died, so nothing will be
				// typed into this window again and requeueing or waiting out a deadline
				// would both be waiting for a writer that is gone. A fourth close site
				// carrying a recoverable error would land here and be treated as a dead
				// window -- give it a branch of its own.
				//
				// The payload the loop was holding is handed to the exit sweep rather
				// than recorded here, so that this return and the three drainStop
				// returns produce the same record. A queue longer than
				// injectExpiryLogSize loses the earliest of them to the ring -- which is
				// the ring's existing behaviour for any sweep that drops more than it
				// holds, not something new here.
				slog.Debug("injection dropped", "window", w.Name(), "id", msg.ID, "err", err)
				held = &msg
				return
			}
			// After the write, so the payload this poll chose is never the one the
			// deadline drops. Without a sweep here the deadline would not bound the
			// wait at all on a window that keeps delivering: the queue is ordered
			// by priority, so a stream of higher-priority payloads can be typed
			// past a low-priority one indefinitely, and every one of those polls
			// takes this branch and would skip the check.
			w.expireStalledInjections(stall, expiryOutranked)
			if cooldown > 0 && !sleep(cooldown) {
				return
			}
		}
	}
}

// isIdleForInjection is the verdict a delivery is gated on: the idle scan's view
// of raw history, matched against this window's prompt pattern. It exists as one
// method so that a test waiting for a window to go idle waits on what drainLoop
// reads — composing the call in a test instead let the PTY tests keep waiting on
// IsIdle over plain history after the daemon had moved to the scan.
//
// A refusal — the scan reaching a line it cannot place on the screen — is
// reported by logScanRefusal, because it is otherwise invisible and unbounded.
func (w *Window) isIdleForInjection() bool {
	idle, _ := w.idleVerdict()
	return idle
}

// idleVerdict is isIdleForInjection with the refusal kept, for the caller that
// needs to tell the two ways of not being idle apart: a screen the scan could not
// read, or one it read and did not recognise as a prompt. Only the drain loop
// needs the distinction, and only to say why it gave up on a payload.
func (w *Window) idleVerdict() (idle, refused bool) {
	idle, refusedAt, refused := w.idleDetector.idleScan(
		idleScanLines(w.scrollback, w.idleDetector.ScanLines()))
	w.logScanRefusal(refused, refusedAt)
	return idle, refused
}

// InjectExpiries returns the window's recent dropped injections, newest last, at
// most injectExpiryLogSize of them. Usually empty: a payload is recorded here only
// if it waited out InjectConfig.StallTimeoutMS, or -- whatever that is set to, zero
// included -- if it was larger than the whole input queue and so could never have
// been typed at all, or if the window died with it still queued. See
// InjectExpiry.Reason.
//
// The last of those three is the only one that empties the queue, and it is the
// reason this method and /inject/queue's `pending` have to be read together: a
// payload is in exactly one of them, never both and, after the window dies, never
// neither.
func (w *Window) InjectExpiries() []InjectExpiry { return w.injectExpiry.all() }

// Why a payload was dropped, as recorded in InjectExpiry.Reason: the window was
// busy or a human was typing, the idle scan could not read the newest output, the
// window was delivering and higher-priority payloads kept going first, the window
// was ready and its input queue had no room for the bytes yet, or it never will, or
// the window is gone.
//
// window-gone is the one reason that is not a verdict about the screen at all, and
// the only one recorded for a payload the loop never dequeued: it is written for
// everything still queued when the delivery loop finds the input queue closed. It
// is therefore also the only reason under which a payload with a fresh CreatedAt
// appears -- the others all mean time was spent waiting.
//
// no-room and too-large are deliberately separate strings, and they are the same
// distinction ErrInputTooLarge draws from ErrInputQueueFull: no-room is
// recoverable — the program reading its input, or another window releasing the
// shared budget, would have let the payload through, and it waited out
// StallTimeoutMS like the others — while too-large is not, so it is dropped on the
// poll that tries it. A client that got one string for both would be told to retry
// a payload no retry can deliver.
//
// shelld-rs is being held to the same two names, with no-room meaning the
// recoverable case there as well. That is the other project's account of itself and
// not something this repo can check — the copy of it available here predates the
// expiry reasons entirely — so read it as a parity commitment rather than as a
// verified fact. The argument for the split stands on the paragraph above without
// it.
const (
	expiryNotIdle     = "not-idle"
	expiryScanRefused = "scan-refused"
	expiryOutranked   = "outranked"
	expiryNoRoom      = "no-room"
	expiryTooLarge    = "too-large"
	expiryWindowGone  = "window-gone"
)

// expireStalledInjections discards the queued payloads that have waited out
// StallTimeoutMS, logging and recording each under reason. It returns nothing:
// the queue is the state, and the caller re-reads its length.
//
// reason is the caller's verdict for this poll, not a stored flag: it has to
// describe the screen the payload was last held against, and a window that stalls
// writes nothing after it, so the current screen is that screen. Reading
// Window.scanStalled instead would be wrong twice over — it is ANDed with the
// queue length for the log's transition test, and the delivery cooldown outlasts
// a short deadline, so a payload can expire on the first poll after a delivery
// with no scan ever having run alongside it.
func (w *Window) expireStalledInjections(stall time.Duration, reason string) {
	expired := w.injectQueue.ExpireOlderThan(stall)
	if len(expired) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	for _, msg := range expired {
		w.recordExpiry(now, msg, reason)
	}
	// logScanRefusal owns the latch that holds the refusal line to one per stall,
	// and it tests refusal AND queue length, so an expiry that empties the queue
	// has to be given the transition or the latch stays set on a queue that is now
	// empty, and the next payload to stall on the same unreadable screen is never
	// reported. Its queue-emptied-while-refusing branch is already right here: it
	// resets without claiming the scan recovered, which for these payloads — they
	// went nowhere — would be false.
	w.logScanRefusal(reason == expiryScanRefused, "")
}

// expireInjection drops one payload the drain loop has already dequeued and
// cannot ever type, rather than the ones a deadline has run out on.
//
// Unlike expireStalledInjections it does no refusal bookkeeping, and that is not an
// omission. Two facts make it dead work here. Reaching the caller means the gate
// found the window idle, and idleScan returns refused only on the path that also
// returns idle false, so this poll's scan cannot have been a refusal. And
// idleVerdict already ran logScanRefusal for this poll before the gate, so the
// latch is in its not-refusing state before this function is entered; the reason
// expireStalledInjections has to call it again -- that removing messages changes
// the queue length the latch is tested on -- needs a refusal to matter, and there
// is none. An earlier version took the poll's refused value as a parameter and
// passed it on; the call could not log anything for either value.
func (w *Window) expireInjection(msg InjectMessage, reason string) {
	w.recordExpiry(time.Now().UnixMilli(), msg, reason)
}

// recordExpiry is the pair of writes every dropped payload gets: the entry a
// client can poll for, and the line an operator reading the log can see. now is
// passed in so a sweep timestamps everything it drops together.
func (w *Window) recordExpiry(now int64, msg InjectMessage, reason string) {
	w.injectExpiry.add(InjectExpiry{ExpiredAt: now, Message: msg, Reason: reason})
	slog.Warn("queued injection expired without being delivered",
		"window", w.Name(), "id", msg.ID, "reason", reason,
		"waited", time.Duration(now-msg.CreatedAt)*time.Millisecond)
}

// logScanRefusal reports that the scan cannot place the newest output, once when
// that starts holding an injection back and once when it stops. It is reported
// because it is otherwise invisible and unbounded: drainLoop has no deadline, a
// shell sitting at a prompt the scan cannot read writes nothing that would
// restore trust, and the client's enqueue has already been answered.
//
// The condition is a refusal with something queued, not a refusal: a window
// whose prompt the scan cannot read refuses from the moment it draws that
// prompt, and a line saying "injections wait" while none is waiting is the line
// an operator learns to ignore before the one that matters arrives. Written on
// the transition, since the verdict is recomputed every 200 ms for as long as a
// payload is queued. The refused line itself goes to Debug rather than Info,
// because it is program output and a prompt carries whatever the shell puts in
// it.
func (w *Window) logScanRefusal(refused bool, refusedAt string) {
	queued := w.injectQueue.Len()
	waiting := refused && queued > 0
	if waiting == w.scanStalled.Swap(waiting) {
		return
	}
	if waiting {
		slog.Info("idle scan cannot read the newest output; injections wait",
			"window", w.Name(), "queued", queued)
		slog.Debug("idle scan refused a line", "window", w.Name(), "line", refusedAt)
		return
	}
	if refused {
		// The queue emptied while the scan still cannot read the screen, so the
		// payloads did not go to a prompt this scan read. Saying the scan
		// recovered would be false; the state is reset either way, so a later
		// enqueue against the same unreadable prompt is reported again.
		return
	}
	slog.Info("idle scan can read the output again", "window", w.Name(), "queued", queued)
}

// Name is the window's name. A Manager.MoveWindow can change it, so a caller
// holding a *Window should read this rather than remember what it looked up.
func (w *Window) Name() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.name
}

func (w *Window) setName(n string) {
	w.mu.Lock()
	w.name = n
	w.mu.Unlock()
}

// Pid is the process id of the program on the far end of the PTY, or 0 for a
// window that has none. It stays set after the process exits, where it no longer
// names anything: pair it with IsAlive before signalling it, and prefer Kill,
// which reaps the whole tree.
func (w *Window) Pid() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.pid
}

// Size is the effective PTY size. Use this rather than Cols and Rows whenever
// both axes are wanted: setEffectiveSize writes the pair under one Lock, so one
// call here observes one size, while two calls observe two -- and a resize
// changes both axes at once, so the mixed pair a reader can assemble that way
// is a size no client ever declared and the grid never had. Anything that
// reports geometry to a client, or reasons about the two axes together, wants
// this. A single axis on its own is still fine from Cols or Rows.
func (w *Window) Size() (cols, rows uint16) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cols, w.rows
}

// Cols is the effective PTY width. For both axes use Size, which cannot mix two
// resizes.
func (w *Window) Cols() uint16 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cols
}

// Rows is the effective PTY height. For both axes use Size, which cannot mix two
// resizes.
func (w *Window) Rows() uint16 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.rows
}

// Shell is the command line the window was started with, as a single string for
// display: WindowOpts.Command joined with spaces, or WindowOpts.Shell as given,
// or the default shell resolved for an empty one. It is not the child's $SHELL,
// and joining Command loses the argument boundaries, so it describes the window
// rather than reproducing it.
func (w *Window) Shell() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.shell
}

// IsAlive reports whether the window's terminal is still open: false once
// every process holding it has exited or the window was killed. See Done for
// the window's own process.
func (w *Window) IsAlive() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.alive
}

// PasteMode is the mode Paste uses when PasteOpts.Mode is empty.
func (w *Window) PasteMode() string {
	if w.injectCfg.PasteMode == "" {
		return "auto"
	}
	return w.injectCfg.PasteMode
}

// KeyModes returns the input modes the program has set, as far as the grid
// has processed its output.
func (w *Window) KeyModes() KeyModes {
	w.vtMu.Lock()
	defer w.vtMu.Unlock()
	return KeyModes{
		AppCursor:      w.vt.AppCursor(),
		AppKeypad:      w.vt.AppKeypad(),
		BracketedPaste: w.vt.BracketedPaste(),
	}
}

// The accessors below hand out the window's sub-objects directly, not copies.
// Each is created once with the window and never replaced, so reading one takes
// no lock; each has its own locking for the state inside it. They are live, so a
// caller can drive them — which is the point, but it means a caller can also
// queue an injection or clear history behind the window's back.

// Scrollback is the window's raw output history.
func (w *Window) Scrollback() *Scrollback { return w.scrollback }

// InjectQueue is the queue of payloads waiting for an idle prompt.
func (w *Window) InjectQueue() *InjectQueue { return w.injectQueue }

// InputTracker records when input was last written, which is what the idle scan
// debounces against.
func (w *Window) InputTracker() *InputTracker { return w.inputTracker }

// IdleDetector decides whether the program is waiting at a prompt.
func (w *Window) IdleDetector() *IdleDetector { return w.idleDetector }

// Stash is the window's stash state, for a library caller that drives the
// stash-and-restore cycle itself; no daemon route reaches it. See StashState.
func (w *Window) Stash() *StashState { return w.stash }

// Dirty is notified after output is processed, so a client can wait for a change
// instead of polling. See Notifier for why a burst of output is one wakeup.
func (w *Window) Dirty() *Notifier { return w.dirty }

// ScrollbackLen is the number of lines in raw history. It is not a change
// signal, in either direction: eviction lowers it, and a full-screen program
// that redraws in place writes no newlines, so it advances only when the open
// line reaches sbMaxLine and is cut — once per 64 KiB of output rather than once
// per screen update. So it tracks neither updates nor bytes. Use OutputBytes to
// tell whether anything happened.
func (w *Window) ScrollbackLen() int { return w.scrollback.Len() }

// OutputBytes is every byte the program has written, including what history has
// evicted. Unlike the line count it advances whenever output arrives, so it is
// what a caller polls to see whether anything happened.
func (w *Window) OutputBytes() uint64 { return w.scrollback.TotalBytes() }

// wBytesPerLine is the byte bound a line-counted read of raw history gets per
// line: what such a read cost when a line ended at every PTY read.
const wBytesPerLine = 4 << 10

// wIdleScanBytes bounds the text the idle scan matches its pattern against. The
// scan runs while an injection is queued, so its cost is paid every 200 ms; five
// lines of a program that writes no '\n' would be 320 KiB of it, where five PTY
// reads were 20 KiB.
const wIdleScanBytes = 16 << 10

// idleScanLines is what the idle pattern is matched against: the end of raw
// history, bounded in bytes, each line reduced to what a terminal would show for
// it and carrying whether that reduction can be trusted.
//
// The trust is per line, not one verdict for the text, because most untrusted lines
// have nothing to do with the bound: any line carrying a sequence the render cannot
// model is untrusted wherever it sits. IsIdleScan reads back only as far as the
// newest line with text on it, so an untrusted line older than that one does not
// block an injection — which is the whole reason the verdict cannot be per read.
func idleScanLines(sb *Scrollback, lines int) []scanLine {
	text, cut := sb.Tail(lines, wIdleScanBytes)
	if !cut {
		return renderScanLines(text)
	}
	// The cut line is rendered on its own so that what the bound did to it can be
	// judged. Its render is trustworthy when the line erased its row after the cut:
	// nothing from before the cut can still be showing, so the text sits at the
	// columns the terminal shows it at. A bare '\r' is not enough — the text it
	// returns over may run past the redraw.
	//
	// Tail only cuts when a single line overran the bound on its own, so today the
	// text here holds no '\n'. Splitting rather than assuming that keeps the rest of
	// the lines rendered as lines if that ever changes; a whole read treated as one
	// line would render the '\n's as unmodelled controls and go untrusted, which
	// would block every injection until history scrolled past.
	first, rest := text, ""
	split := strings.IndexByte(text, '\n')
	if split >= 0 {
		first, rest = text[:split], text[split+1:]
	}
	rendered, exact, fromScratch := renderCR(first)
	out := []scanLine{{text: rendered, trusted: exact && fromScratch}}
	if split < 0 {
		return out
	}
	return append(out, renderScanLines(rest)...)
}

// wMaxReadBytes is the ceiling on one read of raw history, and what keeps
// lines × wBytesPerLine from overflowing for an absurd line count.
//
// It is deliberately separate from the daemon's maxReadBytes, which clamps what
// an HTTP client may ask for: this one clamps the default a window derives when
// the caller names no bound, so the Go API is bounded with no daemon in the
// picture. They are the same number for the same reason, and changing one is a
// reason to look at the other.
const wMaxReadBytes = 64 << 20

// readByteCap is the default byte bound for a read of lines lines: what that
// line count could return when a line of raw history ended at every PTY read, so
// a client that names no bound gets a response within one line of what the same
// request could have returned before lines were joined across reads. Not "no
// smaller than": measured on output that never ends a line, the default returns
// 2-4% fewer bytes than the old store did — Capture(50) 200 782 -> 197 051 bytes,
// ScrollRange(0,50) 204 849 -> 196 583 — because a line joined across reads is
// charged to the bound whole and the last one that does not fit is dropped. It is
// the bound the old store imposed by accident; naming it is what keeps joining
// lines from turning a fixed line count into an unbounded response.
func readByteCap(lines int) int {
	if lines > wMaxReadBytes/wBytesPerLine {
		return wMaxReadBytes
	}
	return lines * wBytesPerLine
}

// ScrollRange joins count lines of raw history starting offset lines after the
// oldest retained one, bounded at count × 4 KiB bytes. Use ScrollRangeBytes to
// choose the bound, or to learn that it dropped something.
func (w *Window) ScrollRange(offset, count int) string {
	text, _, _, _ := w.ScrollRangeBytes(offset, count, 0)
	return text
}

// ScrollRangeBytes is ScrollRange with an explicit byte bound, defaulting to
// count × 4 KiB when maxBytes is 0; truncated reports whether the bound dropped
// anything. Lines are dropped from the newest end, so a client paging forwards
// continues from where the page ended.
//
// lines is how many lines of history the text covers, which is what the next
// offset advances by; see Scrollback.RangeBytes for why a client cannot get it
// by counting '\n'. lineBytes is how long the line at offset renders, so a client
// that ended inside it — lineBytes > len(text) — knows the bound to ask for.
func (w *Window) ScrollRangeBytes(offset, count, maxBytes int) (text string, lines, lineBytes int, truncated bool) {
	if maxBytes == 0 {
		maxBytes = readByteCap(count)
	}
	return w.scrollback.RangeBytes(offset, count, maxBytes)
}

// LockVT locks and returns the grid; pair with UnlockVT.
//
// The read loop takes this lock for every chunk of output, so hold it for as
// little as possible: a caller that walks a large grid under it stalls the
// program's output for that long. Everything a client normally wants — a
// snapshot, a crop, a diff — is a method on Window that takes the lock itself.
// Use this only to read grid state those do not expose.
func (w *Window) LockVT() *VtGrid { w.vtMu.Lock(); return w.vt }

// UnlockVT releases the lock LockVT took. Defer it at the call site.
func (w *Window) UnlockVT() { w.vtMu.Unlock() }

// Capture returns the last lines of raw scrollback (default 50), bounded at
// lines × 4 KiB bytes, and the total line count. Use CaptureBytes to choose the
// bound, or to learn that it dropped something.
//
// The line count counts lines started, so it does not advance while output
// extends the last one; OutputBytes does. The last line's text can change
// between captures.
func (w *Window) Capture(lines int) (string, uint64) {
	text, _, _, total, _ := w.CaptureBytes(lines, 0)
	return text, total
}

// CaptureBytes is Capture with an explicit byte bound, defaulting to
// lines × 4 KiB when maxBytes is 0; truncated reports whether the bound dropped
// anything. Lines are dropped from the oldest end, so the newest output is
// always what a bounded capture returns.
//
// linesReturned is how many lines of history the text covers, so a client that
// wants the page before it can ask for that many more; totalLines counts lines
// started over the window's life, does not shrink as history is evicted, and is
// the count as of the same instant as the text — it never covers a line this
// response left out, whatever the program writes while it is being built.
// lineBytes is how long the newest line renders, which is the bound a client needs
// to see it whole; it is not totalBytes, which counts every byte the window ever
// wrote.
func (w *Window) CaptureBytes(lines, maxBytes int) (text string, linesReturned, lineBytes int, totalLines uint64, truncated bool) {
	if lines == 0 {
		lines = 50
	}
	if maxBytes == 0 {
		maxBytes = readByteCap(lines)
	}
	// One snapshot, not two reads: totalLines was sampled after the text, so a line
	// started in between was counted on a response that does not carry it, and a
	// client that banks the count never comes back for it. Reading them together
	// removes the gap rather than ordering it.
	return w.scrollback.LastNBytesWithTotal(lines, maxBytes)
}

// CaptureVisible renders the live grid as plain text: trailing spaces
// trimmed per row, trailing empty rows dropped, last lines rows kept (all
// when 0). Unlike Capture it reflects in-place redraws.
func (w *Window) CaptureVisible(lines int) string {
	w.vtMu.Lock()
	snap := w.vt.Snapshot()
	w.vtMu.Unlock()
	cols, rows := snap.Cols, snap.Rows
	if cols == 0 || rows == 0 {
		return ""
	}
	out := make([]string, 0, rows)
	var sb strings.Builder
	for r := 0; r < rows; r++ {
		sb.Reset()
		for c := 0; c < cols; {
			cell := snap.Cells[r*cols+c]
			if cell.WideSpacer() {
				c++
				continue
			}
			ch := cell.Ch
			if ch == '\t' && c < cols-1 {
				// A tab cell (see appendRune) reads back as a tab when the
				// cells up to the next multiple of 8 (the row's last column at
				// most) are still blank, so a reader expanding it to 8 columns
				// finds the text that follows where it is; otherwise it reads
				// as a space and text drawn over those cells since shows. A tab
				// in the last column moved nothing and reads as a space, as in
				// tmux 3.7c, which ignores it there.
				end := min((c/8+1)*8, cols-1)
				if tabSkipsBlanks(snap.Cells[r*cols+c+1 : r*cols+end]) {
					sb.WriteByte('\t')
					c = end
					continue
				}
			}
			if ch < 0x20 || ch == 0x7f {
				ch = ' '
			}
			sb.WriteRune(ch)
			// Each cell reads as its whole glyph, as the reference's does.
			for _, m := range snap.Marks(cell) {
				sb.WriteRune(m)
			}
			if cell.Wide() {
				c += 2
			} else {
				c++
			}
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}
	last := len(out)
	for last > 0 && out[last-1] == "" {
		last--
	}
	out = out[:last]
	take := len(out)
	if lines != 0 {
		take = min(lines, len(out))
	}
	return strings.Join(out[len(out)-take:], "\n")
}

// tabSkipsBlanks reports whether every cell a tab moved over is blank. A wide
// character's cell is never blank, and its spacer follows it; nor is a cell
// with marks.
func tabSkipsBlanks(cells []CellSlot) bool {
	for _, s := range cells {
		if s.Ch != ' ' && s.Ch != 0 || s.Attrs&attrMarks != 0 {
			return false
		}
	}
	return true
}

func (w *Window) deadErr() error { return fmt.Errorf("window %q is dead", w.Name()) }

// SendKeys types text, then optionally Enter. Enter is written at least the
// configured delay (enterDelayMS when non-nil) after the text's write
// finishes, so a TUI registers it as a submit rather than part of a paste
// burst; the text and its Enter are queued together, or not at all.
//
// Like every input method, SendKeys returns once the bytes are queued: a
// program that isn't reading doesn't block it. See ErrInputQueueFull.
func (w *Window) SendKeys(text string, enter bool, enterDelayMS *uint64) error {
	if !w.IsAlive() {
		return w.deadErr()
	}
	w.inputTracker.Touch()
	if w.stash.IsActive() {
		stashed := []byte(text)
		if enter {
			stashed = append(stashed, '\r')
		}
		// IsActive above keeps this allocation off the common path; the call
		// below is the check that decides. A false means the stash closed in
		// between, so fall through and type the text rather than dropping it.
		// The guard is not purely an optimisation -- a Begin landing right
		// after it answers false has its stash skipped and the text typed --
		// but both outcomes are legal, so it is safe either way. Note the
		// paths differ in how Enter is sent: stashed, it is appended
		// inline; typed, it is its own chunk after enterDelay.
		if w.stash.accumulateIfActive(stashed) {
			return nil
		}
	}
	if !enter {
		return w.input.push(inputChunk{data: []byte(text)})
	}
	d := w.injectCfg.SendKeysDelayMS
	if enterDelayMS != nil {
		d = *enterDelayMS
	}
	return w.input.push(inputChunk{data: []byte(text)}, enterAfter(enterDelay(d)))
}

// enterDelay converts a caller's millisecond gap, clamping instead of wrapping.
// The bound is maxStallMS because it is the same question — the largest number of
// milliseconds that survives the multiply into a Duration — and the same two
// silent answers past it: a value near 1.8e13 wraps to a sub-millisecond gap, and
// one near math.MaxUint64 wraps negative, which writeInput's `c.gap > 0` reads as
// no gap at all. So asking for a longer delay gets a shorter one, and asking for
// the longest gets none; the Enter lands immediately behind the text, which is
// the outcome a caller passing enter_delay_ms is specifically avoiding.
//
// It is then clamped to MaxInputGap, as shelld-rs's enter_gap is (window.rs:906):
// writeInput waits out each gap in order, so a longer delay parks the window's
// writer and everything queued behind it. The daemon refuses an enter_delay_ms
// past it, so what this bounds is a library caller's delay and SendKeysDelayMS,
// which would otherwise take every Enter past MaxQueuedGap and be refused for ever.
func enterDelay(ms uint64) time.Duration { return min(msDuration(ms), MaxInputGap) }

// enterAfter is an Enter written at least gap after the previous write.
func enterAfter(gap time.Duration) inputChunk {
	return inputChunk{data: []byte{'\r'}, gap: gap}
}

// SendRawKey writes key's bytes as given — control characters and escape
// sequences, not key names.
func (w *Window) SendRawKey(key string) error {
	if !w.IsAlive() {
		return w.deadErr()
	}
	w.inputTracker.Touch()
	if w.stash.accumulateIfActive([]byte(key)) {
		return nil
	}
	return w.input.push(inputChunk{data: []byte(key)})
}

// SendKeyNames sends tmux send-keys arguments: key names (Enter, C-c, Up,
// F5) encoded for the modes the program has set, and any other argument as
// text; see EncodeKey. With strict, an unknown or lossy name fails the call
// with a *KeyNameError and nothing is written. While a stash is active the
// encoded bytes are held and replayed later, after the modes may have
// changed.
func (w *Window) SendKeyNames(names []string, strict bool) error {
	data, err := EncodeKeyNames(names, w.KeyModes(), strict)
	if err != nil {
		return err
	}
	return w.typeInput(data)
}

// WriteInput forwards an attached client's keystrokes, or any bytes written
// as typed input. data is copied; the caller may reuse it.
func (w *Window) WriteInput(data []byte) error {
	return w.typeInput(bytes.Clone(data))
}

// typeInput is WriteInput for data the window may keep.
func (w *Window) typeInput(data []byte) error {
	if !w.IsAlive() {
		return w.deadErr()
	}
	w.inputTracker.Touch()
	if w.stash.accumulateIfActive(data) {
		return nil
	}
	return w.input.push(inputChunk{data: data})
}

// Resize pins a floor under the effective size (POST /resize). A size above
// MaxCols or MaxRows is refused and changes nothing.
func (w *Window) Resize(cols, rows uint16) error {
	if err := CheckSize(cols, rows); err != nil {
		return err
	}
	w.mu.Lock()
	w.pinned, w.pinCols, w.pinRows = true, cols, rows
	w.mu.Unlock()
	return w.RecomputeEffectiveSize()
}

// UnpinSize clears the pin; the size follows the attachers (or freezes when
// there are none).
func (w *Window) UnpinSize() {
	w.mu.Lock()
	w.pinned = false
	w.mu.Unlock()
	_ = w.RecomputeEffectiveSize()
}

// PinnedSize reports the pin, if any.
func (w *Window) PinnedSize() (cols, rows uint16, ok bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.pinCols, w.pinRows, w.pinned
}

// RegisterAttacher adds a voting client geometry, clamped to MaxCols×MaxRows;
// the caller must Release it.
func (w *Window) RegisterAttacher(cols, rows uint16) *Attacher {
	return w.registerAttacher(cols, rows, true)
}

// RegisterObserver adds a non-voting client geometry: it declares the size of
// this client's own cropped view and is left out of the min that sets the
// effective PTY size. A window whose only clients are observers keeps its
// size, exactly as one with no clients does.
func (w *Window) RegisterObserver(cols, rows uint16) *Attacher {
	return w.registerAttacher(cols, rows, false)
}

func (w *Window) registerAttacher(cols, rows uint16, voting bool) *Attacher {
	a := &Attacher{id: w.nextAttID.Add(1), voting: voting, window: w}
	a.size.Store(packSize(cols, rows))
	w.attMu.Lock()
	w.attachers = append(w.attachers, a)
	n := len(w.attachers)
	w.attMu.Unlock()
	if voting {
		_ = w.RecomputeEffectiveSize()
	}
	ec, er := w.Size()
	slog.Debug("attacher registered", "attacher_id", a.id, "voting", voting,
		"declared_cols", cols, "declared_rows", rows,
		"effective_cols", ec, "effective_rows", er, "total_attachers", n)
	return a
}

// AttacherCount is the number of registered attachers.
func (w *Window) AttacherCount() int {
	w.attMu.Lock()
	defer w.attMu.Unlock()
	return len(w.attachers)
}

// RecomputeEffectiveSize applies max(pin, min over voting attachers) floored
// at 20×5 and capped at MaxCols×MaxRows; with neither, the size is left
// unchanged. Observers are not voting attachers, so a window with only
// observers takes the no-attacher branch — the min is never taken over an
// empty set and never collapses to the floor.
func (w *Window) RecomputeEffectiveSize() error {
	w.sizeMu.Lock()
	defer w.sizeMu.Unlock()

	w.attMu.Lock()
	haveMin := false
	var minCols, minRows uint16
	for _, a := range w.attachers {
		if !a.voting {
			continue
		}
		c, r := a.Size()
		if !haveMin || c < minCols {
			minCols = c
		}
		if !haveMin || r < minRows {
			minRows = r
		}
		haveMin = true
	}
	w.attMu.Unlock()

	w.mu.RLock()
	pinned, pc, pr := w.pinned, w.pinCols, w.pinRows
	curCols, curRows := w.cols, w.rows
	w.mu.RUnlock()

	var cols, rows uint16
	switch {
	case haveMin && pinned:
		cols, rows = max(minCols, pc, minEffectiveCols), max(minRows, pr, minEffectiveRows)
	case haveMin:
		cols, rows = max(minCols, minEffectiveCols), max(minRows, minEffectiveRows)
	case pinned:
		cols, rows = max(pc, minEffectiveCols), max(pr, minEffectiveRows)
	default:
		return nil
	}
	cols, rows = min(cols, MaxCols), min(rows, MaxRows)
	if cols == curCols && rows == curRows {
		return nil
	}
	return w.setEffectiveSize(cols, rows)
}

func (w *Window) setEffectiveSize(cols, rows uint16) error {
	// Counted here rather than at any caller: this is the only place a grid's
	// size actually changes, and every path reaches it through
	// RecomputeEffectiveSize's equality short-circuit, so a count here is a count
	// of real reflows. One line earlier — in the recompute, or in Window.Resize —
	// would count resize *requests*, which is the number the investigation this
	// counter serves exists to distrust. See reflow_stats.go.
	countReflow(cols, rows)

	w.mu.Lock()
	w.cols, w.rows = cols, rows
	w.mu.Unlock()

	w.vtMu.Lock()
	w.vt.Resize(int(cols), int(rows))
	w.vtMu.Unlock()

	w.ptyMu.Lock()
	var err error
	if w.pty != nil && !w.ptyClosed {
		if rerr := resizePTY(w.pty, pty.Size{Cols: cols, Rows: rows}); rerr != nil {
			err = errors.New("pty resize")
		}
	}
	w.ptyMu.Unlock()
	// The grid has the new size whether or not the PTY took it, so clients are
	// woken either way, the error return included: no tick follows a frame
	// (internal/daemon's idleWait), so a client left unwoken would hold the old
	// geometry until the idle grid's next instant.
	w.dirty.NotifyWaiters()
	if err != nil {
		return err
	}
	slog.Debug("pty effective size updated", "cols", cols, "rows", rows)
	return nil
}

// resizePTY resizes the PTY, or fails on demand when a test asked it to: no
// real PTY fails when told to, and a failed resize (ConPTY is the plausible
// one) must still wake clients.
func resizePTY(p *pty.PTY, size pty.Size) error {
	if f := testResizePTY.Load(); f != nil {
		return (*f)()
	}
	return p.Resize(size)
}

// testResizePTY stands in for the PTY resize when a test sets it.
var testResizePTY atomic.Pointer[func() error]

// RenderFrame is a self-contained ANSI repaint of the live grid.
func (w *Window) RenderFrame() []byte {
	w.vtMu.Lock()
	defer w.vtMu.Unlock()
	return w.vt.RenderFrame()
}

// SetColors sets the colors reported for OSC 4/10/11/12 queries.
func (w *Window) SetColors(entries map[int]vt.RGB) {
	w.vtMu.Lock()
	colors := w.vt.Colors()
	w.vtMu.Unlock()
	for idx, rgb := range entries {
		colors.Set(idx, rgb)
	}
}

// Kill terminates the shell and its whole process tree, then closes the PTY.
func (w *Window) Kill() {
	if tracing.Enabled {
		span := tracing.Begin(tracing.KindWindowKill, w.traceID, 0)
		defer tracing.End(tracing.KindWindowKill, w.traceID, span, 0, 0)
	}
	// Kill never waits on a write: queued input is dropped and the master
	// is closed without taking a lock the writer holds, which releases a
	// write parked on a program that isn't reading.
	if CurrentKillMode() == KillHangup {
		w.killHangup()
		return
	}
	w.killMu.Lock()
	defer w.killMu.Unlock()
	w.stopDrain()
	w.input.close(w.deadErr())
	w.ptyMu.Lock()
	p := w.pty
	w.pty, w.ptyClosed = nil, true
	w.ptyMu.Unlock()
	if p != nil {
		w.mu.RLock()
		start := w.pidStart
		w.mu.RUnlock()
		reapTree(p, start)
		w.awaitExit(p)
		w.closePTY(p)
	}
	// After the union, and after the wait, for two different reasons. After the
	// union because the union has to run while its anchor is still valid: it walks
	// /proc from the shell's pid, and that pid names the shell only until waitLoop
	// reaps it, whereas the cgroup is a directory and is still exactly this
	// window's whenever we get here. After the wait because Remove needs the
	// cgroup empty, and the shell is a member of it.
	//
	// It also runs when p was nil — a second Kill, or a window whose PTY is
	// already closed — because the directory outlives the pty and something has to
	// remove it.
	w.reapCgroup()
	w.mu.Lock()
	w.alive = false
	w.mu.Unlock()
	// Wake attached tick loops so the window's last state and its death reach
	// clients at once; the read loop's own notify may already be past.
	w.dirty.NotifyWaiters()
}

// killHangup is Kill under SHELLD_KILL_MODE=hangup: tmux's kill-window
// (server_kill_window -> window_pane_destroy, which closes the pane's master fd
// and nothing else). Closing the master makes the kernel hang up the slave, which
// sends SIGHUP (and SIGCONT) to the session leader -- the shell, which pty.Start
// made one -- and an interactive shell passes the SIGHUP on to its jobs before it
// exits. It returns without waiting for any of that: the shell is reaped by
// waitLoop whenever it exits, which is what tmux's server_child_signal does, so
// no zombie is left and nothing here blocks on it.
//
// What it does NOT do, on purpose, is everything the default Kill adds: no
// /proc scan, no SIGKILL, no wait, no cgroup.kill. A process that ignores SIGHUP
// or has left the terminal's session survives, as it survives tmux. The window's
// cgroup is handed to reapCgroupWhenEmpty, which removes it once the last member
// exits (immediately, if it is already empty), without blocking the caller.
func (w *Window) killHangup() {
	w.killMu.Lock()
	defer w.killMu.Unlock()
	w.stopDrain()
	w.input.close(w.deadErr())
	w.ptyMu.Lock()
	p := w.pty
	w.pty, w.ptyClosed = nil, true
	w.ptyMu.Unlock()
	if p != nil {
		w.closePTY(p)
	}
	// Taken off the window under the lock releaseCgroup also takes, so the read
	// loop's EOF teardown -- which the close above triggers -- finds nil and leaves
	// the directory to the reaper. A second Kill finds nil too.
	w.mu.Lock()
	c := w.cg
	w.cg = nil
	w.alive = false
	w.mu.Unlock()
	reapCgroupWhenEmpty(c)
	w.dirty.NotifyWaiters()
}

// killWait bounds each of Kill's waits for the window's process to exit.
var killWait = 2 * time.Second

// reapTree is reapChildTree; a test replaces it to model a kill that cannot land.
var reapTree = reapChildTree

// awaitExit waits for waitLoop to reap the window's process after the tree
// kill, and does not wait forever. The SIGKILL can fail: a root that became
// another user (an authenticated `sudo -i` or pkexec under an unprivileged
// daemon) answers it with EPERM. Closing the master is then the lever left,
// because the kernel hangs up the terminal's session whatever its uid. A root
// that survives that too is left running: Kill holds killMu, and both DELETE
// routes are waiting on it.
func (w *Window) awaitExit(p *pty.PTY) {
	select {
	case <-w.done:
		return
	case <-time.After(killWait):
	}
	slog.Warn("window process survived SIGKILL; hanging up its terminal", "window", w.Name(), "pid", w.Pid())
	w.closePTY(p)
	select {
	case <-w.done:
	case <-time.After(killWait):
		slog.Warn("window process survived its terminal's hangup; leaving it", "window", w.Name(), "pid", w.Pid())
	}
}

// reapChildTree kills the PTY child and its descendants, falling back to the
// direct child whenever the tree kill named nothing.
//
// "Descendants" is TWO sets, not the parent-link closure alone: pty.Start makes the
// child a session leader, so the members of its session are an answer too, and that
// is the id that survives a reparent. A process that setsid'd away is found by its
// parent link while that link is intact; a process orphaned by an exiting launcher
// has no such link and is found by the session it never left. There is no
// process-group set — an earlier version of this comment said there were three, and
// the group term was removed because setpgid(child, sibling) lets a shell build a
// group whose leader never forked the other members, so a group test against such a
// leader names strangers. See proctree.unionDescendants for the full argument.
//
// start comes from the window, recorded when the shell was still unreaped. KillTree
// refuses a root whose start time no longer matches, which is the reaped-and-reused
// case: Window.Kill is reachable after waitLoop has already reaped the shell, since
// nothing removes an exited window from Session.windows.
//
// The fallback fires on an empty answer rather than only on ok == false, so a
// refusal is not silently a no-op — which is what made darwin's dead oracle
// survivable, since KillTree there returned the empty set and true.
//
// p.Kill is the right thing to be left with, and how much it is worth depends on the
// platform. It never signals a bare pid: on unix it goes through the os.Process
// handle, so it cannot hit a recycled pid, but it reaches ONE process and a
// descendant that setsid'd away outlives it. On Windows it terminates the job object
// the child was assigned to before it ran, so the fallback is itself a whole-tree
// kill and loses only the list of pids. The weakest case is therefore a unix with no
// implementation in proctree, not Windows.
//
// On the reaped-root path named above it reaches NOTHING, and that is worth stating
// next to "the right thing to be left with", which reads as a floor and is not one.
// os.Process.Kill on a process the caller has already waited for is ErrProcessDone
// without a signal being sent, and the refusal that leads here is precisely the case
// where waitLoop got there first. So: tree kill refused, fallback a no-op, and
// closePTY's SIGHUP ignorable by the holder that is keeping the slave open. What
// covers it is the cgroup arm in Window.reapCgroup, which is why that arm running
// unconditionally matters and why a failed cgroup.kill puts the handle back rather
// than dropping it. A window with no cgroup — a library embedder, or a host where
// cgroup v2 probing declined — has nothing left, and the error below is then the
// only evidence that the kill did not reach its tree.
//
// The error is logged rather than returned because this path is routine: killing a
// window whose shell has already exited takes it every time, and the window really
// is gone in the ordinary case. It is only the holder case that leaks, and nothing
// here can distinguish the two.
func reapChildTree(p *pty.PTY, start uint64) {
	root := p.Pid()
	killed, ok := proctree.KillTree(root, start)
	if ok && len(killed) > 0 {
		slog.Debug("reaped PTY child process tree", "root", root, "count", len(killed), "pids", killed)
		return
	}
	// "the direct child" is the unix reading; on Windows this ends the job. The
	// message says what was asked for rather than what it reaches, because what it
	// reaches is pty.PTY.Kill's business.
	slog.Debug("tree kill named nothing; falling back to pty.Kill",
		"root", root, "supported", ok, "start", start)
	if err := p.Kill(); err != nil {
		slog.Debug("pty.Kill reached nothing either",
			"root", root, "err", err)
	}
}
