package vt

// The DCS half of the parser's state machine: ESC P and everything reachable
// from it.
//
// Nothing in this package acts on a DCS sequence — there is no sixel image, no
// DECRQSS reply — so the whole subtree exists to swallow one. That is what
// makes it worth testing rather than the opposite: the only way these states can
// fail is by letting a payload out, and a payload that escapes does not look
// like a parser bug from the outside. It looks like a program printing garbage,
// or a status line painted in the wrong colour, long after the sequence that
// caused it.
//
// The DCS states are also the only live callers of subparam and paramSep.
// advanceCSIRun handles digits, ';' and ':' inline for CSI, and only falls back
// to advanceCsiEntry/advanceCsiParam for a byte it does not recognise, so the
// ':' and ';' arms of those two functions are dead code. Everything this file
// asserts about how a subparameter is stored is therefore asserted through the
// path that can reach it.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

var stateNames = map[parserState]string{
	stateGround:             "Ground",
	stateCsiEntry:           "CsiEntry",
	stateCsiIgnore:          "CsiIgnore",
	stateCsiIntermediate:    "CsiIntermediate",
	stateCsiParam:           "CsiParam",
	stateDcsEntry:           "DcsEntry",
	stateDcsIgnore:          "DcsIgnore",
	stateDcsIntermediate:    "DcsIntermediate",
	stateDcsParam:           "DcsParam",
	stateDcsPassthrough:     "DcsPassthrough",
	stateEscape:             "Escape",
	stateEscapeIntermediate: "EscapeIntermediate",
	stateOscString:          "OscString",
	stateSosPmApcString:     "SosPmApcString",
}

func stateName(s parserState) string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("parserState(%d)", s)
}

// paramList renders the parameters the parser has ended, separated by ';' with
// each one's subparameters separated by ':' — the list a dispatch reads.
func paramList(p *Processor) string {
	var sb strings.Builder
	it := paramsIter{p: &p.params}
	for v, ok := it.next(); ok; v, ok = it.next() {
		if sb.Len() > 0 {
			sb.WriteByte(';')
		}
		for i, x := range v {
			if i > 0 {
				sb.WriteByte(':')
			}
			sb.WriteString(strconv.Itoa(int(x)))
		}
	}
	return sb.String()
}

// parserParams is paramList, then '|', then the parameter still being
// accumulated.
//
// Both halves are needed. A parameter is only pushed into the list when
// something ends it — a separator, or the final byte that dispatches — so
// mid-sequence the list is empty and the digits live in p.param. The split is
// also what tells ':' from ';': both leave "1|2" while the sequence is still
// being read, and they differ only once the final byte pushes the last
// parameter, at which point one reads "1:2" and the other "1;2".
//
// The accumulator is not cleared by the push, so after a hook it still holds a
// copy of the last parameter. That shows up in the expectations below and is
// harmless: every sequence starts with a reset.
func parserParams(p *Processor) string {
	return paramList(p) + "|" + strconv.Itoa(int(p.param))
}

// TestDCSStateMachine walks every byte class out of every DCS state and pins
// where it lands, what it left in the parameter list and what it collected.
//
// Each case is a whole feed from Ground, so the prefix names the state under
// test: "\x1bP" is DCS entry, "\x1bP1" is DCS param, "\x1bP " is DCS
// intermediate, "\x1bP@" is DCS passthrough and "\x1bP1<" is DCS ignore.
func TestDCSStateMachine(t *testing.T) {
	cases := []struct {
		seq      string
		state    parserState
		params   string
		inter    string
		ignoring bool
	}{
		// DCS entry.
		{"\x1bP", stateDcsEntry, "|0", "", false},
		{"\x1bP\x05", stateDcsEntry, "|0", "", false},      // C0 is swallowed, not executed
		{"\x1bP ", stateDcsIntermediate, "|0", " ", false}, // 0x20-0x2f collects
		{"\x1bP1", stateDcsParam, "|1", "", false},         // a digit starts a parameter
		{"\x1bP:", stateDcsParam, "0|0", "", false},        // a leading ':' ends an implicit 0 as a subparameter
		{"\x1bP;", stateDcsParam, "0|0", "", false},        // a leading ';' ends an implicit 0 as a parameter
		{"\x1bP<", stateDcsParam, "|0", "<", false},        // a private marker is collected, unlike in CSI
		{"\x1bP\x7f", stateDcsEntry, "|0", "", false},      // DEL is ignored
		{"\x1bP\x80", stateDcsEntry, "|0", "", false},      // only CAN, SUB and ESC mean anything up here
		{"\x1bP\x18", stateGround, "|0", "", false},        // CAN abandons the sequence
		{"\x1bP\x1a", stateGround, "|0", "", false},        // SUB likewise
		{"\x1bP\x1b", stateEscape, "|0", "", false},        // ESC starts over
		{"\x1bP@", stateDcsPassthrough, "0|0", "", false},  // the final byte hooks with one implicit parameter

		// DCS param.
		{"\x1bP1\x05", stateDcsParam, "|1", "", false},
		{"\x1bP1 ", stateDcsIntermediate, "|1", " ", false},
		{"\x1bP12", stateDcsParam, "|12", "", false},
		{"\x1bP1:2", stateDcsParam, "1|2", "", false},
		{"\x1bP1;2", stateDcsParam, "1|2", "", false},
		{"\x1bP1:2@", stateDcsPassthrough, "1:2|2", "", false}, // ':' extended one parameter
		{"\x1bP1;2@", stateDcsPassthrough, "1;2|2", "", false}, // ';' ended it
		{"\x1bP1<", stateDcsIgnore, "|1", "", false},           // a marker after a parameter voids the sequence
		{"\x1bP1@", stateDcsPassthrough, "1|1", "", false},
		{"\x1bP1\x7f", stateDcsParam, "|1", "", false},
		{"\x1bP1\x80", stateDcsParam, "|1", "", false},
		// CAN and SUB return to ground without clearing what was accumulated,
		// which is what anywhere does for the CSI states too. Only ESC resets,
		// and the leftovers are harmless because the next sequence resets.
		{"\x1bP1\x18", stateGround, "|1", "", false},
		{"\x1bP1;2\x1b", stateEscape, "|0", "", false}, // ESC resets what was read

		// DCS intermediate.
		{"\x1bP \x05", stateDcsIntermediate, "|0", " ", false},
		{"\x1bP !", stateDcsIntermediate, "|0", " !", false},
		{"\x1bP !\"", stateDcsIntermediate, "|0", " !", true}, // a third intermediate overflows
		{"\x1bP 1", stateDcsIgnore, "|0", " ", false},         // a parameter after an intermediate voids it
		{"\x1bP @", stateDcsPassthrough, "0|0", " ", false},
		{"\x1bP \x7f", stateDcsIntermediate, "|0", " ", false},
		{"\x1bP \x80", stateDcsIntermediate, "|0", " ", false},
		{"\x1bP \x1a", stateGround, "|0", " ", false},
		{"\x1bP \x1b", stateEscape, "|0", "", false}, // ESC drops the intermediates too

		// DCS passthrough. The payload is consumed without being stored, so
		// what is asserted is that no byte of it changes the parser.
		{"\x1bP@payload", stateDcsPassthrough, "0|0", "", false},
		{"\x1bP@a\x0a", stateDcsPassthrough, "0|0", "", false}, // LF is payload here, not a linefeed
		{"\x1bP@a\x7f", stateDcsPassthrough, "0|0", "", false}, // outside put's range, and no other arm claims it
		{"\x1bP@a\x80", stateDcsPassthrough, "0|0", "", false},
		{"\x1bP@a\x18", stateGround, "0|0", "", false},  // CAN ends it
		{"\x1bP@a\x1a", stateGround, "0|0", "", false},  // SUB ends it
		{"\x1bP@a\x9c", stateGround, "0|0", "", false},  // 8-bit ST ends it, keeping the parameters
		{"\x1bP@a\x1b", stateEscape, "|0", "", false},   // ESC ends it, dropping them
		{"\x1bP@a\x1b\\", stateGround, "|0", "", false}, // ESC \ is the 7-bit ST

		// DCS ignore: everything is dropped until something abandons the
		// sequence.
		{"\x1bP1<payload;9", stateDcsIgnore, "|1", "", false},
		{"\x1bP1<\x0a", stateDcsIgnore, "|1", "", false},
		{"\x1bP1<\x18", stateGround, "|1", "", false},
		{"\x1bP1<\x1b", stateEscape, "|0", "", false},
		{"\x1bP1<\x1b\\", stateGround, "|0", "", false},

		// CSI ignore, whose cold arms are reachable the same way: a private
		// marker after a parameter.
		{"\x1b[1<", stateCsiIgnore, "|1", "", false},
		{"\x1b[1<\x07", stateCsiIgnore, "|1", "", false}, // executable, and BEL has no grid effect
		{"\x1b[1< ", stateCsiIgnore, "|1", "", false},    // 0x20-0x3f ignored
		{"\x1b[1<\x7f", stateCsiIgnore, "|1", "", false}, // DEL ignored
		{"\x1b[1<\x80", stateCsiIgnore, "|1", "", false}, // nothing claims an 8-bit byte
		{"\x1b[1<\x1b", stateEscape, "|0", "", false},
		{"\x1b[1<m", stateGround, "|1", "", false}, // SGR 1 if it were dispatched; it returns to ground instead
	}
	for _, tc := range cases {
		term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(term, p, tc.seq)
		if p.state != tc.state {
			t.Errorf("%q: state %s, want %s", tc.seq, stateName(p.state), stateName(tc.state))
		}
		if got := parserParams(p); got != tc.params {
			t.Errorf("%q: params %q, want %q", tc.seq, got, tc.params)
		}
		if got := string(p.intermediates[:p.intermediateIdx]); got != tc.inter {
			t.Errorf("%q: intermediates %q, want %q", tc.seq, got, tc.inter)
		}
		if p.ignoring != tc.ignoring {
			t.Errorf("%q: ignoring %v, want %v", tc.seq, p.ignoring, tc.ignoring)
		}
		// No case here writes anything, and none of them moves the cursor:
		// every byte belongs to the sequence.
		if got := rowText(term, 0); got != "" {
			t.Errorf("%q: row 0 is %q, want it empty: a byte of the sequence reached the screen", tc.seq, got)
		}
		if c := term.Grid().Cursor.Point; c != (Point{}) {
			t.Errorf("%q: cursor at %+v, want the origin", tc.seq, c)
		}
		// Nor does any of them dispatch. The CSI-ignore rows are what this
		// catches: each ends with a final byte that would be a valid SGR if the
		// sequence had not already been voided, and landing in Ground is the
		// same answer either way.
		if f := term.Grid().Cursor.Template.Flags(); f != 0 {
			t.Errorf("%q: template flags %x, want none: the sequence was dispatched", tc.seq, f)
		}
	}
}

// TestDCSSwallowsControlCharacters is the assertion the state table cannot make
// on its own. Landing in the right state says nothing about what a C0 byte did
// on the way: in every other string state a control character is executed, and
// in these it must not be. A DCS payload holding a newline — a sixel image
// broken across lines, or a shell's DECRQSS reply — would otherwise scroll the
// screen while it was being ignored.
func TestDCSSwallowsControlCharacters(t *testing.T) {
	const controls = "\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x07"
	for _, prefix := range []string{"\x1bP", "\x1bP1", "\x1bP ", "\x1bP@", "\x1bP1<"} {
		term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(term, p, prefix)
		before := p.state
		feed(term, p, controls)
		if p.state != before {
			t.Errorf("%q: control characters moved the parser from %s to %s",
				prefix, stateName(before), stateName(p.state))
		}
		if c := term.Grid().Cursor.Point; c != (Point{}) {
			t.Errorf("%q: cursor at %+v after %q, want the origin", prefix, c, controls)
		}
		if g := term.Grid(); g.Cursor.Charsets[0] != CharsetASCII {
			t.Errorf("%q: SO/SI inside the sequence changed the charset designation", prefix)
		}
		// Feeding a character afterwards shows where the cursor really is:
		// a linefeed that was executed would put it on row 1.
		feed(term, p, "\x1b\\x")
		if got, other := rowText(term, 0), rowText(term, 1); got != "x" || other != "" {
			t.Errorf("%q: after the sequence, rows are %q and %q, want %q and empty",
				prefix, got, other, "x")
		}
	}
}

// TestDCSPayloadNeverReachesTheScreen runs the shapes a real program sends,
// whole, with text on either side. These are the sequences that arrive from
// inside a pane in practice: a DECRQSS request and its reply, and a sixel
// image. Each one has to come out as if it were never sent.
func TestDCSPayloadNeverReachesTheScreen(t *testing.T) {
	cases := []struct {
		name string
		seq  string
	}{
		{"DECRQSS for SGR", "\x1bP$qm\x1b\\"},
		{"a DECRQSS reply", "\x1bP1$r0;1;31m\x1b\\"},
		{"a reply ended by 8-bit ST", "\x1bP1$r0;1;31m\x9c"},
		{"a sixel image", "\x1bPq#0;2;0;0;0#0~~~-#0~~\x1b\\"},
		{"a sixel with parameters and subparameters", "\x1bP0;1:2;8q\"1;1;6;6#0~~\x1b\\"},
		{"a voided sequence", "\x1bP1<anything at all\x1b\\"},
		{"an empty payload", "\x1bPq\x1b\\"},
	}
	for _, tc := range cases {
		term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(term, p, "A"+tc.seq+"B")
		if got := rowText(term, 0); got != "AB" {
			t.Errorf("%s: row 0 is %q, want %q", tc.name, got, "AB")
		}
		if p.state != stateGround {
			t.Errorf("%s: parser left in %s, want Ground", tc.name, stateName(p.state))
		}
		// The payloads hold SGR sequences and colour parameters; none of them
		// is a sequence while the parser is inside a DCS.
		if f := term.Grid().Cursor.Template.Flags(); f != 0 {
			t.Errorf("%s: template flags %x, want none: the payload was interpreted", tc.name, f)
		}
		if c := term.Grid().Cursor.Point; c != (Point{Line: 0, Column: 2}) {
			t.Errorf("%s: cursor at %+v, want line 0 column 2", tc.name, c)
		}
	}
}

// TestDCSSubparametersReachTheParameterList covers how ':' and ';' differ where
// it is visible. A DCS hook stores its parameters the same way a CSI dispatch
// does, so this is the storage layout every sequence with subparameters depends
// on — SGR 38:2:r:g:b included, even though that arrives by a path whose own
// ':' handling is inlined elsewhere.
func TestDCSSubparametersReachTheParameterList(t *testing.T) {
	cases := []struct {
		seq  string
		want string
	}{
		{"\x1bPq", "0"},
		{"\x1bP1q", "1"},
		{"\x1bP1;2;3q", "1;2;3"},
		{"\x1bP1:2:3q", "1:2:3"},
		{"\x1bP1:2;3q", "1:2;3"},
		{"\x1bP1;2:3q", "1;2:3"},
		{"\x1bP:1q", "0:1"}, // an empty first subparameter is a 0
		{"\x1bP;1q", "0;1"}, // and so is an empty first parameter
		{"\x1bP1;q", "1;0"}, // a trailing separator leaves an implicit 0
		{"\x1bP1:q", "1:0"},
		{"\x1bP12:345q", "12:345"},
		{"\x1bP99999q", "65535"}, // a parameter saturates rather than wrapping
	}
	for _, tc := range cases {
		term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(term, p, tc.seq)
		if p.state != stateDcsPassthrough {
			t.Errorf("%q: state %s, want DcsPassthrough", tc.seq, stateName(p.state))
		}
		if got := paramList(p); got != tc.want {
			t.Errorf("%q: parameters %q, want %q", tc.seq, got, tc.want)
		}
	}
}

// TestDCSParametersFillUp covers the boundary shared by every parameter sink.
// The list is a fixed array, and a sequence with more parameters than it holds
// has to stop filling it: writing past the end would corrupt the parameters of
// the next sequence, which is the one that would be blamed.
func TestDCSParametersFillUp(t *testing.T) {
	term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
	feed(term, p, "\x1bP"+strings.Repeat("7;", maxParams))
	if p.ignoring {
		t.Fatalf("ignoring after %d separators, want the list exactly full", maxParams)
	}
	if p.params.len != maxParams {
		t.Fatalf("%d parameters, want %d", p.params.len, maxParams)
	}
	// One parameter too many, in each of the three ways a byte can add one.
	for _, b := range []string{"9", ";", ":"} {
		term, p := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(term, p, "\x1bP"+strings.Repeat("7;", maxParams)+b)
		if !p.ignoring {
			t.Errorf("%q past a full parameter list: ignoring is false", b)
		}
		if p.params.len != maxParams {
			t.Errorf("%q past a full parameter list: %d parameters, want %d", b, p.params.len, maxParams)
		}
	}
	// And the final byte, which pushes the parameter it was accumulating.
	term, p = NewTerm(Config{}, 10, 3, nil), NewProcessor()
	feed(term, p, "\x1bP"+strings.Repeat("7;", maxParams)+"q")
	if p.state != stateDcsPassthrough {
		t.Errorf("state %s after a full sequence, want DcsPassthrough", stateName(p.state))
	}
	if p.params.len != maxParams || !p.ignoring {
		t.Errorf("%d parameters, ignoring=%v, want %d and true", p.params.len, p.ignoring, maxParams)
	}
	feed(term, p, "payload\x1b\\Z")
	if got := rowText(term, 0); got != "Z" {
		t.Errorf("row 0 is %q, want %q: an over-long sequence must still terminate", got, "Z")
	}
}

// TestDCSSplitAcrossWrites feeds each sequence one byte at a time. The parser
// keeps its state between calls to Advance, and a pane's output arrives in
// whatever chunks the pty hands over, so a DCS is routinely split — most often
// by a sixel image far larger than one read.
func TestDCSSplitAcrossWrites(t *testing.T) {
	for _, seq := range []string{
		"\x1bP1$r0;1;31m\x1b\\",
		"\x1bPq#0;2;0;0;0#0~~-~~\x9c",
		"\x1bP1:2;3|\x1b\\",
		"\x1bP1<voided\x1b\\",
	} {
		whole, wp := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		feed(whole, wp, "A"+seq+"B")
		split, sp := NewTerm(Config{}, 10, 3, nil), NewProcessor()
		for _, b := range []byte("A" + seq + "B") {
			sp.Advance(split, []byte{b})
		}
		if wp.state != sp.state {
			t.Errorf("%q: byte at a time leaves %s, whole leaves %s",
				seq, stateName(sp.state), stateName(wp.state))
		}
		if a, b := parserParams(wp), parserParams(sp); a != b {
			t.Errorf("%q: parameters %q byte at a time, %q whole", seq, b, a)
		}
		if a, b := rowText(whole, 0), rowText(split, 0); a != b {
			t.Errorf("%q: row 0 is %q byte at a time, %q whole", seq, b, a)
		}
	}
}
