//go:build windows

package pty

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestHelperProcess is not a test: the tests below run the test binary as a
// child process with PTY_TEST_HELPER set.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("PTY_TEST_HELPER") {
	case "":
		return
	case "spawn-detached":
		// Start a grandchild outside the console, so closing the pseudo
		// console alone would not end it, and report its pid.
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), "PTY_TEST_HELPER=sleep")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("PTY_TEST_PIDFILE"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		time.Sleep(5 * time.Minute)
	case "sleep":
		time.Sleep(5 * time.Minute)
	case "argv":
		// Record the raw command line; the test splits it as a UCRT program
		// (node, behind npm's shim) would. os.Args can't be used: Go splits
		// the command line with the pre-2008 rule, which differs for "".
		_ = os.WriteFile(os.Getenv("PTY_TEST_OUT"), []byte(windows.UTF16PtrToString(windows.GetCommandLine())), 0o644)
	case "read-vt":
		// Read the console as a program asking for VT input does, as a TUI
		// reading a query's answer would, and record it once it holds an ST.
		if err := windows.SetConsoleMode(windows.Handle(os.Stdin.Fd()), windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("ready\r\n")
		var got []byte
		b := make([]byte, 256)
		for !bytes.Contains(got, []byte("\x1b\\")) {
			n, err := os.Stdin.Read(b)
			if err != nil {
				os.Exit(3)
			}
			got = append(got, b[:n]...)
		}
		_ = os.WriteFile(os.Getenv("PTY_TEST_OUT"), got, 0o644)
	}
	os.Exit(0)
}

// output collects everything read from p until Read fails.
type output struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func readAll(p *PTY) *output {
	o := &output{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		b := make([]byte, 4096)
		for {
			n, err := p.Read(b)
			o.mu.Lock()
			o.buf.Write(b[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// waitExit runs p.Wait with a timeout; on timeout it closes p and fails.
func waitExit(t *testing.T, p *PTY, d time.Duration) ExitStatus {
	t.Helper()
	type result struct {
		st  ExitStatus
		err error
	}
	ch := make(chan result, 1)
	go func() {
		st, err := p.Wait()
		ch <- result{st, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		return r.st
	case <-time.After(d):
		p.Close()
		t.Fatalf("Wait did not return within %v", d)
	}
	return ExitStatus{}
}

func startCmd(t *testing.T, args ...string) *PTY {
	t.Helper()
	p, err := Start(Command{Path: args[0], Args: args, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A process that exits by itself must end the output stream, with its last
// output delivered: conhost keeps the pipe open until the console closes.
func TestSelfExitClosesOutput(t *testing.T) {
	p := startCmd(t, "cmd.exe", "/c", "echo last& exit 3")
	defer p.Close()
	out := readAll(p)
	if st := waitExit(t, p, 10*time.Second); st.Code != 3 {
		t.Fatalf("exit status %+v, want code 3", st)
	}
	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
		p.Close()
		t.Fatalf("output did not end within 10 s after exit; got %q", out.String())
	}
	if !strings.Contains(out.String(), "last") {
		t.Fatalf("last output lost: %q", out.String())
	}
}

// Resize after the console closed must not touch the freed HPCON. A pass is
// weak evidence: using a freed HPCON does not reliably crash, so the
// consoleClosed flag under mu is the real protection.
func TestResizeAfterSelfExit(t *testing.T) {
	p := startCmd(t, "cmd.exe", "/c", "exit 0")
	defer p.Close()
	out := readAll(p)
	waitExit(t, p, 10*time.Second)
	deadline := time.Now().Add(time.Second)
	for i := uint16(0); time.Now().Before(deadline); i++ {
		_ = p.Resize(Size{Cols: 40 + i%40, Rows: 10 + i%10})
	}
	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
		t.Fatal("output did not end")
	}
}

// Kill must end descendants that left the console, and Kill then Close must
// return promptly on an idle program.
func TestKillTerminatesDetachedGrandchild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	env := append(os.Environ(), "PTY_TEST_HELPER=spawn-detached", "PTY_TEST_PIDFILE="+pidfile)
	p, err := Start(Command{Path: os.Args[0], Args: []string{os.Args[0], "-test.run=^TestHelperProcess$"}, Env: env}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	readAll(p)

	var pid int
	for deadline := time.Now().Add(20 * time.Second); pid == 0; {
		if b, err := os.ReadFile(pidfile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if pid == 0 && time.Now().After(deadline) {
			t.Fatal("helper never reported its grandchild")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Open the grandchild before killing, so a reused pid cannot stand in.
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess grandchild: %v", err)
	}
	defer windows.CloseHandle(h)

	start := time.Now()
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitExit(t, p, 5*time.Second)
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s after Kill")
	}
	t.Logf("Kill, Wait and Close took %v", time.Since(start))
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatal("detached grandchild still running 5 s after Kill")
	}
}

// Kill, Wait and Close on an idle interactive program return promptly.
func TestKillIdleThenClose(t *testing.T) {
	p := startCmd(t, "cmd.exe")
	readAll(p)
	time.Sleep(500 * time.Millisecond)
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, p, 5*time.Second)
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s")
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill after Close: %v", err)
	}
	if err := p.Resize(Size{Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("Resize after Close: %v", err)
	}
}

// envWith returns this process's environment with PATH replaced.
func envWith(path string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			env = append(env, kv)
		}
	}
	return append(append(env, "PATH="+path), extra...)
}

// A bare name finds npm's .cmd shim, not the extensionless sh script beside
// it, which CreateProcess cannot start.
func TestResolvesNpmShim(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec node cli.js \"$@\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude.cmd"), []byte("@echo SHIM %*\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sys, _ := windows.GetSystemDirectory()
	p, err := Start(Command{Path: "claude", Args: []string{"claude", "hi"}, Env: envWith(dir + ";" + sys)}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	out := readAll(p)
	waitExit(t, p, 10*time.Second)
	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
	}
	if !strings.Contains(out.String(), "SHIM hi") {
		t.Fatalf("output %q, want SHIM hi", out.String())
	}
}

// Arguments passed to a batch file reach the program it forwards %* to
// unchanged: cmd.exe neither runs an operator in them nor expands a variable.
func TestBatchArgumentsReachProgramUnchanged(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "cmdline.txt")
	script := filepath.Join(dir, "fwd.cmd")
	if err := os.WriteFile(script, []byte("@\"%PTY_TEST_BIN%\" -test.run=TestHelperProcess -- %*\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"a&echo.PWNED", `x"&echo.PWNED&"`, "%PATH%", `x"y`, "", `C:\dir\`, "日本語 prompt", "!x!", "a|b", "(c)"}
	sys, _ := windows.GetSystemDirectory()
	env := envWith(sys, "PTY_TEST_HELPER=argv", "PTY_TEST_OUT="+outFile, "PTY_TEST_BIN="+os.Args[0])
	p, err := Start(Command{Path: script, Args: append([]string{script}, args...), Env: env}, Size{Cols: 200, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	out := readAll(p)
	waitExit(t, p, 20*time.Second)
	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "PWNED") && !strings.Contains(line, "echo.PWNED") {
			t.Errorf("an argument ran as a command: %q", line)
		}
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("helper wrote no command line (output %q): %v", out.String(), err)
	}
	var got []string
	all := ucrtArgs(string(b))
	for i, a := range all {
		if a == "--" {
			got = all[i+1:]
			break
		}
	}
	if len(got) != len(args) {
		t.Fatalf("argv %q, want %q", got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d: got %q, want %q", i, got[i], args[i])
		}
	}
}

// A reply to the program's query must reach it as it was sent. Written as
// input is, ConPTY's input parser drops it; WriteReply encodes it as key
// events. The reply is an OSC colour answer, the kind a TUI asks for to pick a
// light or dark theme.
func TestWriteReplyReachesAProgramReadingVT(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "got")
	p, err := Start(Command{
		Path: os.Args[0],
		Args: []string{os.Args[0], "-test.run=^TestHelperProcess$"},
		Env:  append(os.Environ(), "PTY_TEST_HELPER=read-vt", "PTY_TEST_OUT="+outFile),
	}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out := readAll(p)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "ready") {
		if time.Now().After(deadline) {
			t.Fatalf("the helper never set its console mode: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	const reply = "\x1b]11;rgb:1111/2222/3333\x1b\\"
	if n, err := p.WriteReply([]byte(reply)); err != nil || n != len(reply) {
		t.Fatalf("WriteReply = %d, %v; want %d, nil", n, err, len(reply))
	}
	if st := waitExit(t, p, 10*time.Second); st.Code != 0 {
		t.Fatalf("the helper exited with %+v before recording its input", st)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != reply {
		t.Fatalf("the program read %q, want %q", got, reply)
	}
}
