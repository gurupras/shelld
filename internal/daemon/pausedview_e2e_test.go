//go:build linux || darwin

package daemon

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws"
)

// The tests here are the whole paused-view path at once: a real daemon, a real
// shell, two attached clients, and the reader's screen rebuilt from the frames it
// actually receives. The unit tests next door check which events hand a view a copy
// and what the copy holds; these check that the bytes a paused client is sent come
// out of it — the ticker registering the claim, render choosing the frozen source,
// and handleCtrl bounding a further scroll by the copy rather than the live grid.
//
// Both are peer-driven on purpose. A client that clears or resizes its own view
// asked for the change; the case worth pinning is the one where someone else does
// it and this client is promised its content anyway. Neither passes without the
// claim: with VtGrid's BeforeHistoryClear hook unset the reader's screen moves to the
// peer's new text, which is the fault these are worth.

// scrollBack sends a wheel-style scroll of n lines back from wherever the client
// is and waits until the daemon says it took, which is the witness the screen
// cannot supply: a client at the live tail and a client paused deep in history are
// both showing rows of the same output, so nothing about the text distinguishes a
// scroll that has been processed from one still in flight.
func scrollBack(t *testing.T, c *ws.Conn, s *screen, n int) {
	t.Helper()
	before := scrollOffset(t, s)
	if err := c.WriteMessage(ws.OpText, fmt.Appendf(nil, "\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-%d}}", n)); err != nil {
		t.Fatal(err)
	}
	want := fmt.Appendf(nil, `"offset":%d,`, before+n)
	s.await(fmt.Sprintf("the client never reached offset %d", before+n), 10*time.Second, func() bool {
		return bytes.Contains(s.status, want) && bytes.Contains(s.status, []byte(`"paused":true`))
	})
}

// scrollOffset is how far back the daemon last said this client is, and 0 before
// any status frame has arrived, which is where a client starts.
func scrollOffset(t *testing.T, s *screen) int {
	t.Helper()
	_, rest, ok := bytes.Cut(s.status, []byte(`"offset":`))
	if !ok {
		return 0
	}
	num, _, _ := bytes.Cut(rest, []byte(","))
	n, err := strconv.Atoi(string(num))
	if err != nil {
		t.Fatalf("status %q: %v", s.status, err)
	}
	return n
}

// topLine is the first non-blank row of a client's screen. A client paused in
// history has text on every row, so this is the row it is anchored on and the one
// a test can follow across whatever a peer does.
func topLine(s *screen) string {
	for _, row := range s.text() {
		if row != "" {
			return row
		}
	}
	return ""
}

// markAt reads the number out of a "mark-N…" row, so a test can say which line of
// the window's output a client has ended up on rather than only that it moved.
func markAt(t *testing.T, row string) int {
	t.Helper()
	rest, ok := strings.CutPrefix(row, "mark-")
	if !ok {
		t.Fatalf("the client's top row is %q, which is not one of the marker lines", row)
	}
	n, err := strconv.Atoi(strings.SplitN(rest, "-", 2)[0])
	if err != nil {
		t.Fatalf("the client's top row is %q: %v", row, err)
	}
	return n
}

// TestAPausedClientKeepsItsScreenWhenAPeerClearsTheScrollback is the scenario in
// full: one client scrolled back into history, another clearing that history out
// from under it, and the first still reading what it was reading — and still able
// to page further up, which is the question a client asks next.
func TestAPausedClientKeepsItsScreenWhenAPeerClearsTheScrollback(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	defer reader.Close()
	rs := newScreen(t, rch, 80, 24)
	peer, pch := attachAt(t, sock, "s", 80, 24)
	defer peer.Close()
	ps := newScreen(t, pch, 80, 24)

	_ = reader.WriteMessage(ws.OpBinary, []byte("i=1; while [ $i -le 300 ]; do echo mark-$i; i=$((i+1)); done\n"))
	rs.await("the window never printed mark-300", 30*time.Second, func() bool { return rs.contains("mark-300") })
	live := topLine(rs)

	scrollBack(t, reader, rs, 120)
	rs.drain(time.Second)
	held := topLine(rs)
	at := markAt(t, held)
	if held == live {
		t.Fatalf("the reader still shows the live tail %q after scrolling back, so nothing below is about a paused client", live)
	}

	// The peer clears the scrollback and prints enough to refill it. The refill is
	// the point: render already returns a paused client to the live view when there
	// is no history to be in, so a bare clear would be rescued by that guard rather
	// than by the copy. Waiting on the peer's own screen is what makes this setup
	// rather than hope — it says the clear ran and the history is non-empty again.
	_ = peer.WriteMessage(ws.OpBinary, []byte("printf '\\033[3J'; i=1; while [ $i -le 80 ]; do echo post-$i; i=$((i+1)); done\n"))
	ps.await("the peer never printed post-80, so the clear may not have run", 30*time.Second,
		func() bool { return ps.contains("post-80") })
	rs.drain(2 * time.Second)

	if got := topLine(rs); got != held {
		t.Errorf("the reader's top row moved from %q to %q when a peer cleared the scrollback:\n%s",
			held, got, strings.Join(rs.text(), "\n"))
	}
	if rs.contains("post-") {
		t.Errorf("the reader was shown the peer's new output while paused:\n%s", strings.Join(rs.text(), "\n"))
	}

	// And it can still page up, within its copy: the live history holds only the
	// peer's output now, so a client bounded by that would refuse this scroll or
	// land on post- lines.
	scrollBack(t, reader, rs, 50)
	rs.drain(2 * time.Second)
	if got, want := markAt(t, topLine(rs)), at-50; got != want {
		t.Errorf("scrolling 50 further back left the reader on mark-%d, want mark-%d:\n%s",
			got, want, strings.Join(rs.text(), "\n"))
	}
}

// TestAResizedClientDoesNotAdoptACopyTakenWhileItWasLive is the other side of the
// claim: it has to be dropped on every path that leaves the paused state, not only
// the scroll that ends there. A resize ends scroll mode too, and a claim left behind
// is still registered when a peer clears, so the client is handed a copy of history
// it never asked for — and adopts it on its next scroll, showing content from before
// the clear as though it had been paused the whole time.
//
// A rows-only resize on purpose. It is the one that changes no width, so nothing
// about the resize itself takes a copy, and what the client reads afterwards is
// decided by whether its claim went away.
func TestAResizedClientDoesNotAdoptACopyTakenWhileItWasLive(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	defer reader.Close()
	rs := newScreen(t, rch, 80, 24)
	peer, pch := attachAt(t, sock, "s", 80, 24)
	defer peer.Close()
	ps := newScreen(t, pch, 80, 24)

	_ = reader.WriteMessage(ws.OpBinary, []byte("i=1; while [ $i -le 300 ]; do echo mark-$i; i=$((i+1)); done\n"))
	rs.await("the window never printed mark-300", 30*time.Second, func() bool { return rs.contains("mark-300") })

	// Paused, then resized back to the live view. The pause is what registers the
	// claim; the resize is the path that has to drop it.
	scrollBack(t, reader, rs, 120)
	rs.drain(time.Second)
	if err := reader.WriteMessage(ws.OpText, resizeCtl(80, 20)); err != nil {
		t.Fatal(err)
	}
	waitSize(t, sock, "s", [2]int{80, 20})
	rs.rows = 20
	rs.await("the reader never returned to the live view after resizing", 10*time.Second, func() bool {
		return bytes.Contains(rs.status, []byte(`"paused":false`))
	})

	// Now the event. A claim still registered is handed the pre-clear history here.
	_ = peer.WriteMessage(ws.OpBinary, []byte("printf '\\033[3J'; i=1; while [ $i -le 80 ]; do echo post-$i; i=$((i+1)); done\n"))
	ps.await("the peer never printed post-80, so the clear may not have run", 30*time.Second,
		func() bool { return ps.contains("post-80") })
	rs.drain(2 * time.Second)

	// Ten lines back from the live tail is the peer's own output. Ten lines back
	// from a copy taken before the clear is a mark- line near 300, which is what a
	// client that adopted the stale copy shows instead.
	scrollBack(t, reader, rs, 10)
	rs.drain(2 * time.Second)
	if got := topLine(rs); !strings.HasPrefix(got, "post-") {
		t.Errorf("after resizing and scrolling back the reader's top row is %q, want one of the peer's post- lines: it is reading a copy taken while it was live\n%s",
			got, strings.Join(rs.text(), "\n"))
	}
}

// TestAPausedClientKeepsItsScreenWhenAPeerChangesTheWidth is the reflow half.
// Nothing is destroyed — every character is still in the grid — but a narrower
// window rewrites the rows an offset addresses, splitting them, and moves
// HistoryPushed by nothing that describes the rewrite.
//
// The peer narrows the window by attaching at a smaller size, which is how a
// window actually changes width: the effective size is the smallest any voting
// client declared. The reader's own declared size is untouched, so this is the case
// the package doc calls out as leaving a view alone — true of the per-client crop,
// and not of the shared grid the crop reads from.
func TestAPausedClientKeepsItsScreenWhenAPeerChangesTheWidth(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	defer reader.Close()
	rs := newScreen(t, rch, 80, 24)

	// Lines long enough that halving the width has to split them: a row that still
	// fits narrows to itself, and this test would pass with no copy at all. Short of
	// 80 so that they do not wrap at the width they are printed at either.
	_ = reader.WriteMessage(ws.OpBinary, []byte(
		"pad="+strings.Repeat("x", 60)+"; i=1; while [ $i -le 200 ]; do echo mark-$i-$pad; i=$((i+1)); done\n"))
	rs.await("the window never printed mark-200", 30*time.Second, func() bool { return rs.contains("mark-200-") })
	live := topLine(rs)

	scrollBack(t, reader, rs, 90)
	rs.drain(time.Second)
	held := topLine(rs)
	if held == live {
		t.Fatalf("the reader still shows the live tail %q after scrolling back, so nothing below is about a paused client", live)
	}
	if len(held) <= 40 {
		t.Fatalf("the reader's top row %q fits in 40 columns, so narrowing would not split it", held)
	}

	peer, _ := attachAt(t, sock, "s", 40, 24)
	defer peer.Close()
	waitSize(t, sock, "s", [2]int{40, 24})
	rs.drain(2 * time.Second)

	if got := topLine(rs); got != held {
		t.Errorf("the reader's top row moved from %q to %q when a peer narrowed the window:\n%s",
			held, got, strings.Join(rs.text(), "\n"))
	}
}

// TestAFrozenClientIsNotGivenAwayToTheAlternateScreen is a program taking the
// alternate screen after a client has already been handed a copy. Three things
// have to hold, and they are one decision: the copy is what this client reads, and
// the program's screen is not in it.
//
// It is not returned to the live view. It is not told that the application owns
// scrolling, because that is an instruction to send the wheel to the program and
// shelld is what will actually act on this client's wheel. And the wheel still
// moves it through the copy.
//
// The peer is the control for all three. It is attached to the same window at the
// same instant and is live, so it is entitled to the opposite answer — which is
// also the only witness that the alternate screen really is active. Without it, a
// frozen client that is sent no frames at all would pass every assertion below
// while the alternate screen had never been taken.
func TestAFrozenClientIsNotGivenAwayToTheAlternateScreen(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	defer reader.Close()
	rs := newScreen(t, rch, 80, 24)
	peer, pch := attachAt(t, sock, "s", 80, 24)
	defer peer.Close()
	ps := newScreen(t, pch, 80, 24)

	_ = reader.WriteMessage(ws.OpBinary, []byte("i=1; while [ $i -le 300 ]; do echo mark-$i; i=$((i+1)); done\n"))
	rs.await("the window never printed mark-300", 30*time.Second, func() bool { return rs.contains("mark-300") })

	scrollBack(t, reader, rs, 120)
	rs.drain(time.Second)
	held := topLine(rs)
	at := markAt(t, held)

	// The reader has to be holding a copy before any of this is about a frozen
	// client, so a peer clears with output behind it, as in the clear test above.
	_ = peer.WriteMessage(ws.OpBinary, []byte("printf '\\033[3J'; i=1; while [ $i -le 80 ]; do echo post-$i; i=$((i+1)); done\n"))
	ps.await("the peer never printed post-80, so the clear may not have run", 30*time.Second,
		func() bool { return ps.contains("post-80") })
	rs.drain(2 * time.Second)
	if got := topLine(rs); got != held {
		t.Fatalf("the reader reads %q rather than the %q it was reading, so it is not holding a copy and nothing below is about one",
			got, held)
	}

	// Now the alternate screen, and the peer's own status is what says it took.
	_ = peer.WriteMessage(ws.OpBinary, []byte("printf '\\033[?1049h'\n"))
	ps.await("the peer was never told the application owns scrolling, so the alternate screen may not be active",
		30*time.Second, func() bool { return bytes.Contains(ps.status, []byte(`"alt":true`)) })
	rs.drain(2 * time.Second)

	if bytes.Contains(rs.status, []byte(`"paused":false`)) {
		t.Errorf("the reader was returned to the live view when a program took the alternate screen, though its copy still holds what it was reading\nstatus %s",
			rs.status)
	}
	if bytes.Contains(rs.status, []byte(`"alt":true`)) {
		t.Errorf("the reader was told to hand its wheel to the program while it reads a copy, which leaves the copy unscrollable\nstatus %s",
			rs.status)
	}
	if got := topLine(rs); got != held {
		t.Errorf("the reader's top row moved from %q to %q when a program took the alternate screen:\n%s",
			held, got, strings.Join(rs.text(), "\n"))
	}

	// And the wheel still moves it through the copy, which is what being told alt is
	// false is for.
	scrollBack(t, reader, rs, 20)
	rs.drain(2 * time.Second)
	if got, want := markAt(t, topLine(rs)), at-20; got != want {
		t.Errorf("scrolling 20 further back left the reader on mark-%d, want mark-%d:\n%s",
			got, want, strings.Join(rs.text(), "\n"))
	}
}

// TestAPausedClientThatDisconnectsLeavesNoClaim is the fourth caller of resumeView,
// and the only one that is not a client asking for anything: the tick loop's own
// teardown, which runs when the connection breaks or the shell exits. Deleting it
// left the whole package green.
//
// A claim left behind by a client that has gone is the one that cannot be recovered
// from. The other three sites lose a claim that a ticker still points at, so the
// next thing that client does releases it; this one loses the ticker as well, and
// nothing else in the process holds a reference — so the grid copies this window's
// whole history on every later clear and every later width change, for a reader that
// closed its socket, for as long as the window lives.
//
// A real disconnect is what drives it. The loop's exit path is what is under test,
// so the test has to make the loop exit the way a client does, by going away, rather
// than by anything a test could call.
func TestAPausedClientThatDisconnectsLeavesNoClaim(t *testing.T) {
	var st *State
	sock := bootWith(t, func(s *State) { st = s })
	openWindow(t, sock, "s")
	win := st.Mgr.Session("s").Window("w")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	rs := newScreen(t, rch, 80, 24)
	_ = reader.WriteMessage(ws.OpBinary, []byte("i=1; while [ $i -le 300 ]; do echo mark-$i; i=$((i+1)); done\n"))
	rs.await("the window never printed mark-300", 30*time.Second, func() bool { return rs.contains("mark-300") })
	scrollBack(t, reader, rs, 120)
	if claimsHeldBy(win) == 0 {
		t.Fatalf("setup: the paused client registered no claim, so nothing here is about releasing one")
	}

	reader.Close()

	// The teardown is the loop's own defer, so it lands some time after the socket
	// closes. AttacherCount is what says it has landed rather than that it is late:
	// the attach handler waits for the loop to return before releasing the
	// attachment, so a window with no attachers has already run it. Without this the
	// assertion below could only be polled, and a slow teardown would read as a leak.
	deadline := time.Now().Add(10 * time.Second)
	for win.AttacherCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := win.AttacherCount(); n != 0 {
		t.Fatalf("the window still has %d attacher(s) 10s after the client closed, so its tick loop has not finished and this says nothing about the loop's teardown", n)
	}

	if n := claimsHeldBy(win); n != 0 {
		t.Errorf("the grid still holds %d claim(s) for a client that has disconnected, and no ticker is left to release them: every later clear or width change copies this window's whole history for a reader that is gone", n)
	}
}

// TestAClientPausedDuringABurstIsNotShownTheLiveCursor: the cursor stays hidden
// while output streams, and once it settles the daemon shows it again. That
// restore is the live cursor, so it must not reach a client that scrolled back
// mid-burst: it would sit on top of the history the client is reading, and no
// paused frame hides it again. Going back to the live view shows it.
func TestAClientPausedDuringABurstIsNotShownTheLiveCursor(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	reader, rch := attachAt(t, sock, "s", 80, 24)
	defer reader.Close()
	rs := newScreen(t, rch, 80, 24)
	peer, pch := attachAt(t, sock, "s", 80, 24)
	defer peer.Close()
	ps := newScreen(t, pch, 80, 24)

	_ = reader.WriteMessage(ws.OpBinary, []byte("i=1; while [ $i -le 60000 ]; do echo burst-$i; i=$((i+1)); done; echo burst-done\n"))
	// Far enough in that there is history to scroll back into: a scroll with none
	// is refused.
	rs.await("the burst never left history behind", 30*time.Second, func() bool {
		return rs.contains("burst-") && !bytes.Contains(rs.status, []byte(`"max":0,`))
	})
	// Not scrollBack: output keeps moving the offset, so only "paused" can witness
	// that the scroll took.
	if err := reader.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"delta\",\"lines\":-10}}")); err != nil {
		t.Fatal(err)
	}
	rs.await("the reader never paused", 10*time.Second, func() bool { return bytes.Contains(rs.status, []byte(`"paused":true`)) })
	if ps.contains("burst-done") {
		t.Fatal("setup: the burst ended before the reader paused, so no restore was pending for it")
	}
	ps.await("the burst never ended", 60*time.Second, func() bool { return ps.contains("burst-done") })
	rs.drain(time.Second)

	if !bytes.Contains(rs.status, []byte(`"paused":true`)) {
		t.Fatalf("setup: the reader is no longer paused (status %s)", rs.status)
	}
	if rs.term.Mode()&vt.ModeShowCursor != 0 {
		t.Errorf("a paused reader was shown the live cursor once the burst settled:\n%s", strings.Join(rs.text(), "\n"))
	}

	if err := reader.WriteMessage(ws.OpText, []byte("\x01{\"scroll\":{\"op\":\"exit\"}}")); err != nil {
		t.Fatal(err)
	}
	rs.await("the reader's cursor never came back on the live view", 10*time.Second, func() bool {
		return rs.term.Mode()&vt.ModeShowCursor != 0
	})
}
