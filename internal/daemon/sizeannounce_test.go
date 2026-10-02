//go:build linux || darwin

package daemon

import (
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// The tests here are about a client learning how big the PTY it is looking at is.
//
// A client declares a size and gets frames at that size, so it would seem to know.
// It does not: the effective PTY size is floored at minEffectiveCols ×
// minEffectiveRows, and a client that declared less than that renders a top-left
// *crop* of a larger grid. The program was told the larger size, so what it draws
// outside the crop is not reflowed to fit and not sent — it is simply absent, and
// the cursor is clamped into the crop with it. Nothing about that is visible in the
// frames themselves; a client sees a screen that looks complete.
//
// So the status frame's cols and rows are the only mechanism there is, and these
// tests are about the two halves of it: the size at attach, and a change to it
// afterwards. The second half is the one a client cannot work around, because
// GET /windows publishes the same numbers but nothing on this stream invalidates a
// poll, and the client this was found for does not poll.
//
// The floor's constants are unexported and in the root package, hence the literal 20
// and 5 below. Spelling them out is also what makes these tests able to fail: an
// expectation derived from the same constant the code reads cannot notice that
// constant changing.

// awaitStatusGeometry waits for a status frame announcing want, and returns every
// status frame it read on the way whether or not it found one — a test that did not
// see the size it wanted needs to say what it saw instead.
func awaitStatusGeometry(t *testing.T, ch <-chan msg, want [2]uint16, d time.Duration) ([]statusReport, bool) {
	t.Helper()
	var seen []statusReport
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		m, ok := next(ch, time.Until(deadline))
		if !ok {
			break
		}
		if m.op != ws.OpText {
			continue
		}
		s, ok := parseStatus(m.data)
		if !ok {
			continue
		}
		seen = append(seen, s)
		if [2]uint16{s.Cols, s.Rows} == want {
			return seen, true
		}
	}
	return seen, false
}

// The hello announces the effective size, not what the client asked for. This is the
// arm where those differ: 10x3 is under the floor, so the PTY runs at 20x5 and this
// client is cropping 10 columns and 2 rows off a grid the program is drawing into.
//
// It is also the direct regression test for the defect, and the only test in the
// suite that discriminates the two quantities. The sizes are asymmetric and share no
// value with offset or max, so announcing the declaration, or the pair transposed,
// fails here rather than passing on a coincidence.
func TestTheHelloAnnouncesTheFlooredSizeNotTheDeclaredOne(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	conn, ch := attachAt(t, sock, "s", 10, 3)
	defer conn.Close()

	// The same quantity read over HTTP, which establishes what the frame below is
	// supposed to say without reference to the frame. Registering an attacher
	// recomputes the effective size before the hello is written, so this is settled
	// by the time attachAt has returned and there is nothing to wait for.
	if got := effectiveSize(t, sock, "s"); got != [2]int{20, 5} {
		t.Fatalf("setup: the effective size is %v, want 20x5 — a 10x3 declaration was"+
			" supposed to floor, so this is not exercising the crop it is about", got)
	}

	m, ok := next(ch, 2*time.Second)
	if !ok || m.op != ws.OpText {
		t.Fatalf("no hello status frame after the size handshake: %+v", m)
	}
	s, ok := parseStatus(m.data)
	if !ok {
		t.Fatalf("the hello is not a status frame: %q", m.data)
	}
	if s.Cols != 20 || s.Rows != 5 {
		t.Errorf("a client that declared 10x3 was told the PTY is %dx%d, want 20x5:"+
			" the size it is cropping, which is the only thing it cannot work out"+
			" for itself", s.Cols, s.Rows)
	}
}

// And a change to the effective size while the client is attached reaches it. This
// is the half that needs a channel rather than a field: at attach a client could at
// least poll once, and after a resize it has no signal that the answer moved.
//
// The shape of the resize is load-bearing three times over.
//
// A pin, not this client's own resize control: the control moves the declared and
// the effective size together, which is the one case a client already knows about.
// Here the declaration stays 80x24 while the PTY goes to 120x40, so this test also
// discriminates which of the two is announced.
//
// Growing both dimensions, so a transposed or half-applied emit cannot match.
//
// And on a window with no scrollback, which is the subtle one. A resize moves max
// whenever there is history to reflow, so the same test run after some output would
// see the frame re-emitted for a reason that has nothing to do with geometry — and
// would pass against an implementation whose change detection still ignored the
// size. max staying 0 across every frame is what says the re-emit is the geometry's.
func TestAnEffectiveSizeChangeReachesAnAttachedClient(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")
	conn, ch := attachAt(t, sock, "s", 80, 24)
	defer conn.Close()

	hello, ok := next(ch, 2*time.Second)
	if !ok || hello.op != ws.OpText {
		t.Fatalf("no hello status frame after the size handshake: %+v", hello)
	}
	h, ok := parseStatus(hello.data)
	if !ok {
		t.Fatalf("the hello is not a status frame: %q", hello.data)
	}
	if h.Cols != 80 || h.Rows != 24 {
		t.Fatalf("the hello announced %dx%d, want 80x24", h.Cols, h.Rows)
	}
	if h.Max != 0 {
		t.Fatalf("setup: the window already has %d rows of scrollback, so the resize"+
			" below would move max and this test would pass on that instead of on the"+
			" geometry", h.Max)
	}

	if st, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize",
		`{"cols":120,"rows":40}`); st != 200 {
		t.Fatal(raw)
	}
	waitSize(t, sock, "s", [2]int{120, 40})

	seen, found := awaitStatusGeometry(t, ch, [2]uint16{120, 40}, 5*time.Second)
	for _, s := range seen {
		if s.Max != 0 {
			t.Errorf("max reached %d, so scrollback moved across the resize and a"+
				" change check that ignored the size would have re-emitted anyway;"+
				" this window was supposed to have no history", s.Max)
		}
	}
	if !found {
		t.Fatalf("the PTY grew to 120x40 and in 5s no status frame said so; the status"+
			" frames that did arrive were %+v. This client is still declaring 80x24 and"+
			" is now cropping 40 columns and 16 rows off the grid with nothing telling"+
			" it so", seen)
	}
}
