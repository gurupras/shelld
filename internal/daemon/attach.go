package daemon

// WebSocket /attach. Wire format:
//   - server → client binary frames: ANSI repaint deltas of the server-side
//     grid. Live-view frames are built by shelld.LiveFramer: a large one is
//     wrapped in DEC 2026 synchronized-update markers, a small one is sent
//     bare. Paused-view frames are always wrapped
//   - server → client text frames starting with \x01: status JSON
//     {"status":{"offset":N,"max":N,"paused":bool,"alt":bool,"cols":N,"rows":N}}
//     (a "hello" after the first frame, then on every change). cols and rows are
//     the effective PTY size, the same quantity GET /windows and GET …/frame
//     publish under those names — not this client's declared size, which it
//     already knows. They are always present, because a client cannot derive them
//     from anything else on this stream and needs them to learn whether it is
//     seeing all of the grid; see the floor below. A client whose input the window
//     could not queue also gets "droppedBytes":N, cumulative for the connection
//     and counting the cursor keys a scroll in a full-screen application becomes;
//     that key alone is absent while it is zero. droppedBytes is a Go-only
//     extension — shelld-rs does not emit it — so to a client that talks to both
//     daemons its absence has never meant "no drops"
//   - client → server binary frames: stdin bytes
//   - client → server text frames starting with the two bytes \x01{: control JSON
//     {"resize":{"cols":N,"rows":N}} declares this client's size;
//     {"scroll":{"op":"enter"|"exit"|"delta"|"to","lines":N,"offset":N}}.
//     Any other text frame is input, including one that starts with \x01 followed
//     by anything but { -- that is a typed Ctrl-A
//
// Each client registers an Attacher with its declared size; the PTY runs at
// clamp(max(pin, min over voting attachers), minEffectiveCols×minEffectiveRows,
// MaxCols×MaxRows). Both bounds are the *effective* size's, not the declared
// size's: a declaration is taken verbatim, clamped down to the maximum and never
// up to the minimum (Attacher.SetSize), and CheckSize is not called on this wire
// at all. A client renders against a crop of the shared grid at its own size, so
// its own geometry is its own: a peer's resize never changes the shape of the
// frames it gets. What a peer's resize does change is the shared grid the crop
// reads from, since a narrower width reflows history; a client reading a position
// in history is handed a copy of it first, which is shelld.PausedView.
//
// A client that declares less than the lower bound therefore crops a grid strictly
// larger than itself, and the program was told the larger size: whatever it draws
// outside the crop is not reflowed to fit and not sent — it is simply absent, and
// the cursor is clamped into the crop with it. That is what the status frame's cols
// and rows exist to report, and reporting it is the only mechanism there is; a
// client has no other way to notice.
//
// Handshake: wait for the first client message. A resize control declares the size;
// anything else keeps the window's current size. The opening frame is otherwise read by
// exactly the same rules as every later one (classifyText), so its scroll scrolls and its
// input is forwarded after the initial frame. It is not a separate dialect: a client that
// opened with a scroll used to have those bytes typed into its program.
//
// A text frame's leading \x01{ is the client declaring which of the two it meant, and the
// declaration is taken at its word in both directions: without the marker the frame is
// input, with it the frame is a control frame and is honoured or dropped, never typed.
//
// `?observer=1` on the attach URL makes the client read-only and non-voting: it
// gets the same frames and status messages cropped to its own declared size,
// but it never enters the effective-size min and nothing it sends reaches the
// program — not a binary frame, not an unrecognized control frame, and not the
// wheel/arrow keys a scroll control synthesizes on the alternate screen.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/internal/tracing"
	"github.com/gurupras/shelld/ws"
)

const (
	minInterval = 8 * time.Millisecond
	maxInterval = 33 * time.Millisecond
	// cursorSettle is the quiet period after output before the cursor is
	// shown again.
	cursorSettle = 33 * time.Millisecond
	// idlePoll is the safety tick for a quiet window.
	idlePoll = time.Second
	// paceBurst is how many frames may leave back to back before the loop
	// holds one. minInterval alone is a minimum *spacing*, which charges a
	// quantum to the frame that ends a burst: a command whose output is all in
	// the grid within one quantum still sends its first frame at once and then
	// waits out the rest of the quantum to send the frame carrying the tail,
	// so the client learns the command finished up to minInterval late. A
	// burst of 2 sends that tail when it is ready. It does not raise the
	// sustained rate: paceWait spends the allowance once and then paces at one
	// frame per minInterval for as long as output keeps coming, so any window
	// longer than the burst still averages 1/minInterval.
	paceBurst = 2
	// paceCredit is how far ahead of that sustained schedule a send may run.
	// At paceBurst 1 it is zero and paceWait is exactly "minInterval since the
	// last frame" — the rule this generalises.
	paceCredit = (paceBurst - 1) * minInterval
)

// idleEpoch anchors every client's idle deadline to one monotonic grid,
// epoch + k·idlePoll. Idle tick loops then wake at the same instants, and the
// runtime serves all of them with one wake-up per period; with a deadline a
// full idlePoll after each loop's own last wake, every idle client was a
// separate wake-up per second (netpoll, a spinning thread and sysmon each).
var idleEpoch = time.Now()

// idleWait is how long an idle loop waits: to the next grid instant, so at
// most idlePoll and more than zero. It bounds the repair of a change that
// reaches the grid with no dirty notification.
//
// The grid is the only net for a change with no notification: no tick follows
// a frame any more (the loop's wait below), so such a change waits for the
// next grid instant, up to idlePoll. A change that does notify — output, exit,
// an effective-size change — wakes the loop at once, because the dirty channel
// is armed before each render and stays armed through the write and the pacing
// sleep.
func idleWait(now time.Time) time.Duration {
	return idlePoll - now.Sub(idleEpoch)%idlePoll
}

var (
	dec2026Begin = []byte("\x1b[?2026h")
	dec2026End   = []byte("\x1b[?2026l")
	cursorShow   = []byte("\x1b[?25h")
	cursorHide   = []byte("\x1b[?25l")
)

type resizeMsg struct{ cols, rows uint16 }

type scrollMsg struct {
	op        string
	lines     int32
	hasLines  bool
	offset    uint32
	hasOffset bool
}

type ctrl struct {
	resize *resizeMsg
	scroll *scrollMsg
}

// textFrame is what an OpText frame from a client means.
//
// input excludes the other two -- a frame is either a control frame or text -- but
// resize and scroll do not exclude each other, because one control frame may carry
// both objects. Which of the two then happens is the caller's to decide, and both
// callers decide it the same way: a switch whose resize arm comes first, so a frame
// carrying both is the resize. Reporting them separately rather than picking here is
// what lets the two callers be seen to agree; picking here would bury the precedence
// in the classifier, where neither caller could be shown to share it.
//
// All three nil means the frame asked for something this server does not implement and
// the right answer is to do nothing.
//
// Both the opening frame and every later one are classified through here on purpose.
// They used to be classified separately, and the two disagreed: the handshake asked
// only whether the frame was a resize and treated everything else as input, so a
// well-formed scroll sent as a client's *first* message was typed into the program as
// keystrokes, while the identical frame sent second scrolled. One classifier is
// necessary for a frame to mean one thing whenever it arrives, and not sufficient --
// the two callers must also resolve a frame that sets two fields the same way, which
// is why each does it with a switch in the same arm order.
type textFrame struct {
	resize *resizeMsg
	scroll *scrollMsg
	// input is the frame's bytes when it carries no control marker at all, which is
	// how a client sends text as an OpText frame rather than as OpBinary.
	input []byte
}

// classifyText decides what an OpText frame is.
//
// The marker is the two bytes 0x01 '{', the client stating which of the two it meant;
// every control message is a JSON object, so every one starts that way. A frame without
// the marker is text, so it is input -- including one that starts with 0x01 and goes on
// with anything else, which is a typed Ctrl-A: start-of-line to a shell, the prefix key
// to a multiplexer, and the first half of a clear-line (0x01 0x0b), all of which used to be
// dropped because a lone 0x01 was taken for the marker. A frame with the marker that
// does not parse is a control frame this server cannot honour, and it is dropped rather
// than typed: delivering it would send Ctrl-A followed by a line of JSON. A client whose
// control frame is malformed gets nothing done, which is recoverable; the alternative
// writes attacker- or bug-chosen keystrokes into the user's shell.
//
// An empty frame is neither, and is reported as neither so that callers can test the
// fields rather than having to know whether an empty payload arrived as a nil slice or
// a zero-length one. Nothing is lost: typing no bytes writes nothing.
func classifyText(data []byte) textFrame {
	if len(data) == 0 {
		return textFrame{}
	}
	c, ok := parseCtrl(data)
	if ok {
		return textFrame{resize: c.resize, scroll: c.scroll}
	}
	if isCtrlFrame(data) {
		return textFrame{}
	}
	return textFrame{input: data}
}

// isCtrlFrame reports whether a text frame carries the control marker, 0x01 '{'.
func isCtrlFrame(text []byte) bool {
	return len(text) >= 2 && text[0] == 0x01 && text[1] == '{'
}

// parseCtrl decodes a \x01{-prefixed control frame with serde's typing:
// a malformed resize or scroll object invalidates the whole frame.
func parseCtrl(text []byte) (ctrl, bool) {
	if !isCtrlFrame(text) {
		return ctrl{}, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(text[1:], &top); err != nil || top == nil {
		return ctrl{}, false
	}
	var c ctrl
	if raw, ok := top["resize"]; ok && !isNull(raw) {
		var obj body
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return ctrl{}, false
		}
		cols, ok1 := obj.uint("cols", true, 0xffff)
		rows, ok2 := obj.uint("rows", true, 0xffff)
		if !ok1 || !ok2 {
			return ctrl{}, false
		}
		c.resize = &resizeMsg{uint16(cols), uint16(rows)}
	}
	if raw, ok := top["scroll"]; ok && !isNull(raw) {
		var obj body
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return ctrl{}, false
		}
		op, okOp := obj.str("op", true)
		m := &scrollMsg{op: op}
		okLines := true
		if raw, exists := obj["lines"]; exists && !isNull(raw) {
			v, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 32)
			m.lines, m.hasLines, okLines = int32(v), true, err == nil
		}
		offset, hasOffset, okOffset := obj.optUint("offset", 0xffffffff)
		m.offset, m.hasOffset = uint32(offset), hasOffset
		if !okOp || !okLines || !okOffset {
			return ctrl{}, false
		}
		c.scroll = m
	}
	return c, true
}

// formatStatusFrame reports the client's scroll state and the size of the grid it
// is reading. alt says the full-screen alternate screen is active: the application,
// not shelld, owns scrolling, so the client should pass scroll keys through to it.
//
// cols and rows are the grid's own dimensions — the effective PTY size — and not
// the caller's declared size, which it already knows. They are what makes the
// effective-size floor visible: the PTY is clamped up to minEffectiveCols ×
// minEffectiveRows while a declaration is not, so a client that declared less than
// that is cropping a larger grid and never sees what the program drew outside the
// crop. Nothing else on this stream carries geometry and a client cannot derive it,
// so both are always present: absent-when-equal would leave the client unable to
// tell "you are not cropped" from "this daemon does not say".
//
// Callers must read the pair from the same locked snapshot the frame was drawn
// from, so that the geometry always describes the grid that produced the frame it
// accompanies rather than one a concurrent resize has already moved on from.
//
// dropped is how many bytes sent on this client's behalf the window could not
// queue, cumulative for the connection — its keystrokes, and the cursor keys its
// scrolls become in a full-screen application, which are lost the same way and
// look the same to the user. It is omitted while it is zero rather than sent as 0:
// the count is news, and a client that has none should not have to parse a field
// to learn that.
//
// That omission is a rule about an optional *counter* and does not extend to the
// geometry above, which is unconditional. It also buys less than it reads like.
// The reason it was written — that a client without drops sees byte-for-byte the
// frame it saw before the field existed — held only for clients of this daemon:
// droppedBytes is a Go-only extension that shelld-rs does not emit at all, so for
// anything speaking to both, an absent key has never separated "no drops" from
// "not implemented". Emitting the geometry unconditionally spends that property
// deliberately, and TestAttachHandshakeHelloAndInput's exact-byte hello moves with
// it rather than guarding it.
//
// dropped stays last so a reader that stopped at the fields it knew still reads
// every one of them; TestAClientIsToldWhenTheWindowDropsItsInput pins that by
// suffix, which is why cols and rows belong in the first Appendf below and not
// after this block.
func formatStatusFrame(offset, maxOff uint32, paused, alt bool, cols, rows uint16, dropped uint64) []byte {
	b := fmt.Appendf(nil,
		"\x01{\"status\":{\"offset\":%d,\"max\":%d,\"paused\":%t,\"alt\":%t,\"cols\":%d,\"rows\":%d",
		offset, maxOff, paused, alt, cols, rows)
	if dropped > 0 {
		b = fmt.Appendf(b, ",\"droppedBytes\":%d", dropped)
	}
	return append(b, '}', '}')
}

// maxKeptBuffer bounds the render and frame buffers a tick loop keeps between
// ticks; a rare huge repaint does not stay pinned.
const maxKeptBuffer = 4 << 20

func keepBuffer(b []byte) []byte {
	if cap(b) > maxKeptBuffer {
		return nil
	}
	return b[:0]
}

// suppressCursorShow turns a trailing cursor-show into a hide so the cursor
// stays hidden mid-burst.
func suppressCursorShow(frame []byte) []byte {
	if bytes.HasSuffix(frame, cursorShow) {
		frame = append(frame[:len(frame)-len(cursorShow)], cursorHide...)
	}
	return frame
}

type clientMode struct {
	paused     bool
	offset     uint32
	frozenCols uint16
	frozenRows uint16
}

// backToLive reports whether there is nothing left for a paused client to be
// paused on, so its next frame comes from the live view instead. maxOff is the
// largest offset whatever it reads can serve: the live grid's history, or its own
// frozen copy's once it holds one.
//
// There are two ways to have nothing, and they arrive by different events. A client
// is at the live tail either because it paused there — applyScroll admits an
// explicit pause at offset 0 on a window that has history — or because a peer's
// taller screen took rows back out of history until reanchor had walked its offset
// down to 0. The history it reads holds no rows at all when a program takes the
// alternate screen under a client that is still reading the live grid. A scroll that
// ends at the tail is not one of the two: applyScroll returns an unpaused mode for
// that, so such a client never reaches here still paused.
//
// The alternate screen needs no clause of its own, and must not have one that names
// only itself. A client still reading the live grid is bounded by that grid's
// history, and ScrollbackLen reads the active screen's, which on the alternate
// screen is none — so maxOff is already 0 for exactly the clients such a clause
// would return, and a program taking the screen returns them. A client reading a
// copy is bounded by the copy, which that program has not touched, so it must not
// be returned at all.
//
// It is a method rather than the condition written out at its one call site so that
// a test can supply one way of having nothing at a time. Both send the client to the
// live view, so a test that only ever supplies both at once passes with either clause
// deleted — and they are not interchangeable, since neither producer above reaches
// the other's clause.
func (m clientMode) backToLive(maxOff uint32) bool {
	return m.paused && (m.offset == 0 || maxOff == 0)
}

// applyScroll computes the next mode for a scroll op, or false for a no-op.
func applyScroll(mode clientMode, op *scrollMsg, scrollbackLen uint32, altScreen bool, cols, rows uint16) (clientMode, bool) {
	if altScreen {
		return mode, false
	}
	clamp := func(o int64) uint32 { return uint32(max(0, min(o, int64(scrollbackLen)))) }
	frozen := func() (uint16, uint16) {
		if mode.paused {
			return mode.frozenCols, mode.frozenRows
		}
		return cols, rows
	}
	switch op.op {
	case "enter":
		if mode.paused {
			return mode, false
		}
		off := clamp(int64(op.offset))
		if off == 0 && scrollbackLen == 0 {
			return mode, false
		}
		return clientMode{true, off, cols, rows}, true
	case "exit":
		if !mode.paused {
			return mode, false
		}
		return clientMode{}, true
	case "delta":
		cur := int64(0)
		if mode.paused {
			cur = int64(mode.offset)
		}
		// Negative lines scroll back into history.
		off := clamp(cur - int64(op.lines))
		if off == 0 {
			return clientMode{}, true
		}
		fc, fr := frozen()
		return clientMode{true, off, fc, fr}, true
	case "to":
		off := clamp(int64(op.offset))
		if off == 0 {
			if !mode.paused {
				return mode, false
			}
			return clientMode{}, true
		}
		fc, fr := frozen()
		return clientMode{true, off, fc, fr}, true
	}
	return mode, false
}

// wheelInput encodes a scroll delta as mouse-wheel reports for an alt-screen
// app with mouse reporting: SGR when negotiated, else X10; one report per
// notch, at most 8, over the grid center.
func wheelInput(lines int32, cols, rows uint16, sgr bool) []byte {
	btn, count := 65, int64(lines)
	if lines < 0 {
		btn, count = 64, -int64(lines)
	}
	count = max(1, min(count, 8))
	col := max(cols/2, 1)
	row := max(rows/2, 1)
	var out []byte
	for range count {
		if sgr {
			out = fmt.Appendf(out, "\x1b[<%d;%d;%dM", btn, col, row)
		} else {
			out = append(out, 0x1b, '[', 'M', byte(btn+32), byte(min(col, 223)+32), byte(min(row, 223)+32))
		}
	}
	return out
}

// arrowInput encodes a scroll delta as Up/Down arrow keys, which is how a
// terminal scrolls an alternate-screen program that has not asked for mouse
// reports (less, man, git's pager). One key per line, at most 8.
func arrowInput(lines int32, appCursor bool) []byte {
	final, count := byte('B'), int64(lines)
	if lines < 0 {
		final, count = 'A', -int64(lines)
	}
	intro := []byte("\x1b[")
	if appCursor {
		intro = []byte("\x1bO")
	}
	var out []byte
	for range max(1, min(count, 8)) {
		out = append(append(out, intro...), final)
	}
	return out
}

type ctrlEvent struct {
	resize bool
	scroll *scrollMsg
}

// ctrlQueue is an unbounded FIFO, so a ticker blocked on a slow client never
// stalls the reader forwarding keystrokes.
type ctrlQueue struct {
	mu     sync.Mutex
	items  []ctrlEvent
	notify chan struct{}
}

func (q *ctrlQueue) push(ev ctrlEvent) {
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

// wake runs the ticker's next iteration without giving it an event, for state
// the reader changed in place rather than passed along. take returning nothing
// is what the loop does with a wake: it goes on to render and to the status
// frame, which is where that state is read.
func (q *ctrlQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *ctrlQueue) take() []ctrlEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// wsWrite writes one message, traced as a KindWSWrite span on conn id cid.
func wsWrite(conn *ws.Conn, cid uint64, op ws.Opcode, data []byte) error {
	if !tracing.Enabled {
		return conn.WriteMessage(op, data)
	}
	span := tracing.Begin(tracing.KindWSWrite, cid, int64(len(data)))
	err := conn.WriteMessage(op, data)
	tracing.End(tracing.KindWSWrite, cid, span, int64(len(data)), int64(op))
	return err
}

// wsRead reads one message, traced as a KindWSRead span on conn id cid.
func wsRead(conn *ws.Conn, cid uint64) (ws.Opcode, []byte, error) {
	if !tracing.Enabled {
		return conn.ReadMessage()
	}
	span := tracing.Begin(tracing.KindWSRead, cid, 0)
	op, data, err := conn.ReadMessage()
	tracing.End(tracing.KindWSRead, cid, span, int64(len(data)), int64(op))
	return op, data, err
}

// serveAttach serves one attach until the client goes, the shell exits, or stop
// closes for a shutdown. A shutdown sends the client a close 1001 "daemon shutting
// down", as shelld-rs does (attach.rs going_away), and then waits for its reply
// until the drain's deadline, which deadline reports once stop is closed.
func serveAttach(conn *ws.Conn, win *shelld.Window, observer bool, stop <-chan struct{}, deadline func() time.Time) {
	defer conn.Close()
	cid := tracing.NewID()
	if tracing.Enabled {
		defer tracing.Instant(tracing.KindAttachClose, cid, 0, 0)
	}
	// Watched from here, before the opening read, so a client that has not sent
	// its first frame is told as well: it is parked in that read, where nothing
	// else would ever reach it. 1001 and not the 1000 a shell exit sends, so a
	// client can tell its shell ending from the daemon going away.
	served := make(chan struct{})
	defer close(served)
	go func() {
		select {
		case <-stop:
			_ = conn.WriteClose(ws.CloseGoingAway, "daemon shutting down")
			_ = conn.UnderlyingConn().SetReadDeadline(deadline())
		case <-served:
		}
	}()

	op, data, err := wsRead(conn, cid)
	if err != nil {
		return
	}
	// The opening frame is classified by the same rules as every later one. Only its
	// resize can be acted on here, because the size is needed to register the attacher
	// below; input and scroll have to wait for the ticker that carries them and are
	// replayed once it exists.
	var first textFrame
	if op == ws.OpBinary {
		first.input = data
	} else {
		first = classifyText(data)
	}

	cols, rows := win.Size()
	if first.resize != nil {
		cols, rows = first.resize.cols, first.resize.rows
	}
	register := win.RegisterAttacher
	if observer {
		register = win.RegisterObserver
	}
	att := register(cols, rows)
	defer att.Release()
	if tracing.Enabled {
		tracing.Instant(tracing.KindAttachOpen, cid, int64(win.TraceID()), int64(cols)<<16|int64(rows))
	}

	live := &shelld.LiveFramer{}
	g := win.LockVT()
	// One read for the crop, so a resize arriving mid-render cannot give this
	// frame one client's new width with its old height.
	ac, ar := att.Size()
	frame, _, _ := live.Frame(nil, g, int(ac), int(ar), false)
	maxOff := uint32(g.ScrollbackLen())
	alt := g.AltScreenActive()
	// Read under the same lock as the frame above, not from Window.Cols/Rows:
	// setEffectiveSize publishes w.cols in a different critical section from
	// w.vt.Resize, so a peer attaching between here and the hello write could
	// leave the announced size describing a grid this frame was not drawn from.
	gridCols, gridRows := uint16(g.Cols()), uint16(g.Lines())
	win.UnlockVT()

	if len(frame) > 0 && wsWrite(conn, cid, ws.OpBinary, frame) != nil {
		return
	}
	// Hello: the initial scrollback status. No drops can have happened yet, so
	// this frame is the one clients have always received.
	if wsWrite(conn, cid, ws.OpText,
		formatStatusFrame(0, maxOff, false, alt, gridCols, gridRows, 0)) != nil {
		return
	}

	t := &ticker{
		cid:      cid,
		conn:     conn,
		win:      win,
		att:      att,
		observer: observer,
		ctrl:     &ctrlQueue{notify: make(chan struct{}, 1)},
		stop:     make(chan struct{}),
		live:     live,
	}
	// After the ticker exists, so the opening frame is accounted for like every later
	// one: its input is the client's first write and is exactly as droppable as the
	// rest, and its scroll goes through the same queue a later scroll would.
	//
	// A switch in the loop's own arm order, and not a sequence of ifs. classifyText
	// reports resize and scroll independently because one control frame may carry both
	// objects, and which of the two then happens is the caller's to decide -- the loop
	// decides it by taking the resize arm first. Two ifs here would honour both, so a
	// frame setting both would still mean two different things depending on which
	// position it arrived in, which is the whole defect this shares a classifier to
	// avoid.
	switch {
	case first.resize != nil:
		// Already acted on, above, where the size was needed to register the attacher
		// and is the size the initial frame was drawn at. No ctrlEvent to push: there
		// is no earlier frame for the tick loop to redraw.
	case first.scroll != nil:
		// Honoured for an observer too, for the reason the loop honours it: scrolling
		// moves nobody else's view.
		t.ctrl.push(ctrlEvent{scroll: first.scroll})
	case observer:
		// An observer's input is dropped, for the reason the loop drops it.
	case first.input != nil:
		t.typed(first.input)
	}
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		gaveUp := t.run()
		select {
		case <-stop:
			// Shutting down: the close has been sent, which is also what failed any
			// write since, and the read loop is waiting for the reply until the
			// drain's deadline. Neither branch below may cut that short.
			return
		default:
		}
		if gaveUp {
			// A write failed, so the client is unreachable: stop reading it at once,
			// as shelld-rs does, rather than typing its input blind for as long as a
			// client is given to answer a close. No close either -- the write that
			// failed may have left part of a frame on the wire, and a close behind it
			// would only sit in the same full buffer.
			t.unreachable.Store(true)
			_ = conn.UnderlyingConn().SetReadDeadline(time.Now())
			observeGaveUp(win, att)
			return
		}
		// The shell exited: close the socket so the client sees the window go
		// away, and stop waiting on a client that never answers the close.
		_ = conn.SendClose()
		_ = conn.UnderlyingConn().SetReadDeadline(time.Now().Add(10 * time.Second))
	}()

	for {
		op, data, err := wsRead(conn, cid)
		// unreachable as well as the deadline, because a frame already buffered
		// is returned without reaching the socket the deadline is on.
		if err != nil || t.unreachable.Load() {
			break
		}
		if op == ws.OpBinary {
			if observer {
				continue
			}
			if tracing.Enabled {
				span := tracing.Begin(tracing.KindInput, cid, int64(len(data)))
				t.typed(data)
				tracing.End(tracing.KindInput, cid, span, int64(len(data)), 0)
				continue
			}
			t.typed(data)
			continue
		}
		f := classifyText(data)
		if tracing.Enabled && (f.resize != nil || f.scroll != nil) {
			kind := int64(1)
			if f.scroll != nil {
				kind = 2
			}
			tracing.Instant(tracing.KindCtrl, cid, kind, 0)
		}
		switch {
		case f.resize != nil:
			att.SetSize(f.resize.cols, f.resize.rows)
			t.ctrl.push(ctrlEvent{resize: true})
		case f.scroll != nil:
			t.ctrl.push(ctrlEvent{scroll: f.scroll})
		case observer:
			// An unrecognized frame from an observer is dropped, not typed. Not
			// counted either: nothing an observer sends was ever going to be
			// queued, so it is refused by design rather than lost to a full queue.
		case f.input != nil:
			t.typed(f.input)
		}
	}
	close(t.stop)
	<-tickDone
}

// ticker is one client's render loop.
type ticker struct {
	cid  uint64 // trace connection id
	conn *ws.Conn
	win  *shelld.Window
	att  *shelld.Attacher
	// observer clients never write to the program, so a scroll control on the
	// alternate screen synthesizes nothing.
	observer bool
	ctrl     *ctrlQueue
	stop     chan struct{}
	// unreachable is set when run gave up writing to the client, and tells the
	// read loop to stop typing what the client sends.
	unreachable atomic.Bool

	mode clientMode
	// view is this client's claim on the content it is reading, registered
	// exactly while it is paused: a peer about to destroy that content hands the
	// view a private copy of history first, and the client reads that copy from
	// then on. Nil while the client follows the live tail.
	view *shelld.PausedView
	// live frames the live view: cells, the cursor, and when to settle.
	live           *shelld.LiveFramer
	lastSentPaused *shelld.GridSnapshot
	pausedScratch  *shelld.GridSnapshot
	// renderBuf and frameBuf are reused across ticks: a client of a busy
	// window would otherwise leave several frame-sized allocations of garbage
	// per tick, and garbage is what grows Go's heap toward twice its live
	// size between collections.
	renderBuf, frameBuf []byte
	// pausedGen records the grid generation lastSentPaused was captured at;
	// with an unchanged generation and view the diff is empty and the
	// snapshot need not be rebuilt.
	pausedGen    uint64
	pausedOK     bool
	pausedOffset uint32
	// pausedSource is where lastSentPaused was captured from: the frozen copy,
	// or nil for the live grid. A change of source invalidates the baseline, and
	// a frozen source makes the grid's generation irrelevant — a copy cannot
	// change, so an unchanged view needs no frame however busy the window is.
	pausedSource *shelld.FrozenHistory
	// pushed is the grid's HistoryPushed as of the last reanchor. Signed, and
	// differenced rather than compared: see vt.Grid.HistoryPushed.
	pushed int64

	// pause times sleep and settled; a timer per call was most of what
	// a tick loop still allocated.
	pause *time.Timer

	// droppedInput counts bytes of input sent for this client that the window
	// could not queue, cumulatively for the connection. It is per client rather
	// than per window because the client that typed is the one that has to be
	// told: its keystrokes are the ones that vanished.
	//
	// Both of these are atomic because typed has two callers on two goroutines:
	// the read loop for what the client sends, and the tick loop for the keys a
	// scroll is translated into when a full-screen application owns scrolling.
	droppedInput atomic.Uint64
	// droppedLogged records that the first drop has been logged, so a client
	// whose program stopped reading does not write a log line per keystroke.
	// Compare-and-swapped rather than tested and set, so the line is written
	// once even when both goroutines drop at the same moment.
	droppedLogged atomic.Bool
	// lastDropped is the count the last status frame carried, so a drop sends a
	// frame. The tick loop is its only accessor, so it needs nothing. Without it
	// the count would sit unsent until the scroll state happened to change,
	// which for a client typing into a wedged window is never.
	lastDropped uint64

	lastSend time.Time
	// paceSched is the rate limiter's schedule position; see paceWait. The zero
	// value gives a fresh client its full burst allowance, which is what the
	// backdated lastSend below asks for too.
	paceSched time.Time
	// cursorHidden: the client's cursor is kept hidden until output settles.
	// In the live view it is LiveFramer.Bursting after every frame.
	cursorHidden bool
	haveStatus   bool
	// lastStatus is render's whole status tuple, geometry included, so the
	// re-emit test below covers an effective-size change without a second
	// comparison to keep in step with it. Widening this array is what makes a
	// resize reach the client: the loop already wakes on one (setEffectiveSize
	// notifies), but every other tracked value can hold still across it, and a
	// tuple that ended at alt would find nothing changed and send nothing.
	lastStatus [6]uint32
}

// typed writes bytes to the program on this client's behalf and accounts for the
// ones the window would not take. Every write of client input goes through it:
// the read loop's, and the tick loop's when it translates a scroll into cursor
// keys for a full-screen application.
//
// The window refuses input once the program has stopped reading and its queue or
// the shared byte budget is full, and neither caller has anywhere to put the
// bytes: blocking would stop the read loop serving the resize and scroll controls
// that share it, and stop the tick loop rendering — and retrying cannot help
// until the program reads. So the write is abandoned — but the client is told,
// which is the part that was missing. A client whose keystrokes are discarded
// with an unchanging screen and no word from the server cannot tell that from a
// program that is merely busy, and it is the only party that knows what it sent
// and could send it again.
//
// Errors other than a full queue mean the window is gone; the count still covers
// them, since from the client's side the distinction is the same loss and the
// connection is about to close anyway.
func (t *ticker) typed(data []byte) {
	err := t.win.WriteInput(data)
	if err == nil {
		return
	}
	t.droppedInput.Add(uint64(len(data)))
	// The count is only read where the status frame is written, and a refused
	// write produces no output, so nothing else would wake the tick loop: the
	// notice would sit until the idle poll came round, up to idlePoll — a second
	// of a client believing its keystrokes are in flight.
	//
	// What bounds the frames this lets out is not the loop's pacing, which only
	// advances on a rendered frame and so does not see a status-only pass: it is
	// that notify holds one token, so every drop arriving while the loop is awake
	// coalesces into the next single pass, and that the frame is written only when
	// the count has moved. The worst case is one text frame of about 80 bytes per
	// client write the window refused, at the client's own typing rate; measured
	// over fifty back-to-back refused writes it is 15 frames and 1.2 kB, because
	// writes arriving while the loop is awake coalesce into one.
	//
	// From the tick loop the wake is redundant rather than wrong: that pass writes
	// the frame itself, and the token costs one further pass that finds nothing.
	t.ctrl.wake()
	// Once per connection: a wedged program refuses every keystroke, and a line
	// each would bury the one that says why. Swapped rather than tested and set,
	// because the two callers are different goroutines.
	if t.droppedLogged.CompareAndSwap(false, true) {
		slog.Warn("dropping client input the window cannot queue",
			"window", t.win.Name(), "bytes", len(data), "err", err)
	}
}

// frozen is the private copy of history this client reads, or nil while what it
// reads is still in the live grid. Call it under the VT lock.
func (t *ticker) frozen() *shelld.FrozenHistory { return t.view.Frozen() }

// pauseView registers the client's claim on the content it is reading. It runs
// as the client enters the paused state and not a tick later, because the events
// it is a claim against are a peer's, and a peer can land one between the two.
// Call it under the VT lock.
func (t *ticker) pauseView(g *shelld.VtGrid) {
	if t.view == nil {
		t.view = g.NewPausedView()
	}
}

// resumeView drops the claim, on every path that leaves the paused state: what
// is left registered goes on being copied for a client that no longer reads it.
// Call it under the VT lock.
//
// It drops the paused baseline with the claim rather than leaving that to each
// caller. A ticker with no claim cannot send another paused frame without first
// registering a new one, so the baseline is already unusable — and pausedSource is
// the last thing holding a frozen copy once the view has let go of it. There are
// four callers, one of them the resize branch of handleCtrl, which returns before
// reaching the end of that function: a second obligation each caller has to
// remember separately is one a caller will eventually be written without.
func (t *ticker) resumeView() {
	t.view.Release()
	// Not merely for tidiness: pauseView is a no-op while t.view is non-nil, so a
	// husk left here would silently stop the client's next pause from registering a
	// claim at all, and the next peer to clear would destroy what it was reading.
	t.view = nil
	t.invalidatePaused()
}

// reanchor keeps a paused view on the content it shows: offsets count lines
// back from the live tail, so every line pushed into history moves that
// content one line further back, and every line a taller screen pulls back
// moves it one line nearer.
//
// A client reading a frozen copy follows the grid only as far as the instant the
// copy was taken. The correction for what was pushed between its last tick and
// that instant is still owed — it is what puts the offset in the copy's own frame,
// and without it a line that landed in the gap leaves the client one row off what
// it was reading. Nothing pushed after the copy moves its rows, so once that
// reading is recorded every later call differences to zero.
func (t *ticker) reanchor(g *shelld.VtGrid) {
	p, maxOff := g.HistoryPushed(), int64(g.ScrollbackLen())
	if f := t.frozen(); f != nil {
		p, maxOff = f.Pushed(), int64(f.HistoryLen())
	}
	if t.mode.paused {
		// The count is a net figure that can sit below zero while reflowed rows
		// are back on screen, so this differences the two readings rather than
		// comparing them: a dip below the held reading is an ordinary negative
		// delta, and the rows that caused it coming back push the count up
		// through the reading again.
		//
		// The positive arm bounds the result by the history that exists, so a
		// client whose content has aged out of history resumes from the oldest row
		// still held rather than from wherever an unbounded count had carried it.
		// The negative arm bounds what it subtracts by the offset instead, which
		// keeps a client off the far side of the live tail but does not keep the
		// offset inside the history: past the scrollback cap a push counts without
		// adding a row, so a gap holding both pushes and a grow moves the count by
		// less than it moves the history, and the offset is left above the oldest
		// row there is. The clamp at the end of render's paused branch is what puts
		// that back, and reanchor_test.go's cap test is what says the state it puts
		// back is reachable.
		//
		// Neither arm is what keeps a read in range: the two readers of the stored
		// offset that draw from it bound it again on the way past — render before it
		// snapshots, and the status tuple — so an offset left outside the history
		// still shows the client a correct view. SnapshotInto is not a third: it is
		// handed the value render has already bounded, never the stored one, so it
		// cannot be relied on to bound anything. applyScroll is the one
		// reader that does not, because it bounds what it computes from the offset
		// rather than the offset itself, and that is where the whole cost lands: a
		// client scrolls down by the distance the status tuple reported, which is
		// measured from the bound, so it comes up short of the tail by however far
		// the stored offset was outside it and stays paused on the oldest row there
		// is. That is what makes the stored offset a position the client has to be
		// able to scroll out of rather than only a number frames are drawn from.
		switch d := p - t.pushed; {
		case d > 0:
			t.mode.offset = uint32(min(int64(t.mode.offset)+d, maxOff))
		case d < 0: // a taller screen pulled lines back out of history
			t.mode.offset -= uint32(min(int64(t.mode.offset), -d))
		}
	}
	t.pushed = p
}

func (t *ticker) invalidateLive() { t.live.Invalidate() }

// invalidatePaused drops what this client is believed to be showing of a paused
// view, so the next paused frame is a full repaint rather than a diff against a
// baseline the client no longer has on screen.
//
// It releases pausedSource with the rest of the baseline. That pointer is the
// only thing in a ticker that holds a frozen copy once the view itself is
// released, and a copy is the whole of a window's history — so a client that
// scrolled back once and has been live ever since would otherwise pin megabytes
// for as long as it stays attached.
func (t *ticker) invalidatePaused() {
	t.lastSentPaused = shelld.EmptySnapshot()
	t.pausedScratch = &shelld.GridSnapshot{}
	t.pausedOK = false
	t.pausedSource = nil
}

// run is the render loop, until the client is told to stop, the window's shell
// exits, or a write to the client fails. gaveUp reports the last: the client is
// connected but cannot be reached, because the write timed out or the
// connection broke.
func (t *ticker) run() (gaveUp bool) {
	t.lastSend = time.Now().Add(-maxInterval)
	t.lastSentPaused = shelld.EmptySnapshot()
	t.pausedScratch = &shelld.GridSnapshot{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	t.pause = time.NewTimer(time.Hour)
	t.pause.Stop()
	defer t.pause.Stop()
	// The loop returns from a dozen places; a claim left behind would have the
	// window copying history for a client that has gone.
	defer func() {
		t.win.LockVT()
		t.resumeView()
		t.win.UnlockVT()
	}()
	// The dirty channel is armed before each render, so output that lands
	// while rendering wakes the next wait instead of being lost.
	dirty := t.win.Dirty().Wait()

	for first := true; ; first = false {
		if wait := paceWait(t.paceSched, time.Now()); wait > 0 {
			if !t.sleep(wait) {
				return false
			}
		}
		// The window's notifier (output, exit, and effective-size changes all
		// fire it) and control events drive the loop. No tick follows a frame:
		// the dirty channel is armed before rendering, so a change that lands
		// while rendering, writing or pacing wakes this wait at once, and one
		// that arrives with no notification waits for the idle grid instead of
		// the old post-frame tick (idleWait).
		//
		// Arming before rendering is what makes a wait as long as idlePoll safe,
		// and the length of that wait is the whole per-idle-client cost. The
		// shape to avoid is deriving the wait from the time of the last *send*
		// instead: once a client has been quiet for longer than its own pacing
		// interval such a wait is always already expired, so the loop stops
		// being paced by its own constants and starts polling at whatever its
		// runtime's timer granularity happens to be, renders a diff, finds it
		// empty, and goes round again. Here one client attached to a window
		// running `sleep 3600` costs under 1 ms of CPU over five seconds across 5
		// to 15 wakes a second, which is 0.01% of a core (seven reps).
		//
		// The other half is that idleWait is aligned to a process-wide epoch
		// rather than to each loop's own last wake, so idle clients wake on the
		// same instants and the runtime serves them together: sixteen idle clients
		// cost 2.8 ms and 23 wakes a second, so sixteen times the clients buys
		// under twice the wakes. Per-client deadlines would make that sixteen
		// separate timers, which is what idleEpoch's comment is about.
		//
		// Both figures need per-thread sum_exec_runtime
		// (/proc/<pid>/task/<tid>/schedstat field 0) to see at all: utime+stime is
		// quantised to one 10 ms tick per process and reads a flat zero here,
		// which is a floor of the instrument and not a measurement of nothing.
		// They also need sampling to begin only after each client's first frame
		// has been drained. An attach renders a full frame, and counting that as
		// idle overstates the steady state several fold and makes the cost look as
		// though it grows per client when what grows is the number of attaches.
		// TestAttachIdleWakeupsDoNotScaleWithClients is the standing pin, and it
		// bounds the same quantity (voluntary context switches) at 64 clients.
		wait, armed := nextWait(first, t.cursorHidden, t.lastSend, time.Now())
		timer.Reset(wait)

		gotDirty := false
		var waitStart time.Time
		if tracing.Enabled {
			waitStart = time.Now()
		}
		reason := tracing.WakeTimer
		select {
		case <-dirty:
			gotDirty, reason = true, tracing.WakeDirty
		case <-t.ctrl.notify:
			reason = tracing.WakeCtrl
			for _, ev := range t.ctrl.take() {
				t.handleCtrl(ev)
			}
		case <-timer.C:
		case <-t.stop:
			return false
		}
		timer.Stop()
		observeWake(tickWake{win: t.win, site: siteWait, reason: reason, armed: armed})
		if tracing.Enabled {
			if first {
				reason = tracing.WakeFirst
			}
			tracing.Instant(tracing.KindTickWake, t.cid, int64(reason), int64(time.Since(waitStart)))
		}
		if gotDirty {
			dirty = t.win.Dirty().Wait()
		}

		out, restore, status, alive := t.render(gotDirty)
		switch {
		case len(out) > 0 && !t.mode.paused:
			// A live frame is a complete message as LiveFramer built it.
			err := wsWrite(t.conn, t.cid, ws.OpBinary, out)
			t.renderBuf = keepBuffer(out)
			if err != nil {
				return true
			}
			t.sent(time.Now())
		case len(out) > 0:
			frame := append(t.frameBuf[:0], dec2026Begin...)
			if gotDirty && !t.cursorHidden && !bytes.HasPrefix(out, cursorHide) {
				// Keep the cursor hidden while output is streaming so it never
				// visibly jumps between intermediate positions.
				frame = append(frame, cursorHide...)
			}
			frame = append(frame, out...)
			if gotDirty {
				frame = suppressCursorShow(frame)
				t.cursorHidden = true
			}
			frame = append(frame, dec2026End...)
			err := wsWrite(t.conn, t.cid, ws.OpBinary, frame)
			t.frameBuf, t.renderBuf = keepBuffer(frame), keepBuffer(out)
			if err != nil {
				return true
			}
			t.sent(time.Now())
		case t.cursorHidden && !gotDirty && t.settled():
			// Output went quiet: show the cursor where it now is.
			// restore is nil while paused; render says why.
			if restore != nil {
				if seq := restore(); len(seq) > 0 {
					if tracing.Enabled {
						tracing.Instant(tracing.KindCursorShow, t.cid, 0, 0)
					}
					if wsWrite(t.conn, t.cid, ws.OpBinary, seq) != nil {
						return true
					}
				}
			}
			t.cursorHidden = false
		}

		dropped := t.droppedInput.Load()
		if !t.haveStatus || t.lastStatus != status || t.lastDropped != dropped {
			t.haveStatus, t.lastStatus, t.lastDropped = true, status, dropped
			if wsWrite(t.conn, t.cid, ws.OpText,
				formatStatusFrame(status[0], status[1], status[2] != 0, status[3] != 0,
					uint16(status[4]), uint16(status[5]), dropped)) != nil {
				return true
			}
		}
		if !alive {
			return false
		}
	}
}

// nextWait returns how long a tick loop's wait lasts before its timer fires,
// and what the timer is for: the first tick at once, a burst's settle check,
// or the idle safety tick on the shared grid.
func nextWait(first, cursorHidden bool, lastSend, now time.Time) (time.Duration, waitTimer) {
	switch {
	case first:
		return 0, armFirst
	case cursorHidden:
		// A burst keeps the cursor hidden until output is quiet. The first
		// check is maxInterval after the last frame, a later one (after output
		// interrupted a settle) minInterval after the one before: with
		// cursorSettle's quiet wait, the cursor shows maxInterval+cursorSettle
		// after the last frame.
		if wait := lastSend.Add(maxInterval).Sub(now); wait > 0 {
			return wait, armSettle
		}
		return minInterval, armSettle
	}
	return idleWait(now), armIdle
}

// paceWait reports how long a frame ready at now must be held to keep the
// sustained send rate at one per minInterval.
//
// sched is where the client stands on that schedule: the instant its next
// frame would be due if it had no burst allowance left. A send is permitted
// paceCredit early, so the first frame after a quiet period and the frame
// after it both go at once, and a third waits.
//
// This is a leaky bucket kept as one timestamp rather than a token count, so
// there is no refill to run on a clock the loop does not otherwise consult:
// paceSent advances sched, and how far behind now it has fallen *is* the
// credit. lastSend cannot serve as sched, because within a burst the frames
// are deliberately closer together than the schedule.
func paceWait(sched, now time.Time) time.Duration {
	// A client that has not sent yet has no schedule and starts with the whole
	// allowance. Checked explicitly rather than left to the subtraction below:
	// the zero Time is year 1, so that difference is ~2000 years and lands
	// outside a Duration's ±292, and it yields a negative wait only because
	// Sub is documented to clamp rather than wrap. Correct, but not something
	// the next reader should have to know.
	if sched.IsZero() {
		return 0
	}
	if wait := sched.Add(-paceCredit).Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// paceSent returns the schedule after a frame sent at now. Clamping to now
// before advancing is what caps the allowance at paceCredit: an idle client
// does not bank a quantum per idle second and then flood on its next frame.
func paceSent(sched, now time.Time) time.Time {
	if sched.Before(now) {
		sched = now
	}
	return sched.Add(minInterval)
}

// arm starts pause for d. The drain matters only with
// GODEBUG=asynctimerchan=1, where a timer that fired before Stop keeps its
// value buffered and would end the next wait at once.
func (t *ticker) arm(d time.Duration) {
	if !t.pause.Stop() {
		select {
		case <-t.pause.C:
		default:
		}
	}
	t.pause.Reset(d)
}

// sent records a frame leaving at now. lastSend dates the frame, for the
// settle deadlines nextWait builds; paceSched is the rate limiter's position,
// which within a burst runs ahead of it. One setter because they are one
// event, and a send that advanced only one of them would either lose the
// cursor settle or let the allowance refill on every frame.
func (t *ticker) sent(now time.Time) {
	t.lastSend = now
	t.paceSched = paceSent(t.paceSched, now)
}

func (t *ticker) sleep(d time.Duration) bool {
	t.arm(d)
	defer t.pause.Stop()
	select {
	case <-t.pause.C:
		observeWake(tickWake{win: t.win, site: sitePace, reason: tracing.WakeTimer})
		return true
	case <-t.stop:
		return false
	}
}

// settled waits cursorSettle for further output; true when none came.
func (t *ticker) settled() bool {
	dirty := t.win.Dirty().Wait()
	t.arm(cursorSettle)
	defer t.pause.Stop()
	quiet := false
	reason := tracing.WakeStop
	select {
	case <-dirty:
		reason = tracing.WakeDirty
	case <-t.pause.C:
		quiet, reason = true, tracing.WakeTimer
	case <-t.stop:
	}
	observeWake(tickWake{win: t.win, site: siteSettle, reason: reason})
	return quiet
}

// gaveUpObserver, when set (by tests), is told when a tick loop gives up on its
// client, after serveAttach has acted on it, with the client's declared width.
var gaveUpObserver atomic.Pointer[func(win *shelld.Window, cols uint16)]

func observeGaveUp(win *shelld.Window, att *shelld.Attacher) {
	if f := gaveUpObserver.Load(); f != nil {
		cols, _ := att.Size()
		(*f)(win, cols)
	}
}

// tickObserver, when set (by tests), is told of every wake of a tick loop.
var tickObserver atomic.Pointer[func(tickWake)]

// renderHook, when set (by tests), runs at the end of every render with the VT
// lock held. Nil in production.
var renderHook atomic.Pointer[func(*shelld.VtGrid)]

func observeWake(w tickWake) {
	if f := tickObserver.Load(); f != nil {
		(*f)(w)
	}
}

// tickWake is one wake of a tick loop: of its wait (reason says what ended
// it, armed what its timer was for), of its pacing sleep, or of a settle's
// quiet wait (reason WakeTimer when output stayed quiet).
type tickWake struct {
	win    *shelld.Window
	site   wakeSite
	reason tracing.WakeReason
	armed  waitTimer
}

type wakeSite int

const (
	siteWait wakeSite = iota
	sitePace
	siteSettle
)

// waitTimer is what a tick loop's wait armed its timer for.
type waitTimer int

const (
	armIdle   waitTimer = iota // the idle safety tick
	armFirst                   // the first tick
	armSettle                  // a burst's settle check
)

func (t *ticker) handleCtrl(ev ctrlEvent) {
	if ev.resize {
		// A resize ends scroll mode and forces a full repaint.
		t.mode = clientMode{}
		t.win.LockVT()
		t.resumeView() // drops the paused baseline too
		t.win.UnlockVT()
		t.invalidateLive()
		return
	}
	op := ev.scroll
	cols, rows := t.att.Size()
	g := t.win.LockVT()
	// Resolved before applyScroll below stores this pair as the pause's frozen
	// geometry: a client that declared 0x0 would freeze at frozenCols/Rows == 0
	// and snapshotInto would hand it a 0x0 snapshot for the whole pause. It also
	// bounds the paused path's allocation, outCols*outRows CellSlots taken
	// straight from these numbers. Against the grid under this lock, not
	// Window.Cols/Rows, for the reason the hello path gives.
	rc, rr := shelld.ResolveDeclared(g, int(cols), int(rows))
	cols, rows = uint16(rc), uint16(rr)
	t.reanchor(g)
	sbLen, alt, mouse, sgr := uint32(g.ScrollbackLen()), g.AltScreenActive(), g.MouseReporting(), g.SgrMouse()
	altScroll, appCursor := g.AlternateScroll(), g.AppCursor()
	if f := t.frozen(); f != nil {
		// A frozen client reads its own copy, so the live grid says nothing about
		// how far it can scroll, and a program that has since taken the alternate
		// screen does not own this client's wheel: it is scrolling history, not
		// the program's screen.
		sbLen, alt = uint32(f.HistoryLen()), false
	}
	// The mode change and the claim that goes with it are decided inside the one
	// lock window: a peer that clears between them would destroy the content of a
	// client that had just paused and not yet claimed it.
	next, changed := applyScroll(t.mode, op, sbLen, alt, cols, rows)
	wasPaused := t.mode.paused
	if changed {
		t.mode = next
		switch {
		case next.paused:
			t.pauseView(g)
		case wasPaused:
			t.resumeView()
		}
	}
	t.win.UnlockVT()
	if alt {
		// Full-screen apps own their scrolling (the re-rendered grid never
		// passes their mouse-mode escapes on to the client): forward the wheel
		// as mouse reports when they asked for them, otherwise as arrow keys
		// unless they turned alternate scroll off.
		//
		// Through typed, like a keystroke: these are the client's scroll and it is
		// owed the same answer when the window will not take them. A wheel report
		// the program never got looks to the user exactly like one it ignored.
		if op.hasLines && op.lines != 0 && !t.observer {
			switch {
			case mouse:
				t.typed(wheelInput(op.lines, cols, rows, sgr))
			case altScroll:
				t.typed(arrowInput(op.lines, appCursor))
			}
		}
		return
	}
	if !changed {
		return
	}
	if wasPaused && !next.paused {
		// The paused baseline went with the claim, in resumeView above.
		t.invalidateLive()
	}
	// Entering the paused state needs no matching call. Every path that leaves it
	// goes through resumeView, which drops the baseline, and the only writer of one
	// is render's paused branch — so a client arriving here has no baseline to
	// invalidate. Dropping it on the way out rather than clearing it on the way in
	// is what makes that true; the reverse would leave a stale baseline readable by
	// the whole live path in between.
}

// render produces this tick's frame, a cursor-restore builder, the status
// tuple, and whether the window is still alive.
func (t *ticker) render(gotDirty bool) (out []byte, restore func() []byte, status [6]uint32, alive bool) {
	var span, lock uint64
	mode := tracing.RenderSkipped
	if tracing.Enabled {
		span = tracing.Begin(tracing.KindRender, t.cid, 0)
		lock = tracing.Begin(tracing.KindVTLockWait, t.win.TraceID(), int64(tracing.LockRender))
	}
	g := t.win.LockVT()
	defer t.win.UnlockVT()
	if tracing.Enabled {
		tracing.End(tracing.KindVTLockWait, t.win.TraceID(), lock, int64(tracing.LockRender), 0)
		defer func() {
			tracing.End(tracing.KindRender, t.cid, span, int64(len(out)), int64(g.Gen())<<4|int64(mode))
		}()
	}

	t.reanchor(g)
	// maxOff is how far back this client can scroll, which is the live history
	// while it reads the live grid and its own copy's once it has one.
	frozen := t.frozen()
	maxOff := uint32(g.ScrollbackLen())
	if frozen != nil {
		maxOff = uint32(frozen.HistoryLen())
	}
	if t.mode.backToLive(maxOff) {
		t.mode = clientMode{}
		t.resumeView() // drops the paused baseline too
		frozen, maxOff = nil, uint32(g.ScrollbackLen())
		t.invalidateLive()
	}

	gen := g.Gen()
	ac, ar := t.att.Size()
	cols, rows := int(ac), int(ar)
	if !t.mode.paused {
		var rendered, full bool
		out, rendered, full = t.live.Frame(t.renderBuf[:0], g, cols, rows, gotDirty)
		switch {
		case full:
			mode = tracing.RenderFull
		case rendered:
			mode = tracing.RenderDiff
		}
		// After every frame, one that sends nothing included: a hidden cursor
		// that moved starts a burst without changing a cell.
		t.cursorHidden = t.live.Bursting()
	} else {
		off := min(t.mode.offset, maxOff)
		fc, fr := int(t.mode.frozenCols), int(t.mode.frozenRows)
		unchanged := t.pausedOK && frozen == t.pausedSource && off == t.pausedOffset &&
			(frozen != nil || gen == t.pausedGen) &&
			t.lastSentPaused.Cols == fc && t.lastSentPaused.Rows == fr
		if !unchanged {
			var cur *shelld.GridSnapshot
			if frozen != nil {
				cur = frozen.SnapshotInto(t.pausedScratch, int(off), fc, fr)
				out = frozen.RenderDiff(t.renderBuf[:0], t.lastSentPaused, cur)
			} else {
				cur = g.SnapshotInto(t.pausedScratch, int(off), fc, fr)
				out = g.RenderDiffSnapshots(t.renderBuf[:0], t.lastSentPaused, cur, int(off))
			}
			mode = tracing.RenderPaused
			t.lastSentPaused, t.pausedScratch = cur, t.lastSentPaused
			t.pausedGen, t.pausedOK, t.pausedOffset, t.pausedSource = gen, true, off, frozen
		}
		// Written back, and not only read: reanchor's negative arm can leave the
		// offset above the history that is left, and a client scrolls down from
		// what is stored here by the distance the status tuple reported — which is
		// measured from the bound, so an offset outside it never reaches the tail.
		t.mode.offset = off
	}

	switch {
	case len(out) > 0 || !t.cursorHidden || gotDirty:
	case !t.mode.paused:
		restore = func() []byte {
			// Runs after the settle wait: output may have landed since this
			// render, and Settle sends it before showing the cursor.
			g := t.win.LockVT()
			defer t.win.UnlockVT()
			ac, ar := t.att.Size()
			seq := t.live.Settle(t.renderBuf[:0], g, int(ac), int(ar))
			t.renderBuf = keepBuffer(seq)
			return seq
		}
	default:
		// None while paused. The restore is the live cursor, and a paused frame
		// draws history with the cursor hidden: painting it there leaves it on top
		// of the scrollback, and nothing hides it again, since a paused diff sends
		// ?25l only when its own view's visibility changes. Every way back to the
		// live view repaints in full, and that frame carries the cursor.
	}

	status[1] = maxOff
	// The grid's own size, read here rather than from Window.Cols/Rows so it comes
	// from the same held lock as the frame this tuple accompanies. It is the live
	// grid's even for a client reading a frozen copy: the number answers "how big
	// is the PTY you are cropping", which is what the floor makes non-obvious, and
	// a paused client needs it for the same reason a live one does.
	status[4], status[5] = uint32(g.Cols()), uint32(g.Lines())
	// alt tells the client to send its scroll keys to the application instead of
	// to us, so it has to agree with who will actually act on them. A client
	// reading a frozen copy scrolls that copy — handleCtrl decides its scrolling
	// against the copy and ignores the alternate screen — so telling it to hand
	// the wheel to a program it is not looking at would leave the copy
	// unscrollable.
	if g.AltScreenActive() && frozen == nil {
		status[3] = 1
	}
	if t.mode.paused {
		status[0], status[2] = min(t.mode.offset, maxOff), 1
	}
	if f := renderHook.Load(); f != nil {
		// A test's seam into the window this render has already snapshotted,
		// with the VT lock still held: the one place a change can land after a
		// frame's snapshot and before its write.
		(*f)(g)
	}
	return out, restore, status, t.win.IsAlive()
}
