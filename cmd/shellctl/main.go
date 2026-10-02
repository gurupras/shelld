// Command shellctl attaches a terminal to a shelld window.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/gurupras/shelld/internal/daemon"
	"github.com/gurupras/shelld/internal/dotenv"
	"github.com/gurupras/shelld/ws"
)

const version = "0.2.0"

// detachKey is Ctrl-].
const detachKey = 0x1d

const usage = `Client for shelld

Usage: shellctl [OPTIONS] <COMMAND>

Commands:
  attach  Attach to a window: stdin/stdout piped over WebSocket

Options:
      --socket <SOCKET>  Path to the Unix socket / named pipe (overrides SHELLD_SOCKET)
  -h, --help             Print help
  -V, --version          Print version
`

func usageError(msg string) {
	fmt.Fprintf(os.Stderr, "error: %s\n\nUsage: shellctl [OPTIONS] <COMMAND>\n\nFor more information, try '--help'.\n", msg)
	os.Exit(2)
}

func main() {
	_ = dotenv.Load()
	socket := ""
	var positional []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch name {
		case "-h", "--help":
			fmt.Print(usage)
			return
		case "-V", "--version":
			fmt.Printf("shellctl %s\n", version)
			return
		case "--socket":
			if !hasVal {
				if i+1 >= len(args) {
					usageError("a value is required for '--socket <SOCKET>' but none was supplied")
				}
				i++
				val = args[i]
			}
			socket = val
		default:
			if strings.HasPrefix(args[i], "-") {
				usageError(fmt.Sprintf("unexpected argument '%s' found", args[i]))
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if positional[0] != "attach" {
		usageError(fmt.Sprintf("unrecognized subcommand '%s'", positional[0]))
	}
	if len(positional) != 3 {
		fmt.Fprint(os.Stderr, "error: the following required arguments were not provided:\n  <SESSION>\n  <WINDOW>\n\nUsage: shellctl attach <SESSION> <WINDOW>\n\nFor more information, try '--help'.\n")
		os.Exit(2)
	}
	if socket == "" {
		socket = daemon.DefaultSocket()
	}
	// Anchored to the cwd as shelld anchors it, so a relative path into the
	// default directory gets that directory's check below.
	if runtime.GOOS != "windows" {
		if abs, err := filepath.Abs(socket); err == nil {
			socket = abs
		}
	}
	if dir := daemon.DefaultDir(); filepath.Dir(socket) == dir {
		// Refuse a default directory another user could have created.
		if err := daemon.CheckPrivateDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
	if err := attach(socket, positional[1], positional[2]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// forwardInput sends what r reads until detachKey, EOF or an error. The bytes
// read ahead of detachKey are sent first, so detaching loses only the key: a
// paste, or typing faster than one read, can put both in the same read.
func forwardInput(r io.Reader, send func([]byte) error) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			i := bytes.IndexByte(chunk, detachKey)
			if i >= 0 {
				chunk = chunk[:i]
			}
			if len(chunk) > 0 && send(chunk) != nil {
				return
			}
			if i >= 0 {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func resizeFrame(cols, rows int) []byte {
	b, _ := json.Marshal(map[string]any{"resize": map[string]int{"cols": cols, "rows": rows}})
	return append([]byte{0x01}, b...)
}

func termSize() (int, int) {
	if c, r, err := term.GetSize(int(os.Stdout.Fd())); err == nil && c > 0 {
		return c, r
	}
	if c, r, err := term.GetSize(int(os.Stdin.Fd())); err == nil && c > 0 {
		return c, r
	}
	// The platform's own idea of the terminal, for the case where neither
	// descriptor is one but the process is still attached to a console. Only
	// Windows has such a thing; see consoleSize in console_windows.go.
	if c, r, ok := consoleSize(); ok {
		return c, r
	}
	return 80, 24
}

func attach(socket, session, window string) error {
	nc, err := daemon.Dial(context.Background(), socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", socket, err)
	}
	// Both names escaped: neither is a path segment until it is made one. shelld
	// restricts neither, so a slash in either one asks a different route, and a space
	// or a CR reaches ws.Client, which writes the path into a request line it
	// assembles by hand -- the one place where a name can stop being data. The
	// daemon's router reads EscapedPath and unescapes each segment after splitting,
	// so the escape here is what the other end is already expecting.
	path := "/sessions/" + url.PathEscape(session) + "/windows/" + url.PathEscape(window) + "/attach"
	conn, err := ws.Client(nc, "shelld", path)
	if err != nil {
		nc.Close()
		return fmt.Errorf("websocket handshake: %w", err)
	}
	defer conn.Close()

	defer enableVTOutput()()

	// Raw mode only when stdin is a terminal; piped stdin passes through.
	fd := int(os.Stdin.Fd())
	tty := term.IsTerminal(fd)
	// Registered before raw mode is entered, so that no stop signal between the
	// two can end the process with the terminal left raw.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, stopSignals(tty)...)
	defer signal.Stop(stop)
	if tty {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("enter raw mode: %w", err)
		}
		defer term.Restore(fd, state)
	}

	cols, rows := termSize()
	_ = conn.WriteMessage(ws.OpText, resizeFrame(cols, rows))

	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }

	stopResize := watchResize(func() {
		c, r := termSize()
		if conn.WriteMessage(ws.OpText, resizeFrame(c, r)) != nil {
			finish()
		}
	})
	defer stopResize()

	go func() {
		defer finish()
		forwardInput(os.Stdin, func(b []byte) error { return conn.WriteMessage(ws.OpBinary, b) })
	}()

	go func() {
		defer finish()
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// Only what the window drew reaches the terminal. The attach protocol
			// splits the two directions of traffic by opcode: OpBinary carries
			// terminal bytes, OpText carries control messages framed as 0x01 and
			// then JSON — the same framing this client sends a resize in. Writing
			// an OpText frame to stdout puts that JSON on the user's screen, and
			// the daemon sends a status frame on every attach and again whenever
			// the offset, pause, alt-screen, effective PTY size or dropped-byte
			// count changes.
			//
			// Tested for OpBinary rather than against OpText so that an opcode
			// added later is dropped rather than printed. Nothing here needs the
			// status frame: this client has no scrollback UI to move, so the
			// offsets describe nothing it draws.
			if op != ws.OpBinary {
				continue
			}
			if _, err := os.Stdout.Write(data); err != nil {
				return
			}
		}
	}()

	go func() {
		select {
		case <-stop:
			finish()
		case <-done:
		}
	}()

	<-done
	_ = conn.SendClose()
	fmt.Fprint(os.Stderr, "\r\nDetached.\r\n")
	return nil
}
