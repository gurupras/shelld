// Command shelld is the long-lived shell-session daemon.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/internal/daemon"
	"github.com/gurupras/shelld/internal/dotenv"
	"github.com/gurupras/shelld/internal/tracing"
)

const usage = `shelld — long-lived shell-session daemon.

Usage: shelld [OPTIONS] [COMMAND]

Commands:
  run        Serve in the foreground (default)
  daemonize  Start a detached daemon and wait for /health (exit 2 still starting,
             5 something unverified answers)
  status     Report whether the daemon is healthy (exit 3 stale, 4 not running,
             5 something answers that the lockfile does not verify)
  stop       Shut the daemon down gracefully
  restart    stop, then daemonize
  kill       Kill the daemon (requires --force)
  logs       Print the tail of the log file (--lines N, default 100)
  sessions   List sessions (exit 4 nothing answers, 6 the answer is unusable)
  windows    List windows in a session: shelld windows <SESSION> (exit 1 no such
             session, 4 nothing answers, 6 the answer is unusable)

Options:
      --socket <SOCKET>
      --pidfile <PIDFILE>
      --logfile <LOGFILE>
      --shell <SHELL>  (run, daemonize) shell command line for a window whose
                       create request names none, overriding $SHELL. Split on
                       whitespace, so it may carry arguments. Without it the
                       window uses $SHELL when executable, else the passwd
                       entry's shell, else /bin/sh.
      --shared-access  (run, daemonize) let other users reach the daemon (Unix:
                       socket mode 0666; Windows: authenticated users, and a
                       non-elevated process reaching an elevated daemon).
                       By default only the daemon's user can. Go clients
                       (shelld, shellctl) of another user still refuse it:
                       they check the socket's owner.
  -h, --help     Print help
  -V, --version  Print version
`

type cli struct {
	socket, pidfile, logfile string
	shell                    string
	socketExplicit           bool // --socket or SHELLD_SOCKET was given
	pidfileExplicit          bool // --pidfile or SHELLD_PIDFILE was given
	command                  string
	force                    bool
	sharedAccess             bool
	lines                    int
	session                  string

	// unvouched: the pidfile was the only name given and its lockfile is absent,
	// so socket is the default one and nothing the operator named points at it.
	// Set by socketFromLockfile.
	unvouched bool
}

func usageError(msg string) { usageErrorFor(msg, "shelld [OPTIONS] [COMMAND]") }

func usageErrorFor(msg, usageLine string) {
	fmt.Fprintf(os.Stderr, "error: %s\n\nUsage: %s\n\nFor more information, try '--help'.\n", msg, usageLine)
	os.Exit(2)
}

// parseArgs accepts the global flags before or after the subcommand, in
// `--flag value` or `--flag=value` form.
func parseArgs(args []string) cli {
	c := cli{lines: 100}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		takeValue := func() string {
			if hasVal {
				return val
			}
			if i+1 >= len(args) {
				usageError(fmt.Sprintf("a value is required for '%s <%s>' but none was supplied", name, strings.ToUpper(strings.TrimPrefix(name, "--"))))
			}
			i++
			return args[i]
		}
		// A switch takes no value, so `--shared-access=false` must not read
		// as `--shared-access`.
		noValue := func() {
			if hasVal {
				usageError(fmt.Sprintf("unexpected value '%s' for '%s' found; no more were expected", val, name))
			}
		}
		switch name {
		case "-h", "--help", "help":
			if name == "help" && len(positional) > 0 {
				positional = append(positional, a)
				continue
			}
			fmt.Print(usage)
			os.Exit(0)
		case "-V", "--version":
			fmt.Printf("shelld %s\n", shelld.Version)
			os.Exit(0)
		case "--socket":
			c.socket = takeValue()
		case "--pidfile":
			c.pidfile = takeValue()
		case "--logfile":
			c.logfile = takeValue()
		case "--shell":
			c.shell = takeValue()
		case "--force":
			noValue()
			c.force = true
		case "--shared-access":
			noValue()
			c.sharedAccess = true
		case "--lines":
			n, err := strconv.ParseUint(takeValue(), 10, 63)
			if err != nil {
				usageError("invalid value for '--lines <LINES>': invalid digit found in string")
			}
			c.lines = int(n)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				usageError(fmt.Sprintf("unexpected argument '%s' found", a))
			}
			positional = append(positional, a)
		}
	}
	c.command = "run"
	if len(positional) > 0 {
		c.command = positional[0]
	}
	switch c.command {
	case "run", "daemonize", "status", "stop", "restart", "sessions":
		if len(positional) > 1 {
			usageError(fmt.Sprintf("unexpected argument '%s' found", positional[1]))
		}
	case "kill", "logs":
		if len(positional) > 1 {
			usageError(fmt.Sprintf("unexpected argument '%s' found", positional[1]))
		}
	case "windows":
		if len(positional) < 2 {
			usageErrorFor("the following required arguments were not provided:\n  <SESSION>", "shelld windows <SESSION>")
		}
		c.session = positional[1]
	default:
		usageError(fmt.Sprintf("unrecognized subcommand '%s'", c.command))
	}
	if c.sharedAccess && c.command != "run" && c.command != "daemonize" {
		usageErrorFor("unexpected argument '--shared-access' found", "shelld "+c.command+" [OPTIONS]")
	}
	// Rejected rather than ignored on the subcommands that cannot act on it, for
	// the reason --shared-access is: a flag only a serving process can honour,
	// given to `shelld stop`, was meant for a daemon that is not this invocation,
	// and accepting it silently would report success for a shell that never
	// changed. restart is allowed where --shared-access is not, because restart
	// runs cmdDaemonize with this same cli and so does carry the flag through --
	// the --shared-access line below it is stricter than its own plumbing needs.
	if c.shell != "" && c.command != "run" && c.command != "daemonize" && c.command != "restart" {
		usageErrorFor("unexpected argument '--shell' found", "shelld "+c.command+" [OPTIONS]")
	}
	if c.force && c.command != "kill" {
		usageErrorFor("unexpected argument '--force' found", "shelld "+c.command+" [OPTIONS]")
	}
	c.socketExplicit = c.socket != "" || os.Getenv("SHELLD_SOCKET") != ""
	if c.socket == "" {
		c.socket = daemon.DefaultSocket()
	}
	// The lockfile records the socket, and every command compares its own
	// against that record as a string. A relative path names a different file
	// from each directory, so `shelld stop --socket s.sock` run elsewhere would
	// agree with the lockfile and signal a daemon it never dialled. A Windows
	// socket is a pipe name, not a path.
	if runtime.GOOS != "windows" {
		if abs, err := filepath.Abs(c.socket); err == nil {
			c.socket = abs
		}
	}
	c.pidfileExplicit = c.pidfile != "" || os.Getenv("SHELLD_PIDFILE") != ""
	if c.pidfile == "" {
		c.pidfile = daemon.DefaultPidfile()
	}
	if c.logfile == "" {
		c.logfile = daemon.DefaultLogfile()
	}
	return c
}

func main() {
	_ = dotenv.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	c := parseArgs(os.Args[1:])
	if err := checkPaths(c); err != nil {
		fmt.Fprintf(os.Stderr, "shelld: %v\n", err)
		os.Exit(1)
	}
	switch c.command {
	case "status", "stop", "restart", "kill":
		var err error
		if c, err = socketFromLockfile(c); err != nil {
			fmt.Fprintf(os.Stderr, "shelld: %v\n", err)
			os.Exit(1)
		}
	}
	var code int
	switch c.command {
	case "run":
		code = cmdRun(c)
	case "daemonize":
		code = cmdDaemonize(c)
	case "status":
		code = cmdStatus(c)
	case "stop":
		code = cmdStop(c)
	case "restart":
		if code = cmdStop(c); code == 0 {
			code = cmdDaemonize(c)
		}
	case "kill":
		code = cmdKill(c)
	case "logs":
		code = cmdLogs(c)
	case "sessions":
		code = cmdSessions(c)
	case "windows":
		code = cmdWindows(c)
	}
	os.Exit(code)
}

// checkPaths guards the per-user default directory before anything uses a
// path in it: run, daemonize and restart create it, every other command only
// checks it, and all of them refuse a directory another user could control.
// For an explicit socket elsewhere, those three warn when its directory lets
// others replace it.
//
// restart is one of the three because its second half is cmdDaemonize, which
// opens the log file itself before it spawns anything. Left out of this list it
// failed at "open log ...: no such file or directory" on a box where the
// directory is not there yet, rather than starting a first daemon — which is
// what removeStale's comment below says it is meant to be able to do — and it
// was the one command that could create a socket in a directory others can
// replace without saying so.
func checkPaths(c cli) error {
	creates := c.command == "run" || c.command == "daemonize" || c.command == "restart"
	dir := daemon.DefaultDir()
	for _, p := range []string{c.socket, c.pidfile, c.logfile} {
		if filepath.Dir(p) != dir {
			continue
		}
		var err error
		if creates {
			err = daemon.EnsurePrivateDir(dir)
		} else if err = daemon.CheckPrivateDir(dir); errors.Is(err, os.ErrNotExist) {
			err = nil // nothing there to connect to or read
		}
		if err != nil {
			return err
		}
		break
	}
	if creates && filepath.Dir(c.socket) != dir {
		if w := daemon.UnsafeParentWarning(c.socket); w != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
	}
	return nil
}

// exitAlreadyServing is run's exit code when another daemon holds the
// socket, so daemonize can tell a lost race from a failure.
const exitAlreadyServing = 3

func cmdRun(c cli) int {
	// Recorded before the Windows chdir below leaves this directory, and used
	// for a window that names no cwd of its own. Empty on error, which leaves
	// WindowOpts's own $HOME default in place.
	startDir, _ := os.Getwd()
	if runtime.GOOS == "windows" {
		leaveWorkingDirectory(&c)
	}
	lock, err := daemon.AcquireInstance(c.socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shelld: %v\n", err)
		if errors.Is(err, daemon.ErrInstanceHeld) {
			return exitAlreadyServing
		}
		return 1
	}
	// Released on return, after Serve has removed the lockfile and socket.
	defer lock.Release()
	// After AcquireInstance, so that exactly one daemon per socket takes a cgroup
	// base and runs the GC over stale ones: a loser that is about to exit must not
	// collect anything. It returns false on a host with no usable cgroup v2, which
	// is not an error — window kill then uses the /proc union alone, exactly as it
	// did before this existed. It is called here and not in the library because
	// taking a process-wide directory is a decision for a program, not a package.
	shelld.InitCgroups()
	defer shelld.CloseCgroups()
	// Concurrently, because the expansion blocks for a full RCU grace period --
	// 12-25 ms, see InitDescriptorTable -- and whatever the listen waits behind is
	// waited out by every caller that waits for this daemon to come up. Three do:
	// daemonize polls /health until daemonizeWait expires, restart goes through
	// daemonize, and the benchmark harnesses kept outside this repo define their
	// startup figure as exec to the first 200 from /health.
	//
	// 225752c ran it here and ahead of the listen, on the reasoning that nothing is
	// waiting on a daemon that has not bound yet. The three callers above are, and
	// that placement put the whole grace period on the figure they report: 3.4 ms to
	// 18.4 ms exec-to-listen on one linux box, restored by this keyword alone.
	//
	// So the trade is not "free because it is early". It is ~20 ms once, inside
	// whichever window create first takes a descriptor past 64, against a ~16 ms
	// window at the start of this process in which the table is still small and an
	// early create can cross 64 organically and pay it anyway. The expansion is
	// still worth making -- the table never shrinks, so one grace period covers this
	// process's whole life -- it is just not worth putting the listener behind it.
	//
	// Nothing needs to join it. reserveDescriptorTable refuses when the top slot is
	// already in use and dups with O_CLOEXEC for exactly the racing-create case, and
	// every failure it can hit is a logged decline rather than fatal, so the worst a
	// lost race costs is the occasional 20 ms create this is trying to avoid.
	//
	// Here and not in the library for InitCgroups' reason above: resizing a
	// process-wide table is a decision for a program.
	go shelld.InitDescriptorTable()
	// Before the bind, because from the bind onward a client's connect succeeds:
	// the address is live while a default-disposition SIGTERM would still end
	// this process at once, leaving behind the socket file and the lockfile and
	// skipping the cgroup base release and tracing.Close. Not the instance lock,
	// which the kernel releases with any other F_SETLK record lock when the
	// process exits.
	//
	// As low as the bind allows, because a signal caught between here and Serve
	// is absorbed rather than acted on, so this window wants to be short and
	// bounded. What is in it: Listen, whose stale-socket check dials with a
	// 250ms timeout and re-probes a refusal three more times -- about a second,
	// and only when a socket file is already on disk -- then a pidfile write.
	// Everything that can block without a bound stays above this line, notably
	// InitCgroups, which spawns a probe child and waits for it.
	sigCtx, stopSignals := daemon.NotifyShutdown()
	defer stopSignals()
	ln, err := daemon.Listen(c.socket, daemon.ListenOptions{SharedAccess: c.sharedAccess})
	if err != nil {
		fmt.Fprintf(os.Stderr, "shelld: %v\n", err)
		if errors.Is(err, daemon.ErrInstanceHeld) {
			return exitAlreadyServing
		}
		return 1
	}
	// The lockfile is written only once this process owns the socket, so a
	// daemon that lost a race never overwrites or removes the winner's.
	pid := os.Getpid()
	state := daemon.NewState(c.socket)
	state.DefaultCwd = startDir
	state.DefaultShell = c.shell
	state.Nonce = daemon.NewNonce()
	info := daemon.LockInfo{Pid: int32(pid), Socket: c.socket, Version: daemon.Version, StartedAt: time.Now().Unix(), Nonce: state.Nonce}
	// Without its lockfile no client can verify this daemon (CheckNonce), so
	// it would read as unverified for its whole life: fail instead.
	if err := daemon.WriteLockfile(c.pidfile, info); err != nil {
		fmt.Fprintf(os.Stderr, "shelld: lockfile write failed: %v\n", err)
		daemon.CloseListener(ln)
		return 1
	}
	err = daemon.Serve(sigCtx, ln, c.socket, state, func() { daemon.RemoveLockfileIfPid(c.pidfile, pid) })
	// Serving is over, so the signals go back to ending the process: a second
	// SIGTERM to a daemon already shutting down should not be swallowed. Without
	// this, tracing.Close below would run with both still absorbed, since it is a
	// plain call and the defer above only runs after it. The deferred releases
	// are already covered -- stopSignals was registered last, so it runs first.
	stopSignals()
	tracing.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		return 1
	}
	return 0
}

// leaveWorkingDirectory moves the daemon out of the directory it was
// started from: Windows refuses to delete or rename a directory that a
// process uses as its working directory, so a daemon started inside a
// worktree would block removing it. The pidfile and logfile become absolute
// first, and .env was already loaded from the starting directory. A window's
// relative cwd then resolves against the home directory.
func leaveWorkingDirectory(c *cli) {
	for _, p := range []*string{&c.pidfile, &c.logfile} {
		if abs, err := filepath.Abs(*p); err == nil {
			*p = abs
		}
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = os.TempDir()
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: chdir %s: %v\n", dir, err)
	}
}

// daemonizeWait is how long daemonize waits for /health while the spawned
// daemon is alive; SHELLD_DAEMONIZE_WAIT (a Go duration) overrides it, for a
// first run that antivirus scanning delays.
func daemonizeWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("SHELLD_DAEMONIZE_WAIT")); err == nil && d > 0 {
		return d
	}
	return 30 * time.Second
}

// runArgs is the argv cmdDaemonize spawns `shelld run` with. Every flag the
// serving process acts on has to be repeated here, because the child is a fresh
// process that parses its own command line and inherits nothing from this one:
// a flag left out is accepted by `shelld daemonize`, reported as a successful
// start, and then absent from the daemon that actually serves.
//
// A value is a separate argv entry rather than --flag=value so that one
// carrying spaces -- `--shell "bash -l"` -- survives; SpawnDetached execs the
// binary directly, with no shell to re-split it.
//
// Split out from cmdDaemonize so the forwarding can be asserted without
// spawning a daemon, which is the only way to observe it otherwise.
func runArgs(c cli) []string {
	args := []string{"run", "--socket", c.socket, "--pidfile", c.pidfile, "--logfile", c.logfile}
	if c.sharedAccess {
		args = append(args, "--shared-access")
	}
	if c.shell != "" {
		args = append(args, "--shell", c.shell)
	}
	return args
}

// cmdDaemonize removes nothing: a stale lockfile is overwritten by the new
// daemon once it listens, and a stale socket is replaced under its instance
// lock, so a live daemon that was slow to answer keeps its files.
func cmdDaemonize(c cli) int {
	if info, err := daemon.Probe(c.socket, 500*time.Millisecond); err == nil {
		return alreadyRunning(c, info)
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "executable lookup: %v\n", err)
		return 1
	}
	proc, err := daemon.SpawnDetached(bin, runArgs(c), c.logfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		return 1
	}
	pid := proc.Pid
	exited := make(chan spawnResult, 1)
	go func() {
		st, err := proc.Wait()
		r := spawnResult{reaped: true, err: err}
		if err == nil {
			r.code = st.ExitCode()
		}
		exited <- r
	}()
	// reaped asks what is known about the child now. The channel holds one value
	// and is never re-filled, so a read that finds nothing means the child had not
	// been reaped at that moment, which is the answer this wants -- the deadline is
	// the caller's patience and a blocking read here would ignore it.
	reaped := func() spawnResult {
		select {
		case r := <-exited:
			return r
		default:
			return spawnResult{}
		}
	}
	lostRace := false
	deadline := time.Now().Add(daemonizeWait())
	for time.Now().Before(deadline) {
		if info, err := daemon.Probe(c.socket, 250*time.Millisecond); err == nil {
			return daemonized(c, info, pid)
		}
		if r := reaped(); r.reaped {
			if !r.lostSocket() {
				return report(daemonizeGaveUp(r, false, pid, c))
			}
			// Another daemon holds the socket; wait for it to answer.
			lostRace = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Asked again, and this time waited for. The child can exit during the last
	// sleep or while the deadline is being crossed, and nothing reads the channel
	// in that window, so without this a child known to be dead is reported as
	// still starting -- and the two ask the caller for opposite things, one to wait
	// longer and one to go and read the log. The wait is what makes the answer a
	// fact rather than whatever the scheduler had got round to.
	var last spawnResult
	select {
	case r := <-exited:
		last = r
	case <-time.After(daemonizeReapGrace):
	}
	return report(daemonizeGaveUp(last, lostRace, pid, c))
}

// daemonizeReapGrace is how long daemonize waits for the spawned child's exit
// status once its deadline has passed. It is paid only on the path that has
// already waited out daemonizeWait, where it is not measurable, and it buys the
// difference between "it is gone" and "it is still starting".
const daemonizeReapGrace = 500 * time.Millisecond

// spawnResult is what the parent knows about the process daemonize spawned. The
// zero value says nothing has been reaped, so the three states are distinct:
// still running, exited with a status, and reaped with no status to report.
type spawnResult struct {
	reaped bool
	code   int   // its exit status, meaningful only when reaped and err is nil
	err    error // why there is no status, when the wait itself failed
}

// lostSocket reports whether the child exited saying another daemon already
// holds the socket. It is the one exit worth waiting past: the daemon that won
// the race is expected to answer shortly.
func (r spawnResult) lostSocket() bool {
	return r.reaped && r.err == nil && r.code == exitAlreadyServing
}

// daemonizeGaveUp says what daemonize reports when the socket never answered.
// It is pure so that each of its four outcomes is reachable in a test without
// spawning a process, waiting out a deadline, or arranging for a wait to fail.
//
// The distinction the exit code carries is whether waiting longer could help.
// Exit 2 says the child is alive and still starting, so a caller may retry;
// exit 1 says it is gone, so the caller should read the log instead. Reporting
// a dead child as still starting sends a script round a loop that can only time
// out, which is why the caller asks a second time after its deadline.
func daemonizeGaveUp(r spawnResult, lostRace bool, pid int, c cli) (string, int) {
	switch {
	case r.reaped && r.err != nil:
		// No status was obtained, so none is invented. This used to print
		// "status -1", which is not an exit status and reads as one.
		return fmt.Sprintf("shelld run (pid %d) is gone and its status could not be read (%v); see %s\n", pid, r.err, c.logfile), 1
	case r.lostSocket() || lostRace:
		return fmt.Sprintf("shelld is running on %s but /health did not respond\n", c.socket), 1
	case r.reaped:
		return fmt.Sprintf("shelld run (pid %d) exited with status %d; see %s\n", pid, r.code, c.logfile), 1
	default:
		return fmt.Sprintf("shelld spawned (pid %d) and is still starting; /health did not respond on %s\n", pid, c.socket), 2
	}
}

// report writes a decision's message and hands back its exit code, so the
// decision can be a value a test reads rather than output it has to parse.
func report(msg string, code int) int {
	fmt.Fprint(os.Stderr, msg)
	return code
}

// daemonized reports a daemon answering after a daemonize. The nonce is checked
// whoever answered, because the pid in a /health response is a number the
// answering process chose: matching it against the pid that was spawned
// establishes nothing about whether that process is the daemon, and once the
// spawned child is known to have exited the pid does not even name a live
// process. So the pid decides only which sentence to print, and the nonce
// decides the exit code.
func daemonized(c cli, info *daemon.LockInfo, pid int) int {
	if int(info.Pid) != pid {
		return alreadyRunning(c, info)
	}
	if refuseUnverified(c, info) {
		return exitUnverified
	}
	fmt.Fprintf(os.Stderr, "shelld daemonized (pid %d, socket %s)\n", pid, c.socket)
	return 0
}

// socketFromLockfile makes commands that act on a running daemon use the
// socket its lockfile records when no socket was given, so
// `kill --force --pidfile b.pid` reaches the daemon that wrote b.pid rather
// than whatever serves the default socket. A socket that was given and
// differs from the lockfile's is refused.
//
// A lockfile that cannot be read refuses, when it was the only thing that could
// have answered. Returning c unchanged on that arm returns the default socket --
// the value this function exists to replace -- so the command would go on to act
// on whichever daemon serves it, which for stop and kill means shutting down or
// killing a daemon other than the one named. That arm also falls past the refusal
// below, so the single guard against acting on the wrong daemon was skipped by
// the same condition. It is not a wrong message: the command succeeds, reports
// success, and has acted on something else.
//
// With --socket given explicitly the target is known and only the cross-check
// against the lockfile is lost, so that case proceeds and each command's own
// verification reports what it can. Refusing there would reject a request that
// names exactly what to act on.
//
// An absent lockfile the operator named, with no --socket, leaves the default
// socket in c, which is a daemon nothing the operator named points at. It is
// not refused here, because an absent lockfile is also what a successful stop
// leaves and stop has to stay idempotent; c.unvouched carries it to the
// commands instead, as shelld-rs's Vouch::No does (socket_from_lockfile in its
// main.rs).
func socketFromLockfile(c cli) (cli, error) {
	lock := daemon.ReadLock(c.pidfile)
	switch lock.AimsAt(c.socket) {
	case daemon.LockfileAbsent:
		c.unvouched = c.pidfileExplicit && !c.socketExplicit
	case daemon.LockfileUnreadable:
		if c.socketExplicit {
			return c, nil
		}
		return c, fmt.Errorf("%w: %s: %v (it may name the socket to act on, and no --socket was given)", daemon.ErrLockfileUnreadable, c.pidfile, lock.Err)
	case daemon.LockfileNamesAnother:
		if c.socketExplicit {
			return c, fmt.Errorf("the socket %s differs from %s's socket %s; refusing to act on either", c.socket, c.pidfile, lock.Info.Socket)
		}
		c.socket = lock.Info.Socket
	}
	return c, nil
}

// unverifiedReason explains a CheckNonce failure with its likely causes. lock is
// the read the failure came from, so the unreadable case can give the reason the
// file could not be read: a damaged file and a file this user may not open need
// different repairs, and the sentinel on its own cannot tell them apart.
func unverifiedReason(c cli, lock daemon.LockRead, err error) string {
	cause := "a different --pidfile than the daemon's, or another process (not the daemon) holding the socket"
	switch {
	case errors.Is(err, daemon.ErrLockfileUnreadable):
		// Neither the daemon nor another process is named here, on purpose:
		// nothing about a file that cannot be read says anything about who holds
		// the socket. The two causes above lead with the benign reading for the
		// same reason.
		cause = fmt.Sprintf("cannot read the lockfile at %s: %v: --pidfile may name a file that is not this daemon's lockfile, or the daemon may run as another user — a lockfile that cannot be read is not a missing one", c.pidfile, lock.Err)
	case errors.Is(err, daemon.ErrNoLockfile):
		cause = "no lockfile at " + c.pidfile + ": the daemon may use a different --pidfile, or another process (not the daemon) holds the socket"
	case errors.Is(err, daemon.ErrNonceMismatch):
		cause = "the /health nonce does not match " + c.pidfile + ": " + cause
	}
	return err.Error() + " (" + cause + ")"
}

// exitUnverified is daemonize's and status's exit code when something answers
// on the socket but is not shown to be the daemon: its nonce differs from the
// lockfile's, there is no lockfile, or the lockfile cannot be read. status also
// uses it when nothing answers and the lockfile cannot be read, because "not
// running" would be a claim it has not established.
const exitUnverified = 5

// refuseUnverified prints why something answering on the socket is not shown to
// be the daemon, and reports whether it had to. Both callers make this check and
// print this sentence; only the sentence on success differs, so keeping one site
// is what stops a future caller from reporting an unverified daemon as its own.
func refuseUnverified(c cli, info *daemon.LockInfo) bool {
	lock := daemon.ReadLock(c.pidfile)
	err := daemon.CheckNonce(info, lock)
	if err == nil {
		return false
	}
	fmt.Fprintf(os.Stderr, "shelld: %s answers, but it is not verified: %s\n", c.socket, unverifiedReason(c, lock, err))
	return true
}

// alreadyRunning reports a daemon found answering, unless its /health nonce
// doesn't verify it against the lockfile.
func alreadyRunning(c cli, info *daemon.LockInfo) int {
	if refuseUnverified(c, info) {
		return exitUnverified
	}
	fmt.Fprintf(os.Stderr, "shelld already running (pid %d, socket %s)\n", info.Pid, info.Socket)
	return 0
}

func cmdStatus(c cli) int {
	lock := daemon.ReadLock(c.pidfile)
	if info, err := daemon.Probe(c.socket, 500*time.Millisecond); err == nil {
		if err := daemon.CheckNonce(info, lock); err != nil {
			fmt.Printf("shelld: unverified: %s answers, but %s\n", c.socket, unverifiedReason(c, lock, err))
			return exitUnverified
		}
		uptime := max(time.Now().Unix()-info.StartedAt, 0)
		fmt.Printf("shelld: healthy\n  pid:      %d\n  socket:   %s\n  version:  %s\n  uptime:   %ds\n", info.Pid, info.Socket, info.Version, uptime)
		return 0
	}
	// Nothing answered, so the lockfile is the only witness left -- and if it could
	// not be read there is no witness at all. "Not running" and "stale" are both
	// claims about a file this did not manage to look at, and exit 4 is the one
	// callers act on by starting a daemon, which is how an unreadable lockfile
	// becomes a second daemon beside the one already there.
	if lock.Err != nil {
		fmt.Printf("shelld: unverified: nothing answers on %s, and cannot read the lockfile at %s: %v: a lockfile that cannot be read is not a missing one\n", c.socket, lock.Path, lock.Err)
		return exitUnverified
	}
	if lock.Info != nil {
		fmt.Printf("shelld: stale lockfile (pid %d unreachable on %s)\n", lock.Info.Pid, lock.Info.Socket)
		return 3
	}
	fmt.Println("shelld: not running")
	return 4
}

// waitGone waits up to d for nothing to serve the socket.
func waitGone(socket string, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if daemon.NothingServes(socket) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// removeStale removes the daemon's leftover files once nothing serves the
// socket, and says so when it cannot.
// It reports the exit code the command should use, because a refusal that only
// gets printed is a refusal a script cannot see. Every one of these commands used
// to exit 0 after declining to remove the files it was asked to remove, so the one
// outcome where the caller's next step should not be "the daemon is gone" was the
// one it could not detect. The message is not a substitute for the code: the cases
// that reach it are the ones where shelld found something other than what it was
// pointed at.
//
// A path that was never there is not a refusal. On a box where no daemon has run
// there is nothing to remove and the job is already done, so that stays 0 and
// silent -- which is also what keeps `restart` able to start a first daemon, since
// it runs the start half only when stop exits 0. The only thing that reports a
// missing path here is the instance lock, whose directory may not exist yet;
// removing a file that is already gone is success further down.
func removeStale(c cli) int {
	if _, err := daemon.RemoveStale(c.socket, c.pidfile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "shelld: %v\n", err)
		return 1
	}
	return 0
}

func cmdStop(c cli) int {
	// Before /shutdown, which is the action: once it is answered the daemon is
	// gone. Only when something serves the socket, so that a stop retried after
	// its own daemon has gone, whose pidfile is absent because the last call
	// succeeded, still exits 0. restart stops through here, so it refuses too.
	// shelld-rs's cmd_stop (main.rs:1555-1562).
	if c.unvouched && !daemon.NothingServes(c.socket) {
		fmt.Fprintf(os.Stderr, "shelld: %s names no daemon, and something is serving %s; refusing to stop a daemon nothing you named points at (pass --socket %s to stop it anyway)\n", c.pidfile, c.socket, c.socket)
		return 1
	}
	status, _, err := daemon.HTTPRoundTrip(c.socket, http.MethodPost, "/shutdown", 2*time.Second)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("shutdown status %d", status)
	}
	if err == nil {
		// The daemon removes its own files as it exits.
		if !waitGone(c.socket, 5*time.Second) {
			fmt.Fprintf(os.Stderr, "shelld: the daemon accepted /shutdown but still serves %s\n", c.socket)
			return 1
		}
		return 0
	}
	// Say which of the two things went wrong, because the next sentence is about
	// a process and the reader has to know whether that process ever answered.
	// A dial that failed means nothing was listening on the socket, so there was
	// no shutdown to fail: what follows is shelld falling back to a signal because
	// the socket was not usable, not because a daemon ignored the request. Every
	// other round-trip failure happened on a socket something is holding, and
	// those are a shutdown that really did fail.
	//
	// unreachable.Err rather than err: NotReachableError's own message leads with
	// the address, which is already in this sentence.
	shutdownFailed := fmt.Sprintf("graceful shutdown failed (%v)", err)
	var unreachable *daemon.NotReachableError
	if errors.As(err, &unreachable) {
		shutdownFailed = fmt.Sprintf("nothing answered on %s (%v)", c.socket, unreachable.Err)
	}
	lock := daemon.ReadLock(c.pidfile)
	loc := daemon.LocateDaemon(c.socket, lock)
	switch loc.State {
	case daemon.DaemonGone:
		return removeStale(c)
	case daemon.DaemonUnverified:
		fmt.Fprintf(os.Stderr, "shelld: %s; pid %d not signalled: %s\n", shutdownFailed, loc.Pid, loc.Reason)
		return 1
	}
	fmt.Fprintf(os.Stderr, "shelld: %s; terminating pid %d\n", shutdownFailed, loc.Pid)
	if pid, kerr := daemon.TerminateDaemon(c.socket, lock); kerr != nil {
		fmt.Fprintf(os.Stderr, "shelld: terminate pid %d: %v\n", pid, kerr)
		return 1
	}
	if waitGone(c.socket, 5*time.Second) {
		return removeStale(c)
	}
	fmt.Fprintf(os.Stderr, "shelld: pid %d still serves %s after SIGTERM\n", loc.Pid, c.socket)
	return 1
}

func cmdKill(c cli) int {
	if !c.force {
		fmt.Fprintln(os.Stderr, "shelld kill requires --force; use 'shelld stop' for a graceful shutdown")
		return 2
	}
	// The instance lock's holder would otherwise be killed: KillDaemon finds the
	// default socket's daemon without a lockfile. shelld-rs's cmd_kill acts on
	// nothing without one and says so.
	if c.unvouched {
		fmt.Println("shelld: no lockfile, nothing to kill")
		return 0
	}
	lock := daemon.ReadLock(c.pidfile)
	pid, err := daemon.KillDaemon(c.socket, lock)
	var nd *daemon.NotDaemonError
	switch {
	case errors.As(err, &nd) && nd.Located.State == daemon.DaemonGone:
		// The refusal comes first and stands on its own. "nothing to kill" is a claim
		// about the whole request, and printing it beside a refusal to remove the files
		// says the state is clean when shelld has just reported that it is not.
		if code := removeStale(c); code != 0 {
			return code
		}
		fmt.Println("shelld: no daemon running, nothing to kill")
		return 0
	case err != nil:
		fmt.Fprintf(os.Stderr, "shelld: kill: %v\n", err)
		return 1
	}
	if !waitGone(c.socket, 2*time.Second) {
		fmt.Fprintf(os.Stderr, "shelld: pid %d still serves %s after SIGKILL\n", pid, c.socket)
		return 1
	}
	return removeStale(c)
}

func cmdLogs(c cli) int {
	data, err := os.ReadFile(c.logfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		return 1
	}
	for _, line := range daemon.TailLines(string(data), c.lines) {
		fmt.Println(line)
	}
	return 0
}

// exitUnusableAnswer is sessions's and windows's exit code when something
// answered on the socket but its answer cannot be used: a status the command
// does not expect, a body that ended before it was complete, or JSON that does
// not parse. It is separate from the 4 those commands return when nothing
// answered because the two ask for different things — 4 says no daemon is
// running, 6 says the one that is running gave an answer worth looking into.
// shelld-rs allocates 6 for the same case.
const exitUnusableAnswer = 6

// getJSON asks the daemon for path and decodes the reply into v. It decodes only
// on 200, so a caller for which some other status is itself an answer — windows
// and its 404 — sees that status with a nil error and decides what it means.
//
// A body that stops early is named rather than passed on as the "unexpected EOF"
// net/http reports, because the two read differently: one points at the daemon,
// the other reads like a bug in this client.
func getJSON(c cli, path string, v any) (status int, err error) {
	status, body, err := daemon.HTTPRoundTrip(c.socket, http.MethodGet, path, 2*time.Second)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return status, fmt.Errorf("the response body ended before it was complete: %w", err)
	}
	if err != nil || status != http.StatusOK {
		return status, err
	}
	return status, json.Unmarshal(body, v)
}

// unusableAnswer reports a daemon the command could not get an answer out of and
// returns the exit code saying which way it failed. The message is half the fix:
// the one this replaces said "not reachable" for a socket that had completed a
// round trip and returned 200.
//
// The two cases are told apart by whether the socket could be dialled, not by
// whether a status arrived. The status is 0 for a reply whose header never
// parsed as well as for a socket nothing is listening on, and those are opposite
// answers: the first is some other process holding the name, which is worth
// looking into, and the second is no daemon at all.
func unusableAnswer(c cli, err error) int {
	var unreachable *daemon.NotReachableError
	if errors.As(err, &unreachable) {
		fmt.Fprintf(os.Stderr, "shelld not reachable: %v\n", outerError(err))
		return 4
	}
	fmt.Fprintf(os.Stderr, "shelld: reached %s but could not use its answer: %v\n", c.socket, outerError(err))
	return exitUnusableAnswer
}

func cmdSessions(c cli) int {
	var resp struct {
		Sessions []struct {
			Name string `json:"name"`
		} `json:"sessions"`
	}
	status, err := getJSON(c, "/sessions", &resp)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("status %d", status)
	}
	if err != nil {
		return unusableAnswer(c, err)
	}
	for _, s := range resp.Sessions {
		fmt.Println(s.Name)
	}
	return 0
}

func cmdWindows(c cli) int {
	var resp struct {
		Windows []struct {
			Name  string `json:"name"`
			Pid   int64  `json:"pid"`
			Alive bool   `json:"alive"`
		} `json:"windows"`
	}
	// Escaped, because a session name is not a path segment: it is whatever the
	// caller named it, and shelld does not restrict it. A name holding a slash
	// otherwise asked a different route entirely, one holding '?' truncated at the
	// query and asked about the prefix, and one holding a space or CR built a
	// malformed request line -- each a wrong answer about an existing session rather
	// than an error. The daemon is already the matching half: router.go reads
	// EscapedPath and unescapes each segment after the split, so this round-trips.
	status, err := getJSON(c, "/sessions/"+url.PathEscape(c.session)+"/windows", &resp)
	// Only when the exchange itself succeeded: a 404 whose body arrived truncated
	// is an unusable answer, not a session that does not exist.
	if err == nil && status == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "session %q not found\n", c.session)
		return 1
	}
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("status %d", status)
	}
	if err != nil {
		return unusableAnswer(c, err)
	}
	for _, w := range resp.Windows {
		state := "dead"
		if w.Alive {
			state = "alive"
		}
		fmt.Printf("%s\tpid=%d\t%s\n", w.Name, w.Pid, state)
	}
	return 0
}

// outerError drops wrapped causes, printing only the outermost context the
// way shelld-rs's anyhow errors display.
func outerError(err error) string {
	msg := err.Error()
	if inner := errors.Unwrap(err); inner != nil {
		msg = strings.TrimSuffix(msg, ": "+inner.Error())
	}
	return msg
}
