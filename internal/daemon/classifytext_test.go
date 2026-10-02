package daemon

import (
	"bytes"
	"testing"
)

// The opening frame and every later frame must be classified identically. This is the
// property that was broken: serveAttach's handshake asked only "is this a resize?" and
// funnelled everything else into the input path, so a scroll arriving first was typed
// into the program while the same bytes arriving second scrolled.
//
// Each case names what the frame is, so a mutant that changes one arm fails with the
// arm's own name rather than with a count.
func TestClassifyTextSeparatesControlFromInput(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		resize bool
		scroll bool
		input  []byte
	}{{
		name: "text without the control marker is input",
		data: []byte("ls -l\r"),
		// The whole frame, marker-free, reaches the program unchanged.
		input: []byte("ls -l\r"),
	}, {
		name:   "a well-formed resize is a resize",
		data:   []byte("\x01{\"resize\":{\"cols\":80,\"rows\":24}}"),
		resize: true,
	}, {
		// The case that was typed into the user's shell when it arrived first.
		name:   "a well-formed scroll is a scroll",
		data:   []byte("\x01{\"scroll\":{\"op\":\"up\",\"lines\":1}}"),
		scroll: true,
	}, {
		// A frame that carries the marker has said which of the two it is. Typing it
		// would deliver the 0x01 -- Ctrl-A -- and then the JSON to the program.
		name: "a marked frame that does not parse is neither",
		data: []byte("\x01{\"resize\":{\"cols\":\"not-a-number\"}}"),
	}, {
		name: "a marked frame that is not even JSON is neither",
		data: []byte("\x01{ not json at all"),
	}, {
		// The marker is the two bytes 0x01 '{', because every control message is a JSON
		// object. A lone 0x01 is Ctrl-A -- readline's start-of-line, tmux's prefix -- and
		// a client typing it sends exactly this frame.
		name:  "a lone 0x01 is input",
		data:  []byte("\x01"),
		input: []byte("\x01"),
	}, {
		// Ctrl-A then Ctrl-K, which is how a client clears the line.
		name:  "0x01 followed by anything but a brace is input",
		data:  []byte("\x01\x0b"),
		input: []byte("\x01\x0b"),
	}, {
		name: "a marked frame asking for nothing is neither",
		data: []byte("\x01{}"),
	}, {
		name: "an empty frame is neither",
		data: nil,
	}, {
		// A frame may carry both objects, and this reports both rather than choosing:
		// the choice is the caller's, and both callers make it with a switch whose
		// resize arm comes first, so a frame carrying both is the resize to either of
		// them. Choosing here instead would hide that agreement inside the classifier,
		// where no test of a caller could show it.
		// TestAnOpeningScrollScrollsAndAResizeInTheSameFrameOutranksIt pins the
		// agreement itself, against serveAttach in both positions.
		name:   "a frame carrying both reports both, and the caller decides",
		data:   []byte("\x01{\"resize\":{\"cols\":80,\"rows\":24},\"scroll\":{\"op\":\"up\"}}"),
		resize: true,
		scroll: true,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyText(c.data)
			if (got.resize != nil) != c.resize {
				t.Errorf("resize is %v, want set=%v: %q", got.resize, c.resize, c.data)
			}
			if (got.scroll != nil) != c.scroll {
				t.Errorf("scroll is %v, want set=%v: %q", got.scroll, c.scroll, c.data)
			}
			if !bytes.Equal(got.input, c.input) {
				t.Errorf("input is %q, want %q: a frame classified as input is written to the program as keystrokes, so a wrong answer here either loses a client's typing or types a control frame at it", got.input, c.input)
			}
		})
	}
}

// The marker is what makes the drop safe to reason about, so pin the reason rather than
// only the outcome: no frame that fails to parse may ever be classified as input while
// carrying the marker, because the marker is the first byte that would be typed.
func TestAMarkedFrameIsNeverClassifiedAsInput(t *testing.T) {
	marked := [][]byte{
		[]byte("\x01{"),
		[]byte("\x01{nope"),
		[]byte("\x01{}"),
		[]byte("\x01{\"resize\":[90,30]}"),
		[]byte("\x01{\"scroll\":{\"op\":5}}"),
		[]byte("\x01{\"resize\":{\"cols\":70000,\"rows\":24}}"),
		[]byte("\x01{\x01\x01"),
	}
	for _, data := range marked {
		got := classifyText(data)
		if got.input != nil {
			t.Errorf("classifyText(%q).input = %q; a marked frame classified as input types 0x01 (Ctrl-A: start-of-line to a shell, the prefix key to a multiplexer) and then its payload into the program", data, got.input)
		}
	}
}

// The other side of the marker: a frame that starts with 0x01 but not with 0x01 '{' is
// not a control frame at all, so it is typed. Both daemons used to drop every
// 0x01-led text frame, and a browser client sends keystrokes as text frames, so Ctrl-A and
// a clear-line (0x01 0x0b) never reached the program.
func TestA0x01WithoutABraceIsInput(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("\x01"),
		[]byte("\x01\x0b"),
		[]byte("\x01\x01"),
		[]byte("\x01[]"),
		[]byte("\x01 {}"),
	} {
		if got := classifyText(data); !bytes.Equal(got.input, data) || got.resize != nil || got.scroll != nil {
			t.Errorf("classifyText(%q) = %+v, want it typed unchanged: only 0x01 '{' marks a control frame", data, got)
		}
	}
}
