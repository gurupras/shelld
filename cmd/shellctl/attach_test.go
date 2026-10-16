package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/daemon"
)

// The binaries under test, built once. Both are needed: the bug these cases
// exist for is in what the client does with what the daemon sends, so a fake
// server on the other end could only assert the client's behaviour against a
// guess about the protocol. The daemon is the thing that decides which opcode a
// status frame goes out on.
var (
	shellctlBin string
	shelldBin   string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shellctl-attach")
	if err != nil {
		panic(err)
	}
	shellctlBin = filepath.Join(dir, binName("shellctl"))
	shelldBin = filepath.Join(dir, binName("shelld"))
	for _, b := range []struct{ out, pkg, env string }{
		{shellctlBin, ".", "SHELLCTL_TEST_BIN"},
		{shelldBin, "../shelld", "SHELLD_TEST_BIN"},
	} {
		if err := buildOrCopy(b.out, b.pkg, b.env); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildOrCopy puts the binary for pkg at out, building it unless env names one to
// use instead.
//
// The override is there so these cases can be run against a different build to show
// they fail on it, which is the only check that tells a case pinned to the fix from
// one pinned to the tree it was written in. cmd/shelld's TestMain has the same seam
// under SHELLD_TEST_BIN, and the daemon keeps that name here. The client needs its
// own, and is the one that matters for these cases: the defect is in what the client
// does with what it is sent, so a pre-fix daemon proves nothing and a pre-fix client
// is the whole experiment.
func buildOrCopy(out, pkg, env string) error {
	if prebuilt := os.Getenv(env); prebuilt != "" {
		data, err := os.ReadFile(prebuilt)
		if err != nil {
			return fmt.Errorf("read %s=%s: %w", env, prebuilt, err)
		}
		if err := os.WriteFile(out, data, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		return nil
	}
	if msg, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
		return fmt.Errorf("build %s: %w\n%s", pkg, err, msg)
	}
	return nil
}

// bootDaemon starts a daemon on a private socket with one session and one
// window, and returns the socket path.
//
// The window runs cat rather than a shell. A shell draws a prompt, so its output
// would be mixed into the terminal bytes these cases read, and a case that has to
// say "stdout holds nothing but what the window drew" cannot say it while the
// window is drawing something of its own. cat writes nothing unprompted.
func bootDaemon(t *testing.T) string {
	t.Helper()
	// A short directory keeps a socket path inside sun_path's 108 bytes.
	dir, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := daemonSocket(dir)
	pid := filepath.Join(dir, "s.pid")
	c := exec.Command(shelldBin, "run", "--socket", sock, "--pidfile", pid, "--logfile", filepath.Join(dir, "s.log"))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for !daemon.IsAlive(sock, 200*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	post(t, sock, "/sessions", `{"name":"s"}`)
	post(t, sock, "/sessions/s/windows", `{"name":"w","cols":80,"rows":24,"shell":`+strconv.Quote(catShell())+`}`)
	awaitHistoryBase(t, sock)
	return sock
}

// post sends a JSON POST over the daemon's socket. HTTPRoundTrip cannot be
// used here because it sends no body, and creating a session and a window is the
// whole reason these cases speak HTTP at all.
func post(t *testing.T, sock, path, body string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return daemon.Dial(ctx, sock, 5*time.Second)
		},
	}}
	resp, err := client.Post("http://shelld"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	// Drained before closing so the connection can be reused for the next POST
	// rather than being torn down with a body still in flight.
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, raw)
	}
}

// runAttach runs `shellctl attach s w` with stdin held open and nothing written
// to it, and returns what the client put on stdout.
//
// Stdin is a pipe that stays open for the whole window rather than /dev/null,
// because the client's stdin reader ending is what detaches it: with /dev/null it
// would exit on the first read and might do so before the daemon's frames
// arrived, which would make an empty stdout mean nothing.
func runAttach(t *testing.T, sock string, d time.Duration) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := exec.CommandContext(ctx, shellctlBin, "--socket", sock, "attach", "s", "w")
	c.Stdin = pr
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	// The client only stops when its stdin closes or it is killed, so the window
	// is the whole of the observation: wait it out, then end the client.
	time.Sleep(d)
	_ = c.Process.Kill()
	// c.Wait, not c.Process.Wait. Because Stdout and Stderr are buffers rather than
	// files, exec.Cmd handed the child a pipe and copies from it on a goroutine of its
	// own, and c.Wait is the only thing that joins that goroutine; reaping the process
	// directly returns with the copier still free to append. Reading out below was then
	// an unsynchronized read of a buffer another goroutine writes, which -race fails
	// deterministically: 12 warnings and all three cases red, on every run.
	//
	// It is the race that is the defect, not lost output. Losing output is the obvious
	// way for this to matter and it does not happen -- Process.Wait blocks in wait4 for
	// long enough that the copier drains an already-closed pipe, measured at 0 short
	// reads in 1200 trials over payloads from 4 B to 1 MiB. So do not read a missing
	// escape sequence as this coming back.
	_ = c.Wait()
	if s := errb.String(); s != "" && !strings.Contains(s, "Detached") {
		t.Logf("shellctl stderr: %s", s)
	}
	return out.String()
}

// A control frame must not be written to the terminal.
//
// The daemon sends the status frame as OpText -- 0x01 and then JSON -- and the
// client used to discard the opcode and write every message to stdout, so
// {"status":{"offset":0,...}} was printed into the user's session on every
// attach, twice. Asserting on the JSON key rather than on the frame byte-for-byte
// keeps the case pointed at "control JSON reached the terminal" instead of at one
// spelling of the status frame, which is a payload this test has no reason to pin.
func TestAttachDoesNotPrintControlFramesToStdout(t *testing.T) {
	sock := bootDaemon(t)
	out := runAttach(t, sock, 2*time.Second)
	if out == "" {
		t.Fatal("stdout is empty, so nothing was read from the daemon and this case asserted nothing")
	}
	for _, frag := range []string{`"status"`, `"offset"`, `"paused"`, `{"`} {
		if strings.Contains(out, frag) {
			t.Errorf("stdout contains %s, so a control frame was written to the terminal: %q", frag, out)
		}
	}
	// 0x01 is the control framing byte. The window runs cat and is sent nothing,
	// so no terminal byte the daemon draws can be one, and its presence means a
	// control frame arrived whole or in part.
	if i := strings.IndexByte(out, 0x01); i >= 0 {
		t.Errorf("stdout holds the 0x01 control frame marker at offset %d: %q", i, out)
	}
}

// What the window draws still reaches the terminal.
//
// The fix drops frames by opcode, and dropping too much would be just as wrong
// and much harder to notice: a client that printed nothing would pass the case
// above. The daemon paints the window on attach, so a cleared screen and a cursor
// move are what a working attach puts on stdout.
func TestAttachWritesTerminalOutputToStdout(t *testing.T) {
	sock := bootDaemon(t)
	out := runAttach(t, sock, 2*time.Second)
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("stdout carries no escape sequence, so the window was never painted: %q", out)
	}
	// The initial paint clears the screen and homes the cursor. Both, because
	// either alone could come from something other than a frame the window drew.
	for _, want := range []string{"\x1b[2J", "\x1b[H"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout is missing %q from the initial paint: %q", want, out)
		}
	}
}

// Terminal output keeps reaching the terminal after the first frame.
//
// The two cases above are both satisfied by the handshake paint alone, so a client
// that wrote its first frame and dropped every one after it would pass both --
// `\x1b[2J` and `\x1b[H` arrive together in that paint, and neither case looks past
// it. Mutation-checked: `if wroteOnce { continue }` in the read loop leaves both
// green. What that mutant cannot produce is a byte the window draws in response to
// something typed, because that cannot be in the frame the daemon had already sent
// when the client connected.
//
// So this types a marker and requires it back. cat echoes it and the tty echoes it,
// either of which is a frame after the handshake, and the marker is chosen to hold
// no escape byte so that finding it cannot be the paint being found again.
func TestAttachKeepsWritingTerminalOutputAfterTheFirstFrame(t *testing.T) {
	const marker = "zqxj"
	sock := bootDaemon(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shellctlBin, "--socket", sock, "attach", "s", "w")
	c.Stdin = pr
	var errb bytes.Buffer
	c.Stdout, c.Stderr = outW, &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	outW.Close()

	// Read stdout on its own goroutine, because this test has to look at the output
	// while the client is still producing it: the witness it waits for is a byte the
	// client has not written yet. A pipe rather than a bytes.Buffer handed to
	// exec.Cmd, since reading that buffer while the child writes it is a race.
	var mu sync.Mutex
	var got []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	defer func() {
		_ = c.Process.Kill()
		// Stdout is a pipe this test drains itself, so the rule above about not reading
		// a buffer the child is writing was already applied there -- but Stderr is
		// still a buffer, so exec.Cmd copies into it on a goroutine, and only c.Wait
		// joins that one. See runAttach.
		_ = c.Wait()
		outR.Close()
		<-done
		if s := errb.String(); s != "" && !strings.Contains(s, "Detached") {
			t.Logf("shellctl stderr: %s", s)
		}
	}()

	// Wait for the handshake paint before typing, rather than sleeping a guess at
	// it. Typing before the attach completes would be written into a window that is
	// not yet forwarding, and the marker never coming back would then say nothing
	// about the read loop.
	if !waitFor(t, seen, "\x1b[2J", 15*time.Second) {
		t.Fatalf("the window was never painted, so nothing here is about what comes after the first frame: %q", seen())
	}
	if _, err := pw.Write([]byte(marker + "\r")); err != nil {
		t.Fatalf("write to the client's stdin: %v", err)
	}

	if !waitFor(t, seen, marker, 15*time.Second) {
		t.Errorf("%q was typed into the window and never came back on stdout, so the client stopped writing terminal output after its first frame: %q", marker, seen())
	}
}

// waitFor polls until want appears in what has been read so far, and reports
// whether it did. Polling and not a fixed sleep: the thing being waited for is a
// byte that travels a pty, a websocket and a pipe, and a sleep long enough to be
// safe on a loaded box is long enough to make the case slow on an idle one.
func waitFor(t *testing.T, seen func() string, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if strings.Contains(seen(), want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
