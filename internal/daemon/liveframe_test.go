//go:build linux || darwin

package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/internal/tracing"
	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws"
)

// liveRun is one client of a window running a script, recording what the
// client receives and how its tick loop wakes.
type liveRun struct {
	t       *testing.T
	win     *shelld.Window
	ch      <-chan msg
	client  *vt.Term
	proc    *vt.Processor
	dir     string
	mu      sync.Mutex
	wakes   []stampedWake
	frames  [][]byte
	frameAt []time.Time
	counted bool
}

// stampedWake is a wake with the time it was seen, so a test can say which
// wakes came before a message reached the client.
type stampedWake struct {
	tickWake
	at time.Time
}

const liveCols, liveRows = 80, 24

// startLiveRun opens a window running script under /bin/sh and attaches one
// client. The script's first line runs at once; the rest waits for play(), and
// "@DIR@" in it names the run's directory, so a script can wait for a second
// trigger the test drops.
func startLiveRun(t *testing.T, setup, script string) *liveRun {
	t.Helper()
	r := &liveRun{t: t, dir: t.TempDir()}
	var st *State
	sock := bootWith(t, func(s *State) { st = s })
	path := filepath.Join(r.dir, "app.sh")
	body := fmt.Sprintf("printf '%s'\nwhile [ ! -e %s/go ]; do sleep 0.02; done\n%s\n: > %s/done\nexec sleep 60\n",
		setup, r.dir, strings.ReplaceAll(script, "@DIR@", r.dir), r.dir)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); code != 200 {
		t.Fatal(raw)
	}
	req := fmt.Sprintf(`{"name":"w","cols":%d,"rows":%d,"shell":"/bin/sh %s"}`, liveCols, liveRows, path)
	if code, _, raw := request(t, sock, "POST", "/sessions/s/windows", req); code != 200 {
		t.Fatal(raw)
	}
	r.win = st.Mgr.Session("s").Window("w")
	observe := func(w tickWake) {
		if w.win != r.win {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.counted {
			r.wakes = append(r.wakes, stampedWake{w, time.Now()})
		}
	}
	tickObserver.Store(&observe)
	t.Cleanup(func() { tickObserver.Store(nil) })
	c := openWS(t, sock, "s")
	if err := c.WriteMessage(ws.OpText, resizeCtl(liveCols, liveRows)); err != nil {
		t.Fatal(err)
	}
	r.ch = reader(c)
	r.client, r.proc = vt.NewTerm(vt.Config{}, liveCols, liveRows, nil), vt.NewProcessor()
	return r
}

// quiesce feeds the client every binary message until none has arrived for
// 500 ms, far longer than a burst's settle.
func (r *liveRun) quiesce() {
	r.t.Helper()
	for {
		m, ok := next(r.ch, 500*time.Millisecond)
		if !ok {
			return
		}
		if m.err != nil {
			r.t.Fatalf("attach ended: %v", m.err)
		}
		if m.op == ws.OpBinary {
			r.proc.Advance(r.client, m.data)
			if r.counted {
				r.mu.Lock()
				r.frames = append(r.frames, m.data)
				r.frameAt = append(r.frameAt, time.Now())
				r.mu.Unlock()
			}
		}
	}
}

// play runs the rest of the script, counting from its start until the client
// is quiet after the script's end.
func (r *liveRun) play() {
	r.t.Helper()
	r.quiesce()
	r.mu.Lock()
	r.counted = true
	r.mu.Unlock()
	if err := os.WriteFile(filepath.Join(r.dir, "go"), nil, 0o644); err != nil {
		r.t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(r.dir, "done")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			r.t.Fatal("the script did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.quiesce()
	r.mu.Lock()
	r.counted = false
	r.mu.Unlock()
}

// checkScreen compares the client with the window's grid: cells, cursor
// visibility and the cursor's cell.
func (r *liveRun) checkScreen() {
	r.t.Helper()
	g := r.win.LockVT()
	want := g.SnapshotCropped(liveCols, liveRows)
	r.win.UnlockVT()
	got := r.client.Grid()
	for i, w := range want.Cells {
		ch := w.Ch
		if ch == 0 {
			ch = ' '
		}
		if c := got.Row(i / liveCols).Cell(i % liveCols).Rune(); c != ch && !(c == 0 && ch == ' ') {
			r.t.Fatalf("row %d col %d: client %q, grid %q", i/liveCols, i%liveCols, c, ch)
		}
	}
	vis := r.client.Mode()&vt.ModeShowCursor != 0
	p := got.Cursor.Point
	if vis != want.CursorVisible || p.Line != want.CursorRow || p.Column != want.CursorCol {
		r.t.Fatalf("client cursor visible=%v at %d,%d; the application's visible=%v at %d,%d; frames %q",
			vis, p.Line, p.Column, want.CursorVisible, want.CursorRow, want.CursorCol, r.frames)
	}
}

// idleBefore counts the idle safety ticks that woke the loop before message i
// reached the client: timer wakes whose timer was armed for the grid. A dirty
// wake's timer is armed for the grid too, now that no tick follows a frame, so
// the reason has to be checked as well.
func (r *liveRun) idleBefore(i int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.frameAt) {
		return -1
	}
	n := 0
	for _, w := range r.wakes {
		if w.site == siteWait && w.armed == armIdle && w.reason == tracing.WakeTimer && w.at.Before(r.frameAt[i]) {
			n++
		}
	}
	return n
}

// wakeCounts are a tick loop's wakes by kind.
type wakeCounts struct {
	dirty, ctrl, settleTimer, idleTimer, otherTimer, pace, settleQuiet, settleDirty int
}

func (r *liveRun) counts() wakeCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c wakeCounts
	for _, w := range r.wakes {
		switch {
		case w.site == sitePace:
			c.pace++
		case w.site == siteSettle && w.reason == tracing.WakeTimer:
			c.settleQuiet++
		case w.site == siteSettle:
			c.settleDirty++
		case w.reason == tracing.WakeDirty:
			c.dirty++
		case w.reason == tracing.WakeCtrl:
			c.ctrl++
		case w.armed == armSettle:
			c.settleTimer++
		case w.armed == armIdle:
			c.idleTimer++
		default:
			c.otherTimer++
		}
	}
	return c
}

// TestAttachLiveMessagesAndWakes pins, through the daemon, how many messages
// a client receives for the live view's common cases and how often its tick
// loop wakes for them, and that the client ends showing what the application
// does. Each output step is one printf, one PTY read, 200 ms apart.
//
// Wakes per output step: the dirty wake alone. No timer follows a frame, and
// no pacing sleep does either — these steps are 200 ms apart, so each frame
// finds the burst allowance full and paceWait returns 0. A bare minimum
// spacing used to sleep out a quantum here before waiting the remaining
// ~192 ms for the next step, one wake per frame per client for nothing; the
// pace now costs a wake only when output really is arriving faster than
// minInterval. A burst adds the settle check's timer wake and the settle's
// quiet wait.
//
// So a nonzero pace count in any case below means output arrived within a
// quantum of the frame before it, which these scripts do not do.
func TestAttachLiveMessagesAndWakes(t *testing.T) {
	const gap = "sleep 0.2\n"
	cases := []struct {
		name, setup, script string
		frames              int
		want                wakeCounts
	}{
		{
			// A visible cursor that ends where it was: one message per update.
			name:   "spinner",
			setup:  `\r* Thinking 0`,
			script: gap + "for i in 1 2 3 4 5; do printf '\\r* Thinking %d' $i; sleep 0.2; done",
			frames: 5,
			want:   wakeCounts{dirty: 5},
		},
		{
			// A hidden cursor: the same.
			name:   "spinner-hidden-cursor",
			setup:  `\033[?25l\r* Thinking 0`,
			script: gap + "for i in 1 2 3 4 5; do printf '\\r* Thinking %d' $i; sleep 0.2; done",
			frames: 5,
			want:   wakeCounts{dirty: 5},
		},
		{
			// The cursor moves: the frame hides it, and one settle message
			// shows it once output is quiet.
			name:   "moved-cursor",
			setup:  `ab`,
			script: gap + `printf '\033[5;1Hcd'` + "\n" + gap,
			frames: 2,
			want:   wakeCounts{dirty: 1, settleTimer: 1, settleQuiet: 1},
		},
		{
			// H1 of the pre-review: the application hides the cursor, draws
			// elsewhere, returns the cursor, and shows it in a later read. The
			// show waits for quiet, with its cursor move.
			name:   "hide-draw-return-then-show",
			setup:  `\033[2J\033[5;10H`,
			script: gap + `printf '\033[?25l\033[1;1Hhello\033[5;10H'` + "\n" + gap + `printf '\033[?25h'` + "\n" + gap,
			frames: 2,
			want:   wakeCounts{dirty: 2, settleTimer: 1, settleQuiet: 1},
		},
		{
			// H2 of the pre-review: a hidden cursor moves, a frame with no
			// bytes that starts a burst, whose settle places the hidden
			// cursor; the application then shows it, a second burst.
			name:   "hidden-move-then-show",
			setup:  `\033[2J\033[1;1H\033[?25l`,
			script: gap + `printf '\033[8;20H'` + "\n" + gap + `printf '\033[?25h'` + "\n" + gap,
			frames: 2,
			want:   wakeCounts{dirty: 2, settleTimer: 2, settleQuiet: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := startLiveRun(t, tc.setup, tc.script)
			r.play()
			r.checkScreen()
			t.Logf("frames %q", r.frames)
			if len(r.frames) != tc.frames {
				t.Errorf("%d messages, want %d: %q", len(r.frames), tc.frames, r.frames)
			}
			got := r.counts()
			idle := got.idleTimer
			got.idleTimer = 0 // an idle safety tick can land in any window
			if got != tc.want {
				t.Errorf("wakes %+v, want %+v; frames %q", got, tc.want, r.frames)
			}
			if idle > 0 {
				t.Logf("%d idle safety ticks landed in the window", idle)
			}
		})
	}
}

// The cases above leave the pace with no wake of its own, so on their evidence
// alone the loop's pace gate could be deleted outright and every one of them
// would still pass. This is the case that holds it: continuous output, faster
// than one frame per minInterval, where the rate limit must engage. The
// assertion is that it engages at all and that the client is not sent a frame
// per PTY read — a count of frames rather than their arrival times, because
// arrival times at the client carry scheduling jitter and the burst allowance
// legitimately puts two frames back to back.
func TestAttachLivePacesContinuousOutput(t *testing.T) {
	const lines = 20000
	r := startLiveRun(t, `\033[2J\033[1;1H`,
		fmt.Sprintf(`awk 'BEGIN{for(i=0;i<%d;i++) print "line", i}'`, lines))
	r.play()
	r.checkScreen()

	c := r.counts()
	if c.pace == 0 {
		t.Errorf("no pacing wake for %d lines of continuous output: the rate limit never engaged; wakes %+v", lines, c)
	}
	// One frame per quantum of a burst this size is tens of frames, not
	// thousands: a bound loose enough to survive a faster machine and tight
	// enough to fail if every read were forwarded.
	if n := len(r.frames); n > lines/100 {
		t.Errorf("%d frames for %d lines, want far fewer: output is not being coalesced", n, lines)
	} else {
		t.Logf("%d frames for %d lines, %d pacing wakes", n, lines, c.pace)
	}
}

// A change that reaches the grid just after a frame, with no output after it,
// must reach the client without waiting for the idle safety tick: no tick
// follows a frame any more, so the dirty notification is what repairs it, and
// the client's settle signal must survive a frame that sends nothing.
//
// The script waits for a second trigger the test drops the moment it has the
// first frame, so the change lands within a few milliseconds of it.
func TestAttachChangeJustAfterFrame(t *testing.T) {
	cases := []struct {
		name, setup, script string
		frames              int
	}{
		{
			// Cells: two messages, the second from the dirty wake.
			name:   "cells",
			setup:  `\033[2J\033[1;1H\033[?25l`,
			script: "printf 'A'\nwhile [ ! -e @DIR@/go2 ]; do sleep 0.005; done\nprintf 'B'",
			frames: 2,
		},
		{
			// A cursor-only move with the cursor hidden: the frame after it
			// sends nothing, and only the settle signal taken from that empty
			// frame places the cursor.
			name:   "hidden cursor move",
			setup:  `\033[2J\033[1;1H\033[?25lxy`,
			script: "printf 'A'\nwhile [ ! -e @DIR@/go2 ]; do sleep 0.005; done\nprintf '\\033[7;30H'",
			frames: 2,
		},
		{
			// The application shows the cursor and nothing else follows.
			name:   "show the cursor",
			setup:  `\033[2J\033[1;1H\033[?25lxy`,
			script: "printf 'A'\nwhile [ ! -e @DIR@/go2 ]; do sleep 0.005; done\nprintf '\\033[?25h'",
			frames: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := startLiveRun(t, tc.setup, tc.script)
			r.quiesce()
			r.mu.Lock()
			r.counted = true
			r.mu.Unlock()
			if err := os.WriteFile(filepath.Join(r.dir, "go"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			// The first frame, then the change, within milliseconds of it.
			for deadline := time.Now().Add(10 * time.Second); ; {
				m, ok := next(r.ch, 500*time.Millisecond)
				if ok && m.op == ws.OpBinary {
					r.proc.Advance(r.client, m.data)
					r.mu.Lock()
					r.frames = append(r.frames, m.data)
					r.frameAt = append(r.frameAt, time.Now())
					r.mu.Unlock()
					break
				}
				if !ok && time.Now().After(deadline) {
					t.Fatal("no frame for the first write")
				}
			}
			if err := os.WriteFile(filepath.Join(r.dir, "go2"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			// quiesce returns after 500 ms of silence, half the idle grid's
			// period: a change repaired by the idle tick would arrive after it,
			// and the screens would differ.
			r.quiesce()
			r.mu.Lock()
			r.counted = false
			r.mu.Unlock()
			t.Logf("frames %q", r.frames)
			r.checkScreen()
			if len(r.frames) != tc.frames {
				t.Errorf("%d messages, want %d", len(r.frames), tc.frames)
			}
			// The change reached the client on its own dirty notification:
			// one wake for the first write, one for the change. quiesce above
			// returns after 500 ms of silence, so checkScreen would already
			// have failed had the change waited for an idle safety tick
			// (which can be up to about 950 ms away); the tick counts are
			// logged because a tick lands about once a second whatever
			// happens.
			c := r.counts()
			if c.dirty != 2 {
				t.Errorf("%d dirty wakes, want 2; wakes %+v", c.dirty, c)
			}
			t.Logf("wakes %+v, idle ticks before the change reached the client: %d", c, r.idleBefore(1))
		})
	}
}

// idleTicksBetween counts the idle safety ticks that woke the loop between two
// instants (see idleBefore on what counts as one).
func (r *liveRun) idleTicksBetween(from, to time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.wakes {
		if w.site == siteWait && w.armed == armIdle && w.reason == tracing.WakeTimer &&
			w.at.After(from) && w.at.Before(to) {
			n++
		}
	}
	return n
}

// What commit C's safety rests on is that the dirty channel is armed before a
// render takes the VT lock and stays armed through the write: a change that
// notifies while the frame it raced is still being written wakes the loop at
// once, with no post-frame tick to fall back on. A test whose change lands
// after the write cannot tell that apart from arming after the write, so this
// one makes the change from renderHook — under the VT lock at the end of the
// render, after that frame's snapshot and before its write.
//
// Contributed by a post-review of the proposal rather than by the author of the
// code it tests, which is what makes the mutant result below an independent
// prediction: that review measured 8 ms delivery here, and 2 of 3 runs losing
// the change entirely under a "re-arm after the write" mutant.
func TestAttachChangeDuringWriteReachesClient(t *testing.T) {
	r := startLiveRun(t, `\033[2J\033[1;1H\033[?25l`, `printf 'A\033[1;1H'`)
	r.quiesce()
	var raced atomic.Bool
	hook := func(g *shelld.VtGrid) {
		// Only the render that snapshotted the script's write, and only once.
		if s := g.Snapshot(); len(s.Cells) == 0 || s.Cells[0].Ch != 'A' {
			return
		}
		if !raced.CompareAndSwap(false, true) {
			return
		}
		g.Advance([]byte("\x1b[3;1HZZ"))
		// The notification the writer would send. It is dropped unless the
		// loop armed the channel before this render.
		r.win.Dirty().NotifyWaiters()
	}
	renderHook.Store(&hook)
	t.Cleanup(func() { renderHook.Store(nil) })
	r.mu.Lock()
	r.counted = true
	r.mu.Unlock()
	if err := os.WriteFile(filepath.Join(r.dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.quiesce()
	r.mu.Lock()
	r.counted = false
	frames := append([][]byte(nil), r.frames...)
	at := append([]time.Time(nil), r.frameAt...)
	r.mu.Unlock()
	if !raced.Load() {
		t.Fatal("no render raced the change: the hook never ran")
	}

	var first, arrived time.Time
	for i, f := range frames {
		if first.IsZero() {
			first = at[i]
		}
		if arrived.IsZero() && bytes.Contains(f, []byte("ZZ")) {
			arrived = at[i]
		}
	}
	t.Logf("frames %q", frames)
	if arrived.IsZero() {
		t.Fatalf("the change made during the write never reached the client")
	}
	// Deterministic: it came in on its own dirty wake (one for the script's
	// write, one for the change), and no idle safety tick woke the loop in
	// between, so the grid repaired nothing.
	if c, idle := r.counts(), r.idleTicksBetween(first, arrived); c.dirty != 2 || idle != 0 {
		t.Errorf("the change did not arrive on its own notification: %d dirty wakes, %d idle ticks before it; wakes %+v", c.dirty, idle, c)
	}
	// And it was held only by frame pacing (minInterval), not by a timer.
	if d := arrived.Sub(first); d > 100*time.Millisecond {
		t.Errorf("the change reached the client %v after the frame it raced", d)
	} else {
		t.Logf("the change reached the client %v after the frame it raced", d)
	}
	r.checkScreen()
}
