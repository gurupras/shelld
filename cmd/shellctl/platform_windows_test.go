//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/gurupras/shelld/internal/daemon"
)

// catEnv makes the test binary a cat: there is no /bin/cat on Windows, and the
// window program has to write nothing unprompted (see bootDaemon). The console
// echoes what it reads in line mode, so a line typed shows twice, as on a unix
// terminal.
const catEnv = "SHELLCTL_TEST_CAT"

func init() {
	if os.Getenv(catEnv) == "1" {
		os.Exit(windowProgram(os.Args[1:]))
	}
	// The daemon's windows inherit its environment, which inherits this one.
	os.Setenv(catEnv, "1")
	if exe, err := os.Executable(); err == nil {
		catExe = exe
	}
}

var catExe = os.Args[0]

// catShell is the window program the cases type into: this test binary, run
// as a cat. Quoted, because a command line splits on spaces.
func catShell() string { return `"` + catExe + `"` }

// binName is the file name for a built binary: exec finds a program on Windows
// only by an extension it knows.
func binName(name string) string { return name + ".exe" }

// daemonSocket is the address a test daemon listens on: a pipe of its own,
// since a Windows daemon listens on a named pipe and not in dir.
func daemonSocket(string) string { return `\\.\pipe\shellctl-test-` + daemon.NewNonce() }

// missingSocket is an address nothing listens on.
func missingSocket(*testing.T) string { return `\\.\pipe\shellctl-none-` + daemon.NewNonce() }

// listenFake listens where the client can dial, and returns the address. The
// pipe is the daemon's own kind, owned by this user, because the client
// refuses a pipe another user owns.
func listenFake(t *testing.T) (net.Listener, string) {
	t.Helper()
	pipe := `\\.\pipe\shellctl-fake-` + daemon.NewNonce()
	ln, err := daemon.Listen(pipe, daemon.ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ln, pipe
}

// envShell is a window program that prints its working directory, $FOO, $BAR
// and $<homeVar> on one line and then runs as a cat.
func envShell() string { return catShell() + " env" }

// homeVar is a variable every daemon inherits, for --unset-env to remove.
// Windows has no HOME of its own; USERPROFILE is its counterpart.
const homeVar = "USERPROFILE"

// queryShell is a window program that waits for a line, asks the terminal for
// its background, foreground, cursor and palette 1 colours, and prints what
// comes back.
func queryShell() string { return catShell() + " query" }

// windowProgram is the test binary run as a window's program, with SHELLCTL_TEST_CAT
// set: a cat, after printing its environment for "env" or asking for the
// colours for "query".
func windowProgram(args []string) int {
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "env":
		wd, _ := os.Getwd()
		fmt.Printf("cwd=%s foo=%s bar=%s home=[%s]\n", wd, os.Getenv("FOO"), os.Getenv("BAR"), os.Getenv(homeVar))
	case "query":
		return queryColours()
	}
	if mode == "" {
		// See historyBase: the line is ended by the Enter awaitHistoryBase
		// sends, which the console echoes, and is not copied.
		fmt.Print("cat")
		br := bufio.NewReader(os.Stdin)
		if _, err := br.ReadString('\n'); err != nil {
			return 1
		}
		_, _ = io.Copy(os.Stdout, br)
		return 0
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	return 0
}

// queryColours is the "query" mode. The console echoes nothing once its input
// is raw, and passes the terminal's answers on only as virtual-terminal input,
// so what shows is this program's own copy of them, control characters in
// caret notation, as a unix terminal's echo spells them.
func queryColours() int {
	br := bufio.NewReader(os.Stdin)
	if _, err := br.ReadString('\n'); err != nil {
		return 1
	}
	in := windows.Handle(os.Stdin.Fd())
	if err := windows.SetConsoleMode(in, windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
		fmt.Printf("SetConsoleMode: %v\n", err)
		return 1
	}
	fmt.Print("\x1b]11;?\a\x1b]10;?\a\x1b]12;?\a\x1b]4;1;?\a")
	buf := make([]byte, 256)
	for {
		n, err := br.Read(buf)
		var out []byte
		for _, c := range buf[:n] {
			switch {
			case c == 0x7f:
				out = append(out, '^', '?')
			case c < 0x20:
				out = append(out, '^', c+'@')
			default:
				out = append(out, c)
			}
		}
		os.Stdout.Write(out)
		if err != nil {
			return 0
		}
	}
}

// argvCarriesBytes reports whether a command-line argument reaches the program
// byte for byte, invalid UTF-8 included. A Windows command line is UTF-16, so
// exec has already turned an invalid byte into U+FFFD before shellctl starts.
const argvCarriesBytes = false

// historyBase is how many lines of history a fresh catShell window holds
// before anything is typed: the first, where ConPTY's opening paint goes. The
// window's history is ConPTY's rendering, so two things keep the first typed
// line off that one. windowProgram writes text into it, because ConPTY draws
// a newline after an empty line as a cursor move, which leaves the next text on
// the same line of the history. And the line is ended only after the paint,
// whose frame ends with the title and a cursor show: ended inside that frame,
// the next line would start with those.
const historyBase = 1

// awaitHistoryBase waits for ConPTY's first frame and then ends the window's
// first line, so that what is typed next starts the line after it.
func awaitHistoryBase(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		js, errs, code := shellctl(t, "--socket", sock, "capture", "--json", "s", "w")
		var resp struct {
			Output string `json:"output"`
		}
		if code != 0 || json.Unmarshal([]byte(js), &resp) != nil {
			t.Fatalf("capture --json = %q, stderr %q (exit %d)", js, errs, code)
		}
		if _, after, ok := strings.Cut(resp.Output, "cat"); ok && strings.Contains(after, "\x1b[?25h") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ConPTY never finished its first frame: %q", js)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "-K", "s", "w", "Enter"); code != 0 {
		t.Fatalf("send-keys Enter exited %d: %s", code, errs)
	}
}

// tabOnScreen is how a typed tab shows on the visible screen, as a regexp:
// ConPTY draws it as the spaces to the next tab stop.
const tabOnScreen = `      `

// bracketedEcho is what a bracketed paste of "br" puts on the screen. ConPTY
// takes the brackets off the input, so the program reads the text alone.
const bracketedEcho = "\nbr\nbr\n"

// coloursReachTheProgram reports whether a program's colour query is answered
// with what set-colors set. Measured on Windows 11 build 26200: ConPTY answers
// OSC 10, 11 and 12 itself and passes on OSC 4 but not the answer to it.
const coloursReachTheProgram = false
