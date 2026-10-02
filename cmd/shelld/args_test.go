package main

// Unit tests for the parts of shelld's front end that reach no daemon:
// argument parsing, the usage and version output, the socket the lockfile
// names, and the small helpers around them.
//
// Deliberately untagged, and that is the point. The rest of this package's
// tests are //go:build linux || darwin and drive a built binary end to end, so
// cmd/shelld had no test files at all on Windows: `go test` there printed
// "[no test files]", which is indistinguishable from a run that passed. The
// same gap left every function below uncovered on all three platforms --
// nothing anywhere referenced parseArgs, socketFromLockfile, outerError,
// unverifiedReason, daemonizeWait or leaveWorkingDirectory.
//
// Not covered here, on purpose:
//   - checkPaths, whose behaviour turns on daemon.DefaultDir(): that is
//     <tmp>/shelld-<uid> on unix but os.TempDir() itself on Windows, so the
//     "is this path in the private directory" comparison it is built around
//     would be asserting a different thing on each platform.
//   - every command that opens the socket (run, daemonize, status, stop,
//     restart, kill, logs, sessions, windows with a live daemon). Those belong
//     in the end-to-end tests, and running them from here would start or stop
//     a real daemon.
//
// This file reports one SKIP by design: TestShelldMainChild is the entry point
// the usage tests re-execute, and it skips when it is not the child. That skip
// is not an untested case.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/internal/daemon"
)

// clearDefaults unsets the three variables daemon.DefaultSocket,
// DefaultPidfile and DefaultLogfile read, so a developer's environment cannot
// change what parseArgs fills in. daemon.envOr treats an empty value as unset,
// which is why "" is enough.
func clearDefaults(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SHELLD_SOCKET", "SHELLD_PIDFILE", "SHELLD_LOGFILE"} {
		t.Setenv(k, "")
	}
}

// TestParseArgsTakesGlobalFlagsOnEitherSideOfTheSubcommand covers the shape
// the help text promises: global flags before or after the command, in
// `--flag value` or `--flag=value` form.
//
// Every case here is input parseArgs accepts. It calls os.Exit for anything it
// rejects, which would take this test process down with it, so the rejections
// are checked in a child process -- see TestUsageErrorsExitTwoWithTheirMessage.
func TestParseArgsTakesGlobalFlagsOnEitherSideOfTheSubcommand(t *testing.T) {
	clearDefaults(t)
	for _, tc := range []struct {
		name string
		args []string
		want cli
	}{
		{"flags before the command", []string{"--socket", "s", "--pidfile", "p", "--logfile", "l", "status"},
			cli{socket: absSock("s"), pidfile: "p", logfile: "l", command: "status", socketExplicit: true, pidfileExplicit: true, lines: 100}},
		{"flags after the command", []string{"status", "--socket", "s", "--pidfile", "p", "--logfile", "l"},
			cli{socket: absSock("s"), pidfile: "p", logfile: "l", command: "status", socketExplicit: true, pidfileExplicit: true, lines: 100}},
		{"flags on both sides", []string{"--socket", "s", "status", "--pidfile", "p", "--logfile", "l"},
			cli{socket: absSock("s"), pidfile: "p", logfile: "l", command: "status", socketExplicit: true, pidfileExplicit: true, lines: 100}},
		{"equals form", []string{"--socket=s", "--pidfile=p", "--logfile=l", "status"},
			cli{socket: absSock("s"), pidfile: "p", logfile: "l", command: "status", socketExplicit: true, pidfileExplicit: true, lines: 100}},
		{"a value containing an equals sign keeps the rest of it",
			[]string{"--socket=/tmp/a=b", "status"},
			cli{socket: "/tmp/a=b", pidfile: defPid(), logfile: defLog(), command: "status", socketExplicit: true, lines: 100}},
		{"--force with kill", []string{"kill", "--force"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "kill", force: true, lines: 100}},
		{"--shared-access with run", []string{"run", "--shared-access"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "run", sharedAccess: true, lines: 100}},
		{"--shared-access with daemonize", []string{"daemonize", "--shared-access"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "daemonize", sharedAccess: true, lines: 100}},
		{"--lines with logs", []string{"logs", "--lines", "5"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "logs", lines: 5}},
		{"--lines zero is a number, not a missing value", []string{"logs", "--lines=0"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "logs", lines: 0}},
		{"windows takes its session", []string{"windows", "work"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "windows", session: "work", lines: 100}},
		{"a session containing an equals sign is a positional, not a flag",
			[]string{"windows", "a=b"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "windows", session: "a=b", lines: 100}},
		{"help after a command is that command's argument, not a request for help",
			[]string{"windows", "help"},
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "windows", session: "help", lines: 100}},
		{"no arguments at all means run", nil,
			cli{socket: defSock(), pidfile: defPid(), logfile: defLog(), command: "run", lines: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseArgs(tc.args); got != tc.want {
				t.Errorf("parseArgs(%q):\n got %+v\nwant %+v", tc.args, got, tc.want)
			}
		})
	}
}

func defSock() string { return absSock(daemon.DefaultSocket()) }

// absSock is the socket parseArgs keeps for s: absolute, except on Windows,
// where a socket is a pipe name.
func absSock(s string) string {
	if runtime.GOOS == "windows" {
		return s
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		panic(err)
	}
	return abs
}

// TestParseArgsAnchorsARelativeSocketToTheCwd: the lockfile records the
// socket and commands compare against it as a string, so a relative --socket
// must name one file whichever directory a command runs from.
func TestParseArgsAnchorsARelativeSocketToTheCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows socket is a pipe name")
	}
	clearDefaults(t)
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	inA := parseArgs([]string{"stop", "--socket", "s.sock"}).socket
	t.Chdir(b)
	inB := parseArgs([]string{"stop", "--socket", "s.sock"}).socket
	if inA != filepath.Join(a, "s.sock") || inB != filepath.Join(b, "s.sock") {
		t.Errorf("--socket s.sock: %q from %s and %q from %s; want each anchored to its cwd", inA, a, inB, b)
	}
}
func defPid() string { return daemon.DefaultPidfile() }
func defLog() string { return daemon.DefaultLogfile() }

// TestParseArgsReadsTheDefaultPathsFromTheEnvironment pins that the three
// SHELLD_* variables reach the same fields --socket, --pidfile and --logfile
// do, since every command that acts on a running daemon is aimed by them.
func TestParseArgsReadsTheDefaultPathsFromTheEnvironment(t *testing.T) {
	t.Setenv("SHELLD_SOCKET", "env-sock")
	t.Setenv("SHELLD_PIDFILE", "env-pid")
	t.Setenv("SHELLD_LOGFILE", "env-log")
	got := parseArgs([]string{"status"})
	want := cli{socket: absSock("env-sock"), pidfile: "env-pid", logfile: "env-log", command: "status", socketExplicit: true, pidfileExplicit: true, lines: 100}
	if got != want {
		t.Errorf("parseArgs:\n got %+v\nwant %+v", got, want)
	}
}

// TestSocketExplicitComesFromEitherSource guards the one field whose whole job
// is to decide, in socketFromLockfile, whether a socket that disagrees with the
// lockfile's is an error or something to override. A flag and the environment
// variable must count the same, and neither being present must not.
func TestSocketExplicitComesFromEitherSource(t *testing.T) {
	clearDefaults(t)
	if c := parseArgs([]string{"status"}); c.socketExplicit {
		t.Error("no --socket and no SHELLD_SOCKET, yet the socket reads as explicit")
	}
	if c := parseArgs([]string{"status", "--socket", "s"}); !c.socketExplicit {
		t.Error("--socket was given, yet the socket does not read as explicit")
	}
	t.Setenv("SHELLD_SOCKET", "env-sock")
	if c := parseArgs([]string{"status"}); !c.socketExplicit {
		t.Error("SHELLD_SOCKET was set, yet the socket does not read as explicit")
	}
}

// TestPidfileExplicitComesFromEitherSource is TestSocketExplicitComesFromEitherSource
// for the pidfile, which decides whether an absent lockfile leaves the default socket
// unvouched (L13). SHELLD_PIDFILE names a pidfile as the flag does, as it does to
// shelld-rs (pidfile_explicit in its main.rs).
func TestPidfileExplicitComesFromEitherSource(t *testing.T) {
	clearDefaults(t)
	if c := parseArgs([]string{"status"}); c.pidfileExplicit {
		t.Error("no --pidfile and no SHELLD_PIDFILE, yet the pidfile reads as explicit")
	}
	if c := parseArgs([]string{"status", "--pidfile", "p"}); !c.pidfileExplicit {
		t.Error("--pidfile was given, yet the pidfile does not read as explicit")
	}
	t.Setenv("SHELLD_PIDFILE", "env-pid")
	if c := parseArgs([]string{"status"}); !c.pidfileExplicit {
		t.Error("SHELLD_PIDFILE was set, yet the pidfile does not read as explicit")
	}
}

// TestAnEmptySocketValueFallsBackToTheDefault records what --socket= does
// today: the empty value is indistinguishable from an absent one, so the
// default socket is used and the socket does not count as explicit. Written
// down because it is the sort of thing a reader would otherwise have to guess
// at, and because a change to it changes which daemon `--socket= stop` reaches.
func TestAnEmptySocketValueFallsBackToTheDefault(t *testing.T) {
	clearDefaults(t)
	c := parseArgs([]string{"--socket=", "status"})
	if c.socket != daemon.DefaultSocket() {
		t.Errorf("socket is %q; want the default %q", c.socket, daemon.DefaultSocket())
	}
	if c.socketExplicit {
		t.Error("--socket= reads as an explicit socket")
	}
}

// documentedCommands is the Commands: block of the usage text. Kept as a
// literal so that adding a command to the help without teaching parseArgs
// about it fails here, cleanly, rather than in whatever test first runs it.
var documentedCommands = []string{"run", "daemonize", "status", "stop", "restart", "kill", "logs", "sessions", "windows"}

// TestTheUsageTextAndTheParserAgreeOnTheCommandList reads the command names
// out of the help text and checks both directions: the text lists exactly the
// commands this test knows about, and parseArgs accepts each one.
//
// The second half is run only over the literal list above, never over what was
// scraped: an unrecognised command makes parseArgs exit 2, which would kill
// this process instead of failing this test. The first half is what catches
// drift, and it fails cleanly.
func TestTheUsageTextAndTheParserAgreeOnTheCommandList(t *testing.T) {
	clearDefaults(t)
	_, rest, ok := strings.Cut(usage, "Commands:\n")
	if !ok {
		t.Fatal(`the usage text has no "Commands:" block`)
	}
	block, _, ok := strings.Cut(rest, "\nOptions:")
	if !ok {
		t.Fatal(`the usage text's Commands: block is not followed by "Options:"`)
	}
	var found []string
	for _, line := range strings.Split(block, "\n") {
		// A command starts at column 2; a description wrapped onto its own
		// line is indented further.
		if len(line) > 2 && strings.HasPrefix(line, "  ") && line[2] != ' ' {
			found = append(found, strings.Fields(line)[0])
		}
	}
	if strings.Join(found, ",") != strings.Join(documentedCommands, ",") {
		t.Errorf("the usage text lists %q; this test knows %q -- if a command was added or removed, update documentedCommands and check parseArgs and main's switch handle it", found, documentedCommands)
	}
	for _, cmd := range documentedCommands {
		args := []string{cmd}
		if cmd == "windows" {
			args = append(args, "a-session") // the only command with a required argument
		}
		if got := parseArgs(args).command; got != cmd {
			t.Errorf("parseArgs(%q).command = %q", args, got)
		}
	}
}

// shelldArgsEnv makes this test binary run shelld's main with the arguments it
// names, newline-separated. The usage, help and version paths all end in
// os.Exit, so they cannot be tested in this process.
//
// The child is also given SHELLD_TEST_DAEMON_ARGS, which lifecycle_test.go's
// TestMain already honours in the same encoding -- and honours before it builds
// the binary those tests need, so on linux and darwin the child costs one
// process and nothing else. That TestMain is //go:build linux || darwin, which
// is why the branch below exists at all: on Windows there is no TestMain, so
// the child is selected with -test.run instead. If the TestMain hook ever goes
// away this still works; the child just pays that build first.
const shelldArgsEnv = "SHELLD_TEST_MAIN_ARGS"

// childBudget is how long the child is allowed to take before it is treated as
// serving rather than exiting, and exitChildServed is the status it uses to say
// so. 97 is outside the range main uses (0..5).
const (
	childBudget     = 15 * time.Second
	exitChildServed = 97
)

// init arms the watchdog for a child process.
//
// Every invocation runShelld sends is one that must print and exit. If the
// parsing under test regresses so that one of them starts serving instead, the
// child blocks forever -- and a test that hangs is worse than a test that
// fails, because it takes the whole run with it. That is not hypothetical: a
// mutant that made a missing flag value the empty string instead of an error
// turned `shelld --socket` into `shelld run`, and the child held a listening
// socket for eight minutes until it was killed by hand.
//
// In init rather than in TestShelldMainChild because on linux and darwin that
// test never runs: lifecycle_test.go's TestMain dispatches on
// SHELLD_TEST_DAEMON_ARGS and calls main() before m.Run(). init runs before
// TestMain on every platform, so this is the one place that covers both paths.
// In the parent the variable is unset and this does nothing.
func init() {
	if os.Getenv(shelldArgsEnv) == "" {
		return
	}
	time.AfterFunc(childBudget, func() {
		fmt.Fprintf(os.Stderr, "child did not exit within %v: it reached a serving command\n", childBudget)
		os.Exit(exitChildServed)
	})
}

// TestShelldMainChild is the child entry point on Windows, which has no
// TestMain. The watchdog above is already armed by the time it runs.
func TestShelldMainChild(t *testing.T) {
	args := os.Getenv(shelldArgsEnv)
	if args == "" {
		t.Skip("the child-process entry point for the usage tests; nothing to do in the parent")
	}
	os.Args = append([]string{"shelld"}, strings.Split(args, "\n")...)
	main() // exits
}

// runShelld runs shelld's main in a child process and returns what it wrote
// and the status it exited with.
//
// The child's working directory is outside the repository because main starts
// with dotenv.Load, which walks up from the working directory and applies the
// nearest .env without overriding what is already set. Run from this package's
// directory, the repository's own .env would reach these assertions.
func runShelld(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if len(args) == 0 {
		// An empty value reads as unset in both child branches, which would
		// skip and exit 0 with no output -- a pass that tested nothing. It is
		// also the one invocation that would start a daemon.
		t.Fatal("runShelld needs at least one argument")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Outlives the child's own watchdog, so the watchdog's message is what a
	// serving child reports. This only fires if the child cannot run it at all.
	ctx, cancel := context.WithTimeout(context.Background(), 4*childBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestShelldMainChild$")
	sandbox := t.TempDir()
	cmd.Dir = sandbox
	joined := strings.Join(args, "\n")
	cmd.Env = append(os.Environ(),
		shelldArgsEnv+"="+joined, "SHELLD_TEST_DAEMON_ARGS="+joined,
		// Point every default path at this child's own directory. None of these
		// invocations should reach a path that uses them -- but the one that
		// regressed into `shelld run` took the instance lock on the *shared*
		// default socket, so a second copy of these tests, or any daemon the
		// box was already running, was in its way. Sandboxed, the worst a
		// regression can do is serve a socket nobody is listening for, for as
		// long as the watchdog allows.
		"SHELLD_SOCKET="+filepath.Join(sandbox, "shelld.sock"),
		"SHELLD_PIDFILE="+filepath.Join(sandbox, "shelld.pid"),
		"SHELLD_LOGFILE="+filepath.Join(sandbox, "shelld.log"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run shelld %q: %v\nstderr: %s", args, err, errb.String())
	}
	if code == exitChildServed {
		t.Fatalf("shelld %q did not exit: the parsing under test let it reach a serving command\nstderr: %s",
			args, errb.String())
	}
	return out.String(), errb.String(), code
}

// TestUsageErrorsExitTwoWithTheirMessage pins the whole error surface: the exit
// status, the message, and the usage line each error prints. The format is
// shelld-rs's, and a client that parses it would notice a change here.
func TestUsageErrorsExitTwoWithTheirMessage(t *testing.T) {
	const global = "shelld [OPTIONS] [COMMAND]"
	for _, tc := range []struct {
		name, msg, usageLine string
		args                 []string
	}{
		{"a flag with no value", "a value is required for '--socket <SOCKET>' but none was supplied", global,
			[]string{"--socket"}},
		{"the last flag has no value", "a value is required for '--pidfile <PIDFILE>' but none was supplied", global,
			[]string{"status", "--pidfile"}},
		{"--lines is not a number", "invalid value for '--lines <LINES>': invalid digit found in string", global,
			[]string{"logs", "--lines", "x"}},
		{"--lines is negative", "invalid value for '--lines <LINES>': invalid digit found in string", global,
			[]string{"logs", "--lines", "-1"}},
		{"an unknown flag", "unexpected argument '-x' found", global, []string{"-x"}},
		{"an unknown long flag", "unexpected argument '--nope' found", global, []string{"--nope"}},
		{"a bare dash is a command name", "unrecognized subcommand '-'", global, []string{"-"}},
		{"an unknown command", "unrecognized subcommand 'bogus'", global, []string{"bogus"}},
		{"a second positional after status", "unexpected argument 'extra' found", global, []string{"status", "extra"}},
		{"a second positional after logs", "unexpected argument 'extra' found", global, []string{"logs", "extra"}},
		{"windows with no session", "the following required arguments were not provided:\n  <SESSION>",
			"shelld windows <SESSION>", []string{"windows"}},
		{"--shared-access on a command that does not serve", "unexpected argument '--shared-access' found",
			"shelld status [OPTIONS]", []string{"status", "--shared-access"}},
		{"--shared-access with a value", "unexpected value 'false' for '--shared-access' found; no more were expected",
			global, []string{"daemonize", "--shared-access=false"}},
		{"--force with a value", "unexpected value 'false' for '--force' found; no more were expected",
			global, []string{"kill", "--force=false"}},
		{"--force on a command that is not kill", "unexpected argument '--force' found",
			"shelld status [OPTIONS]", []string{"status", "--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runShelld(t, tc.args...)
			want := fmt.Sprintf("error: %s\n\nUsage: %s\n\nFor more information, try '--help'.\n", tc.msg, tc.usageLine)
			if code != 2 {
				t.Errorf("exit status %d, want 2", code)
			}
			if stderr != want {
				t.Errorf("stderr:\n got %q\nwant %q", stderr, want)
			}
			if stdout != "" {
				t.Errorf("an error wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestHelpPrintsTheUsageTextAndExitsZero covers all three spellings. Help is
// not an error, so it goes to stdout and exits 0 -- the difference that lets
// `shelld --help | less` work and keeps it out of an error log.
func TestHelpPrintsTheUsageTextAndExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			stdout, stderr, code := runShelld(t, arg)
			if code != 0 {
				t.Errorf("exit status %d, want 0", code)
			}
			if stdout != usage {
				t.Errorf("stdout is not the usage text:\n got %q\nwant %q", stdout, usage)
			}
			if stderr != "" {
				t.Errorf("help wrote to stderr: %q", stderr)
			}
		})
	}
}

func TestVersionPrintsTheLibraryVersionAndExitsZero(t *testing.T) {
	for _, arg := range []string{"-V", "--version"} {
		t.Run(arg, func(t *testing.T) {
			stdout, stderr, code := runShelld(t, arg)
			if code != 0 {
				t.Errorf("exit status %d, want 0", code)
			}
			if want := "shelld " + shelld.Version + "\n"; stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if stderr != "" {
				t.Errorf("--version wrote to stderr: %q", stderr)
			}
		})
	}
}

// TestHelpWinsOverALaterFlagThatNeedsAValue records the order parsing happens
// in: --help is acted on as it is read, so an incomplete flag after it is never
// reached. Exit 0, not the usage error.
//
// Only this order is tested. In the other one --socket takes the next argument,
// so --help becomes its value, the command defaults to run, and the child would
// start a daemon -- which no test in this file may do.
func TestHelpWinsOverALaterFlagThatNeedsAValue(t *testing.T) {
	stdout, stderr, code := runShelld(t, "--help", "--socket")
	if code != 0 {
		t.Errorf("exit status %d, want 0: --help before an incomplete flag should print help", code)
	}
	if stdout != usage {
		t.Errorf("stdout is not the usage text: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr: %q", stderr)
	}
}

func writeLockfile(t *testing.T, path, socket string) {
	t.Helper()
	info := daemon.LockInfo{Pid: int32(os.Getpid()), Socket: socket, Version: daemon.Version, StartedAt: time.Now().Unix(), Nonce: "test-nonce"}
	if err := daemon.WriteLockfile(path, info); err != nil {
		t.Fatal(err)
	}
}

// TestSocketFromLockfileAimsAtTheDaemonThatWroteThePidfile covers the whole
// decision: a socket nobody gave is taken from the lockfile, and a socket that
// was given and disagrees is refused rather than guessed at. The refusal is the
// point -- acting on either socket could stop the wrong daemon.
func TestSocketFromLockfileAimsAtTheDaemonThatWroteThePidfile(t *testing.T) {
	dir := t.TempDir()
	t.Run("no lockfile leaves the socket alone", func(t *testing.T) {
		c := cli{socket: "given", pidfile: filepath.Join(dir, "absent.pid")}
		got, err := socketFromLockfile(c)
		if err != nil || got != c {
			t.Errorf("got %+v, %v; want the input unchanged and no error", got, err)
		}
	})
	t.Run("a lockfile naming the same socket leaves it alone", func(t *testing.T) {
		p := filepath.Join(dir, "same.pid")
		writeLockfile(t, p, "same-sock")
		c := cli{socket: "same-sock", pidfile: p, socketExplicit: true}
		got, err := socketFromLockfile(c)
		if err != nil || got != c {
			t.Errorf("got %+v, %v; want the input unchanged and no error", got, err)
		}
	})
	// A lockfile whose socket field is empty names no socket, and is refused rather
	// than left alone. Leaving it alone returns the socket the caller came in with,
	// which for an implicit socket is the default one -- so a --pidfile that named a
	// file shelld could not act on used to send stop and kill at whatever serves the
	// default socket instead, reporting success. No daemon writes an empty socket, so
	// the file did not come from one; that is why it is the unreadable arm and not a
	// case of its own.
	t.Run("a lockfile with no socket is refused, not folded into the default", func(t *testing.T) {
		p := filepath.Join(dir, "empty.pid")
		writeLockfile(t, p, "")
		c := cli{socket: "given", pidfile: p}
		got, err := socketFromLockfile(c)
		if err == nil {
			t.Fatalf("socketFromLockfile accepted a lockfile naming no socket and returned %+v", got)
		}
		if !errors.Is(err, daemon.ErrLockfileUnreadable) {
			t.Errorf("error %v is not ErrLockfileUnreadable, so callers cannot tell it from a socket mismatch", err)
		}
		if got.socket != c.socket {
			t.Errorf("socket changed to %q on the refusal path", got.socket)
		}
	})
	// The other half of that refusal: with --socket given, the target is known
	// without the lockfile, so only the cross-check is lost and the command goes on.
	// Without this arm the assertion above is satisfied by refusing every read.
	t.Run("an explicit socket survives a lockfile naming none", func(t *testing.T) {
		p := filepath.Join(dir, "empty-explicit.pid")
		writeLockfile(t, p, "")
		c := cli{socket: "given", pidfile: p, socketExplicit: true}
		got, err := socketFromLockfile(c)
		if err != nil || got != c {
			t.Errorf("got %+v, %v; want the input unchanged and no error", got, err)
		}
	})
	t.Run("an implicit socket is replaced by the lockfile's", func(t *testing.T) {
		p := filepath.Join(dir, "other.pid")
		writeLockfile(t, p, "lock-sock")
		c := cli{socket: "default-sock", pidfile: p} // socketExplicit false
		got, err := socketFromLockfile(c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.socket != "lock-sock" {
			t.Errorf("socket = %q, want the lockfile's %q", got.socket, "lock-sock")
		}
		if got.pidfile != c.pidfile {
			t.Errorf("pidfile changed to %q", got.pidfile)
		}
	})
	t.Run("an explicit socket that disagrees is refused", func(t *testing.T) {
		p := filepath.Join(dir, "conflict.pid")
		writeLockfile(t, p, "lock-sock")
		c := cli{socket: "given-sock", pidfile: p, socketExplicit: true}
		got, err := socketFromLockfile(c)
		if err == nil {
			t.Fatalf("no error; got %+v", got)
		}
		for _, want := range []string{"given-sock", "lock-sock", p} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if got.socket != "given-sock" {
			t.Errorf("the refused socket was changed to %q", got.socket)
		}
	})
	// The arm where returning the input unchanged is not an abstention. This
	// function exists to aim a command at the daemon --pidfile names instead of
	// whatever serves the default socket, so handing the socket back untouched
	// after a failed read picks the default daemon as the target -- and does it
	// past the refusal three lines down, the one guard against acting on the
	// wrong one. `shelld kill --pidfile damaged.pid` would kill the default
	// socket's daemon and report success.
	t.Run("an unreadable lockfile is refused rather than read as absent", func(t *testing.T) {
		p := filepath.Join(dir, "damaged.pid")
		// Unparseable rather than mode 000: a suite running as root reads a
		// mode-000 file, and the fixture would stop reproducing without failing.
		if err := os.WriteFile(p, []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := cli{socket: "default-sock", pidfile: p}
		got, err := socketFromLockfile(c)
		if err == nil {
			t.Fatalf("no error: the command would go on to act on %q", got.socket)
		}
		if !errors.Is(err, daemon.ErrLockfileUnreadable) {
			t.Errorf("err = %v; want it to wrap daemon.ErrLockfileUnreadable", err)
		}
		if strings.Contains(err.Error(), "no lockfile") {
			t.Errorf("err = %v; the file is at %s, so this must not read as absence", err, p)
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("err = %v; want it to name the file it could not read", err)
		}
		if got.socket != c.socket {
			t.Errorf("the socket was changed to %q by a read that failed", got.socket)
		}
	})
	t.Run("an explicit socket survives an unreadable lockfile", func(t *testing.T) {
		p := filepath.Join(dir, "damaged-explicit.pid")
		if err := os.WriteFile(p, []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := cli{socket: "given-sock", pidfile: p, socketExplicit: true}
		got, err := socketFromLockfile(c)
		if err != nil || got != c {
			t.Errorf("got %+v, %v; want the named socket kept and no error: the caller said which socket to act on, so the lockfile is not needed to choose one", got, err)
		}
	})
}

// TestUnverifiedReasonExplainsWhichWayVerificationFailed checks that each of
// the four causes reads differently, and that all of them name the pidfile:
// exit status 5 on its own does not tell anyone which pidfile to look at.
//
// The unreadable arm additionally has to carry the read error itself and must not
// say "no lockfile": it is the one cause whose repair depends on why the file
// would not open, and the caller is told to go looking for a file that is in fact
// sitting at that path.
func TestUnverifiedReasonExplainsWhichWayVerificationFailed(t *testing.T) {
	c := cli{pidfile: "/somewhere/shelld.pid"}
	unreadable := daemon.LockRead{Path: c.pidfile, Err: errors.New("invalid character 'n'")}
	for _, tc := range []struct {
		name string
		lock daemon.LockRead
		err  error
		want string
	}{
		{"no lockfile", daemon.LockRead{Path: c.pidfile}, daemon.ErrNoLockfile, "no lockfile at /somewhere/shelld.pid"},
		{"nonce mismatch", daemon.LockRead{Path: c.pidfile, Info: &daemon.LockInfo{}}, daemon.ErrNonceMismatch, "the /health nonce does not match /somewhere/shelld.pid"},
		{"unreadable", unreadable, daemon.ErrLockfileUnreadable, "cannot read the lockfile at /somewhere/shelld.pid: invalid character 'n'"},
		{"anything else", daemon.LockRead{Path: c.pidfile}, errors.New("read lockfile: permission denied"), "a different --pidfile than the daemon's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unverifiedReason(c, tc.lock, tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("unverifiedReason = %q; want it to contain %q", got, tc.want)
			}
			if !strings.Contains(got, tc.err.Error()) {
				t.Errorf("unverifiedReason = %q; want it to contain the error %q", got, tc.err)
			}
			if tc.name != "no lockfile" && strings.Contains(got, "no lockfile") {
				t.Errorf("unverifiedReason = %q; only the absent case may say there is no lockfile", got)
			}
		})
	}
	// The four must not read alike, or the exit status is all a caller has.
	seen := map[string]string{}
	for _, tc := range []struct {
		name string
		lock daemon.LockRead
		err  error
	}{
		{"absent", daemon.LockRead{Path: c.pidfile}, daemon.ErrNoLockfile},
		{"mismatch", daemon.LockRead{Path: c.pidfile}, daemon.ErrNonceMismatch},
		{"unreadable", unreadable, daemon.ErrLockfileUnreadable},
		{"other", daemon.LockRead{Path: c.pidfile}, errors.New("other")},
	} {
		r := unverifiedReason(c, tc.lock, tc.err)
		if was, ok := seen[r]; ok {
			t.Errorf("%s and %s produce the same explanation: %q", was, tc.name, r)
		}
		seen[r] = tc.name
	}
}

// TestDaemonizeWaitRejectsEveryUnusableOverride pins that only a positive
// duration is honoured. The default exists for antivirus scanning delaying a
// first run on Windows, and a zero or negative override would make daemonize
// give up before the daemon could possibly answer.
func TestDaemonizeWaitRejectsEveryUnusableOverride(t *testing.T) {
	const def = 30 * time.Second
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", def},
		{"5s", 5 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"2m", 2 * time.Minute},
		{"0s", def},
		{"-1s", def},
		{"nonsense", def},
		{"30", def}, // no unit: not a Go duration
	} {
		t.Run("SHELLD_DAEMONIZE_WAIT="+tc.env, func(t *testing.T) {
			t.Setenv("SHELLD_DAEMONIZE_WAIT", tc.env)
			if got := daemonizeWait(); got != tc.want {
				t.Errorf("daemonizeWait = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOuterErrorPrintsOnlyTheOutermostContext covers the helper that makes
// "shelld not reachable: ..." read the way the Rust binary's anyhow errors do.
func TestOuterErrorPrintsOnlyTheOutermostContext(t *testing.T) {
	inner := errors.New("connection refused")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an error wrapping nothing is itself", inner, "connection refused"},
		{"the wrapped cause is dropped", fmt.Errorf("dial unix /tmp/s: %w", inner), "dial unix /tmp/s"},
		{"only a trailing cause is dropped", fmt.Errorf("before %w after", inner), "before connection refused after"},
		{"a wrapper that adds nothing is left alone", fmt.Errorf("%w", inner), "connection refused"},
		{"nested wrapping drops one level, not all of them",
			fmt.Errorf("outer: %w", fmt.Errorf("middle: %w", inner)), "outer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outerError(tc.err); got != tc.want {
				t.Errorf("outerError = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLeaveWorkingDirectoryAbsolutisesThePathsThenMovesOut covers the one
// Windows-only branch in cmdRun. It runs on every platform because the
// function is not itself platform-gated, and because the platform it does run
// on had no tests at all: this is the only coverage it has anywhere.
//
// It is also the only test in this file that changes process state -- it
// chdirs, which is the whole point of the function -- so it must not be made
// parallel. t.Chdir restores the working directory when it ends.
func TestLeaveWorkingDirectoryAbsolutisesThePathsThenMovesOut(t *testing.T) {
	t.Chdir(t.TempDir())
	// Read back rather than reusing t.TempDir()'s value: on macOS it is under a
	// symlinked /var, and filepath.Abs resolves through Getwd, so the two
	// spellings would not compare equal.
	start, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	c := cli{socket: `\\.\pipe\shelld-test`, pidfile: "shelld.pid", logfile: filepath.Join("logs", "shelld.log")}
	leaveWorkingDirectory(&c)

	// The pidfile and logfile become absolute, and against the directory shelld
	// was started in -- not the one it is about to move to. A daemon that
	// resolved them afterwards would write them into the home directory.
	for _, got := range []string{c.pidfile, c.logfile} {
		if !filepath.IsAbs(got) {
			t.Errorf("%q is still relative", got)
		}
	}
	if want := filepath.Join(start, "shelld.pid"); c.pidfile != want {
		t.Errorf("pidfile = %q, want %q", c.pidfile, want)
	}
	if want := filepath.Join(start, "logs", "shelld.log"); c.logfile != want {
		t.Errorf("logfile = %q, want %q", c.logfile, want)
	}
	// The socket is deliberately untouched: on Windows it is a pipe name, which
	// filepath.Abs would mangle into a path.
	if c.socket != `\\.\pipe\shelld-test` {
		t.Errorf("socket = %q; it should not have been touched", c.socket)
	}

	// And the process has left the directory, which is what this is for:
	// Windows refuses to remove a directory that is some process's cwd.
	now, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if now == start {
		t.Fatalf("still in %q", start)
	}
	want, err := os.UserHomeDir()
	if err != nil {
		want = os.TempDir()
	}
	if resolve(t, now) != resolve(t, want) {
		t.Errorf("moved to %q, want the home directory %q", now, want)
	}
}

// resolve makes two spellings of one directory comparable; macOS's temporary
// and home directories both sit under a symlink.
func resolve(t *testing.T, path string) string {
	t.Helper()
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}
