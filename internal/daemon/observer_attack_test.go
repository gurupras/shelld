//go:build linux || darwin

package daemon

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// attachOutcome attaches with an arbitrary query string, declares 40x12, and
// reports how the daemon classified the client. openWindow makes the window
// 80x24, so a client that VOTES drags the effective size down to 40x12 and one
// that does not leaves it at 80x24: the effective size is the observable that
// separates the two without reaching into any internal. A refused handshake is
// reported as "reject" together with the daemon's own message.
func attachOutcome(t *testing.T, sock, session, query string) (outcome, detail string) {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ws.Client(nc, "shelld", "/sessions/"+session+"/windows/w/attach"+query)
	if err != nil {
		_ = nc.Close()
		// Only a 400 counts as a rejection; anything else (a dial error, a
		// bad accept key, a 404) would make the subtest vacuously "correct".
		if !strings.Contains(err.Error(), "400 Bad Request") {
			t.Fatalf("handshake failed for a reason other than 400: %v", err)
		}
		return "reject", strings.TrimPrefix(err.Error(), "websocket: handshake failed: ")
	}
	t.Cleanup(func() { c.Close() })
	if err := c.WriteMessage(ws.OpText, resizeCtl(40, 12)); err != nil {
		t.Fatal(err)
	}
	ch := reader(c)
	if m, ok := next(ch, 2*time.Second); !ok || m.op != ws.OpBinary {
		t.Fatalf("expected initial binary frame, got %+v", m)
	}
	time.Sleep(400 * time.Millisecond)
	switch got := effectiveSize(t, sock, session); got {
	case [2]int{80, 24}:
		return "observer", "effective 80x24, the client did not vote"
	case [2]int{40, 12}:
		return "voting", "effective 40x12, the client voted"
	default:
		t.Fatalf("unexpected effective size %v", got)
		return "", ""
	}
}

// waitAlt reads status frames until one reports alt:true.
func waitAlt(t *testing.T, ch <-chan msg) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := next(ch, time.Until(deadline))
		if !ok {
			break
		}
		if m.op == ws.OpText && strings.Contains(string(m.data), `"alt":true`) {
			return
		}
	}
	t.Fatal("window never entered the alternate screen")
}

// ATTACK 1: the alt-screen wheel/arrow synthesis guard. Their suite only ever
// sends a scroll on the primary screen, so `!t.observer` in handleCtrl is never
// reached by it.
func TestAttackObserverScrollOnAltScreen(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})

	// Enter the alternate screen and run a program that makes every byte it
	// reads visible, so a synthesized arrow key shows up as ^[[A.
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("printf '\\033[?1049h'; cat -v\n"))
	waitAlt(t, agentCh)
	agent := newScreen(t, agentCh, 100, 30)
	agent.drain(500 * time.Millisecond)

	obsConn, _ := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()

	// A wheel scroll on the alternate screen: shelld synthesizes arrow keys
	// (ModeAlternateScroll is on by default) unless the client is an observer.
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-3}}"))
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":5}}"))
	time.Sleep(300 * time.Millisecond)
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("FENCE-ALT\n"))
	agent.await("fence", 5*time.Second, func() bool { return agent.contains("FENCE-ALT") })
	agent.drain(300 * time.Millisecond)
	if agent.contains("^[[A") || agent.contains("^[[B") {
		t.Fatalf("observer's alt-screen scroll synthesized arrow keys into the program; screen:\n%s",
			strings.Join(agent.text(), "\n"))
	}
}

// ATTACK 2: the unrecognized-control-frame drop. Their suite never sends one
// from an observer, so `case observer:` in the read loop is never reached.
func TestAttackObserverUnknownControlFrame(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	agent := newScreen(t, agentCh, 100, 30)

	obsConn, _ := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()
	// The exact frame the spike's own rationale says an old daemon would type
	// into the shell, plus a malformed one and a bare text frame.
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"observer\":true}"))
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"resize\":{\"cols\":\"x\"}}"))
	_ = obsConn.WriteMessage(ws.OpText, []byte("echo CTRL-LEAK\n"))
	time.Sleep(300 * time.Millisecond)
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("echo FENCE-CTRL\n"))
	agent.await("fence", 5*time.Second, func() bool { return agent.contains("FENCE-CTRL") })
	agent.drain(300 * time.Millisecond)
	for _, leak := range []string{"observer", "CTRL-LEAK", "cols"} {
		if agent.contains(leak) {
			t.Fatalf("observer control frame reached the program (%q); screen:\n%s",
				leak, strings.Join(agent.text(), "\n"))
		}
	}
}

// ATTACK 3: flag parsing. This flag decides whether a client can type into the
// program, so a value the daemon cannot parse must not be allowed to decide it
// silently. queryBool used to map every unrecognized value to "off", which
// handed a writable, voting attach to a client that had asked to be a read-only
// watcher — with no error for the client to notice.
//
// The two contracts this table pins against each other: an *unknown parameter*
// stays ignored, so an older daemon or shelld-rs still degrades to a normal
// attach; but a *known parameter with an unparsable value* is refused with 400,
// which is what queryUint's two call sites already do.
//
// Deliberately not a case here: "?observer=1 " with a trailing space. A literal
// space makes an invalid HTTP request line, so the 400 comes from net/http
// before any handler runs and the subtest would prove nothing about the flag.
func TestAttackObserverFlagSpellings(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
		why   string
	}{
		{"?observer=1", "observer", "canonical"},
		{"?observer=true", "observer", "canonical"},
		{"?observer=TRUE", "observer", "the value is case-insensitive"},
		{"?observer", "observer", "bare key, as HTML forms send it"},
		{"?observer=", "observer", "net/url cannot tell this from `?observer`, so it has to mean the same thing"},
		{"?observer=0", "voting", "explicit off"},
		{"?observer=FALSE", "voting", "explicit off, case-insensitive"},
		{"", "voting", "an absent parameter is not an error"},
		{"?somethingelse=1", "voting", "an unknown parameter stays ignored, for version skew"},
		{"?observer=yes", "reject", "unparsable value must not fail open"},
		{"?observer=on", "reject", "ditto"},
		{"?observer=enabled", "reject", "the case that shows widening the accept set never converges"},
		{"?Observer=1", "reject", "a case variant of a key we know is a misspelling, not a newer client's parameter"},

		// The four below were live defects in the first version of this change,
		// found by a review that ran them against a real daemon. Each produced a
		// voting, writable attach for a client that had asked to be a watcher, and
		// each did it silently — no status, nothing for the client to notice.
		{"?observer=1;", "reject", "ParseQuery objects to `;` and URL.Query() discards the error, so this arrived as an EMPTY query"},
		{"?observer%3d1", "reject", "ParseQuery splits on `=` before unescaping, so this is one key named `observer=1`"},
		{"?observer=1&observer=0", "reject", "a repeated key is a contradiction the client never decided"},
		{"?observer=0&observer=1", "reject", "and the outcome must not turn on which order it was sent in"},
		{"?observer=1&observer=1", "reject", "refused even when the two agree: reading a repeat as consensus means parsing both, and `1` vs `true` vs `TRUE` would then have to agree too"},
	} {
		name := tc.query
		if name == "" {
			name = "absent"
		}
		t.Run(strings.TrimPrefix(name, "?"), func(t *testing.T) {
			sock := boot(t)
			openWindow(t, sock, "s")
			got, detail := attachOutcome(t, sock, "s", tc.query)
			t.Logf("%-18s -> %-8s | %s | %s", name, got, detail, tc.why)
			if got != tc.want {
				t.Errorf("%s: got %s, want %s (%s)", name, got, tc.want, tc.why)
			}
		})
	}
}

// ATTACK 4: an observer alone on a window whose shell exits. The socket must
// close like it does for any client.
func TestAttackObserverAloneShellExits(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	obsConn, obsCh := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()

	// Kill the window's program out from under the observer.
	if st, _, raw := request(t, sock, "DELETE", "/sessions/s/windows/w", ""); st != 200 {
		t.Fatal(raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, ok := next(obsCh, time.Until(deadline))
		if !ok {
			t.Fatal("observer socket never closed after the window died")
		}
		if m.err != nil || !ok {
			return // closed
		}
		if time.Now().After(deadline) {
			t.Fatal("observer socket never closed after the window died")
		}
	}
}

// ATTACK 5: many observers plus a voter, all resizing concurrently, while the
// operator pins and unpins. Looking for a torn min, a lock inversion, or a
// window that ends up somewhere the min never allowed.
func TestAttackObserverStorm(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 8 {
		c, _ := attachObserverAt(t, sock, "s", 30+i, 10+i)
		wg.Add(1)
		go func(c *ws.Conn, i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if c.WriteMessage(ws.OpText, resizeCtl(20+(n*7+i)%200, 5+(n*3+i)%60)) != nil {
					return
				}
				_ = c.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-2}}"))
				_ = c.WriteMessage(ws.OpBinary, []byte("echo STORM-TYPED\n"))
				time.Sleep(time.Millisecond)
			}
		}(c, i)
	}
	// The voter resizes too, and the operator pins.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = agentConn.WriteMessage(ws.OpText, resizeCtl(100+n%20, 30+n%10))
			time.Sleep(3 * time.Millisecond)
		}
	}()
	for range 20 {
		request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":150,"rows":50}`)
		time.Sleep(5 * time.Millisecond)
		request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":20,"rows":5}`)
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	// The voter's declared size floor must still hold: with the pin cleared the
	// size follows the single voter, never an observer's 20-column declaration.
	_ = agentConn.WriteMessage(ws.OpText, resizeCtl(111, 33))
	waitSize(t, sock, "s", [2]int{111, 33})
	agent := newScreen(t, agentCh, 111, 33)
	agent.drain(500 * time.Millisecond)
	if agent.contains("STORM-TYPED") {
		t.Fatalf("observer input reached the program under load; screen:\n%s", strings.Join(agent.text(), "\n"))
	}
}

// ATTACK 6: an observer that declares a degenerate size, then scrolls. A
// non-voting size is not protected by the 20x5 effective-size floor.
func TestAttackObserverZeroSize(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, _ := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("printf 'x\\n' ; seq 1 200\n"))
	time.Sleep(500 * time.Millisecond)

	obsConn, obsCh := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()
	for _, ctl := range [][]byte{
		resizeCtl(0, 0),
		[]byte("\x01{\"scroll\":{\"op\":\"enter\"}}"),
		[]byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-50}}"),
		[]byte("\x01{\"scroll\":{\"op\":\"to\",\"offset\":10}}"),
		resizeCtl(0, 5),
		[]byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-1}}"),
		resizeCtl(1024, 1024),
		[]byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-1}}"),
	} {
		if err := obsConn.WriteMessage(ws.OpText, ctl); err != nil {
			t.Fatalf("write %s: %v", ctl, err)
		}
		time.Sleep(80 * time.Millisecond)
	}
	// The daemon must still be alive and the geometry untouched.
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("degenerate observer size moved the shared grid to %v", got)
	}
	// And the observer's socket must still be serving frames.
	_ = obsConn.WriteMessage(ws.OpText, resizeCtl(50, 15))
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("echo ALIVE\n"))
	if _, ok := next(obsCh, 3*time.Second); !ok {
		t.Fatal("observer connection went silent after a degenerate size")
	}
	if st, _, raw := request(t, sock, "GET", "/health", ""); st != 200 {
		t.Fatalf("daemon unhealthy after degenerate observer: %d %s", st, raw)
	}
}

// ATTACK 7: an observer's view must survive the shared grid shrinking under it
// while it is paused in scrollback with a larger declared size.
func TestAttackObserverPausedWhileGridShrinks(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, _ := attachAt(t, sock, "s", 200, 60)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{200, 60})
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("seq 1 300\n"))
	time.Sleep(600 * time.Millisecond)

	obsConn, obsCh := attachObserverAt(t, sock, "s", 180, 50)
	defer obsConn.Close()
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"enter\"}}"))
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-40}}"))
	time.Sleep(200 * time.Millisecond)
	// The voter shrinks the shared grid far below the paused observer's view.
	_ = agentConn.WriteMessage(ws.OpText, resizeCtl(25, 6))
	waitSize(t, sock, "s", [2]int{25, 6})
	time.Sleep(400 * time.Millisecond)
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("echo AFTER-SHRINK\n"))
	obs := newScreen(t, obsCh, 180, 50)
	obs.drain(800 * time.Millisecond)
	if st, _, raw := request(t, sock, "GET", "/health", ""); st != 200 {
		t.Fatalf("daemon unhealthy: %d %s", st, raw)
	}
	fmt.Fprintln(discard, obs.text())
}

var discard = &strings.Builder{}

// ATTACK 8: "?observer=1" is a rendering mode, not access control. The same
// client that opened a read-only attach can still drive the program over HTTP
// on the same socket, and can open a second, writable attach. README says the
// client "is read-only ... nothing it sends reaches the program".
func TestAttackObserverIsNotAccessControl(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	agent := newScreen(t, agentCh, 100, 30)

	obsConn, _ := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()

	// Same socket, same "observer", HTTP instead of the WebSocket.
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/keys", `{"keys":"echo OBS-VIA-KEYS\n"}`); st != 200 {
		t.Fatalf("POST /keys: %d %s", st, raw)
	}
	// And a second attach with no flag votes and types.
	plainConn, _ := attachAt(t, sock, "s", 45, 14)
	defer plainConn.Close()
	_ = plainConn.WriteMessage(ws.OpBinary, []byte("echo OBS-VIA-SECOND-ATTACH\n"))

	agent.await("keys route", 5*time.Second, func() bool { return agent.contains("OBS-VIA-KEYS") })
	agent.await("second attach", 5*time.Second, func() bool { return agent.contains("OBS-VIA-SECOND-ATTACH") })
	got := effectiveSize(t, sock, "s")
	t.Logf("observer attached, yet /keys typed and a second plain attach typed and voted: effective size now %v", got)
	if got != [2]int{45, 14} {
		t.Fatalf("expected the second (voting) attach to size the window, got %v", got)
	}
}
