//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// bin is shelld built for these tests, named shelld so the identity check
// accepts it.
var bin string

// namedSleepEnv marks the child startSleep runs under a name of the test's
// choosing. It is read in TestMain, before the build below, so the child costs
// a process and nothing else.
const namedSleepEnv = "SHELLD_TEST_NAMED_SLEEP"

// detachMarkEnv and detachedMarkEnv are the two halves of stopwindows_test.go's
// detaching job, and they carry the same value: the path the job writes its own
// pid to. The first is set on a process whose whole job is to start the second in
// a new session and then go away; the second is set on that session's leader,
// which is the process those tests are about.
//
// Two steps because one cannot work. setsid(2) fails with EPERM on a process that
// already leads its process group, and whether a shell puts a background command
// in a group of its own is the shell's business, not something to depend on.
// Forking first always leaves the child a non-leader, and SysProcAttr.Setsid makes
// the call between fork and exec, where that error cannot arise. This is what
// setsid(1) does; doing it here is what lets those tests run on macOS, which has
// no setsid(1).
const (
	detachMarkEnv   = "SHELLD_TEST_DETACH_MARK"
	detachedMarkEnv = "SHELLD_TEST_DETACHED_MARK"
)

func TestMain(m *testing.M) {
	// signal_test.go re-executes this test binary as a daemon.
	if args := os.Getenv("SHELLD_TEST_DAEMON_ARGS"); args != "" {
		os.Args = append([]string{"shelld"}, strings.Split(args, "\n")...)
		main()
		return
	}
	// startSleep re-executes this test binary as a process that merely exists
	// under a chosen name. Bounded rather than endless, so a child that outlives
	// the test that started it still goes away.
	if os.Getenv(namedSleepEnv) != "" {
		time.Sleep(2 * time.Minute)
		return
	}
	// The detached half of stopwindows_test.go's window program: the session leader
	// itself, which records its pid and then just exists. Checked before the
	// spawning branch below, because the spawner passes its whole environment on
	// and so this variable is the one that says which of the two a process is.
	if mark := os.Getenv(detachedMarkEnv); mark != "" {
		if err := os.WriteFile(mark, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "detached mark: %v\n", err)
			os.Exit(1)
		}
		// Bounded, like the branch above: a job the test meant to leak still goes.
		time.Sleep(2 * time.Minute)
		return
	}
	// The spawning half. It exits at once, which is what reparents the session
	// leader away from the window and leaves it with nothing of this process tree.
	if mark := os.Getenv(detachMarkEnv); mark != "" {
		// No stdio, so the job does not keep the window's pty slave open. setsid(1)
		// would have passed it on; not passing it on is strictly less able to hold
		// the window alive, which is the other thing those tests measure.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), detachedMarkEnv+"="+mark)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "detach: %v\n", err)
			os.Exit(1)
		}
		return
	}
	dir, err := os.MkdirTemp("", "shelld-lifecycle")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "shelld")
	if prebuilt := os.Getenv("SHELLD_TEST_BIN"); prebuilt != "" {
		// Run the tests against another build, e.g. to show they fail on it.
		data, err := os.ReadFile(prebuilt)
		if err == nil {
			err = os.WriteFile(bin, data, 0o755)
		}
		if err != nil {
			panic(err)
		}
	} else if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build shelld: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if leaked := sweepLeakedDaemons(); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "%d daemon(s) outlived the tests that started them and were killed here:\n", len(leaked))
		for _, l := range leaked {
			fmt.Fprintf(os.Stderr, "\t%s\n", l)
		}
		fmt.Fprintf(os.Stderr, "each line names the --socket it served, which identifies the test; see sweepLeakedDaemons\n")
		if code == 0 {
			code = 1
		}
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// sweepLeakedDaemons kills every process still running this run's binary once the
// tests are over, and returns what it killed.
//
// It exists because a leak here is silent and unbounded. A daemon these tests
// daemonize is detached on purpose -- that is the path a supervisor uses -- so it
// is reparented to pid 1 and nothing reaps it when the test binary goes: a test that
// fails before its own cleanup is registered, or whose cleanup stops the wrong
// daemon, leaves a process serving a socket in a deleted directory for as long as the
// machine is up. Thirteen were found on the macOS host this suite is also run on,
// from five separate runs, the oldest up 2h42m, all of them still answering and all
// of them exiting on a plain SIGTERM. Every one of those runs had reported its result
// and none of them had reported this.
//
// Failing the run is the point rather than a side effect. The leak costs little in
// itself -- 4MB a daemon -- and what it actually hides is a cleanup path that did not
// run, which is a defect in a suite whose subject IS process lifetime. So a sweep
// that found something makes the run red even if every test passed, and prints the
// command lines, because the --socket in them is what says which test it was.
//
// The predicate is the absolute path of the binary built for this run, in a temp
// directory of its own, so it cannot match a daemon belonging to another run of this
// suite, to another package, or to whatever the developer has running for real. It
// deliberately does not require the "run" subcommand: a wedged `shelld stop` is the
// same kind of leak. ps rather than /proc, for darwin -- see daemonProcesses, whose
// parse this shares and whose note explains the -ww.
func sweepLeakedDaemons() []string {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,args=").Output()
	if err != nil {
		// As in daemonProcesses: a sweep that cannot look must not report a clean run.
		// This costs the os.RemoveAll below it, so a machine where ps is broken keeps
		// one build directory under GOTMPDIR, which is the cheaper of the two outcomes.
		panic(fmt.Sprintf("ps failed, so this cannot say whether any daemon leaked: %v", err))
	}
	var killed []string
	for line := range strings.SplitSeq(string(out), "\n") {
		field, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(field)
		// Nothing below this is safe for a pid that is not a real process: 0 signals
		// this process's whole group and -1 every process this user owns.
		if err != nil || pid <= 1 {
			continue
		}
		if args = strings.TrimLeft(args, " "); !strings.HasPrefix(args, bin+" ") && args != bin {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed = append(killed, args)
		}
	}
	return killed
}

type paths struct{ dir, socket, pidfile, logfile string }

func newPaths(t *testing.T) paths {
	// A short directory keeps the socket path under sun_path's limit.
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return paths{dir, filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.pid"), filepath.Join(dir, "s.log")}
}

func (p paths) args(cmd ...string) []string {
	return append(cmd, "--socket", p.socket, "--pidfile", p.pidfile, "--logfile", p.logfile)
}

// shelld runs a shelld command to completion and returns its exit code and
// output.
func (p paths) shelld(t *testing.T, env []string, cmd ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, p.args(cmd...)...)
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("shelld %v: %v", cmd, err)
	}
	return 0, string(out)
}

// startRun starts `shelld run` in the background, killed at cleanup.
//
// In its own session, as SpawnDetached starts the real thing. A daemon left in the
// test binary's session is reachable by job control aimed at the group, and the
// kernel aims some: when a process group becomes orphaned while one of its members
// is stopped, every member is sent SIGHUP and then SIGCONT. A SIGSTOP'd daemon
// sharing that group is resumed by it, mid-test, with no sender to point at —
// si_code reads SI_KERNEL and si_pid 0. Its own process group is not enough, since
// that group is itself newly orphaned the moment the test binary exits; the rule is
// scoped to a session, so a session of its own is what escapes it.
func (p paths) startRun(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command(bin, p.args("run")...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	waitUntil(t, 5*time.Second, func() bool { return daemon.IsAlive(p.socket, 200*time.Millisecond) }, "daemon never answered")
	return c
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
	}
}

// alive reports whether a child process of this test is still running; a
// killed child that has not been reaped yet counts as dead.
func alive(pid int) bool {
	var ws syscall.WaitStatus
	got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return err == nil && got == 0
}

// startSleep starts a process that just exists for a while and returns it,
// killed at cleanup. With name set it runs under that name, so a test can choose
// what the identity check reads.
//
// The named arm re-executes this test binary rather than copying sleep under a
// new name, which is what it did. A byte copy of a macOS platform binary is
// SIGKILLed at exec -- measured as exit 137 on macOS 26.4, and a launch
// constraint rather than a signature that fails to verify, so there is nothing
// to re-sign. The victim died on the spot, alive() said so, and the test read
// that as shelld having signalled a process it should have left alone. See
// sleepNamed in internal/daemon, which had the same fixture.
func startSleep(t *testing.T, dir, name string) *exec.Cmd {
	t.Helper()
	var c *exec.Cmd
	if name == "" {
		sleep, err := exec.LookPath("sleep")
		if err != nil {
			t.Skip("no sleep")
		}
		c = exec.Command(sleep, "60")
	} else {
		self := os.Args[0]
		if _, err := os.Stat(self); err != nil {
			t.Skipf("cannot re-execute this test binary (%s): %v", self, err)
		}
		bin := filepath.Join(dir, name)
		// Unlink first: a write through an entry that is being executed gets
		// ETXTBSY, where unlinking it does not.
		_ = os.Remove(bin)
		// A link names the process just as well as a copy does, since exec reads
		// the path and not the inode. A separate filesystem is the failure to
		// expect, so fall back to copying.
		if err := os.Link(self, bin); err != nil {
			data, rerr := os.ReadFile(self)
			if rerr != nil {
				t.Skipf("cannot link (%v) or read (%v) %s", err, rerr, self)
			}
			if err := os.WriteFile(bin, data, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		c = exec.Command(bin)
		c.Env = append(os.Environ(), namedSleepEnv+"=1")
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	return c
}

func writeLock(t *testing.T, p paths, pid int, startedAt time.Time) {
	t.Helper()
	if err := daemon.WriteLockfile(p.pidfile, daemon.LockInfo{Pid: int32(pid), Socket: p.socket, Version: "test", StartedAt: startedAt.Unix()}); err != nil {
		t.Fatal(err)
	}
}

// stop and kill --force must not signal a process that merely has the
// lockfile's pid. Each case fails a different part of the identity check.
func TestStopAndKillDoNotSignalOtherProcesses(t *testing.T) {
	for _, c := range []struct {
		name  string
		image string        // "" is sleep itself
		start time.Duration // startedAt relative to the process's start
	}{
		{"neither factor", "", -time.Minute},
		{"shelld-named process started after the lockfile", "shelld-sleep", -time.Minute},
		{"old process that is not shelld", "", time.Minute},
	} {
		for _, cmd := range [][]string{{"stop"}, {"kill", "--force"}} {
			t.Run(c.name+"/"+cmd[0], func(t *testing.T) {
				p := newPaths(t)
				victim := startSleep(t, p.dir, c.image)
				time.Sleep(50 * time.Millisecond)
				writeLock(t, p, victim.Process.Pid, time.Now().Add(c.start))
				code, out := p.shelld(t, nil, cmd...)
				if !alive(victim.Process.Pid) {
					t.Fatalf("%v signalled pid %d (exit %d): %s", cmd, victim.Process.Pid, code, out)
				}
				// Nothing serves the socket, so the stale lockfile is removed.
				if code != 0 {
					t.Fatalf("%v: exit %d: %s", cmd, code, out)
				}
				if _, err := os.Stat(p.pidfile); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale lockfile not removed: %v", err)
				}
			})
		}
	}
}

// A process the identity check accepts, and that doesn't take the instance
// lock (an older daemon, or shelld-rs), is signalled.
func TestStopSignalsLocklessDaemon(t *testing.T) {
	p := newPaths(t)
	d := startSleep(t, p.dir, "shelld")
	time.Sleep(50 * time.Millisecond)
	writeLock(t, p, d.Process.Pid, time.Now().Add(time.Second))
	code, out := p.shelld(t, nil, "stop")
	if code != 0 {
		t.Fatalf("stop: exit %d: %s", code, out)
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(d.Process.Pid) }, "lockless daemon not terminated")
}

// With a stale lockfile naming another process, kill targets the daemon that
// holds the instance lock, not the lockfile's pid.
func TestKillTargetsInstanceLockHolder(t *testing.T) {
	p := newPaths(t)
	run := p.startRun(t)
	other := startSleep(t, p.dir, "")
	writeLock(t, p, other.Process.Pid, time.Now().Add(-time.Minute))
	code, out := p.shelld(t, nil, "kill", "--force")
	if code != 0 || !alive(other.Process.Pid) {
		t.Fatalf("kill: exit %d, sleep alive %v: %s", code, alive(other.Process.Pid), out)
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(run.Process.Pid) }, "daemon not killed")
}

func readLock(t *testing.T, p paths) *daemon.LockInfo {
	t.Helper()
	l, err := daemon.ReadLockfile(p.pidfile)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRunRefusesSecondInstance(t *testing.T) {
	p := newPaths(t)
	first := p.startRun(t)
	code, out := p.shelld(t, nil, "run")
	if code != exitAlreadyServing {
		t.Fatalf("second run: exit %d, want %d: %s", code, exitAlreadyServing, out)
	}
	info, err := daemon.Probe(p.socket, time.Second)
	if err != nil || int(info.Pid) != first.Process.Pid {
		t.Fatalf("first daemon no longer answers: %v %+v", err, info)
	}
	if l := readLock(t, p); l == nil || int(l.Pid) != first.Process.Pid {
		t.Fatalf("lockfile %+v, want the first daemon's pid %d", l, first.Process.Pid)
	}
}

// daemonProcesses counts `shelld run` processes serving socket.
//
// ps rather than /proc, because /proc does not exist on darwin and a helper that
// answers "none" there does not fail -- it makes every "exactly one daemon"
// assertion into one that cannot distinguish one daemon from none, which is the
// wrong way round for a check whose whole job is catching a second daemon. Of
// the three callers only one noticed and skipped.
//
// The other consequence was worse than a weak assertion, and it is what this was
// found by. killDaemons is the cleanup every restart test registers, and it kills
// what this returns, so an empty answer made it a no-op: each of
// TestRestartWithNoDaemonStartsOne and TestRestartReplacesTheDaemonWithANewProcess
// left a daemon behind on every macOS run, reparented to pid 1 with nothing to reap
// it. Measured on macOS 26.4 arm64 with the /proc version: 13 survivors from five
// runs, the oldest 2h42m old, and both tests leaked running alone as well as in the
// suite. See sweepLeakedDaemons, which reports a recurrence instead of waiting for
// someone to notice the process list.
//
// -ww stops ps truncating the argument list, which is where the socket is. The
// match is anchored on bin, an absolute path in this run's own temp dir, so it
// cannot pick up another test's daemon -- nor this ps, whose own argv does not
// begin with it.
func daemonProcesses(socket string) []int {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,args=").Output()
	if err != nil {
		// Not a nil return, which is the mistake above in a second costume: "ps did
		// not run" and "no daemon is running" are opposite facts, and reporting the
		// first as the second is what made a leak silent for five runs. A panic in a
		// cleanup still reddens the run, which is the whole requirement here.
		panic(fmt.Sprintf("ps failed, so this cannot say whether a daemon serves %s: %v", socket, err))
	}
	var pids []int
	for line := range strings.SplitSeq(string(out), "\n") {
		field, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		args = strings.TrimLeft(args, " ")
		if strings.HasPrefix(args, bin+" run ") && strings.Contains(args, socket) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestConcurrentDaemonizeStartsOneDaemon(t *testing.T) {
	for round := range 5 {
		p := newPaths(t)
		codes := make(chan string, 2)
		for range 2 {
			go func() {
				code, out := p.shelld(t, nil, "daemonize")
				codes <- fmt.Sprintf("%d %s", code, out)
			}()
		}
		for range 2 {
			if r := <-codes; !strings.HasPrefix(r, "0 ") {
				t.Errorf("round %d: daemonize: %s", round, r)
			}
		}
		info, err := daemon.Probe(p.socket, time.Second)
		if err != nil {
			t.Fatalf("round %d: no daemon: %v", round, err)
		}
		time.Sleep(200 * time.Millisecond)
		if pids := daemonProcesses(p.socket); len(pids) != 1 || pids[0] != int(info.Pid) {
			t.Errorf("round %d: daemons %v, /health pid %d", round, pids, info.Pid)
		}
		if l := readLock(t, p); l == nil || l.Pid != info.Pid {
			t.Errorf("round %d: lockfile %+v, /health pid %d", round, l, info.Pid)
		}
		for _, pid := range daemonProcesses(p.socket) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// taskStates maps each of pid's thread ids to its scheduler state.
//
// The state is the third field of /proc/<pid>/task/<tid>/stat. The second field, comm,
// can contain spaces and parentheses, so the field before it has to be found at the
// last ')' in the line and not by splitting on whitespace: a process named "sh (2)"
// defeats the obvious version.
func taskStates(pid int) (map[string]string, error) {
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(dir + "/" + e.Name() + "/stat")
		if err != nil {
			continue // the thread exited between the ReadDir and the read
		}
		line := string(b)
		i := strings.LastIndexByte(line, ')')
		if i < 0 || i+2 >= len(line) {
			return nil, fmt.Errorf("task %s: cannot parse stat %q", e.Name(), line)
		}
		states[e.Name()] = line[i+2 : i+3]
	}
	return states, nil
}

// notStoppedTasks names the tasks of pid that are not job-control stopped, as
// "<tid>=<state>", and reports how many tasks were seen.
//
// Both ends of the window this file asserts across ask this same question, and the answer
// has to mean the same thing at both: a definition of stopped that drifts between them is
// no longer one premise, which is why the classification lives in one place. "T" is a
// group stop and "t" a ptrace stop, which is equally not answering — rejecting it would
// only make these tests flaky under a debugger. Everything else counts as not stopped,
// including the "Z" of a daemon that exited while its parent had yet to reap it. The
// result is sorted so that a failure message reads the same on a rerun.
func notStoppedTasks(pid int) (awake []string, tasks int, err error) {
	states, err := taskStates(pid)
	if err != nil {
		return nil, 0, err
	}
	for tid, st := range states {
		if st != "T" && st != "t" {
			awake = append(awake, tid+"="+st)
		}
	}
	sort.Strings(awake)
	return awake, len(states), nil
}

// waitAllTasksStopped waits until every one of pid's tasks is job-control stopped.
//
// A group stop is delivered per task: kill(pid, SIGSTOP) makes the signal pending for
// the whole thread group, and each task stops when it next reaches a signal check, so
// the group leader can already be stopped while a sibling still runs. The daemon is Go
// and answers /health from whichever thread the runtime picks, so the leader's state is
// not the property wanted here; every task's is.
func waitAllTasksStopped(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		// TODO(untested): there is no /proc here, so the premise rests on the probe
		// alone and a task that has not reached its signal check yet can still answer
		// it. Closing this needs a darwin way to read a task's state; nothing in this
		// file has been run on darwin.
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		running, tasks, err := notStoppedTasks(pid)
		if err != nil {
			t.Fatalf("daemon %d: %v", pid, err)
		}
		if tasks > 0 && len(running) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon %d did not stop within 5s: %d tasks, still running %v",
				pid, tasks, running)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertTasksStillStopped fails if pid is no longer job-control stopped, and says which
// task is not.
//
// It is the near-end premise check read at the far end, and it belongs only on the path
// where the exit code was not the expected one. On the expected path there is nothing left
// to establish: daemonize reporting that the daemon "did not respond" is itself a
// statement that the daemon was not answering when daemonize looked, made by the subject.
// It is when the exit code is wrong that the cause is ambiguous — a daemon resumed part
// way through the run answers, daemonize takes its already-running path and exits 0, and
// the assertion on the exit code then reads as daemonize having replaced a live daemon,
// which is the misreading the near-end check exists to prevent. Sampling only on that
// path also means a run that earned its claim and was resumed afterwards stays green.
//
// A daemon that exited inside the window fails the premise in the same way and is named
// the same way: its parent has not reaped it, so its task directory still reads, as "Z".
// An unreadable /proc is left as a failure rather than a skip for the same reason.
func assertTasksStillStopped(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return // no /proc; see the TODO in waitAllTasksStopped
	}
	awake, tasks, err := notStoppedTasks(pid)
	if err != nil {
		t.Fatalf("daemon %d: the stop did not last through daemonize: %v", pid, err)
	}
	if tasks == 0 || len(awake) > 0 {
		t.Fatalf("daemon %d: the stop did not last through daemonize: %d tasks, not stopped %v",
			pid, tasks, awake)
	}
}

// A daemon that stops answering keeps its files; daemonize doesn't replace
// it.
func TestDaemonizeKeepsBusyDaemon(t *testing.T) {
	p := newPaths(t)
	run := p.startRun(t)
	if err := run.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer run.Process.Signal(syscall.SIGCONT)
	waitAllTasksStopped(t, run.Process.Pid)
	// The premise, asserted rather than assumed. kill(2) promises the signal is pending,
	// not that it has taken effect, and this test's subject is a daemon that does not
	// answer. A daemon that still answers sends daemonize down its already-running path,
	// which exits 0 — and the failure then lands on the assertion below, reading as if
	// daemonize had replaced a live daemon. Deleting the SIGSTOP above reproduces that
	// failure byte for byte, which is how this line earns its place. The wait above does
	// not replace it: task states are evidence for not answering, and this is the thing
	// itself, so a future change that lets something else answer on the socket reds here
	// rather than on the assertion below.
	//
	// It costs the whole timeout every run, because a stopped daemon leaves the connect
	// queued rather than refusing it, so the probe can only time out. 200ms is what
	// startRun already trusts to tell a live daemon from a dead one — and startRun has
	// already had an answer out of this socket, which is what makes the timeout here
	// evidence of the stop rather than of a socket that never worked.
	if info, err := daemon.Probe(p.socket, 200*time.Millisecond); err == nil {
		t.Fatalf("daemon %d answered /health as pid %d after SIGSTOP", run.Process.Pid, info.Pid)
	}
	code, out := p.shelld(t, []string{"SHELLD_DAEMONIZE_WAIT=1s"}, "daemonize")
	if code != 1 || !strings.Contains(out, "did not respond") {
		// The premise first, so that a stop which did not last is reported as itself
		// rather than as daemonize having replaced a live daemon.
		assertTasksStillStopped(t, run.Process.Pid)
		t.Fatalf("daemonize: exit %d: %s", code, out)
	}
	if l := readLock(t, p); l == nil || int(l.Pid) != run.Process.Pid {
		t.Fatalf("lockfile %+v, want pid %d", l, run.Process.Pid)
	}
	if _, err := os.Stat(p.socket); err != nil {
		t.Fatalf("socket removed: %v", err)
	}
	if pids := daemonProcesses(p.socket); len(pids) != 1 {
		t.Fatalf("daemons %v", pids)
	}
}

// The sentence is asserted as well as the code, and specifically that the child's
// status reaches it. Exit 1 alone is reached by every other refusal daemonize can
// make, and "still starting" -- the only outcome that asks the caller to wait --
// must not be what a caller is told about a child that is already dead.
func TestDaemonizeFailsFastWhenChildDies(t *testing.T) {
	p := newPaths(t)
	p.socket = filepath.Join(p.dir, "missing", "s.sock")
	start := time.Now()
	code, out := p.shelld(t, nil, "daemonize")
	if code != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("daemonize: exit %d after %v: %s", code, time.Since(start), out)
	}
	if !strings.Contains(out, "exited with status") {
		t.Errorf("output does not report the child's status: %s", out)
	}
	if strings.Contains(out, "still starting") {
		t.Errorf("a dead child was reported as still starting: %s", out)
	}
}

// The same wrong report, reached the other way: the child dies after daemonize's
// deadline has passed rather than before it. The loop is the only thing that ever
// read the child's exit, so a deadline that passes first left the status unread
// and the caller was told the daemon was still starting -- which asks a script to
// wait for a process that is already gone.
//
// A one-nanosecond wait puts the death on that side of the deadline without
// timing anything: the loop cannot run, so whatever daemonize reports it reports
// from after the loop.
//
// The premise is asserted afterwards rather than assumed, because a run slow
// enough that the child is still alive would produce the defect's exact output.
// That makes "still starting" ambiguous on its own, and the process list is what
// separates the two: no `shelld run` for this socket means the child really was
// gone when daemonize spoke.
func TestDaemonizeReportsAChildThatDiesAfterTheDeadline(t *testing.T) {
	p := newPaths(t)
	p.socket = filepath.Join(p.dir, "missing", "s.sock")
	code, out := p.shelld(t, []string{"SHELLD_DAEMONIZE_WAIT=1ns"}, "daemonize")
	if pids := daemonProcesses(p.socket); len(pids) > 0 {
		t.Skipf("the child %v is still alive, so this run is not the case this test is about (exit %d: %s)", pids, code, out)
	}
	if code != 1 {
		t.Errorf("exit %d for a child that is gone, want 1: %s", code, out)
	}
	if strings.Contains(out, "still starting") {
		t.Errorf("a child that had already exited was reported as still starting: %s", out)
	}
	if !strings.Contains(out, "exited with status") {
		t.Errorf("output does not report the child's status: %s", out)
	}
}

// POST /shutdown ends the daemon through its cleanup: lockfile and socket
// removed, instance lock free.
func TestShutdownRemovesFiles(t *testing.T) {
	p := newPaths(t)
	p.startRun(t)
	code, out := p.shelld(t, nil, "stop")
	if code != 0 {
		t.Fatalf("stop: exit %d: %s", code, out)
	}
	for _, f := range []string{p.pidfile, p.socket} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", f, err)
		}
	}
	if holder, err := daemon.InstanceHolder(p.socket); err != nil || holder != 0 {
		t.Errorf("instance lock held by %d: %v", holder, err)
	}
}

func TestLockfileWrittenByDaemonNamesIt(t *testing.T) {
	p := newPaths(t)
	run := p.startRun(t)
	raw, _ := os.ReadFile(p.pidfile)
	var l daemon.LockInfo
	if err := json.Unmarshal(raw, &l); err != nil || int(l.Pid) != run.Process.Pid || l.Nonce == "" {
		t.Fatalf("lockfile %s", raw)
	}
	if info, err := daemon.Probe(p.socket, time.Second); err != nil || info.Nonce != l.Nonce {
		t.Fatalf("/health nonce %+v %v, lockfile nonce %q", info, err, l.Nonce)
	}
	if code, out := p.shelld(t, nil, "status"); code != 0 {
		t.Fatalf("status: %d %s", code, out)
	}
	if holder, _ := daemon.InstanceHolder(p.socket); holder != run.Process.Pid {
		t.Fatalf("instance lock holder %d, want %d", holder, run.Process.Pid)
	}
}

// When something still answers on the socket but the lockfile's pid is not
// shown to be it (a renamed or foreign daemon), stop signals nothing and
// leaves the lockfile and socket in place.
func TestStopLeavesAnsweringUnverifiedDaemonAlone(t *testing.T) {
	p := newPaths(t)
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprintf(w, `{"status":"ok","pid":1,"socket":%q,"version":"x","startedAt":0}`, p.socket)
			return
		}
		http.Error(w, "busy", http.StatusInternalServerError)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	other := startSleep(t, p.dir, "")
	writeLock(t, p, other.Process.Pid, time.Now().Add(-time.Minute))
	code, out := p.shelld(t, nil, "stop")
	if code != 1 || !alive(other.Process.Pid) {
		t.Fatalf("stop: exit %d, sleep alive %v: %s", code, alive(other.Process.Pid), out)
	}
	for _, f := range []string{p.pidfile, p.socket} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
}

// A process that answers on the socket without the lockfile's nonce is not
// taken for the daemon.
func TestNonceMismatchIsNotAlreadyRunning(t *testing.T) {
	p := newPaths(t)
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","pid":1,"socket":%q,"version":"x","startedAt":0,"nonce":"impostor"}`, p.socket)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := daemon.WriteLockfile(p.pidfile, daemon.LockInfo{Pid: 1, Socket: p.socket, Version: "x", StartedAt: 0, Nonce: daemon.NewNonce()}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"daemonize", "status"} {
		if code, out := p.shelld(t, nil, cmd); code != exitUnverified || !strings.Contains(out, "not the daemon that wrote the lockfile") {
			t.Errorf("%s: exit %d: %s", cmd, code, out)
		}
	}
	// With no lockfile at all, as after the real daemon stopped cleanly,
	// the responder is not verified either.
	os.Remove(p.pidfile)
	for _, cmd := range []string{"daemonize", "status"} {
		if code, out := p.shelld(t, nil, cmd); code != exitUnverified || !strings.Contains(out, "no lockfile") || !strings.Contains(out, "different --pidfile") {
			t.Errorf("%s without a lockfile: exit %d: %s", cmd, code, out)
		}
	}
	if _, err := os.Stat(p.pidfile); !errors.Is(err, os.ErrNotExist) {
		t.Error("a lockfile appeared: daemonize started a daemon beside the squatter")
	}
}

// runDefault runs shelld with no path flags and TMPDIR set, so it uses the
// per-user default directory under tmp.
func runDefault(t *testing.T, tmp string, cmd ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, cmd...)
	c.Env = append(os.Environ(), "TMPDIR="+tmp, "SHELLD_SOCKET=", "SHELLD_PIDFILE=", "SHELLD_LOGFILE=")
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// A default directory another user could control is refused by every
// command before it connects, starts or reads anything; a missing one is
// created private by daemonize.
func TestDefaultDirChecked(t *testing.T) {
	tmp, err := os.MkdirTemp("", "sdt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "shelld-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777)
	for _, cmd := range [][]string{{"status"}, {"sessions"}, {"stop"}, {"kill", "--force"}, {"daemonize"}} {
		if code, out := runDefault(t, tmp, cmd...); code != 1 || !strings.Contains(out, "unsafe permissions") {
			t.Errorf("%v with a 0777 default dir: exit %d: %s", cmd, code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "shelld.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a daemon started in the unsafe dir")
	}

	os.RemoveAll(dir)
	if code, out := runDefault(t, tmp, "daemonize"); code != 0 {
		t.Fatalf("daemonize: exit %d: %s", code, out)
	}
	defer runDefault(t, tmp, "kill", "--force")
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("default dir %v, %v", st, err)
	}
	sock, _ := os.Lstat(filepath.Join(dir, "shelld.sock"))
	if sock == nil || sock.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket %v", sock)
	}
	if code, out := runDefault(t, tmp, "status"); code != 0 {
		t.Fatalf("status: exit %d: %s", code, out)
	}
}

// TestDamagedLockfileIsNotReportedAsNoDaemon is the whole fold end to end, with
// nothing listening on the socket: the only witness left is a lockfile that will
// not parse, and every answer below would otherwise be a claim about a file
// shelld did not manage to read.
//
// status must not exit 4. Exit 4 is the code a caller acts on by starting a
// daemon, which is how an unreadable lockfile becomes a second daemon beside the
// one already running. stop and kill must not exit 0: both reach a DaemonGone arm
// that deletes the leftover files and reports success, and with an unreadable
// lockfile there is nothing establishing that the daemon is gone.
//
// No output on any of these paths may say there is no lockfile. That sentence
// sends an operator looking for a file that is sitting right there, and a damaged
// file needs a different repair from a missing one.
func TestDamagedLockfileIsNotReportedAsNoDaemon(t *testing.T) {
	p := newPaths(t)
	// Unparseable rather than mode 000: a suite running as root reads a mode-000
	// file, and the fixture would stop reproducing without failing.
	if err := os.WriteFile(p.pidfile, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup: something is at %s: %v", p.socket, err)
	}
	for _, c := range []struct {
		cmd  []string
		want int
	}{
		{[]string{"status"}, 5},
		{[]string{"stop"}, 1},
		{[]string{"kill", "--force"}, 1},
	} {
		code, out := p.shelld(t, nil, c.cmd...)
		if code != c.want {
			t.Errorf("%v: exit %d, want %d: %s", c.cmd, code, c.want, out)
		}
		if strings.Contains(out, "no lockfile") || strings.Contains(out, "not running") {
			t.Errorf("%v says the lockfile or the daemon is absent, and %s is there: %s", c.cmd, p.pidfile, out)
		}
		if !strings.Contains(out, p.pidfile) {
			t.Errorf("%v does not name the file it could not read: %s", c.cmd, out)
		}
	}
	// Nothing deleted it on the way through: every one of those paths had an arm
	// that removes the leftovers of a daemon it decided was gone.
	if _, err := os.Stat(p.pidfile); err != nil {
		t.Errorf("the damaged lockfile was removed by a command that could not read it: %v", err)
	}
}

// TestDamagedPidfileDoesNotAimAtTheDefaultDaemon is the destructive half, with a
// daemon that must survive. --pidfile says which daemon to act on; resolving it
// means reading that file for the socket, and a read that fails leaves the
// default socket in place -- so the command goes on to kill whatever serves the
// default socket, which is the one daemon --pidfile was given to avoid.
//
// The daemon here is on the default paths under a private TMPDIR, so the only
// thing at risk is the one this test started.
func TestDamagedPidfileDoesNotAimAtTheDefaultDaemon(t *testing.T) {
	tmp, err := os.MkdirTemp("", "sdd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	if code, out := runDefault(t, tmp, "daemonize"); code != 0 {
		t.Fatalf("daemonize: exit %d: %s", code, out)
	}
	defer runDefault(t, tmp, "kill", "--force")
	other := filepath.Join(tmp, "other.pid")
	// Unparseable rather than mode 000: a suite running as root reads a mode-000
	// file, and the fixture would stop reproducing without failing.
	if err := os.WriteFile(other, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range [][]string{{"kill", "--force"}, {"stop"}, {"status"}} {
		args := append(append([]string{}, cmd...), "--pidfile", other)
		if code, out := runDefault(t, tmp, args...); code == 0 {
			t.Errorf("%v with an unreadable --pidfile reported success: %s", cmd, out)
		} else if !strings.Contains(out, other) {
			t.Errorf("%v does not name the file it could not read: %s", cmd, out)
		}
	}
	// The assertion the rest of it is for.
	if code, out := runDefault(t, tmp, "status"); code != 0 {
		t.Fatalf("the default daemon is gone after acting on another --pidfile: exit %d: %s", code, out)
	}
}

// A daemon that cannot write its lockfile could never be verified, so run
// fails and leaves nothing behind.
func TestRunFailsWithoutLockfile(t *testing.T) {
	p := newPaths(t)
	p.pidfile = filepath.Join(p.dir, "missing", "s.pid")
	code, out := p.shelld(t, nil, "run")
	if code != 1 || !strings.Contains(out, "lockfile write failed") {
		t.Fatalf("run: exit %d: %s", code, out)
	}
	if _, err := os.Lstat(p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if holder, err := daemon.InstanceHolder(p.socket); err != nil || holder != 0 {
		t.Errorf("instance lock held by %d: %v", holder, err)
	}
}

// With a daemon on the default socket and another on its own socket,
// kill --force --pidfile names the second one's lockfile and must kill that
// daemon, not the one serving the default socket; a socket that contradicts
// the lockfile is refused.
func TestKillUsesLockfileSocket(t *testing.T) {
	tmp, err := os.MkdirTemp("", "sdk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	env := []string{"TMPDIR=" + tmp, "SHELLD_SOCKET=", "SHELLD_PIDFILE=", "SHELLD_LOGFILE="}
	d := exec.Command(bin, "run")
	d.Env = append(os.Environ(), env...)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Kill(); _, _ = d.Process.Wait() })
	defSock := filepath.Join(tmp, "shelld-"+strconv.Itoa(os.Getuid()), "shelld.sock")
	waitUntil(t, 5*time.Second, func() bool { return daemon.IsAlive(defSock, 200*time.Millisecond) }, "default daemon never answered")

	b := newPaths(t)
	bRun := b.startRun(t)

	run := func(args ...string) (int, string) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), env...)
		out, err := c.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		return 0, string(out)
	}
	if code, out := run("kill", "--force", "--pidfile", b.pidfile); code != 0 {
		t.Fatalf("kill --pidfile: exit %d: %s", code, out)
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(bRun.Process.Pid) || !alive(d.Process.Pid) }, "neither daemon was killed")
	if !alive(d.Process.Pid) || !daemon.IsAlive(defSock, time.Second) {
		t.Fatal("the default socket's daemon was killed instead of the lockfile's")
	}
	waitUntil(t, 5*time.Second, func() bool { return !alive(bRun.Process.Pid) }, "the lockfile's daemon was not killed")
	if _, err := os.Stat(filepath.Join(tmp, "shelld-"+strconv.Itoa(os.Getuid()), "shelld.pid")); err != nil {
		t.Fatalf("the default daemon's lockfile was removed: %v", err)
	}
	if _, err := os.Stat(b.pidfile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the killed daemon's stale lockfile remains: %v", err)
	}

	c := newPaths(t)
	cRun := c.startRun(t)
	if code, out := run("kill", "--force", "--socket", defSock, "--pidfile", c.pidfile); code != 1 || !strings.Contains(out, "differs") {
		t.Fatalf("contradicting --socket: exit %d: %s", code, out)
	}
	if !alive(d.Process.Pid) || !alive(cRun.Process.Pid) {
		t.Fatal("a daemon died after a refused kill")
	}
}
