package shelld

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// liveTerm is a terminal fed one byte stream: a client fed messages, or the
// truth fed the application's output.
type liveTerm struct {
	term *vt.Term
	proc *vt.Processor
}

func newLiveTerm(cols, rows int) *liveTerm {
	return &liveTerm{vt.NewTerm(vt.Config{}, cols, rows, nil), vt.NewProcessor()}
}

func (c *liveTerm) feed(b []byte) { c.proc.Advance(c.term, b) }

// cursor is the terminal's cursor cell, clamped as a snapshot clamps it for a
// cols×rows view, and its visibility.
func (c *liveTerm) cursor(cols, rows int) (visible bool, row, col int) {
	g := c.term.Grid()
	p := g.Cursor.Point
	return c.term.Mode()&vt.ModeShowCursor != 0, min(max(p.Line, 0), max(rows-1, 0)), min(p.Column, max(cols-1, 0))
}

var defaultPen = vt.NewTerm(vt.Config{}, 1, 1, nil).Grid().Cursor.Template

// liveSim drives a LiveFramer the way the daemon's tick loop does, for one
// client of a grid, and checks the client against a truth terminal fed the
// application's output directly.
type liveSim struct {
	t          *testing.T
	name       string
	g          *VtGrid
	f          LiveFramer
	truth      *liveTerm
	client     *liveTerm
	cols, rows int // the client's size
	msgs       [][]byte
	bytes      int
	paused     bool
}

func newLiveSim(t *testing.T, name string, gridCols, gridRows, cols, rows int) *liveSim {
	s := &liveSim{t: t, name: name, g: NewVtGrid(gridCols, gridRows), truth: newLiveTerm(gridCols, gridRows),
		client: newLiveTerm(cols, rows), cols: cols, rows: rows}
	s.tick(false) // the attach's first frame
	return s
}

func (s *liveSim) fail(format string, args ...any) {
	s.t.Helper()
	last := ""
	if len(s.msgs) > 0 {
		last = trunc(string(s.msgs[len(s.msgs)-1]))
	}
	s.t.Fatalf("%s: %s\n last message: %q", s.name, fmt.Sprintf(format, args...), last)
}

// app lands output in the grid, as the PTY reader does.
func (s *liveSim) app(b string) {
	s.g.Advance([]byte(b))
	s.truth.feed([]byte(b))
}

// send delivers one message to the client and checks what holds after every
// message: the default pen, a shown cursor on the application's cell as of
// this message, and outside a burst the cursor there whether shown or not.
func (s *liveSim) send(out []byte) {
	s.t.Helper()
	if len(out) == 0 {
		return
	}
	s.msgs = append(s.msgs, append([]byte(nil), out...))
	s.bytes += len(out)
	s.client.feed(out)
	if s.paused {
		return
	}
	if pen := s.client.term.Grid().Cursor.Template; pen != defaultPen {
		s.fail("the client's pen is not the default after a message")
	}
	cv, cr, cc := s.client.cursor(s.cols, s.rows)
	_, tr, tc := s.truth.cursor(s.cols, s.rows)
	if (cv || !s.f.Bursting()) && (cr != tr || cc != tc) {
		s.fail("the client's cursor (visible=%v, bursting=%v) is at %d,%d; the application's is at %d,%d", cv, s.f.Bursting(), cr, cc, tr, tc)
	}
}

// tick is a tick of the loop: woken by output (dirty) or by a control event
// or timer.
func (s *liveSim) tick(dirty bool) []byte {
	s.t.Helper()
	out, _, _ := s.f.Frame(nil, s.g, s.cols, s.rows, dirty)
	s.send(out)
	return out
}

// settle is the loop's settle once output was quiet; it runs only while the
// framer says a burst hides the cursor. Whatever landed since the last tick,
// the client is then up to date.
func (s *liveSim) settle() []byte {
	s.t.Helper()
	if !s.f.Bursting() {
		return nil
	}
	out := s.f.Settle(nil, s.g, s.cols, s.rows)
	s.send(out)
	s.checkTruth("after a settle")
	return out
}

// resize changes the window and the client to cols×rows, as a single client's
// resize does: the client is repainted.
func (s *liveSim) resize(cols, rows int) {
	s.g.Resize(cols, rows)
	s.truth.term.KeepRowsOnShrink(rows)
	s.truth.term.Resize(cols, rows)
	s.client.term.Resize(cols, rows)
	s.cols, s.rows = cols, rows
	s.f.Invalidate()
}

// pause shows the client a paused view offset lines into history, as the
// daemon's paused frames do; unpause returns it to the live view.
func (s *liveSim) pause(offset int) {
	s.paused = true
	s.send(wrapSyncTest(s.g.RenderFullInto(nil, offset, s.cols, s.rows)))
}

func (s *liveSim) unpause() {
	s.paused = false
	s.f.Invalidate()
}

func wrapSyncTest(b []byte) []byte {
	return append(append(append([]byte(nil), syncBegin...), b...), syncEnd...)
}

// quiet runs what the loop does once output stops: a tick, then the settle.
// The client must then show exactly what the application shows.
func (s *liveSim) quiet() {
	s.t.Helper()
	s.tick(false)
	s.settle()
	s.checkTruth("after output settled")
	if s.f.Bursting() {
		s.fail("still bursting after the settle")
	}
}

// checkTruth fails unless the client shows what the application does: cells,
// cursor visibility and the cursor's cell.
func (s *liveSim) checkTruth(when string) {
	s.t.Helper()
	want := snapshotInto(nil, s.truth.term, 0, s.cols, s.rows)
	if d := visibleDiff(clientView(s.client.term), want); d != "" {
		s.fail("%s, cells differ from the application's: %s", when, d)
	}
	tv, tr, tc := s.truth.cursor(s.cols, s.rows)
	cv, cr, cc := s.client.cursor(s.cols, s.rows)
	if tv != cv || tr != cr || tc != cc {
		s.fail("%s: the application's cursor visible=%v at %d,%d; the client's visible=%v at %d,%d", when, tv, tr, tc, cv, cr, cc)
	}
}

// liveStep is one step of a scenario: the application's output, then what
// the loop does.
type liveStep struct {
	app string
	// do: "dirty" (a tick woken by output), "ctrl" (a tick woken by a control
	// event or timer), "settle", "none" (the output lands after the last tick,
	// before the settle takes the lock), "resize C R", "pause N", "unpause".
	do string
}

// TestLiveFramerScenarios checks the client against the truth after output
// settles, in the patterns that break a cursor model: a hidden cursor that
// moves or is shown again across ticks, output racing the settle, resizes
// and paused views mid-burst, cropped and padded clients, wide characters and
// pending wraps at the last column.
func TestLiveFramerScenarios(t *testing.T) {
	cases := []struct {
		name                 string
		gridCols, gridRows   int
		clientCols, clientRo int
		steps                []liveStep
	}{
		{name: "hide-draw-return-then-show", steps: []liveStep{
			{"\x1b[5;10H", "dirty"}, {"", "settle"},
			{"\x1b[?25l\x1b[1;1Hhello\x1b[5;10H", "dirty"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "hidden-move-settle-then-show", steps: []liveStep{
			{"\x1b[?25l", "dirty"}, {"", "settle"},
			{"\x1b[8;20H", "dirty"}, {"", "settle"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "hidden-move-ctrl-then-show", steps: []liveStep{
			{"\x1b[?25l", "dirty"}, {"", "settle"},
			{"\x1b[8;20H", "ctrl"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "settle-race-then-return", steps: []liveStep{
			{"\x1b[3;3H", "dirty"},
			{"\x1b[6;6Hx", "none"}, {"", "settle"},
			{"\x1b[3;3H", "dirty"},
		}},
		{name: "settle-race-cursor-only", steps: []liveStep{
			{"\x1b[3;3H", "dirty"},
			{"\x1b[6;6H", "none"}, {"", "settle"},
			{"\x1b[3;3H", "dirty"},
		}},
		{name: "hidden-draw-empty-frame-then-show", steps: []liveStep{
			{"\x1b[?25l\x1b[1;1H", "dirty"}, {"", "settle"},
			{"\x1b[8;20H", "dirty"},
			{"", "ctrl"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "spinner", steps: []liveStep{
			{"\r\x1b[2K* Thinking 1", "dirty"},
			{"\r\x1b[2K* Thinking 22", "dirty"},
			{"", "settle"},
			{"\r\x1b[2K* Thinking 3", "dirty"},
		}},
		{name: "resize-mid-burst", steps: []liveStep{
			{"hello\x1b[3;3Hworld", "dirty"},
			{"\x1b[5;5Hmore", "dirty"},
			{"", "resize 30 8"},
			{"\x1b[7;1Hstill", "dirty"},
		}},
		{name: "resize-mid-burst-hidden", steps: []liveStep{
			{"\x1b[?25lhello\x1b[3;3Hworld", "dirty"},
			{"", "resize 50 12"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "paused-then-live", gridRows: 5, steps: []liveStep{
			{strings.Repeat("line\r\n", 20) + "tail", "dirty"}, {"", "settle"},
			{"", "pause 3"},
			{"\x1b[?25l\r\nmore\x1b[1;1H", "dirty"},
			{"", "unpause"},
			{"", "ctrl"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "paused-mid-burst", gridRows: 5, steps: []liveStep{
			{strings.Repeat("line\r\n", 20), "dirty"},
			{"\x1b[2;2Hx", "dirty"},
			{"", "pause 4"},
			{"\x1b[3;3Hy", "none"},
			{"", "unpause"},
			{"\x1b[4;4Hz", "dirty"},
		}},
		{name: "cropped", gridCols: 40, gridRows: 10, clientCols: 25, clientRo: 6, steps: []liveStep{
			{"\x1b[2;20Hacross the crop edge", "dirty"},
			{"\x1b[9;35H", "dirty"}, {"", "settle"},
			{"\x1b[?25l\x1b[1;1Hab\x1b[9;35H", "dirty"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "padded", gridCols: 20, gridRows: 5, clientCols: 30, clientRo: 9, steps: []liveStep{
			{"\x1b[5;20Hz", "dirty"}, {"", "settle"},
			{"\x1b[?25l\x1b[1;1Hab\x1b[5;20H", "dirty"},
			{"\x1b[?25h", "ctrl"},
		}},
		{name: "wide-in-last-column", gridCols: 20, gridRows: 5, steps: []liveStep{
			{"\x1b[1;19H中", "dirty"}, {"", "settle"},
			{"\x1b[2;20H中x", "dirty"},
			{"\x1b[3;1H", "dirty"},
		}},
		{name: "wide-then-gap", steps: []liveStep{
			{"\x1b[1;1H中文 abc", "dirty"}, {"", "settle"},
			{"\x1b[1;1H字文 abd", "dirty"},
		}},
		{name: "pending-wrap", steps: []liveStep{
			{"\x1b[1;35Habcdef", "dirty"}, {"", "settle"},
			{"g", "dirty"},
		}},
		{name: "pending-wrap-hidden-show", steps: []liveStep{
			{"\x1b[?25l\x1b[1;35Habcdef", "dirty"}, {"", "settle"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "cursor-only-move-visible", steps: []liveStep{
			{"abc", "dirty"}, {"", "settle"},
			{"\x1b[4;7H", "dirty"},
			{"\x1b[5;8H", "dirty"},
		}},
		{name: "cursor-only-move-hidden", steps: []liveStep{
			{"\x1b[?25labc", "dirty"}, {"", "settle"},
			{"\x1b[4;7H", "dirty"},
			{"\x1b[5;8H", "ctrl"},
		}},
		{name: "visibility-toggles-no-cells", steps: []liveStep{
			{"abc", "dirty"}, {"", "settle"},
			{"\x1b[?25l", "dirty"},
			{"\x1b[?25h", "dirty"},
			{"\x1b[?25l", "ctrl"},
			{"\x1b[?25h", "ctrl"},
			{"\x1b[?25l", "dirty"}, {"", "settle"},
			{"\x1b[?25h", "dirty"},
		}},
		{name: "styled-gap-bridge", steps: []liveStep{
			{"\x1b[31mab\x1b[0mcd\x1b[31mef\x1b[0m", "dirty"}, {"", "settle"},
			{"\x1b[1;1H\x1b[31mxy\x1b[0mcd\x1b[31mzw\x1b[0m", "dirty"},
		}},
		{name: "big-frame-hidden-while-drawing", steps: []liveStep{
			{"x", "dirty"}, {"", "settle"},
			{"\x1b[2;1H" + strings.Repeat("\x1b[31ma\x1b[32mb", 60) + "\x1b[0m\x1b[1;2H", "dirty"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gc, gr := tc.gridCols, tc.gridRows
			if gc == 0 {
				gc = 40
			}
			if gr == 0 {
				gr = 10
			}
			cc, cr := tc.clientCols, tc.clientRo
			if cc == 0 {
				cc, cr = gc, gr
			}
			s := newLiveSim(t, tc.name, gc, gr, cc, cr)
			for _, st := range tc.steps {
				if st.app != "" {
					s.app(st.app)
				}
				var a, b int
				switch {
				case st.do == "dirty" || st.do == "ctrl":
					s.tick(st.do == "dirty")
				case st.do == "settle":
					s.settle()
				case st.do == "none":
				case st.do == "unpause":
					s.unpause()
				case strings.HasPrefix(st.do, "resize"):
					fmt.Sscanf(st.do, "resize %d %d", &a, &b)
					s.resize(a, b)
				case strings.HasPrefix(st.do, "pause"):
					fmt.Sscanf(st.do, "pause %d", &a)
					s.pause(a)
				default:
					t.Fatalf("bad step %q", st.do)
				}
			}
			s.quiet()
			t.Logf("%d messages, %d bytes", len(s.msgs), s.bytes)
		})
	}
}

// TestLiveFramerBytes pins the messages of the live view's common cases.
func TestLiveFramerBytes(t *testing.T) {
	t.Run("spinner-visible-cursor", func(t *testing.T) {
		// The cursor ends where it was: one message with the changed digits,
		// drawn from the cursor's cell back to it, and no settle.
		s := newLiveSim(t, "spinner", 80, 24, 80, 24)
		s.app("\r\x1b[2K* Thinking 123")
		s.tick(true)
		s.settle()
		s.quiet()
		n := len(s.msgs)
		s.app("\r\x1b[2K* Thinking 456")
		got := s.tick(true)
		if want := "\x1b[1;12H456"; string(got) != want {
			t.Fatalf("update: %q, want %q", got, want)
		}
		if s.f.Bursting() || s.settle() != nil || s.tick(false) != nil {
			t.Fatal("an unmoved cursor started a burst")
		}
		if len(s.msgs) != n+1 {
			t.Fatalf("%d messages for one update, want 1", len(s.msgs)-n)
		}
	})
	t.Run("spinner-hidden-cursor", func(t *testing.T) {
		s := newLiveSim(t, "spinner", 80, 24, 80, 24)
		s.app("\x1b[?25l\r\x1b[2K* Thinking 123")
		s.tick(true)
		s.quiet()
		n := len(s.msgs)
		s.app("\r\x1b[2K* Thinking 456")
		if got, want := s.tick(true), "\x1b[1;12H456"; string(got) != want {
			t.Fatalf("update: %q, want %q", got, want)
		}
		s.tick(false)
		s.settle()
		if len(s.msgs) != n+1 {
			t.Fatalf("%d messages for one update, want 1", len(s.msgs)-n)
		}
	})
	t.Run("moved-cursor", func(t *testing.T) {
		// A burst: the frame hides the cursor, the settle shows it.
		s := newLiveSim(t, "moved", 80, 24, 80, 24)
		s.app("ab")
		s.tick(true)
		s.quiet()
		n := len(s.msgs)
		s.app("\x1b[3;1Hcd")
		if got, want := s.tick(true), "\x1b[3;1Hcd\x1b[?25l"; string(got) != want {
			t.Fatalf("frame: %q, want %q", got, want)
		}
		if s.tick(false) != nil {
			t.Fatal("a quiet tick sent a message")
		}
		if got, want := s.settle(), "\x1b[?25h"; string(got) != want {
			// Drawing left the client's cursor on the application's cell.
			t.Fatalf("settle: %q, want %q", got, want)
		}
		s.app("\x1b[5;9H")
		s.tick(true)
		if got, want := s.settle(), "\x1b[5;9H\x1b[?25h"; string(got) != want {
			t.Fatalf("settle after a cursor-only move: %q, want %q", got, want)
		}
		if len(s.msgs) != n+4 {
			t.Fatalf("%d messages, want 4", len(s.msgs)-n)
		}
	})
	t.Run("hidden-cursor-placed", func(t *testing.T) {
		// A hidden cursor is placed on the application's cell too: at once on
		// a frame outside a burst, at the settle after a burst.
		s := newLiveSim(t, "hidden", 80, 24, 80, 24)
		s.app("\x1b[?25l\x1b[5;5H")
		s.tick(false)
		s.quiet()
		s.app("\x1b[1;1Hab\x1b[5;5H")
		if got, want := s.tick(true), "\x1b[1;1Hab\x1b[5;5H"; string(got) != want {
			t.Fatalf("frame: %q, want %q", got, want)
		}
		s.app("\x1b[2;1Hcd\x1b[6;6H")
		if got, want := s.tick(true), "\x1b[2;1Hcd"; string(got) != want {
			t.Fatalf("burst frame: %q, want %q", got, want)
		}
		if got, want := s.settle(), "\x1b[6;6H"; string(got) != want {
			t.Fatalf("settle: %q, want %q", got, want)
		}
	})
	t.Run("typing", func(t *testing.T) {
		s := newLiveSim(t, "typing", 80, 24, 80, 24)
		s.app("$ ")
		s.tick(true)
		s.quiet()
		// The echo moves the cursor, a burst: the settle needs no move, since
		// drawing the echo left the client's cursor on the application's.
		s.app("l")
		if got, want := s.tick(true), "l\x1b[?25l"; string(got) != want {
			t.Fatalf("echo: %q, want %q", got, want)
		}
		if got, want := s.settle(), "\x1b[?25h"; string(got) != want {
			t.Fatalf("settle: %q, want %q", got, want)
		}
	})
	t.Run("unsure-width", func(t *testing.T) {
		// After a non-ASCII glyph the client's column is not trusted: a gap
		// is crossed with an absolute move, and the cursor gets one too.
		s := newLiveSim(t, "width", 80, 24, 80, 24)
		s.app("\x1b[?25l")
		s.tick(false)
		s.app("\x1b[1;1Ha\x1b[1;7Hb")
		if got, want := s.tick(false), "a\x1b[5Cb"; string(got) != want {
			t.Fatalf("ASCII: %q, want %q", got, want)
		}
		s.app("\x1b[2;1H⏺\x1b[2;5Hb\x1b[?25h\x1b[2;6H")
		if got, want := s.tick(false), "\x1b[2;1H⏺\x1b[2;5Hb\x1b[?25h"; string(got) != want {
			t.Fatalf("a gap after non-ASCII: %q, want %q", got, want)
		}
		s.app("\x1b[4;1Hx⏺")
		if got, want := s.tick(false), "\x1b[4;1Hx⏺\x1b[4;3H"; string(got) != want {
			t.Fatalf("the cursor after non-ASCII: %q, want %q", got, want)
		}
		s.app("y")
		if got, want := s.tick(false), "y"; string(got) != want {
			t.Fatalf("after the cursor's move: %q, want %q", got, want)
		}
		s.quiet()
	})
	t.Run("sync-min-boundary", func(t *testing.T) {
		for _, n := range []int{DefaultSyncMin, DefaultSyncMin + 1} {
			s := newLiveSim(t, "sync", 400, 3, 400, 3)
			s.app("\x1b[?25l")
			s.tick(false)
			// A drawing of exactly n bytes: CUP 2;1 (6 bytes), n-6 letters,
			// ending on the hidden cursor's cell.
			s.app("\x1b[2;1H" + strings.Repeat("x", n-6))
			got := s.tick(false)
			bracketed := bytes.HasPrefix(got, syncBegin) && bytes.HasSuffix(got, syncEnd)
			if body := len(got); bracketed {
				body -= len(syncBegin) + len(syncEnd)
				if body != n {
					t.Fatalf("drawing of %d bytes: bracketed body %d", n, body)
				}
			} else if body != n {
				t.Fatalf("drawing of %d bytes: message %d: %q", n, body, got)
			}
			if bracketed != (n > DefaultSyncMin) {
				t.Fatalf("drawing of %d bytes: bracketed %v", n, bracketed)
			}
			s.quiet()
		}
	})
	t.Run("big-frame-visible-cursor", func(t *testing.T) {
		s := newLiveSim(t, "big", 400, 3, 400, 3)
		s.app("\x1b[3;1H" + strings.Repeat("y", 300) + "\x1b[1;1H")
		got := s.tick(false)
		want := "\x1b[?2026h\x1b[?25l\x1b[3;1H" + strings.Repeat("y", 300) + "\x1b[1;1H\x1b[?25h\x1b[?2026l"
		if string(got) != want {
			t.Fatalf("frame: %q", got)
		}
	})
	t.Run("full-repaint-cursor", func(t *testing.T) {
		// A repaint's cursor follows the rule, not only the application.
		for _, tc := range []struct {
			name, before, app string
			dirty             bool
			suffix            string
		}{
			{"shown, kept", "", "\x1b[2J\x1b[H" + strings.Repeat("z", 60), false, "\x1b[1;61H\x1b[?25h"},
			{"shown, burst hides", "", "\x1b[2J\x1b[H" + strings.Repeat("z", 60), true, "\x1b[1;61H"},
			{"hidden by the application", "", "\x1b[?25l\x1b[2J\x1b[H" + strings.Repeat("z", 60), false, "\x1b[1;61H"},
			{"hidden, shown on a quiet tick", "\x1b[?25l", "\x1b[?25h\x1b[2J\x1b[H" + strings.Repeat("z", 60), false, "\x1b[1;61H\x1b[?25h"},
		} {
			s := newLiveSim(t, tc.name, 80, 1, 80, 1)
			s.app(tc.before)
			s.tick(false)
			s.app(tc.app)
			got, _, full := s.f.Frame(nil, s.g, 80, 1, tc.dirty)
			s.send(got)
			if !full || !bytes.HasPrefix(got, []byte("\x1b[?25l\x1b[2J")) || !bytes.HasSuffix(got, []byte(tc.suffix)) {
				t.Fatalf("%s: full=%v %q, want suffix %q", tc.name, full, got, tc.suffix)
			}
			s.quiet()
		}
	})
}
