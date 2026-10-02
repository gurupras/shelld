//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/ws"
)

// The tests here are about a client learning that its keystrokes did not arrive.
// The window refuses input once the program has stopped reading and the queue is
// full, and the read loop has nowhere to put the bytes; what it used to do was
// discard them with `_ = win.WriteInput(data)`, so the client typed into a screen
// that did not change and had no way to tell that from a program thinking about
// it. The bytes cannot be saved. Being told is the whole of the fix, which is why
// what these pin is the telling and not the write.

// dropTicker is a ticker whose typed can be called: a window whose input queue is
// too small to take a paste, and the ctrl queue typed wakes.
//
// The limit is what makes the refusal deterministic without a wedged program. push
// tests the whole chunk against the limit before it looks at what is already
// queued, so a write larger than the limit is refused whatever the program is
// doing — no racing a shell's reads, and no filling a tty buffer first.
func dropTicker(t *testing.T, limit int) (*ticker, *shelld.Window) {
	t.Helper()
	w, err := shelld.NewWindow(shelld.WindowOpts{
		Name:            "drop",
		Command:         []string{"/bin/cat"},
		Cols:            80,
		Rows:            24,
		InputQueueLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	return &ticker{win: w, ctrl: &ctrlQueue{notify: make(chan struct{}, 1)}}, w
}

// countLogs installs a handler counting records whose message is msg, and returns
// how many have arrived. The default logger is restored on cleanup.
func countLogs(t *testing.T, msg string) func() int {
	t.Helper()
	h := &countingHandler{want: msg}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h.count
}

// countingHandler is the minimum slog.Handler that can count one message. It
// locks because the tests that matter here log from several goroutines at once,
// and a counter that raced would be reporting on itself rather than on the latch
// it is there to check.
type countingHandler struct {
	want string
	mu   sync.Mutex
	n    int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.want {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

const msgInputDropped = "dropping client input the window cannot queue"

// A refused write has to be counted, and an accepted one must not be: the count
// is what the client is told, so a count that moved on a write the program got
// would report a loss that never happened.
func TestRefusedClientInputIsCounted(t *testing.T) {
	const limit = 64
	tk, _ := dropTicker(t, limit)

	// Premise: a write within the limit is accepted, so the refusals below are
	// the limit doing its work and not a window that takes nothing at all.
	tk.typed([]byte("ls\n"))
	if got := tk.droppedInput.Load(); got != 0 {
		t.Fatalf("an accepted write of 3 bytes counted %d dropped", got)
	}

	first := bytes.Repeat([]byte("x"), 4096)
	tk.typed(first)
	if got := tk.droppedInput.Load(); got != uint64(len(first)) {
		t.Errorf("after a refused %d-byte write the count is %d, want %d",
			len(first), got, len(first))
	}
	// Cumulative for the connection: a client that keeps typing into a wedged
	// program is owed the total, not the size of the last thing it lost.
	second := bytes.Repeat([]byte("y"), 1024)
	tk.typed(second)
	if got, want := tk.droppedInput.Load(), uint64(len(first)+len(second)); got != want {
		t.Errorf("after two refused writes the count is %d, want %d (%d+%d)",
			got, want, len(first), len(second))
	}
}

// The log line is per connection, not per keystroke. A program that has stopped
// reading refuses everything, and a client holding a key down would write a line
// per repeat — burying the first one, which is the only one that says anything.
func TestTheDropIsLoggedOncePerConnection(t *testing.T) {
	tk, _ := dropTicker(t, 64)
	logs := countLogs(t, msgInputDropped)

	big := bytes.Repeat([]byte("x"), 4096)
	for range 20 {
		tk.typed(big)
	}
	if got := logs(); got != 1 {
		t.Errorf("20 refused writes logged %q %d times, want exactly 1", msgInputDropped, got)
	}
	if got := tk.droppedInput.Load(); got != uint64(20*len(big)) {
		t.Errorf("the count is %d after 20 refused writes of %d, want %d"+
			" — logging once must not stop counting", got, len(big), 20*len(big))
	}
}

// A refused write wakes the tick loop, because the count it changed is read in
// one place only — where the status frame is written — and a refused write
// produces no output, so nothing else brings the loop round. Without this the
// notice waits for the idle poll, up to idlePoll.
//
// An accepted write must not wake it: the output it causes does that, and a wake
// per keystroke on the ordinary path would pay for this on every window that is
// working.
func TestARefusedWriteWakesTheLoopAndAnAcceptedOneDoesNot(t *testing.T) {
	tk, _ := dropTicker(t, 64)
	woke := func() bool {
		select {
		case <-tk.ctrl.notify:
			return true
		default:
			return false
		}
	}

	tk.typed([]byte("ls\n"))
	if woke() {
		t.Error("an accepted write woke the tick loop; its output is what does that")
	}
	tk.typed(bytes.Repeat([]byte("x"), 4096))
	if !woke() {
		t.Error("a refused write left the tick loop asleep, so the client waits" +
			" for the idle poll to hear that its input was dropped")
	}
}

// The log latch is swapped, not tested and set, because typed has two callers on
// two goroutines: the read loop for keystrokes and the tick loop for the cursor
// keys a scroll becomes. The count must stay exact across both, which is the other
// half of the same claim.
//
// What this fixture pins and what it does not, measured: making droppedLogged a
// plain bool is caught in 18 of 20 separate-process runs under -race. Leaving it
// atomic but loading and then storing it is caught in 0 of 20 in either build, and
// the reason is that typed reaches the latch only after the window's queue mutex
// has already serialised the write — the gap between that load and that store is a
// nanosecond or so out of the fifty the refusal costs, so two goroutines are
// almost never inside it together. The compare-and-swap is the right shape by
// construction rather than because this test would notice it changing, and the
// cost of being wrong about it is a duplicate log line, not a wrong count.
func TestTheDropLatchHoldsAcrossBothGoroutines(t *testing.T) {
	const writers, each = 8, 200
	tk, _ := dropTicker(t, 64)
	logs := countLogs(t, msgInputDropped)
	big := bytes.Repeat([]byte("x"), 4096)

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				tk.typed(big)
			}
		}()
	}
	wg.Wait()

	if got := logs(); got != 1 {
		t.Errorf("%d concurrent refused writes logged %q %d times, want exactly 1",
			writers*each, msgInputDropped, got)
	}
	if got, want := tk.droppedInput.Load(), uint64(writers*each*len(big)); got != want {
		t.Errorf("the count is %d after %d concurrent refused writes, want %d",
			got, writers*each, want)
	}
}

// A scroll that a full-screen application owns is forwarded to it as cursor keys
// or mouse reports, and those are the client's scroll: when the window will not
// take them the client is owed the same answer as for a keystroke. They are
// written from the tick loop rather than the read loop, which is why they were the
// two sites the first cut of this fix left discarding their error — a wheel report
// the program never received looks to the user exactly like one it ignored.
//
// This goes through handleCtrl rather than calling typed with the same bytes.
// Calling typed proves only that typed counts, which the tests above already
// establish; it passes with both call sites back to discarding their error, and
// that is the whole defect. What has to be driven is the call.
func TestAScrollForwardedToAFullScreenAppIsCountedWhenItIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		// modes puts the program's screen in the state that routes a wheel event
		// to this arm, written into the grid the way the program would write it.
		modes string
		want  func(cols, rows uint16) []byte
	}{
		// 1049h takes the alternate screen; alternate scroll is on by default, so
		// the wheel becomes cursor keys with nothing further asked for.
		{"arrow keys", "\x1b[?1049h", func(uint16, uint16) []byte { return arrowInput(-3, false) }},
		// 1000h asks for mouse reporting and 1006h for the SGR encoding, which is
		// what a program that wants the wheel itself sends.
		{"mouse report", "\x1b[?1049h\x1b[?1000h\x1b[?1006h",
			func(cols, rows uint16) []byte { return wheelInput(-3, cols, rows, true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			const cols, rows = 80, 24
			want := c.want(cols, rows)
			if len(want) < 2 {
				t.Fatalf("setup: the translation produced %d bytes; the fixture needs"+
					" at least 2 to set a limit under it", len(want))
			}
			// Under the translation's own size, which is the refusal a window whose
			// program has stopped reading gives this write.
			tk := scrollTicker(t, cols, rows, len(want)-1, c.modes)

			// Premise: with room, the same call counts nothing, so the count below
			// is the limit refusing the write and not the mere fact of the call.
			roomy := scrollTicker(t, cols, rows, 1<<20, c.modes)
			roomy.handleCtrl(ctrlEvent{scroll: &scrollMsg{op: "delta", lines: -3, hasLines: true}})
			if got := roomy.droppedInput.Load(); got != 0 {
				t.Fatalf("setup: a scroll a roomy window accepted counted %d dropped", got)
			}

			// Equal to the translation's own length, which is what says the write
			// happened and happened with these bytes: a handleCtrl that forwarded
			// nothing counts 0, and one that took the other branch counts that
			// branch's different length.
			tk.handleCtrl(ctrlEvent{scroll: &scrollMsg{op: "delta", lines: -3, hasLines: true}})
			if got := tk.droppedInput.Load(); got != uint64(len(want)) {
				t.Errorf("a refused %d-byte forwarded scroll counted %d dropped, want %d",
					len(want), got, len(want))
			}
		})
	}
}

// scrollTicker is a ticker whose handleCtrl can forward a wheel event to a
// full-screen program: a window with the given input limit, a client-sized
// attachment for handleCtrl to read the crop from, and a grid put into the modes
// that decide which translation the wheel gets.
func scrollTicker(t *testing.T, cols, rows uint16, limit int, modes string) *ticker {
	t.Helper()
	w, err := shelld.NewWindow(shelld.WindowOpts{
		Name:            "scroll",
		Command:         []string{"/bin/cat"},
		Cols:            cols,
		Rows:            rows,
		InputQueueLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	// An observer attachment so its declared size does not move the window's, but
	// the ticker is not an observer: handleCtrl refuses to forward an observer's
	// scroll, and it is a real client's scroll that is at issue.
	att := w.RegisterObserver(cols, rows)
	t.Cleanup(att.Release)
	tk := &ticker{
		win: w, att: att, live: &shelld.LiveFramer{},
		ctrl: &ctrlQueue{notify: make(chan struct{}, 1)},
	}
	tk.lastSentPaused, tk.pausedScratch = shelld.EmptySnapshot(), &shelld.GridSnapshot{}

	g := w.LockVT()
	g.Advance([]byte(modes))
	alt, mouse := g.AltScreenActive(), g.MouseReporting()
	w.UnlockVT()
	if !alt {
		t.Fatalf("setup: %q left the grid off the alternate screen, so handleCtrl"+
			" would not forward the wheel at all", modes)
	}
	if strings.Contains(modes, "1000h") != mouse {
		t.Fatalf("setup: %q left mouse reporting %v, so this arm takes the other branch",
			modes, mouse)
	}
	return tk
}

// The wire shape, both ways round, and the geometry that is never omitted either
// way. The count is omitted while it is zero rather than sent as 0, so a client
// that never overran a queue does not have to parse a field to learn it has no
// drops. That is a rule about an optional counter and stops there: cols and rows
// are unconditional, because a client cannot derive them from anything else on the
// stream, so an absent pair would not separate "you are not cropped" from "this
// daemon does not report it".
//
// The geometry in each arm is asymmetric and shares no value with offset or max.
// Both properties are load-bearing: cols == rows would pass under a transposed
// emit, and reusing 3 or 9 would pass under one that emitted the wrong pair.
func TestStatusFrameCarriesTheDropCountOnlyWhenThereIsOne(t *testing.T) {
	const noDrops = "\x01{\"status\":{\"offset\":3,\"max\":9,\"paused\":true,\"alt\":false," +
		"\"cols\":120,\"rows\":40}}"
	if got := string(formatStatusFrame(3, 9, true, false, 120, 40, 0)); got != noDrops {
		t.Errorf("with no drops the frame is\n%s\nwant\n%s", got, noDrops)
	}
	const dropped = "\x01{\"status\":{\"offset\":0,\"max\":0,\"paused\":false,\"alt\":true," +
		"\"cols\":20,\"rows\":5,\"droppedBytes\":4096}}"
	if got := string(formatStatusFrame(0, 0, false, true, 20, 5, 4096)); got != dropped {
		t.Errorf("with drops the frame is\n%s\nwant\n%s", got, dropped)
	}
	// A zero geometry is still sent. This arm is here because the wire cannot reach
	// it — the effective size is floored at minEffectiveCols×minEffectiveRows — so
	// no socket test in the suite would notice the pair being made conditional on
	// being nonzero, which is the shape droppedBytes above would invite copying.
	const zeroGeom = "\x01{\"status\":{\"offset\":0,\"max\":0,\"paused\":false,\"alt\":false," +
		"\"cols\":0,\"rows\":0}}"
	if got := string(formatStatusFrame(0, 0, false, false, 0, 0, 0)); got != zeroGeom {
		t.Errorf("a zero geometry was omitted:\n%s\nwant\n%s", got, zeroGeom)
	}
}

// End to end: a real daemon, a real client, and a window whose queue is too small
// for what the client sends. What the client receives is the whole point, so this
// asserts on the frames off the wire rather than on the counter behind them.
//
// The small queue stands in for a wedged program. The two are the same refusal —
// push tests the chunk against the limit first — and a limit is a fixture a test
// can set, where a program that has stopped reading needs its tty buffer filled
// first and refuses on a boundary no test controls.
func TestAClientIsToldWhenTheWindowDropsItsInput(t *testing.T) {
	sock := bootWith(t, func(st *State) { st.InputQueueLimit = 64 })
	openWindow(t, sock, "s")
	conn, ch := attachAt(t, sock, "s", 80, 24)
	defer conn.Close()
	s := newScreen(t, ch, 80, 24)

	// A client with nothing dropped is told nothing. Checked before the drop, on
	// frames the daemon sent for its own reasons, so it holds for every client
	// that never overruns a queue.
	s.await("a first status frame", 5*time.Second, func() bool { return len(s.status) > 0 })
	s.drain(300 * time.Millisecond)
	if bytes.Contains(s.status, []byte("droppedBytes")) {
		t.Fatalf("a client with no drops was sent a count: %s", s.status)
	}
	clean := append([]byte(nil), s.status...)

	paste := bytes.Repeat([]byte("x"), 4096)
	if err := conn.WriteMessage(ws.OpBinary, paste); err != nil {
		t.Fatal(err)
	}
	s.await("the dropped-byte count", 5*time.Second, func() bool {
		return bytes.Contains(s.status, []byte(`"droppedBytes":4096`))
	})

	// The rest of the frame is unchanged: the count is added to the state the
	// client already reads, not sent instead of it. Asserted key by key rather
	// than as a prefix of the earlier frame, because max is the scrollback length
	// and a frame that legitimately reported a different one would fail a prefix
	// check without the field the client reads having gone anywhere.
	for _, k := range [][]byte{[]byte(`"offset":`), []byte(`"max":`), []byte(`"paused":`), []byte(`"alt":`)} {
		if !bytes.Contains(s.status, k) {
			t.Errorf("the frame carrying the count lost %s:\n%s\nthe frame before it was\n%s",
				k, s.status, clean)
		}
	}
	// Appended, and last: a reader that stopped at the fields it knew still reads
	// every one of them.
	if !bytes.HasSuffix(s.status, []byte(`,"droppedBytes":4096}}`)) {
		t.Errorf("the count is not appended to the end of the frame:\n%s", s.status)
	}
	if strings.Count(string(s.status), "droppedBytes") != 1 {
		t.Errorf("the count appears more than once in %s", s.status)
	}

	// And sent once. The count the client was told is remembered, so a total that
	// has not moved is not news and no later pass of the tick loop repeats it.
	// Without that the client is sent the same number for as long as it stays
	// attached: every idle poll while the window is quiet, and every paced frame
	// while it is not. Two seconds covers more than one idlePoll, which is the
	// slowest rate at which a repeat could arrive.
	repeats := 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		m, ok := next(ch, time.Until(deadline))
		if !ok {
			break
		}
		if m.op == ws.OpText && bytes.Contains(m.data, []byte("droppedBytes")) {
			repeats++
		}
	}
	if repeats != 0 {
		t.Errorf("the count was sent %d more times in the 2s after it changed;"+
			" an unchanged total is not news", repeats)
	}
}

// The handshake's first frame is input too. A client may open an attach and send
// its keys straight away rather than declaring a size first, and serveAttach
// holds that frame back until the ticker exists; it is the one input site the
// read loop never sees, so nothing above would notice it going back to
// discarding its error.
func TestTheHandshakesDeferredInputIsCountedWhenItIsRefused(t *testing.T) {
	sock := bootWith(t, func(st *State) { st.InputQueueLimit = 64 })
	openWindow(t, sock, "s")
	conn := openWS(t, sock, "s")
	defer conn.Close()

	// No resize first: an opening frame that does not parse as a resize control is
	// the deferred input, which is the path this test is about.
	paste := bytes.Repeat([]byte("x"), 4096)
	if err := conn.WriteMessage(ws.OpBinary, paste); err != nil {
		t.Fatal(err)
	}
	s := newScreen(t, reader(conn), 80, 24)
	s.await("the dropped-byte count for the handshake's own keys", 5*time.Second, func() bool {
		return bytes.Contains(s.status, []byte(`"droppedBytes":4096`))
	})
}
