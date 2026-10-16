package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// shellctlIn is shellctl with stdin.
func shellctlIn(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shellctlBin, args...)
	c.Stdin = strings.NewReader(stdin)
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

// mustRun runs a command that must succeed and returns its stdout.
func mustRun(t *testing.T, sock string, args ...string) string {
	t.Helper()
	out, errs, code := shellctl(t, append([]string{"--socket", sock}, args...)...)
	if code != 0 || errs != "" {
		t.Fatalf("%v: exit %d, stderr %q", args, code, errs)
	}
	return out
}

// sameSet reports whether a and b hold the same elements in any order.
func sameSet(a, b []map[string]any) bool {
	key := func(m []map[string]any) []string {
		var k []string
		for _, e := range m {
			j, _ := json.Marshal(e)
			k = append(k, string(j))
		}
		sort.Strings(k)
		return k
	}
	return slices.Equal(key(a), key(b))
}

// waitScreen polls capture --visible of session/window until it matches want.
func waitScreen(t *testing.T, sock, session, window string, want *regexp.Regexp) string {
	t.Helper()
	return waitCaptured(t, sock, session, window, want, "--visible", "--lines", "50")
}

// waitCaptured polls capture with opts until its output matches want.
func waitCaptured(t *testing.T, sock, session, window string, want *regexp.Regexp, opts ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := mustRun(t, sock, append(append([]string{"capture"}, opts...), session, window)...)
		if want.MatchString(out) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/%s never showed %v; last screen %q", session, window, want, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sessions is list-sessions decoded: each session's windows by name.
func sessions(t *testing.T, sock string) map[string][]string {
	t.Helper()
	var resp struct {
		Sessions []struct {
			Name    string   `json:"name"`
			Windows []string `json:"windows"`
		} `json:"sessions"`
	}
	out := mustRun(t, sock, "list-sessions")
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("list-sessions printed %q: %v", out, err)
	}
	m := map[string][]string{}
	for _, s := range resp.Sessions {
		m[s.Name] = s.Windows
	}
	return m
}

type windowInfo struct {
	Name      string `json:"name"`
	Pid       int    `json:"pid"`
	Alive     bool   `json:"alive"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	PasteMode string `json:"pasteMode"`
}

// listWindows is list-windows decoded, by name.
func listWindows(t *testing.T, sock, session string) map[string]windowInfo {
	t.Helper()
	var resp struct {
		Windows []windowInfo `json:"windows"`
	}
	out := mustRun(t, sock, "list-windows", session)
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("list-windows printed %q: %v", out, err)
	}
	m := map[string]windowInfo{}
	for _, w := range resp.Windows {
		m[w.Name] = w
	}
	return m
}

// refused runs a command that the daemon must refuse with status and returns
// its stderr. stdout must stay empty.
func refused(t *testing.T, sock string, status int, args ...string) string {
	t.Helper()
	out, errs, code := shellctl(t, append([]string{"--socket", sock}, args...)...)
	if code != 1 || out != "" || !strings.Contains(errs, "HTTP "+strconv.Itoa(status)+":") {
		t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 1, nothing, and HTTP %d", args, code, out, errs, status)
	}
	return errs
}

func TestSessionCommands(t *testing.T) {
	sock := bootDaemon(t)
	if out := mustRun(t, sock, "new-session", "a"); out != "" {
		t.Fatalf("new-session printed %q; want nothing", out)
	}
	mustRun(t, sock, "new", "b")
	if got := sessions(t, sock); !slices.Equal(got["s"], []string{"w"}) || got["a"] == nil || got["b"] == nil || len(got) != 3 {
		t.Fatalf("list-sessions after new-session a, new b: %v", got)
	}
	if out := mustRun(t, sock, "show-session", "a"); out != "{\"exists\":true}\n" {
		t.Fatalf("show-session a printed %q", out)
	}
	mustRun(t, sock, "has", "b")
	refused(t, sock, 409, "new-session", "a")
	refused(t, sock, 400, "new-session", "x\ty")
	if out := mustRun(t, sock, "kill-session", "a"); out != "" {
		t.Fatalf("kill-session printed %q; want nothing", out)
	}
	if errs := refused(t, sock, 404, "show-session", "a"); !strings.Contains(errs, `"exists":false`) {
		t.Fatalf("show-session of a killed session: %q", errs)
	}
	if got := sessions(t, sock); got["a"] != nil || got["b"] == nil {
		t.Fatalf("list-sessions after kill-session a: %v", got)
	}
	// The daemon lists sessions in no fixed order, so the two are compared as sets.
	var a, b struct{ Sessions []map[string]any }
	if json.Unmarshal([]byte(mustRun(t, sock, "ls")), &a) != nil || json.Unmarshal([]byte(mustRun(t, sock, "list-sessions")), &b) != nil ||
		len(a.Sessions) != 2 || !sameSet(a.Sessions, b.Sessions) {
		t.Fatalf("ls printed %v, list-sessions %v", a, b)
	}
}

func TestNewWindowAppliesItsOptions(t *testing.T) {
	// Made first, so that it is removed after the daemon and its windows are
	// gone: Windows refuses to remove a directory a live process works in.
	dir := t.TempDir()
	sock := bootDaemon(t)
	out := mustRun(t, sock, "new-window", "-c", dir, "--cols", "91", "--rows=31", "-e", "FOO=one", "--env", "BAR=two",
		"--unset-env", homeVar, "--paste-mode", "bracketed", "--shell", envShell(), "s", "w2")
	var resp struct {
		OK  bool `json:"ok"`
		Pid int  `json:"pid"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || !resp.OK || resp.Pid <= 0 {
		t.Fatalf("new-window printed %q (%v); want ok and a pid", out, err)
	}
	// Raw history, because the line is wider than the window and the screen wraps it.
	waitCaptured(t, sock, "s", "w2", regexp.MustCompile(regexp.QuoteMeta("cwd="+dir+" foo=one bar=two home=[]")))
	w := listWindows(t, sock, "s")["w2"]
	if w.Pid != resp.Pid || w.Cols != 91 || w.Rows != 31 || w.PasteMode != "bracketed" || !w.Alive {
		t.Fatalf("list-windows has %+v; want pid %d, 91x31, bracketed, alive", w, resp.Pid)
	}
	mustRun(t, sock, "neww", "--shell", catShell(), "s", "w3")
	if got := listWindows(t, sock, "s"); len(got) != 3 {
		t.Fatalf("list-windows after neww: %v", got)
	}
	var a, b struct{ Windows []map[string]any }
	if json.Unmarshal([]byte(mustRun(t, sock, "lsw", "s")), &a) != nil || json.Unmarshal([]byte(mustRun(t, sock, "list-windows", "s")), &b) != nil ||
		len(a.Windows) != 3 || !sameSet(a.Windows, b.Windows) {
		t.Fatalf("lsw printed %v, list-windows %v", a, b)
	}
	refused(t, sock, 409, "new-window", "--shell", catShell(), "s", "w2")
	refused(t, sock, 400, "new-window", "-c", filepath.Join(dir, "none"), "s", "w4")
	refused(t, sock, 400, "new-window", "--paste-mode", "sideways", "s", "w4")
	refused(t, sock, 404, "new-window", "nope", "w4")
}

func TestKillAndMoveWindow(t *testing.T) {
	sock := bootDaemon(t)
	mustRun(t, sock, "new-window", "--shell", catShell(), "s", "w2")
	if out := mustRun(t, sock, "kill-window", "s", "w2"); out != "" {
		t.Fatalf("kill-window printed %q; want nothing", out)
	}
	if got := listWindows(t, sock, "s"); len(got) != 1 || got["w"].Name != "w" {
		t.Fatalf("list-windows after kill-window w2: %v", got)
	}
	refused(t, sock, 404, "kill-window", "nope", "w")

	if out := mustRun(t, sock, "move-window", "--name", "moved", "s", "w", "t"); out != "" {
		t.Fatalf("move-window printed %q; want nothing", out)
	}
	if got := sessions(t, sock); len(got["s"]) != 0 || !slices.Equal(got["t"], []string{"moved"}) {
		t.Fatalf("list-sessions after move-window s w t --name moved: %v", got)
	}
	mustRun(t, sock, "movew", "t", "moved", "s")
	if got := sessions(t, sock); !slices.Equal(got["s"], []string{"moved"}) {
		t.Fatalf("list-sessions after movew t moved s: %v", got)
	}
	refused(t, sock, 409, "move-window", "s", "nope", "t")
	mustRun(t, sock, "killw", "s", "moved")
	if got := listWindows(t, sock, "s"); len(got) != 0 {
		t.Fatalf("list-windows after killw: %v", got)
	}
}

func TestResizeWindow(t *testing.T) {
	sock := bootDaemon(t)
	if out := mustRun(t, sock, "resize-window", "s", "w", "100", "40"); out != "" {
		t.Fatalf("resize-window printed %q; want nothing", out)
	}
	if w := listWindows(t, sock, "s")["w"]; w.Cols != 100 || w.Rows != 40 {
		t.Fatalf("after resize-window 100 40: %+v", w)
	}
	mustRun(t, sock, "resizew", "s", "w", "60", "20")
	if w := listWindows(t, sock, "s")["w"]; w.Cols != 60 || w.Rows != 20 {
		t.Fatalf("after resizew 60 20: %+v", w)
	}
	refused(t, sock, 400, "resize-window", "s", "w", "0", "20")
	refused(t, sock, 404, "resize-window", "s", "nope", "10", "10")
}

// allColourAnswers is what queryShell shows of the answers to all four of its
// colour queries.
const allColourAnswers = "]11;rgb:1010/2020/3030^G^[]10;rgb:a0a0/b0b0/c0c0^G^[]12;rgb:0101/0202/0303^G^[]4;1;rgb:ffff/8080/0000^G"

// The colours are read back the way a program reads them: the window answers
// an OSC 11 or OSC 4 query with what set-colors set, and cat's terminal echoes
// the answer onto the screen.
func TestSetColorsAnswersTheProgramsQuery(t *testing.T) {
	sock := bootDaemon(t)
	mustRun(t, sock, "new-window", "--cols", "200", "--shell", queryShell(), "s", "q")
	if out := mustRun(t, sock, "set-colors", "--background", "#102030", "--foreground", "a0b0c0", "--cursor", "#010203",
		"--palette", "1=#ff8000", "s", "q"); out != "" {
		t.Fatalf("set-colors printed %q; want nothing", out)
	}
	mustRun(t, sock, "send-keys", "s", "q", "go")
	waitScreen(t, sock, "s", "q", regexp.MustCompile(regexp.QuoteMeta(colourAnswers())))
	refused(t, sock, 400, "set-colors", "--background", "nothex", "s", "q")
	refused(t, sock, 400, "set-colors", "s", "q")
	refused(t, sock, 404, "set-colors", "--background", "#000000", "s", "nope")
}

func TestSendKeysNamesAndHex(t *testing.T) {
	sock := bootDaemon(t)
	if out := mustRun(t, sock, "send-keys", "-K", "s", "w", "a", "b", "Tab", "c", "Enter"); out != "" {
		t.Fatalf("send-keys -K printed %q; want nothing", out)
	}
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`ab`+tabOnScreen+`c\nab`+tabOnScreen+`c`))
	mustRun(t, sock, "send-keys", "-H", "s", "w", "6869", "0d")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nhi\nhi\n`))
	mustRun(t, sock, "send", "--key-names", "s", "w", "zz", "Enter")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nzz\nzz\n`))
	mustRun(t, sock, "send-keys", "--hex", "s", "w", "7979", "0d")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nyy\nyy\n`))
	// strict refuses the call whole: the text before the unknown name is not typed.
	errs := refused(t, sock, 400, "send-keys", "-K", "--strict", "s", "w", "q", "NoSuchKey", "Enter")
	if !strings.Contains(errs, "NoSuchKey") {
		t.Fatalf("strict refusal %q does not name the key", errs)
	}
	mustRun(t, sock, "send-keys", "s", "w", "after")
	screen := waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nafter\nafter\n`))
	if strings.Contains(screen, "q") {
		t.Fatalf("a strict refusal typed its text: %q", screen)
	}
	refused(t, sock, 404, "send-keys", "-K", "s", "nope", "a")
	refused(t, sock, 404, "send-keys", "-H", "s", "nope", "61")
}

func TestPaste(t *testing.T) {
	sock := bootDaemon(t)
	if out := mustRun(t, sock, "paste", "s", "w", "argtext"); out != "" {
		t.Fatalf("paste printed %q; want nothing", out)
	}
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`argtext\nargtext\n`))
	out, errs, code := shellctlIn(t, "from stdin", "--socket", sock, "paste", "s", "w")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("paste from stdin: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nfrom stdin\nfrom stdin\n`))
	mustRun(t, sock, "paste", "--mode", "bracketed", "s", "w", "br")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(regexp.QuoteMeta(bracketedEcho)))
	// Sanitized, DEL arrives as the text ^?; literal, it is the terminal's
	// erase character and takes the X with it.
	mustRun(t, sock, "paste", "s", "w", "saX\x7fn")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(regexp.QuoteMeta("saX^?n\nsaX^?n\n")))
	mustRun(t, sock, "paste", "--literal", "s", "w", "liX\x7ft")
	waitScreen(t, sock, "s", "w", regexp.MustCompile(`\nlit\nlit\n`))
	refused(t, sock, 400, "paste", "--mode", "sideways", "s", "w", "x")
	refused(t, sock, 404, "paste", "s", "nope", "x")
}

func TestInjectAndItsQueue(t *testing.T) {
	sock := bootDaemon(t)
	out := mustRun(t, sock, "inject", "--priority", "-3", "s", "w", "queued text")
	var resp struct {
		ID          string `json:"id"`
		QueueLength int    `json:"queueLength"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.ID == "" || resp.QueueLength < 1 {
		t.Fatalf("inject printed %q (%v); want an id and a queue length", out, err)
	}
	var q struct {
		Length  int `json:"length"`
		Pending []struct {
			ID       string `json:"id"`
			Payload  string `json:"payload"`
			Priority int    `json:"priority"`
		} `json:"pending"`
		Expired []any `json:"expired"`
	}
	out = mustRun(t, sock, "inject-queue", "s", "w")
	if err := json.Unmarshal([]byte(out), &q); err != nil || q.Expired == nil {
		t.Fatalf("inject-queue printed %q (%v)", out, err)
	}
	// cat never shows a prompt, so the payload waits; measured, not assumed.
	if q.Length != 1 || len(q.Pending) != 1 || q.Pending[0].ID != resp.ID || q.Pending[0].Payload != "queued text" || q.Pending[0].Priority != -3 {
		t.Fatalf("inject-queue = %+v; want the one payload, id %s, priority -3", q, resp.ID)
	}
	refused(t, sock, 400, "inject", "s", "w", "")
	refused(t, sock, 404, "inject", "s", "nope", "x")
	refused(t, sock, 404, "inject-queue", "s", "nope")
}

func TestCapturePaneAndFrame(t *testing.T) {
	sock := bootDaemon(t)
	for _, l := range []string{"one", "two", "three"} {
		mustRun(t, sock, "send-keys", "s", "w", l)
		waitCapture(t, sock, l+"\n"+l)
	}
	if out := mustRun(t, sock, "capture-pane", "--lines", "2", "s", "w"); out != "three\nthree\n" {
		t.Fatalf("capture-pane --lines 2 = %q", out)
	}
	if out := mustRun(t, sock, "capture-pane", "-S", strconv.Itoa(historyBase), "--lines", "2", "s", "w"); out != "one\none\n" {
		t.Fatalf("capture-pane -S %d --lines 2 = %q", historyBase, out)
	}
	if out := mustRun(t, sock, "capturep", "--offset="+strconv.Itoa(historyBase+2), "--lines", "1", "s", "w"); out != "two\n" {
		t.Fatalf("capturep --offset=%d --lines 1 = %q", historyBase+2, out)
	}
	if a, b := mustRun(t, sock, "capture-pane", "--visible", "--json", "s", "w"), mustRun(t, sock, "capture", "--visible", "--json", "s", "w"); !strings.Contains(a, `"mode":"visible"`) || !strings.Contains(b, `"mode":"visible"`) {
		t.Fatalf("capture-pane --visible --json = %q, capture's %q", a, b)
	}
	var js struct {
		Offset int `json:"offset"`
	}
	if out := mustRun(t, sock, "capture-pane", "-S", "1", "--json", "s", "w"); json.Unmarshal([]byte(out), &js) != nil || js.Offset != 1 {
		t.Fatalf("capture-pane -S 1 --json = %q; want scroll's response at offset 1", out)
	}
	refused(t, sock, 404, "capture-pane", "s", "nope")
	refused(t, sock, 404, "capture-pane", "-S", "0", "s", "nope")

	text := mustRun(t, sock, "frame", "s", "w")
	var fr struct {
		Frame string `json:"frame"`
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
	}
	raw := mustRun(t, sock, "frame", "--json", "s", "w")
	if err := json.Unmarshal([]byte(raw), &fr); err != nil || fr.Cols != 80 || fr.Rows != 24 {
		t.Fatalf("frame --json = %.200q (%v)", raw, err)
	}
	want := fr.Frame
	if !strings.HasSuffix(want, "\n") {
		want += "\n"
	}
	if text != want || !strings.Contains(text, "three\r\nthree") || !strings.Contains(text, "\x1b[") {
		t.Fatalf("frame printed %.200q; want the route's frame %.200q, escapes and all", text, fr.Frame)
	}
	refused(t, sock, 404, "frame", "s", "nope")
}

func TestKillServerStopsTheDaemon(t *testing.T) {
	sock := bootDaemon(t)
	if out := mustRun(t, sock, "kill-server"); out != "" {
		t.Fatalf("kill-server printed %q; want nothing", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, errs, code := shellctl(t, "--socket", sock, "health")
		if code == 1 && strings.Contains(errs, "connect to") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon still answers health after kill-server: exit %d, %q", code, errs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	sock = bootDaemon(t)
	mustRun(t, sock, "shutdown")
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, _, code := shellctl(t, "--socket", sock, "health"); code == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon still answers health after shutdown")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every window command reaches the missing session and the missing window as
// the daemon's 404, exits 1, and prints nothing.
func TestWindowCommandsRefuseAMissingWindow(t *testing.T) {
	sock := bootDaemon(t)
	for _, c := range [][]string{
		{"list-windows", "S"}, {"new-window", "S", "x"}, {"kill-window", "S", "W"},
		{"resize-window", "S", "W", "10", "10"}, {"set-colors", "--cursor", "#000000", "S", "W"},
		{"send-keys", "S", "W", "x"}, {"send-keys", "-K", "S", "W", "x"}, {"send-keys", "-H", "S", "W", "78"},
		{"paste", "S", "W", "x"}, {"inject", "S", "W", "x"}, {"inject-queue", "S", "W"},
		{"capture", "S", "W"}, {"scroll", "S", "W"}, {"capture-pane", "S", "W"}, {"frame", "S", "W"},
	} {
		for _, names := range [][2]string{{"nope", "w"}, {"s", "nope"}} {
			args := slices.Clone(c)
			for i, a := range args {
				switch a {
				case "S":
					args[i] = names[0]
				case "W":
					args[i] = names[1]
				}
			}
			if names[0] == "s" && !slices.Contains(c, "W") {
				continue // a session-only command has no missing window
			}
			errs := refused(t, sock, 404, args...)
			want := "window not found"
			if names[0] == "nope" {
				want = "session not found"
			}
			if !strings.Contains(errs, want) {
				t.Fatalf("%v: stderr %q; want %q", args, errs, want)
			}
		}
	}
}

// Killing what is not there fails, as tmux's kill-window and kill-session do:
// the daemon answers 404 and the command exits 1 with its error.
func TestKillOfWhatIsNotThereFails(t *testing.T) {
	sock := bootDaemon(t)
	if errs := refused(t, sock, 404, "kill-window", "s", "nope"); !strings.Contains(errs, "window not found") {
		t.Fatalf("kill-window s nope: stderr %q", errs)
	}
	if errs := refused(t, sock, 404, "kill-session", "nope"); !strings.Contains(errs, "session not found") {
		t.Fatalf("kill-session nope: stderr %q", errs)
	}
}

// One fake daemon answers every request with a body every command can read,
// so that what is checked is the request alone: method, escaped path, query
// and body, byte for byte.
func TestEveryCommandsRequestOnTheWire(t *testing.T) {
	const body = `{"ok":true,"output":"o","frame":"f"}`
	resp := "HTTP/1.0 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	for _, c := range wireCases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			sock, reqs := fakeDaemon(t, answer(resp))
			_, errs, code := shellctlIn(t, c.stdin, append([]string{"--socket", sock}, c.args...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			want := c.method + " " + c.path + " HTTP/1.0\r\nHost: shelld\r\n"
			if c.body != "" {
				want += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(c.body)) + "\r\n"
			}
			want += "\r\n" + c.body
			select {
			case got := <-reqs:
				if got != want {
					t.Fatalf("request\n%q\nwant\n%q", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no request arrived")
			}
		})
	}
}

type wireCase struct {
	args                      []string
	stdin                     string
	method, path, route, body string
}

// wireCases is one request per command and per option, and route the pattern
// it reaches, as internal/daemon's Handler names it.
var wireCases = []wireCase{
	{args: []string{"health"}, method: "GET", path: "/health", route: "/health"},
	{args: []string{"kill-server"}, method: "POST", path: "/shutdown", route: "/shutdown"},
	{args: []string{"shutdown"}, method: "POST", path: "/shutdown", route: "/shutdown"},
	{args: []string{"list-sessions"}, method: "GET", path: "/sessions", route: "/sessions"},
	{args: []string{"ls"}, method: "GET", path: "/sessions", route: "/sessions"},
	{args: []string{"new-session", "a b"}, method: "POST", path: "/sessions", route: "/sessions", body: `{"name":"a b"}`},
	{args: []string{"new", "n"}, method: "POST", path: "/sessions", route: "/sessions", body: `{"name":"n"}`},
	{args: []string{"show-session", "a/b"}, method: "GET", path: "/sessions/a%2Fb", route: "/sessions/:session"},
	{args: []string{"has", "a"}, method: "GET", path: "/sessions/a", route: "/sessions/:session"},
	{args: []string{"kill-session", "a b"}, method: "DELETE", path: "/sessions/a%20b", route: "/sessions/:session"},
	{args: []string{"list-windows", "s"}, method: "GET", path: "/sessions/s/windows", route: "/sessions/:session/windows"},
	{args: []string{"lsw", "s"}, method: "GET", path: "/sessions/s/windows", route: "/sessions/:session/windows"},
	{args: []string{"new-window", "s", "w"}, method: "POST", path: "/sessions/s/windows", route: "/sessions/:session/windows", body: `{"name":"w"}`},
	{args: []string{"neww", "-c", "/d", "--shell", "sh -c x", "--cols", "90", "--rows=30", "-e", "A=1", "--env=B=2=3",
		"--unset-env", "HOME", "--unset-env", "PATH", "--paste-mode", "raw", "s", "w"},
		method: "POST", path: "/sessions/s/windows", route: "/sessions/:session/windows",
		body: `{"cols":90,"cwd":"/d","env":["A=1","B=2=3"],"name":"w","pasteMode":"raw","rows":30,"shell":"sh -c x","unsetEnv":["HOME","PATH"]}`},
	{args: []string{"kill-window", "s", "w"}, method: "DELETE", path: "/sessions/s/windows/w", route: "/sessions/:session/windows/:window"},
	{args: []string{"killw", "s", "x?y"}, method: "DELETE", path: "/sessions/s/windows/x%3Fy", route: "/sessions/:session/windows/:window"},
	{args: []string{"move-window", "s", "w", "t"}, method: "POST", path: "/move-window", route: "/move-window",
		body: `{"dstSession":"t","srcName":"w","srcSession":"s"}`},
	{args: []string{"movew", "--name", "n", "s", "w", "t"}, method: "POST", path: "/move-window", route: "/move-window",
		body: `{"dstName":"n","dstSession":"t","srcName":"w","srcSession":"s"}`},
	{args: []string{"resize-window", "s", "w", "100", "40"}, method: "POST", path: "/sessions/s/windows/w/resize",
		route: "/sessions/:session/windows/:window/resize", body: `{"cols":100,"rows":40}`},
	{args: []string{"resizew", "s", "w", "1", "2"}, method: "POST", path: "/sessions/s/windows/w/resize",
		route: "/sessions/:session/windows/:window/resize", body: `{"cols":1,"rows":2}`},
	{args: []string{"set-colors", "--background", "#000001", "--foreground", "000002", "--cursor", "#000003",
		"--palette", "1=#ff0000", "--palette=+2=00ff00", "s", "w"}, method: "POST", path: "/sessions/s/windows/w/colors",
		route: "/sessions/:session/windows/:window/colors",
		body:  `{"background":"#000001","cursor":"#000003","foreground":"000002","palette":{"+2":"00ff00","1":"#ff0000"}}`},
	{args: []string{"set-colors", "s", "w"}, method: "POST", path: "/sessions/s/windows/w/colors",
		route: "/sessions/:session/windows/:window/colors", body: `{}`},
	{args: []string{"send-keys", "s", "w", "ls"}, method: "POST", path: "/sessions/s/windows/w/keys",
		route: "/sessions/:session/windows/:window/keys", body: `{"keys":"ls","enter":true}`},
	{args: []string{"send", "--no-enter", "--enter-delay-ms", "5", "s", "w", "x"}, method: "POST", path: "/sessions/s/windows/w/keys",
		route: "/sessions/:session/windows/:window/keys", body: `{"keys":"x","enter":false,"enter_delay_ms":5}`},
	{args: []string{"send-keys", "-K", "s", "w", "C-c", "Enter", "é"}, method: "POST", path: "/sessions/s/windows/w/key-names",
		route: "/sessions/:session/windows/:window/key-names", body: `{"names":["C-c","Enter","é"],"strict":false}`},
	{args: []string{"send-keys", "--key-names", "--strict", "s", "w", "Up"}, method: "POST", path: "/sessions/s/windows/w/key-names",
		route: "/sessions/:session/windows/:window/key-names", body: `{"names":["Up"],"strict":true}`},
	{args: []string{"send-keys", "-H", "s", "w", "1b5b", "41", "C3A9"}, method: "POST", path: "/sessions/s/windows/w/raw-key",
		route: "/sessions/:session/windows/:window/raw-key", body: `{"key":"\u001b[Aé"}`},
	{args: []string{"send-keys", "--hex", "s", "w", "00"}, method: "POST", path: "/sessions/s/windows/w/raw-key",
		route: "/sessions/:session/windows/:window/raw-key", body: `{"key":"\u0000"}`},
	{args: []string{"paste", "s", "w", "a\nb"}, method: "POST", path: "/sessions/s/windows/w/paste",
		route: "/sessions/:session/windows/:window/paste", body: `{"text":"a\nb"}`},
	{args: []string{"paste", "--mode", "bracketed", "--literal", "s", "w"}, stdin: "from\nstdin\n", method: "POST",
		path: "/sessions/s/windows/w/paste", route: "/sessions/:session/windows/:window/paste",
		body: `{"literal":true,"mode":"bracketed","text":"from\nstdin\n"}`},
	{args: []string{"paste", "s", "w"}, method: "POST", path: "/sessions/s/windows/w/paste",
		route: "/sessions/:session/windows/:window/paste", body: `{"text":""}`},
	{args: []string{"inject", "s", "w", "p"}, method: "POST", path: "/sessions/s/windows/w/inject",
		route: "/sessions/:session/windows/:window/inject", body: `{"payload":"p"}`},
	{args: []string{"inject", "--priority", "-7", "s", "w", "p"}, method: "POST", path: "/sessions/s/windows/w/inject",
		route: "/sessions/:session/windows/:window/inject", body: `{"payload":"p","priority":-7}`},
	{args: []string{"inject-queue", "s", "w"}, method: "GET", path: "/sessions/s/windows/w/inject/queue",
		route: "/sessions/:session/windows/:window/inject/queue"},
	{args: []string{"capture", "s", "w"}, method: "GET", path: "/sessions/s/windows/w/capture",
		route: "/sessions/:session/windows/:window/capture"},
	{args: []string{"scroll", "s", "w"}, method: "GET", path: "/sessions/s/windows/w/scroll",
		route: "/sessions/:session/windows/:window/scroll"},
	{args: []string{"capture-pane", "--visible", "--lines", "3", "s", "w"}, method: "GET",
		path: "/sessions/s/windows/w/capture?lines=3&mode=visible", route: "/sessions/:session/windows/:window/capture"},
	{args: []string{"capturep", "--max-bytes", "9", "--json", "s", "w"}, method: "GET",
		path: "/sessions/s/windows/w/capture?max_bytes=9", route: "/sessions/:session/windows/:window/capture"},
	{args: []string{"capture-pane", "-S", "4", "--lines", "3", "--max-bytes=9", "s", "w"}, method: "GET",
		path: "/sessions/s/windows/w/scroll?lines=3&max_bytes=9&offset=4", route: "/sessions/:session/windows/:window/scroll"},
	{args: []string{"capture-pane", "--offset", "0", "s", "w"}, method: "GET",
		path: "/sessions/s/windows/w/scroll?offset=0", route: "/sessions/:session/windows/:window/scroll"},
	{args: []string{"frame", "s", "w"}, method: "GET", path: "/sessions/s/windows/w/frame",
		route: "/sessions/:session/windows/:window/frame"},
	{args: []string{"frame", "--json", "s", "w"}, method: "GET", path: "/sessions/s/windows/w/frame",
		route: "/sessions/:session/windows/:window/frame"},
}

// Every route the daemon serves is reached by a command: the routes are read
// from internal/daemon's Handler, so a route added there without a command
// here fails this. attach is reached by `shellctl attach`, which speaks
// WebSocket and has its own tests.
func TestEveryRouteHasACommand(t *testing.T) {
	src, err := os.ReadFile("../../internal/daemon/routes.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`rt\.handle\("([A-Z]+)", "([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1]+" "+m[2])
	}
	if len(routes) < 20 {
		t.Fatalf("found %d routes in routes.go; the pattern no longer reads Handler", len(routes))
	}
	reached := map[string]bool{"GET /sessions/:session/windows/:window/attach": true}
	for _, c := range wireCases {
		reached[c.method+" "+c.route] = true
	}
	sort.Strings(routes)
	for _, r := range routes {
		if !reached[r] {
			t.Errorf("no command reaches %s", r)
		}
		delete(reached, r)
	}
	for r := range reached {
		t.Errorf("a case names %s, which the daemon does not serve", r)
	}
	t.Logf("%d routes, every one reached", len(routes))
}

// Mistakes on the command line exit 2 before anything is sent.
func TestNewCommandsUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"new-session"}, {"new-session", "a", "b"}, {"show-session"}, {"kill-session"}, {"list-windows"},
		{"list-sessions", "x"}, {"kill-server", "x"},
		{"new-window", "s"}, {"new-window", "--cols", "x", "s", "w"}, {"new-window", "-e", "NOEQUALS", "s", "w"},
		{"new-window", "--cols"}, {"kill-window", "s"}, {"move-window", "s", "w"},
		{"resize-window", "s", "w", "1"}, {"resize-window", "s", "w", "x", "1"}, {"resize-window", "s", "w", "1", "-1"},
		{"set-colors", "--palette", "1", "s", "w"}, {"set-colors", "--visible", "s", "w"},
		{"send-keys", "-K", "s", "w"}, {"send-keys", "s", "w", "a", "b"},
		{"send-keys", "-K", "-H", "s", "w", "61"}, {"send-keys", "-K", "--no-enter", "s", "w", "a"},
		{"send-keys", "-H", "--enter-delay-ms", "1", "s", "w", "61"}, {"send-keys", "--strict", "s", "w", "a"},
		{"send-keys", "-H", "s", "w", "6"}, {"send-keys", "-H", "s", "w", "zz"}, {"send-keys", "-H", "s", "w", "ff"},
		{"send-keys", "-K", "s", "w", "\xff"}, {"paste", "s", "w", "a\xffb"}, {"paste", "s"},
		{"paste", "s", "w", "a", "b"}, {"inject", "s", "w"}, {"inject", "--priority", "x", "s", "w", "p"},
		{"inject", "s", "w", "\xff"}, {"inject-queue", "s"}, {"frame", "s"}, {"frame", "--visible", "s", "w"},
		{"capture-pane", "--visible", "-S", "0", "s", "w"}, {"capture-pane", "-S", "-1", "s", "w"},
		{"capture", "-S", "0", "s", "w"}, {"has"}, {"ls", "x"},
	} {
		if !argvCarriesBytes && slices.ContainsFunc(args, func(a string) bool { return !utf8.ValidString(a) }) {
			continue // see argvCarriesBytes; paste's stdin can carry it, and is tested
		}
		_, errs, code := shellctl(t, append([]string{"--socket", "/nonexistent/x.sock"}, args...)...)
		if code != 2 || strings.Contains(errs, "connect to") {
			t.Fatalf("%q: exit %d, stderr %q; want a usage error (2) before any connect", args, code, errs)
		}
	}
}

// paste reads stdin only when TEXT is left out, and invalid UTF-8 on stdin is
// refused as an argument's would be.
func TestPasteRefusesInvalidUTF8OnStdin(t *testing.T) {
	_, errs, code := shellctlIn(t, "a\xffb", "--socket", "/nonexistent/x.sock", "paste", "s", "w")
	if code != 2 || !strings.Contains(errs, "not valid UTF-8") {
		t.Fatalf("exit %d, stderr %q; want a usage error naming UTF-8", code, errs)
	}
}

// Each command's --help names its route and its usage line, an alias's help
// is its command's, and the top-level help lists every command.
func TestEveryCommandHasHelp(t *testing.T) {
	for cmd := range subOptions {
		out, _, code := shellctl(t, cmd, "--help")
		if code != 0 || !strings.Contains(out, "Usage: shellctl "+cmd) {
			t.Errorf("%s --help = %q (exit %d)", cmd, out, code)
		}
		for opt := range subOptions[cmd] {
			if !strings.Contains(out, opt) {
				t.Errorf("%s --help does not document %s", cmd, opt)
			}
		}
	}
	for _, c := range wireCases {
		out, _, _ := shellctl(t, c.args[0], "--help")
		last := strings.NewReplacer(":session", "<SESSION>", ":window", "<WINDOW>").Replace(c.route[strings.LastIndex(c.route, "/"):])
		if !strings.Contains(out, last) {
			t.Errorf("%s --help does not name %s %s: %q", c.args[0], c.method, c.route, out)
		}
	}
	for alias, cmd := range aliases {
		a, _, _ := shellctl(t, alias, "--help")
		b, _, _ := shellctl(t, cmd, "--help")
		if a != b || !strings.Contains(b, alias) {
			t.Errorf("%s --help = %q; want %s's, naming the alias", alias, a, cmd)
		}
	}
	top, _, _ := shellctl(t, "--help")
	for cmd := range subOptions {
		if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(cmd) + ` `).MatchString(top) {
			t.Errorf("--help does not list %s", cmd)
		}
	}
}
