//go:build linux || darwin

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws"
)

// boot serves the API on a temp socket for the duration of the test.
func boot(t *testing.T) string {
	t.Helper()
	return bootWith(t, nil)
}

// bootWith is boot with configure applied to the state before serving.
func bootWith(t *testing.T, configure func(*State)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shelld")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := listen(sock, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(sock)
	st.Exit = func(int) {}
	if configure != nil {
		configure(st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = ServeListener(ctx, ln, st)
		close(done)
	}()
	t.Cleanup(func() {
		st.Mgr.KillAll()
		cancel()
		<-done
		os.RemoveAll(dir)
	})
	return sock
}

func request(t *testing.T, sock, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: unix\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", method, path, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	return resp.StatusCode, v, string(raw)
}

func TestFullAPISmoke(t *testing.T) {
	sock := boot(t)
	must := func(want int, method, path, body string) map[string]any {
		t.Helper()
		st, v, raw := request(t, sock, method, path, body)
		if st != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, st, want, raw)
		}
		return v
	}

	v := must(200, "GET", "/health", "")
	if v["status"] != "ok" || v["version"] != "1" || v["pid"].(float64) <= 0 {
		t.Fatalf("health: %v", v)
	}
	must(200, "POST", "/sessions", `{"name":"s1"}`)
	must(409, "POST", "/sessions", `{"name":"s1"}`)
	must(400, "POST", "/sessions", `{"name":""}`)
	must(400, "POST", "/sessions", `{"name":null}`)
	v = must(200, "GET", "/sessions", "")
	if s := v["sessions"].([]any); len(s) != 1 || s[0].(map[string]any)["name"] != "s1" {
		t.Fatalf("sessions: %v", v)
	}
	if v := must(200, "GET", "/sessions/s1", ""); v["exists"] != true {
		t.Fatal("exists")
	}
	must(404, "GET", "/sessions/missing", "")

	v = must(200, "POST", "/sessions/s1/windows", `{"name":"w1","cols":80,"rows":24,"shell":"/bin/sh"}`)
	if v["ok"] != true || v["pid"].(float64) <= 0 {
		t.Fatalf("create window: %v", v)
	}
	must(409, "POST", "/sessions/s1/windows", `{"name":"w1"}`)
	must(400, "POST", "/sessions/s1/windows", `{"name":"w2","cols":-1}`)
	w0 := must(200, "GET", "/sessions/s1/windows", "")["windows"].([]any)[0].(map[string]any)
	if w0["name"] != "w1" || w0["cols"] != 80.0 || w0["rows"] != 24.0 || w0["alive"] != true {
		t.Fatalf("window: %v", w0)
	}

	must(200, "POST", "/sessions/s1/windows/w1/keys", `{"keys":"echo hi-$((40+2))","enter":true}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v = must(200, "GET", "/sessions/s1/windows/w1/capture?lines=10", "")
		if strings.Contains(v["output"].(string), "hi-42") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never showed output: %v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v := must(200, "GET", "/sessions/s1/windows/w1/capture?mode=visible&lines=5", ""); v["mode"] != "visible" || !strings.Contains(v["output"].(string), "hi-42") {
		t.Fatalf("visible capture: %v", v)
	}
	must(400, "GET", "/sessions/s1/windows/w1/capture?lines=abc", "")
	if v := must(200, "GET", "/sessions/s1/windows/w1/scroll?offset=0&lines=5", ""); v["offset"] != 0.0 || v["lines"] != 5.0 {
		t.Fatalf("scroll: %v", v)
	}
	must(200, "POST", "/sessions/s1/windows/w1/resize", `{"cols":120,"rows":40}`)
	must(400, "POST", "/sessions/s1/windows/w1/resize", `{"cols":0,"rows":40}`)
	must(400, "POST", "/sessions/s1/windows/w1/resize", `{"cols":1025,"rows":40}`)
	must(400, "POST", "/sessions/s1/windows/w1/resize", `{"cols":120,"rows":1025}`)
	must(200, "POST", "/sessions/s1/windows/w1/raw-key", "{\"key\":\"\\u0003\"}")
	must(200, "POST", "/sessions/s1/windows/w1/paste", `{"text":"hello"}`)
	must(200, "POST", "/sessions/s1/windows/w1/colors", `{"background":"#1e1e2e","palette":{"1":"ff0000","999":"000000"}}`)
	must(400, "POST", "/sessions/s1/windows/w1/colors", `{"background":"nope"}`)
	if v := must(200, "GET", "/sessions/s1/windows/w1/frame", ""); v["cols"] != 120.0 || v["rows"] != 40.0 || v["frame"] == nil {
		t.Fatalf("frame: %v", v)
	}
	must(404, "GET", "/sessions/s1/windows/nope/frame", "")

	v = must(202, "POST", "/sessions/s1/windows/w1/inject", `{"payload":"hi","priority":0}`)
	if !strings.HasPrefix(v["id"].(string), "inj-") {
		t.Fatalf("inject: %v", v)
	}
	if v := must(200, "GET", "/sessions/s1/windows/w1/inject/queue", ""); v["length"] != 1.0 {
		t.Fatalf("queue: %v", v)
	}

	must(200, "POST", "/sessions", `{"name":"s2"}`)
	must(200, "POST", "/move-window", `{"srcSession":"s1","srcName":"w1","dstSession":"s2","dstName":""}`)
	if w := must(200, "GET", "/sessions/s2/windows", "")["windows"].([]any); w[0].(map[string]any)["name"] != "w1" {
		t.Fatal("move")
	}
	must(400, "POST", "/move-window", `{"srcSession":"s1"}`)
	must(200, "DELETE", "/sessions/s2/windows/w1", "")
	must(200, "DELETE", "/sessions/s1", "")
	must(405, "PUT", "/sessions", "")
	must(404, "GET", "/nope", "")
}

func TestJSONKeysSortedLikeSerde(t *testing.T) {
	sock := boot(t)
	_, _, raw := request(t, sock, "GET", "/sessions/missing/windows", "")
	if raw != `{"error":"session not found"}` {
		t.Fatalf("body %q", raw)
	}
	request(t, sock, "POST", "/sessions", `{"name":"a"}`)
	_, _, raw = request(t, sock, "POST", "/sessions", `{"name":"a"}`)
	if raw != `{"error":"session \"a\" already exists"}` {
		t.Fatalf("body %q", raw)
	}
}

// Input routes: key names, paste options, the window's paste mode and the
// input modes the program has set.
func TestInputAPI(t *testing.T) {
	sock := boot(t)
	must := func(want int, method, path, body string) (map[string]any, string) {
		t.Helper()
		st, v, raw := request(t, sock, method, path, body)
		if st != want {
			t.Fatalf("%s %s %s: status %d want %d: %s", method, path, body, st, want, raw)
		}
		return v, raw
	}
	window := func() map[string]any {
		v, _ := must(200, "GET", "/sessions/s/windows", "")
		return v["windows"].([]any)[0].(map[string]any)
	}
	must(200, "POST", "/sessions", `{"name":"s"}`)
	must(400, "POST", "/sessions/s/windows", `{"name":"w","shell":"/bin/sh","pasteMode":"brackets"}`)
	must(400, "POST", "/sessions/s/windows", `{"name":"w","shell":"/bin/sh","pasteMode":1}`)
	must(200, "POST", "/sessions/s/windows", `{"name":"w","cols":80,"rows":24,"shell":"/bin/sh","pasteMode":"bracketed"}`)
	w := window()
	if w["pasteMode"] != "bracketed" || fmt.Sprint(w["inputModes"]) != "map[appCursor:false appKeypad:false bracketedPaste:false]" {
		t.Fatalf("window: %v", w)
	}

	const keys = "/sessions/s/windows/w/key-names"
	must(200, "POST", keys, `{"names":["printf '\\033[?2004h\\033[?1h'; echo hi-$((40+2))","Enter"]}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, _ := must(200, "GET", "/sessions/s/windows/w/capture?mode=visible&lines=10", "")
		if strings.Contains(v["output"].(string), "hi-42") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key names never ran the command: %v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// DECCKM and DECKPAM set apart, so a swapped field shows.
	if m := fmt.Sprint(window()["inputModes"]); m != "map[appCursor:true appKeypad:false bracketedPaste:true]" {
		t.Fatalf("input modes after DECCKM and 2004: %s", m)
	}
	must(200, "POST", keys, `{"names":["printf '\\033[?1l\\033=\\033[?2004l'; echo modes-2","Enter"]}`)
	deadline = time.Now().Add(5 * time.Second)
	for {
		m := fmt.Sprint(window()["inputModes"])
		if m == "map[appCursor:false appKeypad:true bracketedPaste:false]" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("input modes after DECKPAM only: %s", m)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, raw := must(400, "POST", keys, `{"names":["Up","Entr","S-Enter"],"strict":true}`); raw != `{"error":"unknown key names [\"Entr\"]; key names that lose modifiers [\"S-Enter\"]","lossy":["S-Enter"],"unknown":["Entr"]}` {
		t.Fatalf("strict body %s", raw)
	}
	must(200, "POST", keys, `{"names":["C-u"],"strict":true}`)
	for _, body := range []string{`{}`, `{"names":null}`, `{"names":"Enter"}`, `{"names":[1]}`, `{"names":[null]}`, `{"names":["Up"],"strict":"yes"}`} {
		must(400, "POST", keys, body)
	}
	must(404, "POST", "/sessions/s/windows/nope/key-names", `{"names":["Up"]}`)

	const paste = "/sessions/s/windows/w/paste"
	must(200, "POST", paste, `{"text":"x","mode":"raw","literal":true}`)
	must(200, "POST", paste, `{"text":"x","mode":null,"literal":null}`)
	must(400, "POST", paste, `{"text":"x","mode":"brackets"}`)
	must(400, "POST", paste, `{"text":"x","literal":"no"}`)

	must(200, "POST", keys, `{"names":["C-u","exit","Enter"]}`)
	deadline = time.Now().Add(5 * time.Second)
	for window()["alive"] != false {
		if time.Now().After(deadline) {
			t.Fatal("shell never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(500, "POST", keys, `{"names":["Up"]}`)
	must(500, "POST", paste, `{"text":"x"}`)
}

// The paste route's mode, literal and the window's pasteMode change the bytes
// the program reads, not just the status code. Each window's program writes
// its raw input to a file.
func TestPasteAPIDeliversBytes(t *testing.T) {
	sock := boot(t)
	dir := t.TempDir()
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"p"}`); st != 200 {
		t.Fatal(raw)
	}
	type tc struct {
		name, modes, pasteMode, body, want string
	}
	cases := []tc{
		{"auto without 2004", "", "", `{"text":"a\u001b[201~b\u0003\nc"}`, "a^[[201~b^C\rc\r"},
		{"auto with 2004", `\033[?2004h`, "", `{"text":"hi"}`, "\x1b[200~hi\x1b[201~\r"},
		{"bracketed without 2004", "", "", `{"text":"a\u001b[201~b","mode":"bracketed"}`, "\x1b[200~a^[[201~b\x1b[201~\r"},
		{"raw with 2004", `\033[?2004h`, "", `{"text":"hi","mode":"raw"}`, "hi\r"},
		{"literal with 2004", `\033[?2004h`, "", `{"text":"x\u0003\n","literal":true}`, "\x1b[200~x\x03\n\x1b[201~\r"},
		{"bracketed window without 2004", "", "bracketed", `{"text":"hi"}`, "\x1b[200~hi\x1b[201~\r"},
		{"raw window with 2004", `\033[?2004h`, "raw", `{"text":"hi"}`, "hi\r"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			win := fmt.Sprintf("w%d", i)
			out := filepath.Join(dir, win+".in")
			script := filepath.Join(dir, win+".sh")
			prog := fmt.Sprintf("#!/bin/sh\nstty raw -echo\nprintf '%sready'\nexec cat > %s\n", c.modes, out)
			if err := os.WriteFile(script, []byte(prog), 0o755); err != nil {
				t.Fatal(err)
			}
			create := fmt.Sprintf(`{"name":%q,"cols":80,"rows":24,"shell":%q`, win, script)
			if c.pasteMode != "" {
				create += fmt.Sprintf(`,"pasteMode":%q`, c.pasteMode)
			}
			if st, _, raw := request(t, sock, "POST", "/sessions/p/windows", create+"}"); st != 200 {
				t.Fatal(raw)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, v, _ := request(t, sock, "GET", "/sessions/p/windows/"+win+"/capture?mode=visible", "")
				if s, _ := v["output"].(string); strings.Contains(s, "ready") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("program never started")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if st, _, raw := request(t, sock, "POST", "/sessions/p/windows/"+win+"/paste", c.body); st != 200 {
				t.Fatal(raw)
			}
			var got []byte
			for deadline = time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if got, _ = os.ReadFile(out); len(got) >= len(c.want) {
					break
				}
			}
			if string(got) != c.want {
				t.Fatalf("program read %q, want %q", got, c.want)
			}
		})
	}
}

// A program that doesn't read its input doesn't wedge the window over HTTP:
// input returns at once, a full queue is 429, and DELETE still works. An
// oversized body is 413.
func TestInputDoesNotWedgeWindow(t *testing.T) {
	sock := bootWith(t, func(st *State) {
		st.InputQueueLimit = 1 << 20
		st.MaxInputBody = 2 << 20
	})
	script := filepath.Join(t.TempDir(), "noread.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nstty raw -echo\nprintf ready\nexec sleep 1000\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	timed := func(want int, method, path, body string) string {
		t.Helper()
		start := time.Now()
		st, _, raw := request(t, sock, method, path, body)
		if st != want {
			t.Fatalf("%s %s: status %d want %d: %.200s", method, path, st, want, raw)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s %s took %v", method, path, d)
		}
		return raw
	}
	timed(200, "POST", "/sessions", `{"name":"s"}`)
	timed(200, "POST", "/sessions/s/windows", fmt.Sprintf(`{"name":"w","cols":80,"rows":24,"shell":%q}`, script))
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, v, _ := request(t, sock, "GET", "/sessions/s/windows/w/capture?mode=visible", "")
		if out, _ := v["output"].(string); strings.Contains(out, "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("program never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := strings.Repeat("x", 700_000)
	timed(200, "POST", "/sessions/s/windows/w/paste", fmt.Sprintf(`{"text":%q,"mode":"raw"}`, text))
	if raw := timed(429, "POST", "/sessions/s/windows/w/paste", fmt.Sprintf(`{"text":%q,"mode":"raw"}`, text)); raw != `{"error":"input queue full"}` {
		t.Fatalf("429 body %s", raw)
	}
	timed(200, "POST", "/sessions/s/windows/w/raw-key", `{"key":"\u0003"}`)
	timed(413, "POST", "/sessions/s/windows/w/keys", fmt.Sprintf(`{"keys":%q}`, strings.Repeat("y", 3<<20)))
	timed(200, "DELETE", "/sessions/s/windows/w", "")
}

// ── attach ───────────────────────────────────────────────────────────────

func openWindow(t *testing.T, sock, session string) {
	t.Helper()
	if st, _, raw := request(t, sock, "POST", "/sessions", fmt.Sprintf(`{"name":%q}`, session)); st != 200 {
		t.Fatal(raw)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/"+session+"/windows", `{"name":"w","cols":80,"rows":24,"shell":"/bin/sh"}`); st != 200 {
		t.Fatal(raw)
	}
}

func openWS(t *testing.T, sock, session string) *ws.Conn {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ws.Client(nc, "shelld", "/sessions/"+session+"/windows/w/attach")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type msg struct {
	op   ws.Opcode
	data []byte
	err  error
}

// reader pumps a connection into a channel so tests can wait with timeouts.
func reader(c *ws.Conn) <-chan msg {
	ch := make(chan msg, 1024)
	go func() {
		for {
			op, data, err := c.ReadMessage()
			ch <- msg{op, data, err}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	return ch
}

func next(ch <-chan msg, d time.Duration) (msg, bool) {
	select {
	case m, ok := <-ch:
		return m, ok
	case <-time.After(d):
		return msg{}, false
	}
}

func resizeCtl(c, r int) []byte {
	return fmt.Appendf(nil, "\x01{\"resize\":{\"cols\":%d,\"rows\":%d}}", c, r)
}

func attachAt(t *testing.T, sock, session string, cols, rows int) (*ws.Conn, <-chan msg) {
	t.Helper()
	c := openWS(t, sock, session)
	if err := c.WriteMessage(ws.OpText, resizeCtl(cols, rows)); err != nil {
		t.Fatal(err)
	}
	ch := reader(c)
	if m, ok := next(ch, 2*time.Second); !ok || m.op != ws.OpBinary {
		t.Fatalf("expected initial binary frame, got %+v", m)
	}
	return c, ch
}

func effectiveSize(t *testing.T, sock, session string) [2]int {
	_, v, _ := request(t, sock, "GET", "/sessions/"+session+"/windows", "")
	w := v["windows"].([]any)[0].(map[string]any)
	return [2]int{int(w["cols"].(float64)), int(w["rows"].(float64))}
}

func waitSize(t *testing.T, sock, session string, want [2]int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := effectiveSize(t, sock, session)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("effective size %v did not converge to %v", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAttachHandshakeHelloAndInput(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c := openWS(t, sock, "s")
	ch := reader(c)
	if m, ok := next(ch, 150*time.Millisecond); ok {
		t.Fatalf("server sent data before the size handshake: %+v", m)
	}
	_ = c.WriteMessage(ws.OpText, resizeCtl(120, 40))
	m, ok := next(ch, 2*time.Second)
	if !ok || m.op != ws.OpBinary || !bytes.HasPrefix(m.data, []byte("\x1b[?25l\x1b[2J")) {
		t.Fatalf("first frame: %+v", m)
	}
	m, ok = next(ch, 2*time.Second)
	// This client declared 120x40 and the effective size is 120x40, so the geometry
	// here does not say which of the two the frame carries; the assertion below is
	// the byte shape only. TestTheHelloAnnouncesTheFlooredSizeNotTheDeclaredOne is
	// the arm where they differ.
	if !ok || m.op != ws.OpText || string(m.data) != "\x01{\"status\":{\"offset\":0,\"max\":0,"+
		"\"paused\":false,\"alt\":false,\"cols\":120,\"rows\":40}}" {
		t.Fatalf("hello: %q", m.data)
	}
	_, v, _ := request(t, sock, "GET", "/sessions/s/windows/w/frame", "")
	if v["cols"] != 120.0 || v["rows"] != 40.0 {
		t.Fatalf("handshake size not applied: %v", v)
	}

	// A frame whose drawing exceeds shelld.DefaultSyncMin bytes is wrapped in
	// DEC 2026; a smaller one, with at most a cursor move and visibility
	// change, is sent bare.
	for _, tc := range []struct {
		input, marker string
		wrapped       bool
	}{
		{"printf hello-go\n", "hello-go", false},
		{"printf '%0300d\\n' 7\n", "00000007", true}, // 300 digits, 3 rows
	} {
		_ = c.WriteMessage(ws.OpBinary, []byte(tc.input))
		found := false
		for deadline := time.Now().Add(3 * time.Second); !found && time.Now().Before(deadline); {
			m, ok := next(ch, 200*time.Millisecond)
			if !ok || m.op != ws.OpBinary || !bytes.Contains(m.data, []byte(tc.marker)) {
				continue
			}
			found = true
			wrapped := bytes.HasPrefix(m.data, dec2026Begin) && bytes.HasSuffix(m.data, dec2026End)
			if wrapped != tc.wrapped {
				t.Fatalf("frame with %q: wrapped %v, want %v: %q", tc.input, wrapped, tc.wrapped, m.data)
			}
			if !wrapped && len(m.data) > shelld.DefaultSyncMin+len("\x1b[24;120H\x1b[?25h") {
				t.Fatalf("bare frame of %d bytes: %q", len(m.data), m.data)
			}
		}
		if !found {
			t.Fatalf("no frame with the output of %q", tc.input)
		}
	}
}

func TestAttachSilentWhenIdleAndRepaintOnResize(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	until := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(until) {
		next(ch, 100*time.Millisecond)
	}
	frames := 0
	until = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(until) {
		if m, ok := next(ch, 50*time.Millisecond); ok && m.op == ws.OpBinary {
			frames++
		}
	}
	if frames != 0 {
		t.Fatalf("expected silence, got %d frames", frames)
	}
	_ = c.WriteMessage(ws.OpText, resizeCtl(100, 30))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpBinary && bytes.Contains(m.data, []byte("\x1b[2J")) {
			return
		}
	}
	t.Fatal("no full repaint after resize")
}

func TestAttachMultiClientGeometry(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	a, _ := attachAt(t, sock, "s", 100, 30)
	waitSize(t, sock, "s", [2]int{100, 30})
	b, _ := attachAt(t, sock, "s", 80, 20)
	waitSize(t, sock, "s", [2]int{80, 20})
	b.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	a.Close()
	time.Sleep(100 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("no attachers should freeze size, got %v", got)
	}

	// Storm: no ghost attachers.
	base, _ := attachAt(t, sock, "s", 120, 40)
	defer base.Close()
	waitSize(t, sock, "s", [2]int{120, 40})
	for range 5 {
		x, _ := attachAt(t, sock, "s", 100, 30)
		y, _ := attachAt(t, sock, "s", 80, 20)
		waitSize(t, sock, "s", [2]int{80, 20})
		x.Close()
		y.Close()
		waitSize(t, sock, "s", [2]int{120, 40})
	}

	// Pin beats the min; re-pinning at the min follows it down.
	request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":150,"rows":50}`)
	waitSize(t, sock, "s", [2]int{150, 50})
	request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":100,"rows":30}`)
	waitSize(t, sock, "s", [2]int{120, 40})
}

func TestAttachBinaryFirstKeepsCurrentSize(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	peer, _ := attachAt(t, sock, "s", 80, 20)
	defer peer.Close()
	waitSize(t, sock, "s", [2]int{80, 20})
	c := openWS(t, sock, "s")
	_ = c.WriteMessage(ws.OpBinary, []byte{})
	time.Sleep(200 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{80, 20} {
		t.Fatalf("binary-first attach reflowed peers: %v", got)
	}
}

func TestAttachScrollModeStatusFrames(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	_ = c.WriteMessage(ws.OpBinary, []byte("i=0; while [ $i -lt 200 ]; do echo line$i; i=$((i+1)); done\n"))
	// Wait until history accumulates (status max grows).
	var maxOff float64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && maxOff < 100 {
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpText {
			var st struct{ Status map[string]any }
			_ = json.Unmarshal(m.data[1:], &st)
			maxOff = st.Status["max"].(float64)
		}
	}
	if maxOff < 100 {
		t.Fatalf("scrollback never grew: %v", maxOff)
	}
	_ = c.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-10}}"))
	sawPaused := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sawPaused {
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpText && bytes.Contains(m.data, []byte(`"offset":10,`)) && bytes.Contains(m.data, []byte(`"paused":true`)) {
			sawPaused = true
		}
	}
	if !sawPaused {
		t.Fatal("no paused status after scroll delta")
	}
	_ = c.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"exit\"}}"))
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpText && bytes.Contains(m.data, []byte(`"paused":false`)) {
			return
		}
	}
	t.Fatal("no live status after scroll exit")
}

// The status frame says when the alternate screen is active, so the client
// can hand scroll keys to the full-screen application.
func TestAttachStatusReportsAltScreen(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	want := func(sub string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpText && bytes.Contains(m.data, []byte(sub)) {
				return
			}
		}
		t.Fatalf("no status frame with %s", sub)
	}
	_ = c.WriteMessage(ws.OpBinary, []byte("printf '\\033[?1049h'; sleep 1; printf '\\033[?1049l'\n"))
	want(`"alt":true`)
	want(`"alt":false`)
}

func TestArrowInput(t *testing.T) {
	if got := string(arrowInput(-2, false)); got != "\x1b[A\x1b[A" {
		t.Fatalf("up: %q", got)
	}
	if got := string(arrowInput(1, true)); got != "\x1bOB" {
		t.Fatalf("app cursor down: %q", got)
	}
	if got := arrowInput(-100, false); len(got) != 8*3 {
		t.Fatalf("cap: %d bytes", len(got))
	}
}

// The wheel scrolls a pager: on the alternate screen without mouse reporting
// a scroll delta arrives as arrow keys.
func TestAttachWheelSendsArrowsToPager(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	// On the alternate screen, read six raw input bytes and print them as hex
	// once back on the primary screen.
	out := filepath.Join(t.TempDir(), "keys")
	_ = c.WriteMessage(ws.OpBinary, fmt.Appendf(nil,
		"printf '\\033[?1049h'; stty raw -echo; od -An -tx1 -N6 > %s; stty sane; printf '\\033[?1049l'; cat %s\n", out, out))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpText && bytes.Contains(m.data, []byte(`"alt":true`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never entered the alternate screen")
		}
	}
	time.Sleep(200 * time.Millisecond) // let stty raw take effect
	_ = c.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-2}}"))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && strings.Contains(hexBytes(b), "1b 5b 41 1b 5b 41") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(out)
	t.Fatalf("arrow keys never reached the program: %q", b)
}

// hexBytes is od's output with its column padding collapsed to one space per
// byte, so a byte sequence can be looked for in it.
//
// od's spacing is not portable and the bytes are: BSD od pads each column to
// three characters and indents the line, so macOS wrote
// "           1b  5b  41  1b  5b  41" where coreutils writes " 1b 5b 41 1b 5b 41".
// Matching the raw text made this test fail on macOS with "arrow keys never
// reached the program" while printing, in that same message, the arrow keys that
// had reached it.
func hexBytes(b []byte) string {
	return strings.Join(strings.Fields(string(b)), " ")
}

// A paused view stays on the lines it shows while output pushes more lines
// into history: the offset follows the content instead of the tail.
func TestAttachPausedViewStaysAnchored(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	client, p := vt.NewTerm(vt.Config{}, 80, 24, nil), vt.NewProcessor()
	var status struct {
		Offset, Max int
		Paused      bool
	}
	// pump applies frames to the client screen until cond holds.
	pump := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if m, ok := next(ch, 100*time.Millisecond); ok {
				if m.op == ws.OpBinary {
					p.Advance(client, m.data)
				} else if m.op == ws.OpText {
					var st struct{ Status json.RawMessage }
					_ = json.Unmarshal(m.data[1:], &st)
					_ = json.Unmarshal(st.Status, &status)
				}
			}
			if cond() {
				return
			}
		}
		t.Fatalf("%s: status %+v", what, status)
	}
	topRow := func() string {
		var sb strings.Builder
		for col := range 80 {
			sb.WriteRune(client.Grid().Row(0).Cell(col).Rune())
		}
		return strings.TrimSpace(sb.String())
	}

	_ = c.WriteMessage(ws.OpBinary, []byte("i=0; while [ $i -lt 200 ]; do echo line$i; i=$((i+1)); done; echo done-a\n"))
	pump("first batch", func() bool { return status.Max >= 176 })
	time.Sleep(300 * time.Millisecond)
	pump("settle", func() bool { return true })

	_ = c.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-50}}"))
	pump("pause", func() bool { return status.Paused && status.Offset == 50 })
	time.Sleep(200 * time.Millisecond)
	pump("paused frame", func() bool { return true })
	before, maxBefore := topRow(), status.Max
	if !strings.HasPrefix(before, "line") {
		t.Fatalf("paused top row %q", before)
	}

	_ = c.WriteMessage(ws.OpBinary, []byte("i=0; while [ $i -lt 30 ]; do echo more$i; i=$((i+1)); done\n"))
	pump("second batch", func() bool { return status.Max > maxBefore && status.Offset == 50+status.Max-maxBefore })
	time.Sleep(300 * time.Millisecond)
	pump("settle", func() bool { return true })
	if !status.Paused || status.Offset != 50+status.Max-maxBefore {
		t.Fatalf("offset did not follow the content: %+v (max before %d)", status, maxBefore)
	}
	if got := topRow(); got != before {
		t.Fatalf("paused view moved: top row %q, was %q", got, before)
	}
}

func TestAttachClosesWhenShellExits(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	c, ch := attachAt(t, sock, "s", 80, 24)
	_ = c.WriteMessage(ws.OpBinary, []byte("exit\n"))
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return
			}
			var ce *ws.CloseError
			if m.err != nil && (errors.As(m.err, &ce) || errors.Is(m.err, io.EOF)) {
				return
			}
		case <-deadline:
			t.Fatal("attach socket stayed open after the shell exited")
		}
	}
}

func TestConcurrentHealthClients(t *testing.T) {
	sock := boot(t)
	var wg sync.WaitGroup
	start := time.Now()
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Probe(sock, time.Second); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if time.Since(start) > time.Second {
		t.Fatalf("too slow: %v", time.Since(start))
	}
}

func TestLifecycle(t *testing.T) {
	dir := t.TempDir()
	if _, err := Probe(filepath.Join(dir, "dead.sock"), 200*time.Millisecond); err == nil {
		t.Fatal("probe of a dead address must fail")
	}
	p := filepath.Join(dir, "shelld.pid")
	if err := WriteLockfile(p, LockInfo{Pid: 12345, Socket: "/tmp/anywhere.sock", Version: "1", StartedAt: 42}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "{\n  \"pid\": 12345,\n  \"socket\": \"/tmp/anywhere.sock\",\n  \"version\": \"1\",\n  \"startedAt\": 42\n}" {
		t.Fatalf("lockfile format: %q", raw)
	}
	info, err := ReadLockfile(p)
	if err != nil || info.Pid != 12345 || info.StartedAt != 42 {
		t.Fatalf("read: %+v %v", info, err)
	}
	if err := removeRegularFile(p); err != nil {
		t.Fatal(err)
	}
	if info, err := ReadLockfile(p); info != nil || err != nil {
		t.Fatal("removed lockfile must read as absent")
	}
}

func TestParseCtrlAndHelpers(t *testing.T) {
	if c, ok := parseCtrl([]byte("\x01{\"resize\":{\"cols\":120,\"rows\":40}}")); !ok || *c.resize != (resizeMsg{120, 40}) {
		t.Fatal("resize")
	}
	for _, bad := range []string{`{"resize":{"cols":120,"rows":40}}`, "\x01not-json", "", "\x01{\"resize\":{\"cols\":-1,\"rows\":1}}"} {
		if c, ok := parseCtrl([]byte(bad)); ok && c.resize != nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if c, ok := parseCtrl([]byte("\x01{\"other\":1}")); !ok || c.resize != nil || c.scroll != nil {
		t.Fatal("unknown field tolerated as no-op")
	}
	c, ok := parseCtrl([]byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-5},\"resize\":{\"cols\":80,\"rows\":24}}"))
	if !ok || c.scroll.op != "delta" || c.scroll.lines != -5 || c.resize == nil {
		t.Fatal("both fields")
	}
	if string(wheelInput(-1, 80, 24, true)) != "\x1b[<64;40;12M" || string(wheelInput(1, 80, 24, true)) != "\x1b[<65;40;12M" {
		t.Fatal("sgr wheel")
	}
	if !bytes.Equal(wheelInput(-1, 80, 24, false), []byte{0x1b, '[', 'M', 0x60, 0x48, 0x2c}) {
		t.Fatal("x10 wheel")
	}
	if string(wheelInput(-100, 80, 24, true)) != strings.Repeat("\x1b[<64;40;12M", 8) || string(wheelInput(1, 0, 0, true)) != "\x1b[<65;1;1M" {
		t.Fatal("wheel caps")
	}
	sm := func(op string, lines int32, offset uint32) *scrollMsg {
		return &scrollMsg{op: op, lines: lines, hasLines: true, offset: offset, hasOffset: true}
	}
	if _, ok := applyScroll(clientMode{}, sm("enter", 0, 0), 0, false, 80, 24); ok {
		t.Fatal("enter with no history")
	}
	// The same op on a window that has history, which is what produces the tail case
	// backToLive returns a client on: a client can be paused at offset 0, so that
	// case is reachable rather than only defensive.
	if m, ok := applyScroll(clientMode{}, sm("enter", 0, 0), 1000, false, 80, 24); !ok || !m.paused || m.offset != 0 {
		t.Fatalf("enter at the tail with history: mode %+v ok %t", m, ok)
	}
	if m, _ := applyScroll(clientMode{}, sm("delta", -3, 0), 1000, false, 80, 24); !m.paused || m.offset != 3 || m.frozenCols != 80 {
		t.Fatal("delta back")
	}
	if m, _ := applyScroll(clientMode{true, 50, 80, 24}, sm("delta", -9999, 0), 100, false, 80, 24); m.offset != 100 {
		t.Fatal("clamp")
	}
	if m, _ := applyScroll(clientMode{true, 3, 80, 24}, sm("delta", 10, 0), 1000, false, 80, 24); m.paused {
		t.Fatal("forward to zero exits")
	}
	if _, ok := applyScroll(clientMode{}, sm("delta", -3, 0), 1000, true, 80, 24); ok {
		t.Fatal("alt screen suppressed")
	}
}

func TestReleaserCoalesces(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time
	r := &releaser{delay: 20 * time.Millisecond, minInterval: 150 * time.Millisecond, free: func() {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
	}}
	start := time.Now()
	for range 5 {
		r.schedule() // a burst of deletions frees once
	}
	time.Sleep(60 * time.Millisecond)
	r.schedule() // soon after: waits out the minimum interval
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("free called %d times, want 2", len(calls))
	}
	if d := calls[0].Sub(start); d < 20*time.Millisecond {
		t.Fatalf("first free after %v, before the delay", d)
	}
	if d := calls[1].Sub(calls[0]); d < 150*time.Millisecond {
		t.Fatalf("second free %v after the first, inside the minimum interval", d)
	}
	var nilReleaser *releaser
	nilReleaser.schedule() // States built without NewState
}

// Sizes above shelld.MaxCols×MaxRows, and one column, are refused over HTTP before anything is
// spawned or resized, and an attached client declaring one is clamped.
func TestSizeBoundOverHTTPAndAttach(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	if st, v, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"big","cols":65535,"rows":24,"shell":"/bin/sh"}`); st != 400 || v["error"] != "cols must be at most 1024" {
		t.Fatalf("create 65535 cols: %d %s", st, raw)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"tall","cols":80,"rows":65535,"shell":"/bin/sh"}`); st != 400 {
		t.Fatalf("create 65535 rows: %d %s", st, raw)
	}
	if st, v, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"thin","cols":1,"rows":24,"shell":"/bin/sh"}`); st != 400 || v["error"] != "cols must be at least 2" {
		t.Fatalf("create 1 col: %d %s", st, raw)
	}
	_, v, _ := request(t, sock, "GET", "/sessions/s/windows", "")
	if n := len(v["windows"].([]any)); n != 1 {
		t.Fatalf("refused creates left %d windows", n)
	}
	if st, v, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":65535,"rows":65535}`); st != 400 || v["error"] != "cols must be at most 1024" {
		t.Fatalf("resize 65535: %d %s", st, raw)
	}
	if st, v, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":1,"rows":24}`); st != 400 || v["error"] != "cols must be at least 2" {
		t.Fatalf("resize 1 col: %d %s", st, raw)
	}
	if got := effectiveSize(t, sock, "s"); got != [2]int{80, 24} {
		t.Fatalf("refused resize changed the size to %v", got)
	}
	// No pin yet, so the lone attacher's declared size sets the effective size.
	c, _ := attachAt(t, sock, "s", 65535, 65535)
	defer c.Close()
	waitSize(t, sock, "s", [2]int{1024, 1024})
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":1024,"rows":1024}`); st != 200 {
		t.Fatalf("resize to the maximum: %d %s", st, raw)
	}
}

// A cwd that is not a directory is 400 naming the path; an existing window
// name is still 409 first, which a client treats as "attach".
func TestCreateWindowBadCwd(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	missing := filepath.Join(t.TempDir(), "gone")
	st, v, raw := request(t, sock, "POST", "/sessions/s/windows", fmt.Sprintf(`{"name":"new","cwd":%q,"shell":"/bin/sh"}`, missing))
	if msg, _ := v["error"].(string); st != 400 || !strings.Contains(msg, missing) {
		t.Fatalf("missing cwd: %d %s", st, raw)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows", fmt.Sprintf(`{"name":"w","cwd":%q,"shell":"/bin/sh"}`, missing)); st != 409 {
		t.Fatalf("existing name with a bad cwd: %d %s, want 409", st, raw)
	}
}

// A shell that cannot be run is 400 naming it, not the 409 a client
// reads as "already live, attach": each case here is one the request itself
// gets wrong, so no retry of it can succeed. The name-in-use 409 keeps
// "already exists" in its body, which is what those clients match.
func TestCreateWindowUnrunnableShellIs400(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	dir := t.TempDir()
	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Executable, but no #! and no binary header: resolve accepts it, and the
	// execve refuses it with ENOEXEC, so this is the arm that classifies the
	// spawn's errno rather than resolve's own refusal.
	notImage := filepath.Join(dir, "not-image")
	if err := os.WriteFile(notImage, []byte("\x00\x01\x02\x03 not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ name, shell, want string }{
		{"missing", filepath.Join(dir, "gone"), filepath.Join(dir, "gone")},
		{"not executable", notExec, notExec},
		{"not an image", notImage, notImage},
		{"not on PATH", "shelld-no-such-program", "shelld-no-such-program"},
		{"empty", "   ", "empty shell command"},
	} {
		body := fmt.Sprintf(`{"name":"w-%d","shell":%q}`, i, tc.shell)
		st, v, raw := request(t, sock, "POST", "/sessions/s/windows", body)
		if msg, _ := v["error"].(string); st != 400 || !strings.Contains(msg, tc.want) {
			t.Errorf("%s shell: %d %s, want 400 naming %q", tc.name, st, raw, tc.want)
		}
	}
	st, v, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"w","shell":"/bin/sh"}`)
	if msg, _ := v["error"].(string); st != 409 || !strings.Contains(msg, "already exists") {
		t.Fatalf("existing name: %d %s, want 409 saying already exists", st, raw)
	}
}

// TestInjectOnADeadWindowIsRefusedAsKeysRefusesIt: once the window's shell has
// exited and its delivery loop is gone, /inject used to answer 202 and leave
// the payload pending for ever, with nothing left to deliver or expire it.
func TestInjectOnADeadWindowIsRefusedAsKeysRefusesIt(t *testing.T) {
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"w","shell":"/bin/true"}`); st != 200 {
		t.Fatalf("create window: %d %s", st, raw)
	}
	var keysSt int
	var keysRaw string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		keysSt, _, keysRaw = request(t, sock, "POST", "/sessions/s/windows/w/keys", `{"keys":"x"}`)
		if keysSt == 500 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if keysSt != 500 || !strings.Contains(keysRaw, "is dead") {
		t.Fatalf("premise: /keys on the exited window: %d %s, want 500 is dead", keysSt, keysRaw)
	}
	// The delivery loop exits on the window's death, shortly after /keys sees it.
	var st int
	var raw string
	for time.Now().Before(deadline) {
		st, _, raw = request(t, sock, "POST", "/sessions/s/windows/w/inject", `{"payload":"echo hi"}`)
		if st != 202 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st != 500 || raw != keysRaw {
		t.Fatalf("/inject on a dead window: %d %s, want /keys's 500 %s", st, raw, keysRaw)
	}
}
