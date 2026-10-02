package shelld

import (
	"math"
	"testing"
	"time"
)

// StallTimeoutMS is an unsigned millisecond count and time.Duration is a signed
// nanosecond one, so the range a caller can write is far wider than the range that
// converts. The values past the limit below are the ones that matter: the wrap is
// not a large duration but an arbitrary one, and each arbitrary result is silent.
// They are measured, not derived — the multiply's result past the limit depends on
// the input's low bits, so which failure a given input produces cannot be read off
// its size.
func TestStallTimeoutClampsInsteadOfWrapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		// Zero stays the opt-out, and an ordinary value is untouched: a clamp that
		// changed either would break the documented configuration.
		{"zero is the opt-out", 0, 0},
		{"the default", 300_000, 5 * time.Minute},
		{"the largest value that converts", maxStallMS, time.Duration(maxStallMS) * time.Millisecond},
		// Unclamped this is about -2562047h, which ExpireOlderThan reads as the
		// opt-out, so the deadline the caller asked for disappears.
		{"just past the limit", maxStallMS + 1, math.MaxInt64},
		// The other direction, and the reason this row exists: unclamped it
		// multiplies to 448.384µs, which expires every payload on its first poll, so
		// a caller asking to wait a very long time loses everything it queued
		// instead. It is the only case here that wraps to a positive duration, so
		// the positivity check below cannot stand in for it.
		{"a value that wraps to a usable-looking deadline", 18_446_744_073_710, math.MaxInt64},
		// Unclamped this is -1ms: the opt-out again.
		{"the largest uint64", math.MaxUint64, math.MaxInt64},
	} {
		if got := stallTimeout(tc.ms); got != tc.want {
			t.Errorf("%s: stallTimeout(%d) = %v, want %v", tc.name, tc.ms, got, tc.want)
		}
	}
	// The clamp has to land on a value ExpireOlderThan treats as a deadline rather
	// than as the opt-out, which is the trap the negative wrap falls into.
	if stallTimeout(math.MaxUint64) <= 0 {
		t.Error("the clamped deadline is not positive, so ExpireOlderThan ignores it")
	}
}

// classifyEscape decides whether a line of history can be placed, and the scan's
// direction of error depends on it: calling a sequence harmless when it is not is
// how a busy program's screen gets read as an idle prompt, which is the one
// outcome the whole design forbids. The finals 'h' and 'l' and the ESC
// intermediate range both cover harmless and harmful sequences, so a check on the
// final byte or the range alone is wrong for the ones listed here.
func TestClassifyEscapeRefusesSequencesThatChangeTheLine(t *testing.T) {
	for _, tc := range []struct {
		seq  string
		why  string
		kind escKind
	}{
		{"\x1b[?2004h", "bracketed paste, bash's prompt bracket", escHarmless},
		{"\x1b[?2004l", "and its reset", escHarmless},
		{"\x1b[?25l", "cursor hidden, ConPTY's repaint bracket", escHarmless},
		{"\x1b[?25h", "and shown again", escHarmless},
		{"\x1b[?1h", "application cursor keys: an input encoding", escHarmless},
		{"\x1b[0m", "SGR: colour moves no glyph", escHarmless},
		{"\x1b(B", "charset designation", escHarmless},
		{"\x1b[K", "erase in line, the one editing sequence the replay models", escEraseLine},

		{"\x1b[?1049h", "the alternate screen: a different screen entirely", escUnknown},
		{"\x1b[?1049l", "and back off it", escUnknown},
		{"\x1b[4h", "IRM: later glyphs shift the line instead of overwriting", escUnknown},
		{"\x1b[20h", "LNM: changes what a bare CR does", escUnknown},
		{"\x1b[?7l", "autowrap off: the line no longer wraps where the replay wraps it", escUnknown},
		{"\x1b[?3h", "DECCOLM: 132 columns, and it clears the screen", escUnknown},
		{"\x1b#8", "DECALN: fills the screen with E and homes the cursor", escUnknown},
		{"\x1b#6", "double-width line: every glyph on it covers two columns", escUnknown},
		{"\x1b[?25;1049h", "a list is not parsed, so it is refused even for one that is", escUnknown},
		{"\x1b[2J", "erase in display", escUnknown},
		{"\x1b[5;1H", "cursor addressing", escUnknown},
	} {
		n, kind, _ := classifyEscape(tc.seq)
		if kind != tc.kind {
			t.Errorf("%q (%s) classified %v, want %v", tc.seq, tc.why, kind, tc.kind)
		}
		// The length is what the replay advances by, so a sequence classified
		// correctly but measured short leaves its own bytes to be read as text.
		if n != len(tc.seq) {
			t.Errorf("%q consumed %d bytes of %d", tc.seq, n, len(tc.seq))
		}
	}
}

// The end the classifier serves: a screen holding one of those sequences is not
// reported as an idle prompt. This drives the scan rather than the classifier, so
// it fails if the sequence is reclassified or if the scan stops consulting the
// verdict — and it is the path the drain loop actually takes.
func TestTheIdleScanRefusesAPromptOnALineThatChangedTheScreen(t *testing.T) {
	d := NewIdleDetector(`(?m)^\s*\$\s*$`, 5)
	// Premise: the bare prompt is recognised, so a refusal below is the sequence
	// and not a pattern that never matched.
	if idle, _, refused := d.idleScan(renderScanLines("$ ")); !idle || refused {
		t.Fatalf("the plain prompt scanned idle=%v refused=%v; the fixture is wrong", idle, refused)
	}
	for _, seq := range []string{"\x1b[?1049h", "\x1b[4h", "\x1b#8", "\x1b[?7l"} {
		// trusted is what the render sets from the classifier's verdict, so the
		// line is rendered here rather than asserted on directly.
		lines := renderScanLines(seq + "$ ")
		if idle, _, refused := d.idleScan(lines); idle || !refused {
			t.Errorf("a line carrying %q scanned as an idle prompt (idle=%v refused=%v)",
				seq, idle, refused)
		}
	}
}
