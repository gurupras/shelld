//go:build linux || darwin

package daemon

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// What the opening frame of an attach connection is allowed to do to the program.
//
// This runs against serveAttach rather than against the classifier, because the defect
// was not in classifying a frame -- it was that the handshake did not use the
// classification. A test that calls classifyText directly passes on a handshake that
// ignores it, which is the shape that lets a mutation-verified test pin nothing.
//
// Each case sends its frame first, then a canary as OpBinary. Both writes travel one
// socket in order, so among echoes that DO arrive the frame's precedes the canary's, and
// an absence check that waits for the canary is therefore bounded by an arrival rather
// than by a timeout. The canary arrives whether or not the bug is present -- if the frame
// is typed, /bin/sh echoes it and the canary on the same line -- so these cases terminate
// either way and the substring is what discriminates.
//
// What that ordering does NOT bound is the DELIVERY of either write, and the distinction
// is not academic: nothing here waits for the shell. openWindow returns when the POST
// returns, so both writes can reach the tty before anything is reading it, and a first
// write lost there looks exactly like a frame that was never typed. Observed once in
// seven whole-package runs, as a false failure of "unmarked text is still typed" -- the
// canary came back and the probe did not. It does not reproduce in isolation (156
// consecutive iterations clean), so it needs the package's concurrency.
//
// The mechanism is not identified, and the leading guess is measured WRONG rather than
// merely unconfirmed: /bin/sh flushing its tty input during startup would produce this
// shape exactly, and it does not do so. Spawning dash on a fresh PTY and writing to the
// master before it can be ready loses nothing, 0 of 80 trials over gaps of 0, 5ms, 50ms
// and 300ms, with the canary arriving in all 80. That rejection is narrower than "the
// shell is innocent": it writes to the master directly, so it tests dash against a tty
// and not this package's attach path. So do not add a wait-for-the-shell here and call
// the flake fixed -- that would be treating a rejected hypothesis as the cause.
func TestWhatAnOpeningFrameMayDoToTheProgram(t *testing.T) {
	const canary = "zqxj-canary"

	cases := []struct {
		name string
		op   ws.Opcode
		data []byte
		// echoed is whether the frame's own bytes must come back from the program.
		echoed bool
		// probe is a substring of the frame that the program would echo if the frame
		// reached it. It must not appear in the canary command.
		probe string
	}{{
		// The defect: the handshake asked only "is this a resize?", so a well-formed
		// scroll first became keystrokes, while the same bytes sent second scrolled.
		name:   "a well-formed scroll is not typed",
		op:     ws.OpText,
		data:   []byte("\x01{\"scroll\":{\"op\":\"exit\"}}"),
		probe:  "scroll",
		echoed: false,
	}, {
		// A frame carrying the 0x01 marker has said it is a control frame. Typing it
		// delivers Ctrl-A and then the JSON.
		name:   "a marked frame that does not parse is not typed",
		op:     ws.OpText,
		data:   []byte("\x01{\"resize\":{\"cols\":\"not-a-number\"}}"),
		probe:  "not-a-number",
		echoed: false,
	}, {
		// The other direction, and the reason the fix is not simply "drop OpText": a
		// client may send text as an OpText frame, and that is input. A fix that
		// refused all OpText would pass the two cases above and silently eat typing.
		name:   "unmarked text is still typed",
		op:     ws.OpText,
		data:   []byte("qqmarkerqq"),
		probe:  "qqmarkerqq",
		echoed: true,
	}, {
		name:   "binary is still typed",
		op:     ws.OpBinary,
		data:   []byte("wwmarkerww"),
		probe:  "wwmarkerww",
		echoed: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Contains([]byte("echo "+canary+"\n"), []byte(tc.probe)) {
				t.Fatalf("probe %q occurs in the canary command, so this case cannot tell the two apart", tc.probe)
			}
			sock := boot(t)
			openWindow(t, sock, "s")
			c := openWS(t, sock, "s")
			ch := reader(c)

			if err := c.WriteMessage(tc.op, tc.data); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteMessage(ws.OpBinary, []byte("echo "+canary+"\n")); err != nil {
				t.Fatal(err)
			}

			var seen []byte
			deadline := time.Now().Add(15 * time.Second)
			for !bytes.Contains(seen, []byte(canary)) {
				left := time.Until(deadline)
				if left <= 0 {
					t.Fatalf("the canary never came back from the program, so this case established nothing about %q; saw %q", tc.data, seen)
				}
				if m, ok := next(ch, left); ok && m.op == ws.OpBinary {
					seen = append(seen, m.data...)
				}
			}

			got := bytes.Contains(seen, []byte(tc.probe))
			if got != tc.echoed {
				if tc.echoed {
					t.Errorf("%q did not reach the program: a client that sends text this way has its typing silently discarded", tc.data)
				} else {
					t.Errorf("%q reached the program as keystrokes: a control frame's bytes begin with 0x01, so the program was sent Ctrl-A and then the frame's payload", tc.data)
				}
			}
		})
	}
}

// The same frame in the two positions must do the same thing. Sending it second exercised
// the loop, which was already right; the point of asserting both here is that the two are
// now one code path, so a future change cannot fix one position and leave the other.
func TestAScrollMeansTheSameThingInEitherPosition(t *testing.T) {
	scroll := []byte("\x01{\"scroll\":{\"op\":\"exit\"}}")
	const canary = "zqxj-second"

	for _, first := range []bool{true, false} {
		name := "sent second"
		if first {
			name = "sent first"
		}
		t.Run(name, func(t *testing.T) {
			sock := boot(t)
			openWindow(t, sock, "s")
			c := openWS(t, sock, "s")
			ch := reader(c)

			if !first {
				// Complete the handshake with a resize, so the scroll below lands in
				// the steady-state loop instead.
				if err := c.WriteMessage(ws.OpText, resizeCtl(80, 24)); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.WriteMessage(ws.OpText, scroll); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteMessage(ws.OpBinary, []byte("echo "+canary+"\n")); err != nil {
				t.Fatal(err)
			}

			var seen []byte
			deadline := time.Now().Add(15 * time.Second)
			for !bytes.Contains(seen, []byte(canary)) {
				left := time.Until(deadline)
				if left <= 0 {
					t.Fatalf("the canary never came back, so this case established nothing; saw %q", seen)
				}
				if m, ok := next(ch, left); ok && m.op == ws.OpBinary {
					seen = append(seen, m.data...)
				}
			}
			if bytes.Contains(seen, []byte("scroll")) {
				t.Errorf("the scroll frame was typed into the program when %s; the two positions disagree, which is the defect this pins", name)
			}
		})
	}
}

// An opening scroll actually scrolls, and a frame that carries a resize as well is the
// resize -- the precedence the steady-state loop applies, now applied in both positions.
//
// Every case here needs history to exist. applyScroll clamps each offset to the
// scrollback there is, so on a window with none "enter at offset 3" and a dropped scroll
// are the same observable: offset 0, not paused. The cases above send {"op":"exit"},
// which applyScroll refuses outright while the client is not paused, so none of them can
// tell "replayed and did nothing" from "never replayed" -- deleting the replay in
// serveAttach leaves every other test in this package green and fails only case A below.
//
// Cases B and C assert an absence, and the window they allow for it is a measured margin
// rather than a chosen constant. Case A is the same observable in the same fixture on the
// same box with the scroll honoured, so it says how long the daemon takes to report one,
// and a case A too slow for a 5x margin makes this a setup failure rather than letting B
// and C pass by outrunning the daemon instead of by the resize winning. Measured here:
// 501us against a 2s window, so the margin the check enforces is the floor and not the
// figure.
func TestAnOpeningScrollScrollsAndAResizeInTheSameFrameOutranksIt(t *testing.T) {
	const offset = 3
	// Marked control frames, spelled with the marker outside the literal because a raw
	// string cannot hold a \x01.
	scrollOnly := []byte("\x01" + `{"scroll":{"op":"enter","offset":3}}`)
	both := []byte("\x01" + `{"resize":{"cols":80,"rows":24},"scroll":{"op":"enter","offset":3}}`)

	sock := boot(t)
	openWindow(t, sock, "s")
	fillHistory(t, sock, "s", offset)

	// Case A, and the calibration for B and C.
	c := openWS(t, sock, "s")
	ch := reader(c)
	if err := c.WriteMessage(ws.OpText, scrollOnly); err != nil {
		t.Fatal(err)
	}
	took, ok := awaitPaused(t, ch, offset, 15*time.Second)
	if !ok {
		t.Fatalf("a scroll sent as the opening frame never put the client at offset %d, so the opening frame's scroll was dropped instead of replayed: the client that opens by asking for a position in history is left reading the live tail", offset)
	}
	c.Close()
	t.Logf("an opening scroll was reported honoured after %v", took)

	// Five times the margin case A just measured, at the least. Longer would only make
	// B and C slower to fail; shorter is what would make them pass for the wrong reason.
	const absence = 2 * time.Second
	if took > absence/5 {
		t.Fatalf("setup: the daemon took %v to report an opening scroll it honoured, leaving less than the 5x margin the %v absence checks below need; on a box this loaded they could pass by being quicker than the daemon rather than by the resize winning", took, absence)
	}

	for _, tc := range []struct {
		name  string
		after bool
	}{
		// The position the fix changed.
		{"sent as the opening frame", false},
		// The steady-state loop, which is the reference: its switch takes the resize
		// arm first and always has. This case is here so that a future change cannot
		// make the two agree by moving the loop to match a wrong handshake.
		{"sent after the handshake", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openWS(t, sock, "s")
			ch := reader(c)
			if tc.after {
				if err := c.WriteMessage(ws.OpText, resizeCtl(80, 24)); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.WriteMessage(ws.OpText, both); err != nil {
				t.Fatal(err)
			}
			if took, ok := awaitPaused(t, ch, offset, absence); ok {
				t.Errorf("after %v the daemon reported the client paused at offset %d, so the scroll in a frame that also carried a resize was honoured when it was %s; the loop takes its resize arm first, and the same bytes meaning one thing first and another thing second is the defect this file exists for", took, offset, tc.name)
			}
			c.Close()
		})
	}
}

// statusReport is the body of a status frame, which is the daemon's own account of where
// it thinks a client is reading.
type statusReport struct {
	Offset uint32 `json:"offset"`
	Max    uint32 `json:"max"`
	Paused bool   `json:"paused"`
	Alt    bool   `json:"alt"`
	// Cols and Rows are the effective PTY size, which is not the same quantity as
	// the client's declared size; sizeannounce_test.go is about where they differ.
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// parseStatus decodes a \x01-marked status frame, reporting false for a text frame that
// is not one.
//
// False and not a zero statusReport, because "offset 0, not paused" is exactly what the
// absence checks above are looking for: a frame type this function did not understand
// must not be able to satisfy one.
func parseStatus(data []byte) (statusReport, bool) {
	var env struct {
		Status *statusReport `json:"status"`
	}
	if len(data) == 0 || data[0] != 0x01 {
		return statusReport{}, false
	}
	if json.Unmarshal(data[1:], &env) != nil || env.Status == nil {
		return statusReport{}, false
	}
	return *env.Status, true
}

// awaitPaused waits for a status frame reporting the client paused at offset want, and
// reports how long that took.
//
// Only a frame from the tick loop can answer: the hello frame serveAttach sends before
// the loop exists reports offset 0 whatever the opening frame asked for, so a check that
// read the first status frame would see the handshake's own placeholder.
func awaitPaused(t *testing.T, ch <-chan msg, want uint32, d time.Duration) (time.Duration, bool) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return time.Since(start), false
		}
		// next waits out the whole remaining budget, so a false here is the deadline
		// or a closed connection and never a spin.
		m, ok := next(ch, left)
		if !ok {
			return time.Since(start), false
		}
		if m.op != ws.OpText {
			continue
		}
		if s, ok := parseStatus(m.data); ok && s.Paused && s.Offset == want {
			return time.Since(start), true
		}
	}
}

// fillHistory runs a command in the window until the daemon reports at least want lines
// of scrollback, and returns the count it reported.
//
// The witness is the daemon's own "max" rather than a sleep, because every offset the
// cases above send is clamped to it: a case that ran before the history existed would
// assert nothing. The connection is closed before returning so that the attacher it
// registered does not go on voting on the size of the one the caller opens next.
func fillHistory(t *testing.T, sock, session string, want uint32) uint32 {
	t.Helper()
	c := openWS(t, sock, session)
	ch := reader(c)
	if err := c.WriteMessage(ws.OpText, resizeCtl(80, 24)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(ws.OpBinary, []byte("seq 1 200\n")); err != nil {
		t.Fatal(err)
	}
	const d = 20 * time.Second
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			t.Fatalf("the window's scrollback never reached %d lines in %v", want, d)
		}
		m, ok := next(ch, left)
		if !ok {
			t.Fatalf("the window's scrollback never reached %d lines in %v, so an offset inside it could not have been told from one clamped to zero", want, d)
		}
		if m.op != ws.OpText {
			continue
		}
		if s, ok := parseStatus(m.data); ok && s.Max >= want {
			c.Close()
			return s.Max
		}
	}
}
