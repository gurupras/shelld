//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// shellctl runs the built client with args and returns stdout, stderr and the
// exit code.
func shellctl(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shellctlBin, args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("shellctl %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// waitCapture polls `shellctl capture` with opts until its output contains want.
func waitCapture(t *testing.T, sock, want string, opts ...string) string {
	t.Helper()
	args := append([]string{"--socket", sock, "capture"}, opts...)
	args = append(args, "s", "w")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, errs, code := shellctl(t, args...)
		if code != 0 {
			t.Fatalf("capture exited %d: %s", code, errs)
		}
		if strings.Contains(out, want) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never showed %q; last output %q", want, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHealthPrintsTheDaemonsReport(t *testing.T) {
	sock := bootDaemon(t)
	out, errs, code := shellctl(t, "--socket", sock, "health")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var h map[string]any
	if err := json.Unmarshal([]byte(out), &h); err != nil || h["status"] != "ok" {
		t.Fatalf("health printed %q (%v), want the JSON report with status ok", out, err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("health output %q does not end its line", out)
	}
}

// send-keys --no-enter types without pressing Enter: cat's tty echoes the text
// but, with no newline, cat itself never repeats it. With Enter, the line is
// read and cat writes it back, so it appears twice.
func TestSendKeysTypesTextAndEnterOnlyWhenAsked(t *testing.T) {
	sock := bootDaemon(t)
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "--no-enter", "s", "w", "abc"); code != 0 {
		t.Fatalf("send-keys exited %d: %s", code, errs)
	}
	out := waitCapture(t, sock, "abc", "--visible")
	if strings.Count(out, "abc") != 1 {
		t.Fatalf("visible screen %q: want the text once, typed and not entered", out)
	}
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "s", "w", "def"); code != 0 {
		t.Fatalf("send-keys exited %d: %s", code, errs)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := waitCapture(t, sock, "abcdef", "--visible")
		if strings.Count(out, "abcdef") == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("visible screen %q: want the entered line echoed back by cat", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A KEYS beginning with '-' is reached through --, as with any option parser.
func TestSendKeysTakesADashLeadingTextAfterDoubleDash(t *testing.T) {
	sock := bootDaemon(t)
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "--no-enter", "s", "w", "--", "-x-"); code != 0 {
		t.Fatalf("send-keys exited %d: %s", code, errs)
	}
	waitCapture(t, sock, "-x-", "--visible")
}

// Raw capture prints the history as text; --json prints the route's response;
// scroll pages it from the oldest line. Each prints what the route returned,
// checked against the route's own fields.
func TestCaptureAndScrollPrintTheHistory(t *testing.T) {
	sock := bootDaemon(t)
	// One line at a time: the tty's echo of a line and cat's copy of the one
	// before it would otherwise interleave in the history.
	for _, l := range []string{"one", "two", "three"} {
		if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "s", "w", l); code != 0 {
			t.Fatalf("send-keys exited %d: %s", code, errs)
		}
		waitCapture(t, sock, l+"\n"+l)
	}
	out, _, code := shellctl(t, "--socket", sock, "capture", "--lines", "2", "s", "w")
	if code != 0 || out != "three\nthree\n" {
		t.Fatalf("capture --lines 2 = %q (exit %d), want the newest two lines", out, code)
	}
	js, _, code := shellctl(t, "--socket", sock, "capture", "--json", "--lines=2", "s", "w")
	var resp struct {
		Output        string `json:"output"`
		LinesReturned int    `json:"linesReturned"`
	}
	if code != 0 || json.Unmarshal([]byte(js), &resp) != nil || resp.Output != "three\nthree" || resp.LinesReturned != 2 {
		t.Fatalf("capture --json = %q (exit %d)", js, code)
	}
	out, _, code = shellctl(t, "--socket", sock, "scroll", "--offset", "0", "--lines", "2", "s", "w")
	if code != 0 || out != "one\none\n" {
		t.Fatalf("scroll --offset 0 --lines 2 = %q (exit %d), want the oldest two lines", out, code)
	}
	out, _, code = shellctl(t, "--socket", sock, "scroll", "--offset", "2", "--lines", "1", "s", "w")
	if code != 0 || out != "two\n" {
		t.Fatalf("scroll --offset 2 --lines 1 = %q (exit %d)", out, code)
	}
	if _, errs, _ := shellctl(t, "--socket", sock, "scroll", "--offset", "0", "--lines", "2", "s", "w"); errs != "" {
		t.Fatalf("an untruncated scroll wrote stderr %q", errs)
	}
	out, errs, code := shellctl(t, "--socket", sock, "scroll", "--offset", "0", "--lines", "4", "--max-bytes", "5", "s", "w")
	if code != 0 || !strings.Contains(errs, "truncated") || strings.Count(out, "\n") >= 4 {
		t.Fatalf("scroll --max-bytes 5 = %q, stderr %q (exit %d); want a short read named on stderr", out, errs, code)
	}
}

// The visible screen is a screen: its line count is the rows asked for, and
// capture --visible prints the same text as the route's output field.
func TestCaptureVisibleMatchesTheRoute(t *testing.T) {
	sock := bootDaemon(t)
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "s", "w", "vis"); code != 0 {
		t.Fatalf("send-keys exited %d: %s", code, errs)
	}
	waitCapture(t, sock, "vis\nvis", "--visible")
	text, _, _ := shellctl(t, "--socket", sock, "capture", "--visible", "--lines", "24", "s", "w")
	js, _, _ := shellctl(t, "--socket", sock, "capture", "--visible", "--lines", "24", "--json", "s", "w")
	var resp struct {
		Output string `json:"output"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(js), &resp); err != nil || resp.Mode != "visible" {
		t.Fatalf("capture --visible --json = %q (%v)", js, err)
	}
	want := resp.Output
	if !strings.HasSuffix(want, "\n") {
		want += "\n"
	}
	if text != want {
		t.Fatalf("capture --visible printed %q, the route's output is %q", text, resp.Output)
	}
}

// A window whose names are not path segments is still reached: both names are
// escaped into the request line, not spliced into it.
func TestOneShotEscapesTheNames(t *testing.T) {
	sock := bootDaemon(t)
	post(t, sock, "/sessions", `{"name":"a b/c"}`)
	post(t, sock, "/sessions/a%20b%2Fc/windows", `{"name":"x?y#z","cols":80,"rows":24,"shell":"/bin/cat"}`)
	if _, errs, code := shellctl(t, "--socket", sock, "send-keys", "a b/c", "x?y#z", "esc"); code != 0 {
		t.Fatalf("send-keys exited %d: %s", code, errs)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, errs, code := shellctl(t, "--socket", sock, "capture", "--visible", "a b/c", "x?y#z")
		if code != 0 {
			t.Fatalf("capture exited %d: %s", code, errs)
		}
		if strings.Contains(out, "esc") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never showed the keys: %q", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A refusal by the daemon exits 1 and names the status and the daemon's error;
// stdout stays empty, so a caller piping the output gets nothing to misread.
func TestARefusalExitsOneWithTheDaemonsError(t *testing.T) {
	sock := bootDaemon(t)
	for _, args := range [][]string{
		{"capture", "s", "nope"},
		{"scroll", "nope", "w"},
		{"send-keys", "s", "nope", "x"},
	} {
		out, errs, code := shellctl(t, append([]string{"--socket", sock}, args...)...)
		if code != 1 || out != "" || !strings.Contains(errs, "HTTP 404") || !strings.Contains(errs, "not found") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 1, nothing, and the 404", args, code, out, errs)
		}
	}
}

func TestOneShotWithNoDaemonFailsToConnect(t *testing.T) {
	out, errs, code := shellctl(t, "--socket", t.TempDir()+"/none.sock", "health")
	if code != 1 || out != "" || !strings.Contains(errs, "connect to") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// Command-line mistakes exit 2 without reaching the daemon, as attach's do.
func TestOneShotUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"capture", "s"},
		{"capture", "s", "w", "extra"},
		{"capture", "--lines", "x", "s", "w"},
		{"capture", "--lines"},
		{"capture", "--offset", "1", "s", "w"},
		{"send-keys", "s", "w"},
		{"send-keys", "--visible", "s", "w", "k"},
		{"health", "x"},
		{"--json", "capture", "s", "w"},
		{"nosuch"},
	} {
		_, errs, code := shellctl(t, append([]string{"--socket", "/nonexistent/x.sock"}, args...)...)
		if code != 2 || strings.Contains(errs, "connect to") {
			t.Fatalf("%v: exit %d, stderr %q; want a usage error (2) before any connect", args, code, errs)
		}
	}
}

func TestSubcommandHelpNamesTheRoute(t *testing.T) {
	for cmd, route := range map[string]string{"capture": "/capture", "scroll": "/scroll", "send-keys": "/keys", "health": "/health"} {
		out, _, code := shellctl(t, cmd, "--help")
		if code != 0 || !strings.Contains(out, route) || !strings.Contains(out, "Usage: shellctl "+cmd) {
			t.Fatalf("%s --help = %q (exit %d)", cmd, out, code)
		}
	}
	out, _, _ := shellctl(t, "--help")
	sc := bufio.NewScanner(strings.NewReader(out))
	n := 0
	for sc.Scan() {
		for _, cmd := range []string{"attach", "capture", "scroll", "send-keys", "health"} {
			if strings.HasPrefix(strings.TrimSpace(sc.Text()), cmd+" ") {
				n++
			}
		}
	}
	if n != 5 {
		t.Fatalf("--help lists %d of the 5 commands:\n%s", n, out)
	}
}

// The one-shot path's startup is its latency, so the binary must not link the
// server: internal/daemon (and through it the root package's regexps) or
// net/http, whose package inits run on every start whatever the command.
func TestShellctlDoesNotLinkTheServer(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		switch p {
		case "github.com/gurupras/shelld/internal/daemon", "github.com/gurupras/shelld", "net/http", "crypto/tls":
			t.Errorf("shellctl links %s", p)
		}
	}
}
