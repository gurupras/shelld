package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
)

// oldLiveTicker is the daemon's live-view policy before LiveFramer
// (internal/daemon/attach.go at 1b35ded, ticker.run and ticker.render, live
// mode only), over the frozen reference renderer: the stream a client
// received before.
type oldLiveTicker struct {
	last         *GridSnapshot
	gen          uint64
	ok           bool
	cursorHidden bool
}

func (o *oldLiveTicker) tick(g *VtGrid, cols, rows int, dirty bool) []byte {
	if o.last == nil {
		o.last = EmptySnapshot()
	}
	if o.ok && g.Gen() == o.gen && o.last.Cols == cols && o.last.Rows == rows {
		return nil
	}
	cur := refSnapshotInto(nil, g.term, 0, cols, rows)
	prev := o.last
	var out []byte
	total := cur.Cols * cur.Rows
	n := 0
	geom := prev.Cols == cur.Cols && prev.Rows == cur.Rows && len(prev.Cells) > 0
	if geom {
		for i := range cur.Cells {
			if prev.Cells[i] != cur.Cells[i] {
				n++
			}
		}
	}
	if geom && total > 0 && n > total/2 {
		out = refRenderFull(nil, g.term, 0, cols, rows)
	} else {
		out = refRenderDiff(nil, prev, cur, g.term, 0)
	}
	o.last, o.gen, o.ok = cur, g.Gen(), true
	if len(out) == 0 {
		return nil
	}
	frame := append([]byte(nil), syncBegin...)
	if dirty && !o.cursorHidden && !bytes.HasPrefix(out, cursorHide) {
		frame = append(frame, cursorHide...)
	}
	frame = append(frame, out...)
	if dirty {
		if bytes.HasSuffix(frame, cursorShow) {
			frame = append(frame[:len(frame)-len(cursorShow)], cursorHide...)
		}
		o.cursorHidden = true
	}
	return append(frame, syncEnd...)
}

func (o *oldLiveTicker) settle(g *VtGrid) []byte {
	if !o.cursorHidden {
		return nil
	}
	o.cursorHidden = false
	term := g.term
	if term.Mode()&vt.ModeShowCursor == 0 {
		return nil
	}
	grid := term.Grid()
	p := grid.Cursor.Point
	row := min(max(p.Line, 0), max(grid.ScreenLines()-1, 0))
	col := min(p.Column, max(grid.Columns()-1, 0))
	return fmt.Appendf(nil, "\x1b[?2026h\x1b[%d;%dH\x1b[?25h\x1b[?2026l", row+1, col+1)
}

// liveEvent is one output write: its time on the replay's clock and bytes.
type liveEvent struct {
	at   time.Duration
	data []byte
}

// The daemon's schedule, simulated: frames at least simMinInterval apart, and
// a settle once output has been quiet for simSettle (maxInterval, then the
// cursorSettle wait).
//
// The spacing rule is this file's own, not a call into the daemon, and it no
// longer matches it: attach.go paces with an allowance (paceBurst) that lets one
// frame go a full minInterval early, so the daemon can emit two adjacent frames
// where this model would not. The divergence is deliberate. What these tests
// compare is the old renderer against LiveFramer, and the schedule is the
// fixture they are compared over, so it only has to be the same for both and
// plausible -- it does not have to track attach.go. The consequence to keep in
// mind is the other direction: nothing here exercises paceWait, so these tests
// cannot fail for a pacing change and their passing is not evidence about one.
// Pacing is covered in internal/daemon (ticker_test.go, liveframe_test.go).
const (
	simMinInterval = 8 * time.Millisecond
	simSettle      = 66 * time.Millisecond
)

// policyView is one client of the replayed grid, served by both policies.
type policyView struct {
	cols, rows int
	old        oldLiveTicker
	new        LiveFramer
	oc, nc     *liveTerm
	steps      []policyStep
	oldBytes   int
	newBytes   int
	// oldStale: after a race the old policy's client is wrong (its settle
	// shows the cursor over cells it did not send, and a later empty diff
	// leaves it there), so the clients are not compared until they and both
	// policies' records of what they sent agree again.
	oldStale bool
}

// policyStep is what both clients show after one step (a tick or a settle).
type policyStep struct {
	settle, dirty bool
	settled       bool // the new policy settled: it was bursting
	oldMsg        []byte
	newMsg        []byte
	old, new      cursorState
	app           cursorState // the application's cursor, clamped to the view
}

type cursorState struct {
	visible  bool
	row, col int
}

func termCursor(term *vt.Term, cols, rows int) cursorState {
	g := term.Grid()
	p := g.Cursor.Point
	return cursorState{term.Mode()&vt.ModeShowCursor != 0, min(max(p.Line, 0), max(rows-1, 0)), min(p.Column, max(cols-1, 0))}
}

// policyRun is one replay: a grid, its views and the splitting schedule.
type policyRun struct {
	t     *testing.T
	name  string
	g     *VtGrid
	views []*policyView
	// crossings counts split parts that reached the grid in a later tick than
	// the part before them; splits counts all splits.
	crossings, splits int
	steps             int
	// races counts events that landed between a settle's wait and the settle.
	races int
}

// splitPoints returns where an event is split: before every cursor show or
// hide, so a cursor move and the show that follows it, or a hide and the
// drawing after it, reach the grid in different ticks.
func splitPoints(data []byte) []int {
	var ks []int
	for _, seq := range [][]byte{cursorShow, cursorHide} {
		for off := 0; ; {
			i := bytes.Index(data[off:], seq)
			if i < 0 {
				break
			}
			if k := off + i; k > 0 {
				ks = append(ks, k)
			}
			off += i + len(seq)
		}
	}
	return ks
}

// replay feeds evs at speed through both policies for every view. With split,
// an event is split before each cursor show and hide (and at random once in
// eight events at a random byte), and the rest of it arrives 1 ms after the
// tick that took the first part, so the parts land in different ticks; and
// once in sixteen settles the next event lands just before the settle.
func (p *policyRun) replay(evs []liveEvent, speed float64, rng *rand.Rand, split bool) {
	type queued struct {
		at    time.Duration
		data  []byte
		split bool // already split at random
		// splitAt is the tick that took this event's first part, or -1.
		splitAt int
	}
	q := make([]queued, len(evs))
	for i, e := range evs {
		q[i] = queued{at: time.Duration(float64(e.at) / speed), data: e.data, splitAt: -1}
	}
	ticks := 0
	p.step(false, false) // the attach's first frame
	lastFrame := time.Duration(-1 << 62)
	var floor time.Duration // events keep their order: none is earlier than a delayed part
	bursting := func() bool {
		for _, v := range p.views {
			if v.old.cursorHidden || v.new.Bursting() {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(q); {
		at := max(q[i].at, floor)
		if bursting() && at-lastFrame >= simSettle {
			if split && rng.Intn(16) == 0 {
				// Output that lands after the settle's quiet wait, before
				// the settle takes the lock: the settle must send it.
				p.g.Advance(q[i].data)
				i++
				p.races++
				for _, v := range p.views {
					v.oldStale = true
				}
			}
			p.step(false, true)
			continue
		}
		// A tick at max(output, lastFrame+minInterval) takes every event that
		// arrived by then.
		tickAt := max(at, lastFrame+simMinInterval)
		for i < len(q) && max(q[i].at, floor) <= tickAt {
			e := &q[i]
			k := 0
			if split {
				if ks := splitPoints(e.data); len(ks) > 0 {
					k = ks[0]
					for _, x := range ks {
						k = min(k, x)
					}
				} else if !e.split && len(e.data) > 8 && rng.Intn(8) == 0 {
					k = 1 + rng.Intn(len(e.data)-1)
					e.split = true
				}
			}
			if e.splitAt >= 0 && e.splitAt != ticks {
				p.crossings++
			}
			if k == 0 {
				p.g.Advance(e.data)
				i++
				continue
			}
			p.g.Advance(e.data[:k])
			e.data, e.splitAt = e.data[k:], ticks
			e.at = tickAt + time.Millisecond
			floor = e.at
			p.splits++
			break
		}
		p.step(true, false)
		ticks++
		lastFrame = tickAt
	}
	if bursting() {
		p.step(false, true)
	}
}

// step runs one tick (or settle) of both policies for every view.
func (p *policyRun) step(dirty, settle bool) {
	p.steps++
	for _, v := range p.views {
		var om, nm []byte
		settled := false
		if settle {
			om = v.old.settle(p.g)
			if settled = v.new.Bursting(); settled {
				nm = v.new.Settle(nil, p.g, v.cols, v.rows)
			}
		} else {
			om = v.old.tick(p.g, v.cols, v.rows, dirty)
			nm, _, _ = v.new.Frame(nil, p.g, v.cols, v.rows, dirty)
		}
		v.oc.feed(om)
		v.nc.feed(nm)
		v.oldBytes += len(om)
		v.newBytes += len(nm)
		st := policyStep{settle: settle, settled: settled, dirty: dirty, oldMsg: om, newMsg: nm,
			old: termCursor(v.oc.term, v.cols, v.rows), new: termCursor(v.nc.term, v.cols, v.rows),
			app: termCursor(p.g.term, v.cols, v.rows)}
		k := len(v.steps)
		v.steps = append(v.steps, st)
		p.check(v, k, settle)
	}
}

// check tests one step of a view:
//   - the clients' cells look the same, and the new client's pen is the
//     default one;
//   - where the new client shows the cursor, and wherever it is not bursting,
//     its cursor is on the application's cell;
//   - where the old client shows the cursor, the new one shows it at the same
//     cell;
//   - after a settle, both show exactly what the application does: cells,
//     cursor visibility and cell;
//   - after a tick woken by output, the new client shows the cursor only if
//     it showed it at the same cell after the previous step: while output
//     flows the cursor is kept where it was or hidden, and never appears at a
//     position the application only passed through. (The old client hid it;
//     keeping it is the one difference, and after a settle there is none.)
func (p *policyRun) check(v *policyView, k int, settle bool) {
	p.t.Helper()
	s := v.steps[k]
	fail := func(format string, args ...any) {
		p.t.Helper()
		p.t.Fatalf("%s %dx%d step %d (settle %v dirty %v): %s\n old: %q\n new: %q", p.name, v.cols, v.rows, k, s.settle, s.dirty,
			fmt.Sprintf(format, args...), trunc(string(s.oldMsg)), trunc(string(s.newMsg)))
	}
	nv := clientView(v.nc.term)
	cellsDiff := visibleDiff(nv, clientView(v.oc.term))
	if v.oldStale && cellsDiff == "" && s.old == s.new && v.old.last != nil && v.new.last != nil &&
		v.old.last.CursorVisible == v.new.last.CursorVisible && v.old.last.CursorRow == v.new.last.CursorRow &&
		v.old.last.CursorCol == v.new.last.CursorCol && slotsEqual(v.old.last.Cells, v.new.last.Cells) {
		v.oldStale = false
	}
	stale := v.oldStale
	if cellsDiff != "" && !stale {
		fail("cells: %s", cellsDiff)
	}
	if v.nc.term.Grid().Cursor.Template != defaultPen {
		fail("the new client's pen is not the default")
	}
	// A tick takes all output; a settle step a view did not settle can follow
	// a race, and its client is then as up to date as its last tick.
	current := !settle || s.settled
	if current && (s.new.visible || !v.new.Bursting()) && (s.new.row != s.app.row || s.new.col != s.app.col) {
		fail("new's cursor (visible=%v, bursting=%v) is at %d,%d; the application's is at %d,%d", s.new.visible, v.new.Bursting(), s.new.row, s.new.col, s.app.row, s.app.col)
	}
	if s.old.visible && s.new != s.old && !stale {
		fail("old shows the cursor at %d,%d; new %+v", s.old.row, s.old.col, s.new)
	}
	if s.dirty && s.new.visible && (k == 0 || v.steps[k-1].new != s.new) {
		fail("after a tick woken by output, new shows the cursor at %d,%d; after the previous step %+v", s.new.row, s.new.col, v.steps[max(k-1, 0)].new)
	}
	if s.settled {
		if (s.old.visible != s.new.visible || s.new.visible && s.new != s.old) && !stale {
			fail("after a settle: old %+v, new %+v", s.old, s.new)
		}
		if d := visibleDiff(nv, snapshotInto(nil, p.g.term, 0, v.cols, v.rows)); d != "" {
			fail("after a settle the cells differ from the application's: %s", d)
		}
		if s.new != s.app {
			fail("after a settle: new %+v, the application's %+v", s.new, s.app)
		}
	}
}

func newPolicyRun(t *testing.T, name string, cols, rows int, views [][2]int) *policyRun {
	p := &policyRun{t: t, name: name, g: NewVtGrid(cols, rows)}
	for _, sz := range views {
		p.views = append(p.views, &policyView{cols: sz[0], rows: sz[1], oc: newLiveTerm(sz[0], sz[1]), nc: newLiveTerm(sz[0], sz[1])})
	}
	return p
}

// liveFuzzChunk is fuzzChunk with cursor traffic mixed in: shows, hides,
// absolute and relative moves, and hide-draw-return-show redraws.
func liveFuzzChunk(rng *rand.Rand, cols, rows int) []byte {
	cup := func() string { return fmt.Sprintf("\x1b[%d;%dH", 1+rng.Intn(rows+2), 1+rng.Intn(cols+2)) }
	switch rng.Intn(10) {
	case 0:
		return cursorShow
	case 1:
		return cursorHide
	case 2:
		return []byte(cup())
	case 3:
		return fmt.Appendf(nil, "\x1b[%dC", rng.Intn(cols))
	case 4:
		c := cup()
		return []byte("\x1b[?25l" + cup() + "draw" + c + "\x1b[?25h")
	}
	return fuzzChunk(rng)
}

// TestLiveFramerFuzzMatchesOldPolicy replays the old policy against the
// LiveFramer over fuzzed escape streams with cursor traffic, at random gaps, for a client of
// the grid's size and cropped and padded ones.
func TestLiveFramerFuzzMatchesOldPolicy(t *testing.T) {
	var crossings int
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cols, rows := 10+rng.Intn(70), 3+rng.Intn(30)
		var evs []liveEvent
		var at time.Duration
		for k := 0; k < 300; k++ {
			at += []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 12 * time.Millisecond, 40 * time.Millisecond, 100 * time.Millisecond}[rng.Intn(6)]
			evs = append(evs, liveEvent{at, liveFuzzChunk(rng, cols, rows)})
		}
		p := newPolicyRun(t, fmt.Sprintf("fuzz seed %d", seed), cols, rows,
			[][2]int{{cols, rows}, {max(cols-7, 2), max(rows-3, 1)}, {cols + 5, rows + 2}})
		p.replay(evs, 1, rng, true)
		crossings += p.crossings
	}
	if crossings == 0 {
		t.Fatal("no split crossed a tick")
	}
}
