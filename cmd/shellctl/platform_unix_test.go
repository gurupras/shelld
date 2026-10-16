//go:build linux || darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// catShell is the window program the cases type into. It writes nothing
// unprompted, and the terminal echoes what it is sent, so a line typed shows
// twice: once echoed, once written back.
func catShell() string { return "/bin/cat" }

// binName is the file name for a built binary.
func binName(name string) string { return name }

// daemonSocket is the address a test daemon listens on, in its private dir.
func daemonSocket(dir string) string { return filepath.Join(dir, "s.sock") }

// missingSocket is an address nothing listens on.
func missingSocket(t *testing.T) string { return t.TempDir() + "/none.sock" }

// listenFake listens where the client can dial, and returns the address.
func listenFake(t *testing.T) (net.Listener, string) {
	t.Helper()
	// A short directory keeps the socket path inside sun_path's 108 bytes.
	dir, err := os.MkdirTemp("", "sw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "f.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return ln, path
}

// envShell is a window program that prints its working directory, $FOO, $BAR
// and $<homeVar> on one line and then runs as a cat.
func envShell() string {
	return `sh -c 'echo "cwd=$PWD foo=$FOO bar=$BAR home=[$` + homeVar + `]"; exec cat'`
}

// homeVar is a variable every daemon inherits, for --unset-env to remove.
const homeVar = "HOME"

// queryShell is a window program that waits for a line, asks the terminal for
// its background, foreground, cursor and palette 1 colours, and then runs as a
// cat, so the terminal's echo shows the answers.
func queryShell() string {
	return `sh -c 'read x; printf "\033]11;?\007\033]10;?\007\033]12;?\007\033]4;1;?\007"; exec cat'`
}

// argvCarriesBytes reports whether a command-line argument reaches the program
// byte for byte, invalid UTF-8 included.
const argvCarriesBytes = true

// historyBase is how many lines of history a fresh catShell window holds
// before anything is typed.
const historyBase = 0

// awaitHistoryBase waits until the window's first historyBase lines are
// written. There are none to wait for.
func awaitHistoryBase(*testing.T, string) {}

// tabOnScreen is how a typed tab shows on the visible screen, as a regexp.
const tabOnScreen = `\t`

// bracketedEcho is what a bracketed paste of "br" puts on the screen: the
// terminal echoes the brackets as typed, and cat writes back the text.
const bracketedEcho = "^[[200~br^[[201~\nbr\n"

// coloursReachTheProgram reports whether a program's colour query is answered
// with what set-colors set.
const coloursReachTheProgram = true
