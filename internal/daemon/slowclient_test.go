//go:build linux || darwin

package daemon

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws"
)

// replayClient is an attach client that replays every frame into its own
// terminal, optionally pausing delay after each message.
type replayClient struct {
	c    *ws.Conn
	mu   sync.Mutex
	term *vt.Term
	p    *vt.Processor
	n    int
	err  error
	done chan struct{}
}

func attachReplay(t *testing.T, sock, session string, cols, rows int, delay time.Duration) *replayClient {
	t.Helper()
	c := openWS(t, sock, session)
	if err := c.WriteMessage(ws.OpText, resizeCtl(cols, rows)); err != nil {
		t.Fatal(err)
	}
	rc := &replayClient{
		c: c, term: vt.NewTerm(vt.Config{}, cols, rows, nil), p: vt.NewProcessor(),
		done: make(chan struct{}),
	}
	go func() {
		defer close(rc.done)
		for {
			op, data, err := c.ReadMessage()
			if err != nil {
				rc.mu.Lock()
				rc.err = err
				rc.mu.Unlock()
				return
			}
			rc.mu.Lock()
			if op == ws.OpBinary {
				rc.p.Advance(rc.term, data)
				rc.n++
			}
			rc.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}()
	return rc
}

func (rc *replayClient) frames() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.n
}

func (rc *replayClient) failed() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.err
}

// screen is the visible grid as text, rows trimmed on the right.
func (rc *replayClient) screen() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	g := rc.term.Grid()
	var sb strings.Builder
	for line := range g.ScreenLines() {
		row := g.Row(line)
		var s strings.Builder
		for col := range g.Columns() {
			s.WriteRune(row.Cell(col).Rune())
		}
		sb.WriteString(strings.TrimRight(s.String(), " \x00"))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// runShell types a command into the window.
func runShell(t *testing.T, sock, cmd string) {
	t.Helper()
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/key-names",
		fmt.Sprintf(`{"names":[%q,"Enter"]}`, cmd)); st != 200 {
		t.Fatal(raw)
	}
}

// quiesce waits until the healthy client stops receiving frames.
func quiesce(rc *replayClient, limit time.Duration) {
	stop := time.Now().Add(limit)
	for last := -1; time.Now().Before(stop); {
		n := rc.frames()
		if n == last {
			return
		}
		last = n
		time.Sleep(500 * time.Millisecond)
	}
}

// serverScreen renders the window's live grid at the effective size and
// returns it as text, for comparison with a client's replayed screen.
func serverScreen(t *testing.T, sock string) string {
	t.Helper()
	_, v, _ := request(t, sock, "GET", "/sessions/s/windows/w/frame", "")
	cols, rows := int(v["cols"].(float64)), int(v["rows"].(float64))
	rc := &replayClient{term: vt.NewTerm(vt.Config{}, cols, rows, nil), p: vt.NewProcessor()}
	rc.p.Advance(rc.term, []byte(v["frame"].(string)))
	return rc.screen()
}

// A client that reads at half the frame cadence stays attached through a
// sustained flood, keeps its geometry vote, and ends with the server's screen.
//
// This is the other half of the write bound, and the half a bound cannot supply
// on its own: TestAClientThatStopsReadingIsDroppedRatherThanPinningEveryOtherClient
// says a client that has stopped reading is dropped, and every way of satisfying
// it more eagerly -- a shorter deadline, a deadline set once per connection
// rather than per write -- also drops a client that is merely behind. So the
// bound is only worth what the distinction between "slow" and "stopped" is worth,
// and that distinction is what is asserted here.
//
// Which is why the arms are what they are. Still attached is the disconnection.
// The geometry vote held at 200x50 for the whole flood is the consequence a user
// would see if the drop happened anyway -- the other clients' grids snapping to
// a size nobody asked for. And the screen comparison after the catch-up is the
// last thing a dropped-and-reattached client would pass by accident: a client
// that lost frames mid-flood renders a screen the server's grid does not hold.
//
// The bound it covers is the shipped default, not a value this test sets, so it
// is the one test in the suite that a tightening of that default reaches: with
// defaultWSWriteTimeout at 100ms, internal/daemon and ws both stay green except
// here.
func TestSlowButAliveClientStaysCorrect(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	fast := attachReplay(t, sock, "s", 300, 60, 0)
	defer fast.c.Close()
	waitSize(t, sock, "s", [2]int{300, 60})
	// 66 ms per message is half the 33 ms frame cadence, so this client falls
	// behind, fills its socket buffer and puts the server's writes into real
	// backpressure — but it always makes progress.
	slow := attachReplay(t, sock, "s", 200, 50, 66*time.Millisecond)
	defer slow.c.Close()
	waitSize(t, sock, "s", [2]int{200, 50})

	runShell(t, sock, `awk 'BEGIN{for(i=0;;i++) printf "%d-%0*d\n", i, 190, i}'`)
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		if got := effectiveSize(t, sock, "s"); got != [2]int{200, 50} {
			t.Fatalf("slow client lost its geometry vote mid-flood: %v", got)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := slow.failed(); err != nil {
		t.Fatalf("slow client was disconnected mid-flood: %v", err)
	}
	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/key-names", `{"names":["C-c"]}`); st != 200 {
		t.Fatal(raw)
	}
	// Let the slow client catch up, then compare its screen with the server's.
	quiesce(slow, 90*time.Second)
	time.Sleep(time.Second)
	if err := slow.failed(); err != nil {
		t.Fatalf("slow client was disconnected: %v", err)
	}
	want, got := serverScreen(t, sock), slow.screen()
	if got != want {
		t.Fatalf("slow client screen diverged\nslow:\n%s\nserver:\n%s", got, want)
	}
	t.Logf("slow client attached with the server's screen; frames fast=%d slow=%d",
		fast.frames(), slow.frames())
}
