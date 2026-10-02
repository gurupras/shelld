//go:build linux || darwin

package daemon

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws"
)

// screen is a client-side emulator: it applies the binary frames a daemon
// sends to a vt.Term of the client's declared size, so a test can assert what
// the user actually sees instead of only the arithmetic behind it.
type screen struct {
	t    *testing.T
	term *vt.Term
	p    *vt.Processor
	ch   <-chan msg
	// status is the most recent status frame, kept because it is the only thing
	// that says what state the daemon thinks this client is in. A test that acts
	// on a mode change needs that witness: the screen alone cannot distinguish a
	// scroll that has not been processed yet from one that changed nothing.
	status     []byte
	cols, rows int
}

func newScreen(t *testing.T, ch <-chan msg, cols, rows int) *screen {
	return &screen{t: t, term: vt.NewTerm(vt.Config{}, cols, rows, nil), p: vt.NewProcessor(), ch: ch, cols: cols, rows: rows}
}

// step applies one frame that arrives within d; a status frame is recorded
// rather than drawn.
func (s *screen) step(d time.Duration) bool {
	m, ok := next(s.ch, d)
	switch {
	case !ok:
	case m.op == ws.OpBinary:
		s.p.Advance(s.term, m.data)
	case m.op == ws.OpText:
		s.status = m.data
	}
	return ok
}

// drain applies every frame that arrives within d.
func (s *screen) drain(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && s.step(time.Until(deadline)) {
	}
}

// await applies frames until cond holds or the deadline passes.
func (s *screen) await(what string, d time.Duration, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if !time.Now().Before(deadline) {
			s.t.Fatalf("%s: timed out; screen:\n%s", what, strings.Join(s.text(), "\n"))
		}
		s.step(100 * time.Millisecond)
	}
}

// text is the visible screen, one string per row, trailing blanks trimmed.
func (s *screen) text() []string {
	g := s.term.Grid()
	out := make([]string, 0, s.rows)
	for r := range min(s.rows, g.ScreenLines()) {
		var sb strings.Builder
		for c := range min(s.cols, g.Columns()) {
			sb.WriteRune(g.Row(r).Cell(c).Rune())
		}
		out = append(out, strings.TrimRight(sb.String(), " \x00"))
	}
	return out
}

// runs reports the length of every maximal run of ch that is at least 2 long,
// per row. A run identifies the marker line the test printed without matching
// the echoed command that printed it.
func (s *screen) runs(ch rune) []int {
	var out []int
	for _, row := range s.text() {
		n := 0
		for _, r := range row {
			if r == ch {
				n++
				continue
			}
			if n >= 2 {
				out = append(out, n)
			}
			n = 0
		}
		if n >= 2 {
			out = append(out, n)
		}
	}
	return out
}

func (s *screen) contains(sub string) bool {
	return strings.Contains(strings.Join(s.text(), "\n"), sub)
}

// attachObserverAt attaches with ?observer=1 and the given declared size.
func attachObserverAt(t *testing.T, sock, session string, cols, rows int) (*ws.Conn, <-chan msg) {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ws.Client(nc, "shelld", "/sessions/"+session+"/windows/w/attach?observer=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.WriteMessage(ws.OpText, resizeCtl(cols, rows)); err != nil {
		t.Fatal(err)
	}
	ch := reader(c)
	if m, ok := next(ch, 2*time.Second); !ok || m.op != ws.OpBinary {
		t.Fatalf("expected initial binary frame, got %+v", m)
	}
	return c, ch
}

// marker prints a line of n '0' characters. The command text itself has no run
// of '0' longer than one, so a run of zeros on the screen is output, never the
// echoed command.
func marker(n int) []byte {
	return fmt.Appendf(nil, "printf '%%0%dd\\n' 0\n", n)
}

// A watcher attached read-only must not shrink the agent's grid and must not
// be able to type into the agent's program.
func TestAttachObserverNeitherVotesNorTypes(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	agent := newScreen(t, agentCh, 100, 30)

	obsConn, obsCh := attachObserverAt(t, sock, "s", 40, 12)
	defer obsConn.Close()
	obs := newScreen(t, obsCh, 40, 12)

	// (1) The observer does not vote: the grid stays at the agent's size.
	time.Sleep(300 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("observer voted on the shared geometry: effective size %v, want [100 30]", got)
	}

	// (2) Known output: a 90-column line the agent's 100-column grid holds on
	// one row and a 40-column grid would wrap over three.
	_ = agentConn.WriteMessage(ws.OpBinary, marker(90))
	agent.await("agent marker line", 5*time.Second, func() bool {
		for _, n := range agent.runs('0') {
			if n >= 20 {
				return true
			}
		}
		return false
	})
	agent.drain(300 * time.Millisecond)
	if got := agent.runs('0'); len(got) != 1 || got[0] != 90 {
		t.Fatalf("agent screen shows zero-runs %v, want exactly one run of 90; screen:\n%s",
			got, strings.Join(agent.text(), "\n"))
	}

	// (3) The observer sees a correct crop: the same line truncated to its own
	// 40 columns, on one row, not reflowed.
	obs.await("observer crop", 5*time.Second, func() bool { return len(obs.runs('0')) > 0 })
	obs.drain(300 * time.Millisecond)
	if got := obs.runs('0'); len(got) != 1 || got[0] != 40 {
		t.Fatalf("observer screen shows zero-runs %v, want exactly one run of 40; screen:\n%s",
			got, strings.Join(obs.text(), "\n"))
	}

	// (3b) An observer's own resize still re-crops its view — and still does not
	// move the shared grid.
	_ = obsConn.WriteMessage(ws.OpText, resizeCtl(70, 20))
	wide := newScreen(t, obsCh, 70, 20)
	wide.await("observer re-crop", 5*time.Second, func() bool {
		for _, n := range wide.runs('0') {
			if n == 70 {
				return true
			}
		}
		return false
	})
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("observer resize moved the shared grid to %v", got)
	}

	// (4) An observer's binary frame must not reach the program. Send it, then
	// send a fence from the agent and wait for the fence: if the observer's
	// bytes were going to be written, they were written before the fence.
	_ = obsConn.WriteMessage(ws.OpBinary, []byte("echo OBSERVER-TYPED\n"))
	_ = obsConn.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-3}}"))
	time.Sleep(200 * time.Millisecond)
	_ = agentConn.WriteMessage(ws.OpBinary, []byte("echo FENCE-OK\n"))
	agent.await("fence", 5*time.Second, func() bool { return agent.contains("FENCE-OK") })
	agent.drain(300 * time.Millisecond)
	if agent.contains("OBSERVER-TYPED") {
		t.Fatalf("observer input reached the program; agent screen:\n%s", strings.Join(agent.text(), "\n"))
	}
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("effective size drifted to %v after observer traffic", got)
	}
}

// The handshake's binary-first path must not write an observer's first frame to
// the program either, and ?observer=0 must still be a normal client: a flag
// that reads as "on" for any value would silently mute a real user.
func TestAttachObserverHandshakeAndFlagParsing(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	agentConn, agentCh := attachAt(t, sock, "s", 100, 30)
	defer agentConn.Close()
	waitSize(t, sock, "s", [2]int{100, 30})
	agent := newScreen(t, agentCh, 100, 30)

	// Binary-first observer attach: the deferred first frame is not input.
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := ws.Client(nc, "shelld", "/sessions/s/windows/w/attach?observer=1")
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()
	_ = obs.WriteMessage(ws.OpBinary, []byte("echo HANDSHAKE-LEAK\n"))

	// observer=0 is a normal client: it votes and it types.
	nc2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ws.Client(nc2, "shelld", "/sessions/s/windows/w/attach?observer=0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	_ = plain.WriteMessage(ws.OpText, resizeCtl(60, 18))
	waitSize(t, sock, "s", [2]int{60, 18})
	_ = plain.WriteMessage(ws.OpBinary, []byte("echo PLAIN-TYPED\n"))

	agent.await("plain client typed", 5*time.Second, func() bool { return agent.contains("PLAIN-TYPED") })
	agent.drain(300 * time.Millisecond)
	if agent.contains("HANDSHAKE-LEAK") {
		t.Fatalf("observer's binary handshake frame reached the program; screen:\n%s", strings.Join(agent.text(), "\n"))
	}
}

// The min over attachers must never be taken over an empty set: a window whose
// only clients are observers keeps its size, exactly as one with no clients
// does, instead of collapsing to the 20x5 floor.
func TestAttachAllObserversFreezeSize(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	// Observer-only from the start: the window keeps its creation size.
	obs, _ := attachObserverAt(t, sock, "s", 40, 12)
	time.Sleep(300 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{80, 24} {
		t.Fatalf("observer-only attach resized the window to %v, want [80 24]", got)
	}

	// A voter arrives, sizes the window, and leaves: the size freezes where the
	// voter left it, not at the observer's size and not at the floor.
	agent, _ := attachAt(t, sock, "s", 100, 30)
	waitSize(t, sock, "s", [2]int{100, 30})
	agent.Close()
	time.Sleep(300 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{100, 30} {
		t.Fatalf("size after the only voter left: %v, want the frozen [100 30]", got)
	}

	// A pin still applies with observers attached.
	request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":150,"rows":50}`)
	waitSize(t, sock, "s", [2]int{150, 50})
	obs.Close()
	time.Sleep(200 * time.Millisecond)
	if got := effectiveSize(t, sock, "s"); got != [2]int{150, 50} {
		t.Fatalf("size after the observer left: %v, want [150 50]", got)
	}
}
