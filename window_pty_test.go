//go:build linux || darwin

package shelld

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSpawnEchoReachesScrollback(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "echo", Shell: "/bin/sh", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if w.Pid() == 0 || !w.IsAlive() {
		t.Fatal("not running")
	}
	if err := w.SendKeys("echo shelld-$((6*7))", true, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(20)
		return strings.Contains(out, "shelld-42")
	}, "marker not seen in scrollback")
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "shelld-42")
	}, "marker not seen in grid")
}

// A one-row window is a working window, not a degenerate one: the shell starts,
// output reaches the history, and the window stays alive. This is the evidence
// behind MinCols having no counterpart for rows -- CheckSize accepting 1 only
// says the check lets it through, and says nothing about whether the grid and
// the pty survive it.
//
// The marker is asserted in the history rather than in the visible grid. With
// one row there is exactly one line on screen and the shell's next prompt takes
// it, so a grid assertion would be a race against the prompt rather than a
// statement about the size.
func TestOneRowWindowWorks(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "onerow", Shell: "/bin/sh", Cols: 80, Rows: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if w.Cols() != 80 || w.Rows() != 1 {
		t.Fatalf("size %dx%d, want 80x1 -- a row was added behind the caller's back", w.Cols(), w.Rows())
	}
	// stty reads the size from the kernel's pty, so this is the one assertion
	// that cannot pass on bookkeeping alone: a floor applied anywhere between
	// WindowOpts and the pty shows up here as a different number of rows.
	if err := w.SendKeys("stty size; echo shelld-$((6*7))", true, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(40)
		return strings.Contains(out, "shelld-42")
	}, "marker not seen in a one-row window's history")
	if out, _ := w.Capture(40); !strings.Contains(out, "1 80") {
		t.Errorf("stty size did not report 1 80 in:\n%s", out)
	}
	// The grid is asserted separately from the pty: the two are sized from the
	// same pair of numbers, and a floor applied to one of them would leave a
	// window that reports and drives one size while parsing output into another.
	func() {
		g := w.LockVT()
		defer w.UnlockVT()
		if c, r := g.Cols(), g.Lines(); c != 80 || r != 1 {
			t.Errorf("grid is %dx%d, want 80x1 -- the grid and the pty must be the same size", c, r)
		}
	}()
	if !w.IsAlive() {
		t.Fatal("window died after writing one line to a one-row grid")
	}
}

func TestShellExitMarksDeadAndWakesWaiters(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "exiter", Shell: `/bin/sh -c "sleep 0.2; exit 0"`, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	woke := w.Dirty().Wait()
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("dirty waiter not woken")
	}
	waitFor(t, 5*time.Second, func() bool { return !w.IsAlive() }, "window still alive after exit")
}

func TestInjectDrainDeliversWhenIdle(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "inj", Shell: "/bin/sh", Env: [][2]string{{"PS1", "$ "}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, w.isIdleForInjection, "prompt never idle")
	if _, err := w.InjectQueue().Enqueue("echo injected-marker", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(50)
		return strings.Count(out, "injected-marker") >= 2 // echoed input + output
	}, "inject never delivered")
	if w.InjectQueue().Len() != 0 {
		t.Fatal("queue not drained")
	}
}

// bash 5.2's readline writes ESC[?2004h before every prompt; inject must
// still see the prompt.
func TestInjectDeliversToBashReadline(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	// readline enables bracketed paste only on a terminal that supports it.
	w, err := NewWindow(WindowOpts{Name: "bash", Command: []string{bash, "--norc", "--noprofile", "-i"},
		Env: [][2]string{{"PS1", "$ "}, {"TERM", "xterm-256color"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if _, err := w.InjectQueue().Enqueue("echo injected-$((6*7))", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		out, _ := w.Capture(50)
		return strings.Contains(out, "injected-42")
	}, "inject never delivered to bash readline")
}

// A live witness for the reduction this branch is about: bash's prompt returning
// onto a line a program redrew with a bare CR, which is what raw history holds
// whenever a program ends its output with `\r` rather than `\n` — every spinner
// and progress bar an agent TUI draws.
//
// The daemon's predicate reads idle there, because the render places the prompt
// over the text the CR sent the cursor back across, and delivers. IsIdle over
// plain history does not, because the line it matches still holds the redrawn
// text and the CR. Both directions are asserted: over the three flows the other
// PTY tests run the two predicates agree at every sampled point, so without a
// case that separates them there is no live evidence that the daemon's verdict is
// a different function from the one it replaced.
func TestInjectDeliversWhenACRRedrawnLineHoldsThePrompt(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	w, err := NewWindow(WindowOpts{Name: "crprompt", Command: []string{bash, "--norc", "--noprofile", "-i"},
		Env: [][2]string{{"PS1", "$ "}, {"TERM", "xterm-256color"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, w.isIdleForInjection, "prompt never idle")
	if err := w.SendKeys(`printf 'ab\r'`, true, nil); err != nil {
		t.Fatal(err)
	}
	// The premise: the prompt is on a line whose text was redrawn by a CR. Without
	// it the assertions below could be about a plain prompt line, which the two
	// predicates read the same way.
	lines := func() string { return w.Scrollback().LastN(w.IdleDetector().ScanLines()) }
	waitFor(t, 5*time.Second, func() bool {
		last := lines()
		i := strings.LastIndex(last, "ab\r")
		return i >= 0 && strings.HasSuffix(last, "$ ") && !strings.Contains(last[i:], "\n")
	}, "bash never put its prompt on the CR-redrawn line")
	if w.IdleDetector().IsIdle(lines()) {
		t.Errorf("IsIdle over plain history reads idle for %q; this case exists because it "+
			"does not, and the assertion below is not about a different function", lines())
	}
	if !w.isIdleForInjection() {
		t.Fatalf("the daemon reads %q as busy; the terminal shows a bare prompt there", lines())
	}
	if _, err := w.InjectQueue().Enqueue("echo cr-marker-$((6*7))", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		out, _ := w.Capture(50)
		return strings.Contains(out, "cr-marker-42")
	}, "inject never delivered at a prompt on a CR-redrawn line")
}

// While a command runs in bash, the window is not idle, though the prompt
// line it was typed after is still among the recent lines.
func TestNotIdleWhileBashCommandRuns(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	w, err := NewWindow(WindowOpts{Name: "bash", Command: []string{bash, "--norc", "--noprofile", "-i"},
		Env: [][2]string{{"PS1", "$ "}, {"TERM", "xterm-256color"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	idle := w.isIdleForInjection
	waitFor(t, 5*time.Second, idle, "prompt never idle")
	for _, cmd := range []string{"cat", "sleep 3"} {
		if err := w.SendKeys(cmd, true, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		for i := 0; i < 10; i++ {
			if idle() {
				t.Fatalf("%s: idle while running; lines %q", cmd, w.Scrollback().LastN(5))
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := w.SendRawKey("\x03"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, idle, cmd+": prompt never came back")
	}
}

// A bare command never runs a file of that name from the window's working
// directory, even with the real program on PATH.
func TestBareCommandIgnoresWorkingDirectory(t *testing.T) {
	work, bin := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(work, "claude"), []byte("#!/bin/sh\necho FROM-WORKTREE; sleep 5\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho FROM-PATH; sleep 5\n"), 0o755)
	w, err := NewWindow(WindowOpts{Name: "p", Cwd: work, Command: []string{"claude"}, Environ: []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + work}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, func() bool { out, _ := w.Capture(10); return strings.Contains(out, "FROM-") }, "no output")
	if out, _ := w.Capture(10); !strings.Contains(out, "FROM-PATH") {
		t.Fatalf("ran the worktree's file: %q", out)
	}
	if _, err := NewWindow(WindowOpts{Name: "q", Cwd: work, Command: []string{"claude"}, Environ: []string{"PATH=/usr/bin:/bin"}}); err == nil {
		t.Fatal("a bare name found only in the working directory started")
	}
}

// The idle pattern comes from the program itself, even when its path has
// spaces and was passed unquoted as Command[0].
func TestIdlePatternFromSpacedProgramPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files", "PowerShell")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "pwsh")
	if err := os.Symlink("/bin/sh", prog); err != nil {
		t.Fatal(err)
	}
	w, err := NewWindow(WindowOpts{Name: "pwsh", Command: []string{prog}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if got, want := w.IdleDetector().Pattern().String(), RustRegex(`(?m)^PS [^>]+>\s*$`); got != want {
		t.Fatalf("pattern %s, want the PowerShell pattern", got)
	}
}

func TestInjectHoldsWhileHumanTyping(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "hold", Shell: "/bin/sh", Env: [][2]string{{"PS1", "$ "}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, w.isIdleForInjection, "prompt never idle")
	_, _ = w.InjectQueue().Enqueue("echo held", 0)
	stop := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(stop) {
		w.InputTracker().Touch()
		time.Sleep(50 * time.Millisecond)
	}
	if w.InjectQueue().Len() != 1 {
		t.Fatal("delivered while the human was typing")
	}
}

// TestKillReapsLinkedGrandchild mirrors shelld-rs's regression test for
// leaked claude sessions: the PTY shell owns a still-linked descendant, here
// one that setsid()s into its own session the way a launcher
// leaves the program it starts. (setsid(1) does not fork because a non-interactive shell
// does not make it a process-group leader.)
// Subtests rather than a bare loop, because the second arm needs a tool the first
// does not and a skip has to be attributable to the arm that was skipped.
func TestKillReapsLinkedGrandchild(t *testing.T) {
	for _, arm := range []struct{ name, prefix string }{
		{"the shell's own session", ""},
		{"a session of its own", "setsid "},
	} {
		t.Run(arm.name, func(t *testing.T) {
			if arm.prefix != "" {
				// setsid(1) is util-linux and is not on macOS, where the script
				// instead runs to "/bin/sh: setsid: command not found" and no pid is
				// ever written -- which is how this read before: as the grandchild
				// failing to start. Skipping says which arm did not run; the first
				// arm covers the linked case everywhere.
				if _, err := exec.LookPath("setsid"); err != nil {
					t.Skipf("no setsid(1) to put the grandchild in its own session: %v", err)
				}
			}
			pidFile := t.TempDir() + "/gc.pid"
			inner := "echo $$ > " + pidFile + " ; exec sleep 300"
			shell := `/bin/sh -c "` + arm.prefix + `sh -c '` + inner + `' ; true & wait"`
			w, err := NewWindow(WindowOpts{Name: "gc", Shell: shell, Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			var gc int
			waitFor(t, 5*time.Second, func() bool {
				b, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				gc, err = strconv.Atoi(strings.TrimSpace(string(b)))
				return err == nil && gc > 0
			}, "grandchild pid never written")
			if arm.prefix != "" {
				if sid, own := sessionID(gc), sessionID(w.Pid()); sid == own {
					t.Fatalf("grandchild shares the shell's session %d; test topology is wrong", sid)
				}
			}
			// The premise, in the test rather than in a comment: the wait below is
			// only evidence that the kill reached the grandchild if the grandchild
			// was there to reach. Asserted because the check this replaced answered
			// "gone" for every pid on darwin, and an assertion that cannot see a live
			// process cannot see a leaked one either.
			if processGone(gc) {
				t.Fatalf("grandchild %d is already gone before the window was killed", gc)
			}
			w.Kill()
			waitFor(t, 3*time.Second, func() bool { return processGone(gc) },
				"linked grandchild leaked after window kill")
		})
	}
}

// processGone reports whether pid is neither running nor waiting to be reaped.
//
// kill(pid, 0) rather than a Stat of /proc/<pid>, which darwin has no /proc for:
// there that Stat fails for every pid alive or dead, so the wait it backed here
// ended on its first poll and the assertion could not have caught a leak. It is
// the assertion's whole subject, so the check has to work on both platforms
// rather than be skipped on one.
//
// A zombie still answers signal 0, so its state is asked for as well. This
// grandchild is reparented to init when the shell dies and reaped promptly, but a
// wait satisfied by the zombie would be reporting the reparenting rather than the
// exit, which is a weaker fact than the one claimed.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true // ps has no such process; the kill above raced its exit
	}
	f := strings.Fields(string(out))
	return len(f) > 0 && f[0][0] == 'Z'
}

// sessionID is pid's session id, the thing setsid changes, or -1 if it cannot be
// read. Both callers compare two of these, so a pair of -1s reads as "same
// session" and trips their topology guard rather than passing.
//
// getsid(2) rather than field 6 of /proc/<pid>/stat: the same number on both
// platforms, without a procfs or a parse of a line whose first field can contain
// the separator.
func sessionID(pid int) int {
	sid, err := unix.Getsid(pid)
	if err != nil {
		return -1
	}
	return sid
}

func TestCommandRunsArgvAsGiven(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "argv", Command: []string{"/bin/sh", "-c", `printf '[%s]\n' "$1"`, "sh", "two words"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(10), "[two words]")
	}, "argument with a space was split: "+w.CaptureVisible(10))
}

func TestExactEnvironment(t *testing.T) {
	t.Setenv("SHELLD_TEST_INHERITED", "yes")
	// A daemon started from a terminal that identifies itself, which is the case
	// the child's TERM has to override rather than pass on: the window's
	// terminal is shelld's emulator, not the one shelld was launched from, and a
	// program reading tmux-256color here would ask for keys and capabilities of
	// a terminal it is not talking to.
	t.Setenv("TERM", "tmux-256color")
	// A wrong inherited value rather than none: this box's own COLORTERM is
	// already truecolor, so an absent one would let the child inherit the right
	// answer and the assertion below would pass without shelld setting anything.
	t.Setenv("COLORTERM", "24bit")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("TMUX_PANE", "%1")
	t.Setenv("STY", "1234.pts-0.host")
	t.Setenv("TERM_PROGRAM", "vscode")
	// The child prints the environment rather than expanding it, and that
	// distinction is the test. `echo "TERM=${TERM-unset}"` asks a SHELL what the
	// variable is, and a shell is entitled to have opinions: Apple's bash 3.2 --
	// the /bin/sh of every stock macOS -- sets TERM to "dumb" when it starts
	// without one, so the unset-TERM case below read TERM=dumb and blamed shelld
	// for a value shelld had correctly removed. (Measured: `env -u TERM /bin/sh -c
	// 'echo $TERM'` prints dumb there, while `env -u TERM /usr/bin/env` has no TERM
	// line at all, because the invention is a shell variable and is never
	// exported.) Reading env's output instead means no shell's defaults sit between
	// what shelld built and what is asserted; the sh here only pipes.
	keys := []string{"SHELLD_A", "SHELLD_B", "SHELLD_TEST_INHERITED", "HISTFILE", "TERM", "COLORTERM", "TMUX", "STY", "TERM_PROGRAM"}
	const envEnd = "SHELLD-ENV-END"
	// grep, so that a window 24 rows tall holds the whole answer whatever else the
	// environment carries, and the END marker last so a partial read is not read as
	// a complete one.
	cmd := []string{"/bin/sh", "-c", "env | grep -E '^(" + strings.Join(keys, "|") + ")='; echo " + envEnd}
	run := func(opts WindowOpts) string {
		t.Helper()
		opts.Name, opts.Command, opts.Cols, opts.Rows = "env", cmd, 200, 24
		w, err := NewWindow(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Kill()
		<-w.Done()
		env := map[string]string{}
		waitFor(t, 5*time.Second, func() bool {
			done := false
			for _, l := range strings.Split(w.CaptureVisible(0), "\n") {
				l = strings.TrimSpace(l)
				if l == envEnd {
					done = true
					continue
				}
				if k, v, ok := strings.Cut(l, "="); ok {
					env[k] = v
				}
			}
			return done
		}, "no output")
		val := func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return "unset"
		}
		return fmt.Sprintf("A=%s B=%s INH=%s HIST=%s TERM=%s CT=%s TMUX=%s STY=%s TP=%s END",
			val("SHELLD_A"), val("SHELLD_B"), val("SHELLD_TEST_INHERITED"), val("HISTFILE"),
			val("TERM"), val("COLORTERM"), val("TMUX"), val("STY"), val("TERM_PROGRAM"))
	}

	// Default: inherit, add, HISTFILE disabled, unset removes, and the terminal
	// the child is told about is shelld's own — TERM and COLORTERM set, the
	// outer terminal's TMUX, STY and TERM_PROGRAM identity gone (TERM_PROGRAM
	// names shelld instead, as tmux names itself).
	got := run(WindowOpts{Env: [][2]string{{"SHELLD_A", "1"}}, UnsetEnv: []string{"SHELLD_TEST_INHERITED"}})
	if got != "A=1 B=unset INH=unset HIST=/dev/null TERM=xterm-256color CT=truecolor TMUX=unset STY=unset TP=shelld END" {
		t.Fatalf("inherited: %q", got)
	}
	// A caller that wants another terminal says so: Env is applied after the
	// default, so it wins, and UnsetEnv after that, so it can remove it.
	got = run(WindowOpts{Env: [][2]string{{"SHELLD_A", "1"}, {"TERM", "dumb"}}, UnsetEnv: []string{"SHELLD_TEST_INHERITED", "COLORTERM"}})
	if got != "A=1 B=unset INH=unset HIST=/dev/null TERM=dumb CT=unset TMUX=unset STY=unset TP=shelld END" {
		t.Fatalf("env TERM wins: %q", got)
	}
	got = run(WindowOpts{Env: [][2]string{{"SHELLD_A", "1"}}, UnsetEnv: []string{"SHELLD_TEST_INHERITED", "TERM"}})
	if got != "A=1 B=unset INH=unset HIST=/dev/null TERM=unset CT=truecolor TMUX=unset STY=unset TP=shelld END" {
		t.Fatalf("unset TERM: %q", got)
	}
	// Exact: only Environ, plus Env, minus UnsetEnv; nothing inherited or
	// injected, TERM and COLORTERM included — the caller owns the environment,
	// so a default here would be shelld overruling it.
	got = run(WindowOpts{Environ: []string{"PATH=/usr/bin:/bin", "SHELLD_B=2", "SHELLD_A=0"}, Env: [][2]string{{"SHELLD_A", "1"}}, UnsetEnv: []string{"SHELLD_B"}})
	if got != "A=1 B=unset INH=unset HIST=unset TERM=unset CT=unset TMUX=unset STY=unset TP=unset END" {
		t.Fatalf("exact: %q", got)
	}
}

func TestExitNotification(t *testing.T) {
	notified := make(chan ExitStatus, 1)
	w, err := NewWindow(WindowOpts{Name: "exit", Command: []string{"/bin/sh", "-c", "sleep 0.2; exit 7"}, Cols: 80, Rows: 24,
		OnExit: func(st ExitStatus) { notified <- st }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if _, ok := w.ExitStatus(); ok {
		t.Fatal("exit status before the process ended")
	}
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
	if st, ok := w.ExitStatus(); !ok || st.Code != 7 {
		t.Fatalf("status %+v %v", st, ok)
	}
	select {
	case st := <-notified:
		if st.Code != 7 {
			t.Fatalf("OnExit %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnExit not called")
	}
	// Killing a window whose process already exited returns promptly.
	killed := make(chan struct{})
	go func() { w.Kill(); close(killed) }()
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Fatal("Kill hung after exit")
	}
}

func TestKillReportsSignal(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "kill", Command: []string{"/bin/sleep", "60"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	w.Kill()
	select {
	case <-w.Done():
	default:
		t.Fatal("Done open after Kill")
	}
	if st, ok := w.ExitStatus(); !ok || st.Signal != "SIGKILL" || st.Code != -1 {
		t.Fatalf("status %+v %v", st, ok)
	}
}

// An inherited environment loses the variables that identify the terminal
// the daemon was started from, and names shelld instead; callers can still
// set any of them, and an explicit Environ is left as given.
func TestChildEnvStripsOuterTerminal(t *testing.T) {
	outer := map[string]string{
		"TMUX": "/tmp/tmux-1000/default,1,0", "TMUX_PANE": "%3", "STY": "1.pts", "WINDOW": "0",
		"TERM_PROGRAM": "vscode", "TERM_PROGRAM_VERSION": "1.99", "TERM_SESSION_ID": "w0t0p0",
		"WT_SESSION": "abc", "WT_PROFILE_ID": "{1}", "KITTY_WINDOW_ID": "1", "KITTY_PID": "2",
		"GHOSTTY_RESOURCES_DIR": "/g", "VTE_VERSION": "7600", "XTERM_VERSION": "XTerm(390)",
		"TILIX_ID": "t", "TERMINATOR_UUID": "u", "LC_TERMINAL": "iTerm2", "LC_TERMINAL_VERSION": "3",
		"ZELLIJ": "0", "ZELLIJ_SESSION_NAME": "z", "WEZTERM_EXECUTABLE": "/w", "ITERM_PROFILE": "p",
		"KONSOLE_VERSION": "23", "GNOME_TERMINAL_SCREEN": "/s", "ALACRITTY_LOG": "/a",
	}
	for k, v := range outer {
		t.Setenv(k, v)
	}
	t.Setenv("TMUX_TMPDIR", "/kept")
	// Wrong inherited values, not the ones shelld writes and not absent ones.
	// Seeding TERM=xterm-256color made this test agree with itself: the linux arm
	// could not tell "shelld set this" from "the child inherited it", and the
	// windows arm, where shelld deliberately sets nothing, asserted the same
	// literal and so passed whether the setting was guarded by goos or not.
	// tmux-256color and 24bit are values the code never writes, so each arm has
	// one outcome. An absent COLORTERM would not do either, since this box's own
	// is truecolor -- the same trap TestExactEnvironment's comment names.
	t.Setenv("TERM", "tmux-256color")
	t.Setenv("COLORTERM", "24bit")
	// A SHELL that is not executable, so loginShell has to reach its /etc/passwd
	// lookup: with an executable one it returns exactly what the child would have
	// inherited anyway, and deleting the SHELL line entirely changes nothing.
	t.Setenv("SHELL", "/nonexistent/sh")
	asMap := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			env := asMap(childEnvFor(goos, nil, nil, nil))
			for k := range outer {
				if v, ok := env[k]; ok && k != "TERM_PROGRAM" && k != "TERM_PROGRAM_VERSION" {
					t.Errorf("%s=%s inherited", k, v)
				}
			}
			if env["TERM_PROGRAM"] != "shelld" || env["TERM_PROGRAM_VERSION"] != Version || env["TMUX_TMPDIR"] != "/kept" {
				t.Errorf("TERM_PROGRAM=%q TERM_PROGRAM_VERSION=%q TMUX_TMPDIR=%q",
					env["TERM_PROGRAM"], env["TERM_PROGRAM_VERSION"], env["TMUX_TMPDIR"])
			}
			// The terminal the child is told about is decided per OS, and the
			// windows arm is the one nothing else asserts: console programs do
			// not read TERM, so shelld leaves the inherited value alone there.
			// These four checks are what distinguish the shipped code from the
			// same settings hoisted out of the goos guard.
			wantTerm, wantColor := "xterm-256color", "truecolor"
			if goos == "windows" {
				wantTerm, wantColor = "tmux-256color", "24bit"
			}
			if env["TERM"] != wantTerm || env["COLORTERM"] != wantColor {
				t.Errorf("%s: TERM=%q COLORTERM=%q, want %q and %q",
					goos, env["TERM"], env["COLORTERM"], wantTerm, wantColor)
			}
			// SHELL is set from the passwd entry on unix and left alone on
			// windows. The seeded value is not executable, so a shipped-code
			// SHELL cannot be it, and a deleted set() cannot be anything else.
			switch shell := env["SHELL"]; {
			case goos == "windows":
				if shell != "/nonexistent/sh" {
					t.Errorf("windows: SHELL=%q, want the inherited value untouched", shell)
				}
			case shell == "/nonexistent/sh":
				t.Errorf("%s: SHELL=%q was inherited; a non-executable $SHELL must fall back to the passwd entry", goos, shell)
			case !isExecutable(shell):
				t.Errorf("%s: SHELL=%q is not executable", goos, shell)
			}
			// The value and not only the presence, since a default set to the wrong
			// thing is present too.
			wantHist := "/dev/null"
			if goos == "windows" {
				wantHist = ""
			}
			if env["HISTFILE"] != wantHist {
				t.Errorf("%s: HISTFILE=%q, want %q", goos, env["HISTFILE"], wantHist)
			}
			env = asMap(childEnvFor(goos, nil, [][2]string{{"TMUX", "mine"}, {"TERM_PROGRAM", "other"}}, nil))
			if env["TMUX"] != "mine" || env["TERM_PROGRAM"] != "other" {
				t.Errorf("caller's env lost: TMUX=%q TERM_PROGRAM=%q", env["TMUX"], env["TERM_PROGRAM"])
			}
			// HISTFILE is a default like the rest, so Env overrides it rather than
			// being silently discarded. It is worth its own assertion because it is
			// the one default whose set() sat after the caller's overrides, which is
			// invisible to a presence check and to any test that only removes it:
			// UnsetEnv worked throughout, and Env did not.
			env = asMap(childEnvFor(goos, nil, [][2]string{{"HISTFILE", "/keep"}}, nil))
			if env["HISTFILE"] != "/keep" {
				t.Errorf("%s: Env asked for HISTFILE=/keep and got %q; it must win over the default, as it does for TERM", goos, env["HISTFILE"])
			}
			env = asMap(childEnvFor(goos, nil, nil, []string{"HISTFILE"}))
			if v, ok := env["HISTFILE"]; ok {
				t.Errorf("%s: UnsetEnv left HISTFILE=%q", goos, v)
			}
			exact := []string{"TMUX=x", "WT_SESSION=y"}
			if got := childEnvFor(goos, exact, nil, nil); strings.Join(got, " ") != "TMUX=x WT_SESSION=y" {
				t.Errorf("explicit Environ changed: %v", got)
			}
		})
	}
	// Windows names are case-insensitive.
	t.Setenv("wt_session", "lower")
	t.Setenv("Zellij_Session_Name", "mixed")
	env := asMap(childEnvFor("windows", nil, nil, nil))
	for _, k := range []string{"wt_session", "Zellij_Session_Name"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s inherited on windows", k)
		}
	}
}

// drainDirty waits until the window has been quiet for d, so a following Wait
// can only be ended by what the test does next.
func drainDirty(t *testing.T, w *Window, d time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		select {
		case <-w.Dirty().Wait():
		case <-time.After(d):
			return
		}
	}
	t.Fatal("the window never went quiet")
}

// A resize whose PTY resize fails must still wake attached clients: the grid
// has the new size, and no tick follows a frame (internal/daemon's idleWait),
// so an unwoken client would show the old geometry until the idle grid's next
// instant, up to a second later.
func TestResizeNotifiesWhenPTYResizeFails(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "resize-fail", Shell: "/bin/sh", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	drainDirty(t, w, 300*time.Millisecond)

	fail := func() error { return errors.New("pty resize refused") }
	testResizePTY.Store(&fail)
	defer testResizePTY.Store(nil)

	dirty := w.Dirty().Wait()
	if err := w.Resize(100, 30); err == nil {
		t.Fatal("a failed PTY resize must still be reported")
	}
	select {
	case <-dirty:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed PTY resize woke no client")
	}
	if c, r := w.Cols(), w.Rows(); c != 100 || r != 30 {
		t.Fatalf("window size %dx%d, want 100x30", c, r)
	}
	g := w.LockVT()
	cols, rows := g.Cols(), g.Lines()
	w.UnlockVT()
	if cols != 100 || rows != 30 {
		t.Fatalf("grid %dx%d, want 100x30", cols, rows)
	}
}

// Kill must wake attached clients too: the shell here has already exited, so
// the read loop's own notify is long past and only Kill's can arrive.
func TestKillNotifiesWaiters(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "kill-notify", Shell: "/bin/sh -c exit", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !w.IsAlive() }, "the shell did not exit")
	drainDirty(t, w, 300*time.Millisecond)

	dirty := w.Dirty().Wait()
	w.Kill()
	select {
	case <-dirty:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill woke no client")
	}
}
