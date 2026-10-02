//go:build linux || darwin

package shelld

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// terminalQueries are the queries TestHelperQueries sends, in order.
var terminalQueries = []struct{ name, query string }{
	{"da1", "\x1b[c"},
	{"da2", "\x1b[>c"},
	{"dsr", "\x1b[5n"},
	{"cpr", "\x1b[6n"},
	{"decrqm-2026", "\x1b[?2026$p"},
	{"size", "\x1b[18t"},
}

// TestHelperQueries is not a test: run as a window's program in raw mode
// with SHELLD_TEST_HELPER=queries, it moves the cursor to row 5 col 10, sends
// each terminal query and waits up to 1 s for the reply, then prints
// "<name>=<reply hex>" per query and "queries-done".
func TestHelperQueries(t *testing.T) {
	if os.Getenv("SHELLD_TEST_HELPER") != "queries" {
		return
	}
	in := make(chan []byte, 16)
	go func() {
		for {
			buf := make([]byte, 256)
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				in <- buf[:n]
			}
			if err != nil {
				close(in)
				return
			}
		}
	}()
	os.Stdout.WriteString("\x1b[5;10H")
	var lines []string
	for _, q := range terminalQueries {
		os.Stdout.WriteString(q.query)
		var got []byte
		select {
		case b := <-in:
			got = b
			// A reply can arrive in pieces; collect what follows at once.
			for more := true; more; {
				select {
				case b := <-in:
					got = append(got, b...)
				case <-time.After(20 * time.Millisecond):
					more = false
				}
			}
		case <-time.After(time.Second):
		}
		lines = append(lines, fmt.Sprintf("%s=%s", q.name, hex.EncodeToString(got)))
	}
	os.Stdout.WriteString("\x1b[H\x1b[2J" + strings.Join(lines, "\r\n") + "\r\nqueries-done\r\n")
	time.Sleep(time.Minute)
}

// The VT's replies to device attribute, status, cursor position, mode
// (DECRQM) and text area size queries reach the program, as tmux and xterm
// answer them.
func TestTerminalQueriesAnswered(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWindow(WindowOpts{
		Name:    "queries",
		Command: []string{"/bin/sh", "-c", `stty raw -echo && exec "$0" -test.run='^TestHelperQueries$'`, exe},
		Env:     [][2]string{{"SHELLD_TEST_HELPER", "queries"}},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	var screen string
	waitFor(t, 10*time.Second, func() bool {
		screen = w.CaptureVisible(0)
		return strings.Contains(screen, "queries-done")
	}, "helper never finished")
	want := map[string]string{
		"da1": "\x1b[?6c",
		"da2": "\x1b[>0;2600;1c",
		"dsr": "\x1b[0n",
		"cpr": "\x1b[5;10R",
		// Synchronized output (2026) is reported as supported and reset.
		"decrqm-2026": "\x1b[?2026;2$y",
		"size":        "\x1b[8;24;80t",
	}
	replies := map[string]string{}
	for _, line := range strings.Split(screen, "\n") {
		if name, h, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			replies[name] = h
		}
	}
	for _, q := range terminalQueries {
		h, ok := replies[q.name]
		if !ok {
			t.Fatalf("no %s line in %q", q.name, screen)
		}
		got, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s line %q: %v", q.name, h, err)
		}
		if string(got) != want[q.name] {
			t.Errorf("%s: program read %q, want %q", q.name, got, want[q.name])
		}
	}
}
