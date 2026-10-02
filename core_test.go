package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
)

// ── Scrollback ─────────────────────────────────────────────────────────────

func TestScrollback(t *testing.T) {
	b := NewScrollback(100)
	if b.Len() != 0 || b.TotalLines() != 0 {
		t.Fatal("new buffer not empty")
	}
	if NewScrollback(0).Capacity() != 10_000 {
		t.Fatal("default capacity")
	}
	b.Write([]byte("hello world\n"))
	if b.Len() != 1 || b.LastN(1) != "hello world" {
		t.Fatalf("single line: %q", b.LastN(1))
	}

	b = NewScrollback(100)
	b.Write([]byte("line1\nline2\nline3\n"))
	if b.Len() != 3 || b.LastN(999) != "line1\nline2\nline3" || b.LastN(0) != "" {
		t.Fatalf("multi: %q", b.LastN(3))
	}

	b = NewScrollback(5)
	for range 10 {
		b.Write([]byte("line\n"))
	}
	if b.Len() != 5 || b.TotalLines() != 10 {
		t.Fatal("wrap")
	}

	b = NewScrollback(100)
	b.Write([]byte("partial"))
	if b.LastN(1) != "partial" {
		t.Fatal("partial")
	}

	b = NewScrollback(100)
	b.Write([]byte("\x1b[32mgreen\x1b[0m\n"))
	if !strings.Contains(b.LastN(1), "\x1b[32m") {
		t.Fatal("ansi preserved")
	}

	b = NewScrollback(100)
	b.Write([]byte("a\nb\nc\nd\ne\n"))
	if b.Range(1, 3) != "b\nc\nd" || b.Range(0, 2) != "a\nb" || b.Range(4, 100) != "e" || b.Range(5, 3) != "" || b.Range(0, 0) != "" {
		t.Fatal("range")
	}

	b = NewScrollback(5)
	for i := range 10 {
		b.Write([]byte(fmt.Sprintf("line%d\n", i)))
	}
	if b.Range(0, 2) != "line5\nline6" || b.Range(3, 2) != "line8\nline9" {
		t.Fatalf("range wrap: %q %q", b.Range(0, 2), b.Range(3, 2))
	}

	b = NewScrollback(100)
	b.Write([]byte("hello\r\nworld\r\n"))
	b.Write([]byte("a\nb\r\nc\n"))
	if b.LastN(5) != "hello\nworld\na\nb\nc" {
		t.Fatalf("crlf: %q", b.LastN(5))
	}
}

// The chunked line store must read back the lines an independent reference
// does, across chunk boundaries, releases, and lines longer than a chunk. Its
// generator leaves some writes without a terminator, which now leaves a line
// open for the next write to extend rather than ending it, so the reference is
// lineModel; and the byte budget can evict a long line before the line count
// would, so what Scrollback retains is checked as the newest of the reference's
// lines rather than as all of them.
func TestScrollbackMatchesLineRing(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	writes := scrollbackIters(20_000)
	for _, capacity := range []int{1, 2, 7, 100, 1000} {
		b, m := NewScrollback(capacity), newLineModel(capacity+4)
		for i := range writes {
			var data []byte
			for range rng.Intn(4) {
				n := rng.Intn(200)
				switch rng.Intn(50) {
				case 0:
					n = sbMaxChunk + rng.Intn(3*sbMaxChunk)
				case 1:
					n = sbMaxChunk/2 + rng.Intn(sbMaxChunk/2)
				}
				data = append(data, bytes.Repeat([]byte{byte('a' + i%26)}, n)...)
				data = append(data, fmt.Sprint(i)...)
				if rng.Intn(8) != 0 {
					data = append(data, "\r\n"...)
				}
			}
			b.Write(data)
			m.Write(data)
			// Cheap every write, so a divergence names the write that caused
			// it; the full comparison copies the whole history.
			if b.TotalLines() != m.count {
				t.Fatalf("cap %d write %d: started %d lines, want %d", capacity, i, b.TotalLines(), m.count)
			}
			if i%97 == 0 {
				tag := fmt.Sprintf("cap %d write %d", capacity, i)
				checkAgainstModel(t, tag, b, m)
				checkJoinedReads(t, tag, b, m)
			}
		}
		// Only chunks holding retained lines stay referenced, plus spares.
		if used, live := b.retainedBytes(), b.liveBytes(); used > 2*live+2*sbMaxChunk+sbSpares*sbMaxChunk {
			t.Fatalf("cap %d: chunks hold %d bytes for %d live", capacity, used, live)
		}
	}
}

// Once history is full, storing a line allocates nothing, whatever the mix
// of line lengths.
func TestScrollbackSteadyStateAllocs(t *testing.T) {
	b := NewScrollback(1000)
	var burst []byte
	for i := range 101 {
		burst = append(append(burst, bytes.Repeat([]byte("x"), (i*397)%3000)...), '\n')
	}
	for range 50 {
		b.Write(burst)
	}
	if n := testing.AllocsPerRun(100, func() { b.Write(burst) }); n != 0 {
		t.Fatalf("%v allocations per 101 lines", n)
	}
}

// A window whose history went from wide lines to short ones keeps spares in
// proportion to what it still holds, not the full pool.
func TestScrollbackSparesShrinkWithHistory(t *testing.T) {
	lines := func(n, width int) []byte {
		var b []byte
		for i := range n {
			b = append(append(b, bytes.Repeat([]byte{byte('a' + i%26)}, width)...), '\n')
		}
		return b
	}
	b := NewScrollback(10_000)
	for _, phase := range [][]byte{lines(10_000, 4000), lines(10_000, 10)} {
		for len(phase) > 0 {
			k := min(len(phase), 4096)
			b.Write(phase[:k])
			phase = phase[k:]
		}
	}
	spares := b.retainedBytes()
	for _, c := range b.chunks {
		spares -= cap(c)
	}
	if held := b.retainedBytes() - spares; spares > held/sbSpareShare {
		t.Fatalf("%d spare bytes kept beside %d bytes of chunks for %d live", spares, held, b.liveBytes())
	}
}

// retainedBytes is the chunk capacity a scrollback holds, spares included.
func (s *Scrollback) retainedBytes() int {
	n := 0
	for _, c := range s.chunks {
		n += cap(c)
	}
	for _, c := range s.spares[:s.nspare] {
		n += cap(c)
	}
	return n
}

// liveBytes is the length of the lines a scrollback retains. Slots the ring
// has evicted from the front still hold their old ref, so it counts the
// retained range rather than every slot.
func (s *Scrollback) liveBytes() int {
	n := 0
	for i := range s.n {
		n += len(s.line(i))
	}
	return n
}

// heldAndSpares is the chunk capacity a scrollback holds for retained lines,
// and separately the capacity parked in spares.
func (s *Scrollback) heldAndSpares() (held, spares int) {
	for _, c := range s.chunks {
		held += cap(c)
	}
	for _, c := range s.spares[:s.nspare] {
		spares += cap(c)
	}
	return held, spares
}

// ── Inject ─────────────────────────────────────────────────────────────────

func TestInjectQueue(t *testing.T) {
	q := NewInjectQueue(64)
	msg, err := q.Enqueue("hello", 0)
	if err != nil || msg.Payload != "hello" || !strings.HasPrefix(msg.ID, "inj-") || q.Len() != 1 {
		t.Fatal("enqueue")
	}
	if !q.TryRecvSignal() || q.TryRecvSignal() {
		t.Fatal("signal is one-shot")
	}
	if out, _ := q.Dequeue(); out.Payload != "hello" || q.Len() != 0 {
		t.Fatal("dequeue")
	}

	for _, p := range []struct {
		s string
		p int32
	}{{"normal1", 0}, {"normal2", 0}, {"high1", 1}, {"normal3", 0}} {
		_, _ = q.Enqueue(p.s, p.p)
	}
	if a, _ := q.Dequeue(); a.Payload != "high1" {
		t.Fatal("priority first")
	}
	if a, _ := q.Dequeue(); a.Payload != "normal1" {
		t.Fatal("fifo within priority")
	}

	small := NewInjectQueue(3)
	for _, s := range []string{"a", "b", "c"} {
		_, _ = small.Enqueue(s, 0)
	}
	if _, err := small.Enqueue("d", 0); err == nil || small.Len() != 3 {
		t.Fatal("overflow")
	}
	if _, ok := NewInjectQueue(64).Dequeue(); ok {
		t.Fatal("empty dequeue")
	}
}

func TestInputTracker(t *testing.T) {
	tr := NewInputTracker(50)
	if !tr.IsQuiet() {
		t.Fatal("initially quiet")
	}
	tr.Touch()
	if tr.IsQuiet() {
		t.Fatal("not quiet after touch")
	}
	time.Sleep(80 * time.Millisecond)
	if !tr.IsQuiet() {
		t.Fatal("quiet after debounce")
	}
}

func TestIdleDetector(t *testing.T) {
	d := NewIdleDetector("", 5)
	for in, want := range map[string]bool{
		"❯": true, "  ❯  ": true, "❯\n": true, "some output\n❯\n": true, "some output\n❯": true,
		"working...": false, "": false, "❯ typing something": false,
		"\x1b[32m\x1b[0m❯\x1b[0m\n": true,
		"\u00a0❯\u3000":             true, // Rust \s is Unicode White_Space
		"out\n\x1b[?2004h❯":         true,
		"\x1b[?25l❯\x1b[?25h":       true,
	} {
		if got := d.IsIdle(in); got != want {
			t.Errorf("IsIdle(%q) = %v", in, got)
		}
	}
	custom := NewIdleDetector(`(?m)^\$\s*$`, 5)
	if !custom.IsIdle("$ \n") || custom.IsIdle("❯") {
		t.Fatal("custom pattern")
	}
	if ExtractPendingInput("\x1b[32m❯\x1b[0m", d.Pattern()) != "" || ExtractPendingInput("❯ hello", d.Pattern()) != "" {
		t.Fatal("extract")
	}
}

// Bash writes its prompt, then echoes the typed command in a separate read,
// and the scrollback keeps each read's unterminated tail as its own line. A
// bare prompt line then stays within the scanned lines while the command
// runs; only the last non-empty line may count, or injected input is typed
// into cat, a REPL or a y/n prompt.
func TestIdleOnlyOnLastLine(t *testing.T) {
	d := NewIdleDetector(IdlePatternForShell("/bin/bash"), 5)
	sb := NewScrollback(100)
	steps := []struct {
		chunk string
		idle  bool
	}{
		{"\x1b[?2004h$ ", true},       // prompt
		{"cat", false},                // echoed command, its own read
		{"\r\n\x1b[?2004l\r", false},  // Enter; readline leaves bracketed paste
		{"some output\r\n", false},    // the program's output
		{"", false},                   // cat waiting for input
		{"\x1b[?2004h$ ", true},       // next prompt
		{"read -p '> ' x", false},     // a y/n-style prompt command
		{"\r\n\x1b[?2004l\r> ", true}, // "> " is a prompt by the pattern's design
	}
	for i, st := range steps {
		sb.Write([]byte(st.chunk))
		if got := d.IsIdleScan(idleScanLines(sb, d.ScanLines())); got != st.idle {
			t.Errorf("after step %d (%q): IsIdleScan = %v, want %v; lines %q",
				i, st.chunk, got, st.idle, scanShow(idleScanLines(sb, d.ScanLines())))
		}
	}
}

// scanShow is a []scanLine as a test message: the lines joined with '\n', with a
// "!" on each line the scan could not model. The marker belongs to the test — the
// scan carries the flag beside the text, so that nothing downstream has to know a
// byte value to know what it is reading.
func scanShow(lines []scanLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.text
		if !l.trusted {
			parts[i] = "!" + l.text
		}
	}
	return strings.Join(parts, "\n")
}

// scanText is what the render produced, trust aside.
func scanText(lines []scanLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.text
	}
	return strings.Join(parts, "\n")
}

// trustAll is the same lines with every line trusted: what the scan would have
// reported had it been able to model them. A case that is not idle must be not idle
// because of the trust, and this is what tells that apart from a case whose text
// was never a prompt.
func trustAll(lines []scanLine) []scanLine {
	out := make([]scanLine, len(lines))
	for i, l := range lines {
		out[i] = scanLine{text: l.text, trusted: true}
	}
	return out
}

// untrusted is the indices the scan could not model.
func untrusted(lines []scanLine) []int {
	var out []int
	for i, l := range lines {
		if !l.trusted {
			out = append(out, i)
		}
	}
	return out
}

// A line of raw history holds every state a program drew over in place, because
// nothing rewrites history when the cursor goes back to column 0. The idle scan
// has to see the state a terminal is showing: a prompt written after a '\r' is
// at the start of its line, and a progress bar's earlier states are gone.
//
// Each case asserts what the plain history gives as well, so the test says which
// of the two the pattern needs rather than only that one of them works.
func TestIdleScanSeesWhatTheTerminalShows(t *testing.T) {
	d := NewIdleDetector("", 5)
	for _, c := range []struct {
		name    string
		writes  []string
		scanned string // what the render produced, trust aside
		blocked bool   // ...and the scan could not model the line it would match
		idle    bool
		rawIdle bool
	}{{
		// The regression the reviewer of the joining change predicted, with no
		// escape sequences in the way: once a line spans PTY reads a prompt
		// written after a '\r' is no longer at the start of its line, so an
		// anchored pattern stops matching it. rawIdle false is that regression
		// and idle true is the render repairing it — if this case ever reads
		// rawIdle true, the render has stopped being load-bearing and the rest
		// of these cases no longer justify it.
		//
		// A spinner is the one-character redraw that leaves nothing behind, so
		// this is the shape where a bare '\r' really does hand the whole row to
		// what follows it.
		name:    "a spinner overwritten by the prompt",
		writes:  []string{"⠋\r❯ "},
		scanned: "❯ ",
		idle:    true,
	}, {
		// zsh and fish redraw the prompt from column 0 after the command's
		// output, and joining reads puts both in one line.
		name:    "a prompt redrawn after a carriage return",
		writes:  []string{"building...", "\r\x1b[K❯ "},
		scanned: "❯ ",
		idle:    true,
	}, {
		// A '\r' overwrites, it does not replace: a redraw shorter than what it
		// drew over leaves the tail of the old text on screen. This is the case
		// the reviewer named, and reducing the line to what follows the last
		// '\r' reported a prompt where the terminal shows a test run.
		name:    "a prompt drawn over a longer line",
		writes:  []string{"❯ mycmd\n", "Running 100 tests...\r❯ "},
		scanned: "❯ mycmd\n❯ nning 100 tests...",
	}, {
		// Two carriage returns before the newline: ONLCR turns the program's
		// '\n' into "\r\n", so a line the program ended with a '\r' of its own
		// arrives as "\r\r\n". History keeps the one the terminator did not
		// account for, the line is still on screen, and reducing it to what
		// follows the last '\r' made it vanish.
		name:    "a line ending in a carriage return",
		writes:  []string{"❯ mycmd\n", "still working\r\r\n"},
		scanned: "❯ mycmd\nstill working",
	}, {
		// Fail closed: the modelled part of this line reads as a bare prompt,
		// and it is not one — CSI H put the cursor somewhere this cannot follow,
		// so the glyphs are on a row and at a column the line does not say. The
		// line comes back untrusted and that is what stops the match; before,
		// StripANSI removed the addressing and the leftover glyphs matched as
		// though they were a line of their own.
		name:    "a prompt drawn by a cursor-addressed program",
		writes:  []string{"\x1b[2J\x1b[12;40H❯ "},
		scanned: "❯ ",
		blocked: true,
		rawIdle: true,
	}, {
		name:    "a progress bar still running",
		writes:  []string{"  10%\r", "  55%\r", " 100%"},
		scanned: " 100%",
	}, {
		name:    "a progress bar that finished, then a prompt",
		writes:  []string{"  10%\r 100%\r\n", "❯ "},
		scanned: " 100%\n❯ ",
		idle:    true,
		rawIdle: true,
	}, {
		// The overwritten text is what the whole line would offer a pattern,
		// and it is not on screen: only the last state may count.
		name:    "a prompt overwritten by a command that is running",
		writes:  []string{"❯ \rrunning make"},
		scanned: "running make",
	}, {
		// Both readlines this project meets, as a pair, because which one a host
		// has is not a choice anyone makes: bash 5.2 brackets its prompt with
		// ?2004 and bash 3.2 -- every stock macOS -- with ?1034 instead. Both are
		// input encodings and neither moves a glyph, so both have to leave the line
		// trusted; ?1034 did not, and the cost was every injection on darwin
		// refused forever. rawIdle true on both says StripANSI was never the part
		// that was wrong, so a case that goes blocked here is harmlessMode's.
		name:    "a prompt bracketed by bash 5.2's readline",
		writes:  []string{"\x1b[?2004h❯ "},
		scanned: "❯ ",
		idle:    true,
		rawIdle: true,
	}, {
		name:    "a prompt bracketed by bash 3.2's readline",
		writes:  []string{"\x1b[?1034h❯ "},
		scanned: "❯ ",
		idle:    true,
		rawIdle: true,
	}} {
		t.Run(c.name, func(t *testing.T) {
			sb := NewScrollback(100)
			for _, w := range c.writes {
				sb.Write([]byte(w))
			}
			lines := idleScanLines(sb, d.ScanLines())
			if got := scanText(lines); got != c.scanned {
				t.Errorf("the idle scan sees %q, want %q (history holds %q)", got, c.scanned, sb.LastN(5))
			}
			if got := d.IsIdleScan(lines); got != c.idle {
				t.Errorf("IsIdleScan = %v, want %v (scan %q)", got, c.idle, scanShow(lines))
			}
			// Which of the two reasons a case is not idle for: the text is not a
			// prompt, or the scan cannot vouch for text that reads like one. Every
			// blocked case must be idle once trusted, or it is pinning the text
			// and reporting it as the trust.
			if blocked := len(untrusted(lines)) > 0; blocked != c.blocked {
				t.Errorf("the scan marks lines %v as unmodelled, want blocked = %v (scan %q)",
					untrusted(lines), c.blocked, scanShow(lines))
			} else if blocked && !d.IsIdleScan(trustAll(lines)) {
				t.Errorf("scan %q is not idle even with every line trusted, so this case does "+
					"not show the trust doing anything", scanShow(lines))
			}
			// What the unreduced history gives, so a case that needs the
			// reduction is told apart from one that only confirms it changes
			// nothing.
			if got := d.IsIdle(sb.LastN(d.ScanLines())); got != c.rawIdle {
				t.Errorf("IsIdle over the unreduced history = %v, want %v", got, c.rawIdle)
			}
		})
	}
}

// The scan is bounded in bytes, so it can begin inside a line — at a column the
// program never began a line at. A pattern anchored at the start of a line must
// not match there, but a '\r' after the cut makes the start genuine again, and
// that is the case a full-screen program's prompt line is in.
//
// The guarantee is carried by the line's trusted field rather than by a NUL in its
// text: an untrusted line's text is still the render, so a reader that ignores the
// flag sees a prompt. What stops the match is IsIdleScan refusing to read one from
// a line the scan could not model, which is asserted here rather than inferred from
// the text.
func TestIdleScanBlocksAStartTheByteBoundInvented(t *testing.T) {
	d := NewIdleDetector("", 5)
	long := strings.Repeat("x", wIdleScanBytes+1000)

	// A cut line with no '\r': the text the scan sees starts mid-line, and a
	// prompt that only looks like one because of where the cut fell must not
	// count. The padding is what a full-screen program writes to clear to the end
	// of a line, and is what leaves the cut text looking like a bare prompt.
	cut := NewScrollback(100)
	cut.Write([]byte("drawing" + strings.Repeat(" ", wIdleScanBytes) + "❯"))
	lines := idleScanLines(cut, d.ScanLines())
	if len(lines) != 1 || lines[0].trusted {
		t.Errorf("a cut line comes back as %q, want one line the scan cannot vouch for",
			scanShow(lines))
	}
	if n := len(scanText(lines)); n > wIdleScanBytes {
		t.Errorf("the idle scan read %d bytes, over its %d-byte bound", n, wIdleScanBytes)
	}
	// Anti-vacuity: the same lines trusted are idle, so it is the trust that the
	// assertion below is about and not the text.
	if !d.IsIdleScan(trustAll(lines)) {
		t.Fatal("the cut text does not read as a prompt even trusted; the case tests nothing")
	}
	if d.IsIdleScan(lines) {
		t.Error("a prompt at a position the byte bound invented counts as idle")
	}

	// A cut line whose redraw erased the row: nothing from before the cut can
	// still be showing, so the render stands on its own and the prompt counts.
	// This is the shell prompt a full-screen program leaves behind.
	erased := NewScrollback(100)
	erased.Write([]byte(long + "\r\x1b[K❯ "))
	lines = idleScanLines(erased, d.ScanLines())
	if scanShow(lines) != "❯ " {
		t.Errorf("the idle scan sees %q, want the erased line's redraw, trusted", scanShow(lines))
	}
	if !d.IsIdleScan(lines) {
		t.Error("a prompt that erased its row in a cut line does not count as idle")
	}

	// A cut line with a bare '\r': the redraw overwrites from column 0 but the
	// row it drew over is longer than the cut can show, so what the terminal is
	// showing past the redraw is unknown. The line stays untrusted.
	redrawn := NewScrollback(100)
	redrawn.Write([]byte(long + "\r❯ "))
	lines = idleScanLines(redrawn, d.ScanLines())
	if len(lines) != 1 || lines[0].trusted || !strings.HasPrefix(lines[0].text, "❯ x") {
		t.Errorf("the idle scan sees %q, want one untrusted render keeping the overwritten tail",
			clip(scanShow(lines)))
	}
	if d.IsIdleScan(lines) {
		t.Error("a prompt drawn over a cut line's unknown tail counts as idle")
	}

	// Every cut above came back as a single line, and that is all the bound can
	// produce: Tail only trims when one line overran the bound on its own, so a
	// read that was cut holds nothing but the end of that line. The cases where
	// trusted and untrusted lines sit in one scan are therefore not about the
	// bound at all, and are covered by TestIdleScanTrustIsPerLine.
	for _, sb := range []*Scrollback{cut, erased, redrawn} {
		if _, cutFlag := sb.Tail(d.ScanLines(), wIdleScanBytes); !cutFlag {
			t.Error("premise: a line over the scan's byte bound came back uncut")
		}
	}
}

// The trust is recorded per line and read back only as far as the line IsIdle would
// match. Both halves of that matter: a line the scan cannot model must not block an
// injection forever just by sitting in history, and must not be stepped over on the
// way to an older prompt either.
func TestIdleScanTrustIsPerLine(t *testing.T) {
	d := NewIdleDetector("", 5)

	// An unmodelled line older than the prompt: the scan stops at the prompt, which
	// it can vouch for, and never reads the older line's trust. A verdict for the
	// whole read rather than per line would suppress every injection while a
	// cursor-addressing line stayed within the last five.
	older := NewScrollback(100)
	older.Write([]byte("\x1b[2;5Hdrawing\n❯ "))
	lines := idleScanLines(older, d.ScanLines())
	if len(lines) != 2 || lines[0].trusted || !lines[1].trusted {
		t.Fatalf("premise: the scan sees %q, want an unmodelled line then a line it can vouch for",
			clip(scanShow(lines)))
	}
	if !d.IsIdleScan(lines) {
		t.Error("a prompt after an unmodelled line does not count as idle, so any cursor-addressed " +
			"line in the scan blocks every injection")
	}

	// An unmodelled line that rendered to nothing, after the prompt: "the render
	// produced no text" is not evidence that the screen holds none, so this one must
	// stop the scan rather than be skipped. Skipping it would match the prompt above
	// — the last one the shell drew — and type into whatever is running now.
	blank := NewScrollback(100)
	blank.Write([]byte("❯ \n\x1b[2;5H"))
	lines = idleScanLines(blank, d.ScanLines())
	if len(lines) != 2 || lines[1].text != "" || lines[1].trusted {
		t.Fatalf("premise: the scan sees %q, want an unmodelled line rendering to nothing after "+
			"the prompt", clip(scanShow(lines)))
	}
	if !d.IsIdleScan(trustAll(lines)) {
		t.Fatal("the prompt above does not read as idle even with every line trusted; the case " +
			"tests nothing")
	}
	if d.IsIdleScan(lines) {
		t.Error("an unmodelled line that rendered to nothing was skipped, and the prompt before " +
			"it read as idle")
	}

	// The contrast that shows the refusal is about the trust and not the blankness:
	// a blank line the scan *can* vouch for is skipped, as IsIdle skips it, because
	// a program that wrote an empty line wrote nothing to the screen.
	trailing := NewScrollback(100)
	trailing.Write([]byte("❯ \n\n"))
	lines = idleScanLines(trailing, d.ScanLines())
	if len(untrusted(lines)) != 0 {
		t.Fatalf("premise: the scan sees %q, want every line trusted", clip(scanShow(lines)))
	}
	if strings.TrimSpace(lines[len(lines)-1].text) != "" {
		t.Fatalf("premise: the scan's last line is %q, want a blank one", clip(scanShow(lines)))
	}
	if !d.IsIdleScan(lines) {
		t.Error("a blank line the scan can vouch for stopped it, so a prompt followed by an empty " +
			"line never counts as idle")
	}
}

// The refusal holds for any idle pattern, which is what a mark inside the text
// cannot do. The mark the flag replaced was a NUL prefixed to an untrusted line, and
// it only stopped a match because the default pattern is anchored at the start of a
// line: IdlePattern is configurable, and a pattern that matches its prompt anywhere
// in the line — the obvious way to write one — matched straight past the NUL. The
// guarantee cannot depend on how the caller wrote their regex, so it is not in the
// text.
//
// The corpus cannot show this: over the 131 replayable recordings, 534 of 1309
// sampled points hold a line the scan cannot model, but at none of them does such a
// line's text match either pattern, so the two shapes return the same verdict
// everywhere. That is what the fixtures below are for.
func TestIdleScanRefusalDoesNotDependOnThePattern(t *testing.T) {
	// A prompt at a column the byte bound invented, and a prompt on a line the scan
	// cannot place at all: the two ways a line goes untrusted.
	cut := NewScrollback(100)
	cut.Write([]byte("drawing" + strings.Repeat(" ", wIdleScanBytes) + "❯"))
	addressed := NewScrollback(100)
	addressed.Write([]byte("\x1b[9;1H❯ "))

	for _, pattern := range []string{`(?m)^\s*❯\s*$`, `❯`, `❯\s*$`, `(?s).*❯.*`} {
		d := NewIdleDetector(pattern, 5)
		for name, sb := range map[string]*Scrollback{"a cut line": cut, "a cursor-addressed line": addressed} {
			lines := idleScanLines(sb, d.ScanLines())
			if len(untrusted(lines)) == 0 {
				t.Fatalf("premise: %s comes back trusted (scan %q)", name, clip(scanShow(lines)))
			}
			// Anti-vacuity, and the point of the case: the pattern does match the
			// text, so a shape that let the text answer would report idle here.
			if !d.IsIdleScan(trustAll(lines)) {
				t.Fatalf("pattern %q does not match %s even trusted (scan %q); the case tests nothing",
					pattern, name, clip(scanShow(lines)))
			}
			if d.IsIdleScan(lines) {
				t.Errorf("pattern %q reads idle from %s, which the scan cannot vouch for",
					pattern, name)
			}
		}
	}
}

// §3d of the D4/F1 post-review: reducing a line to the text after its last '\r'
// turned a line whose final byte is '\r' into "", and a pattern that matches
// blankness then read a prompt-shaped idle out of a line the terminal is still
// showing. The reduction that did it is gone — renderCR replays a '\r' as a column
// move, so glyphs it drew over stay in the buffer — but §3d is a claim about the
// consumer rather than about the reduction, so it is asserted through IsIdleScan,
// over the patterns that make it visible. The default pattern needs a prompt glyph
// and so cannot see this class at all; TestIdleScanSeesWhatTheTerminalShows pins the
// same two writes under the default and reports only that the text survives.
//
// The erase case is why this is not "no line ever renders empty": CSI 2K really does
// leave the row blank, so an empty render there is what the terminal shows, and a
// pattern that matches blankness is right to fire on it.
func TestIdleScanCannotBlankALineTheTerminalShows(t *testing.T) {
	for _, pattern := range []string{`(?m)^\s*$`, `(?m)^$`} {
		d := NewIdleDetector(pattern, 5)
		// Anti-vacuity: the pattern matches the shape a reduction to "" produces, so
		// a fixture whose lines all came back empty would read idle below. The
		// fixtures hold no prompt line for the same reason — with one above, an
		// emptied line makes the scan skip to the prompt instead, which is the
		// older-line half of the same defect and is pinned under the default pattern
		// in TestIdleScanSeesWhatTheTerminalShows.
		if !d.IsIdleScan([]scanLine{{text: "", trusted: true}}) {
			t.Fatalf("premise: pattern %q does not match an empty last line", pattern)
		}
		// Every fixture but the erase has to leave a '\r' as its line's last stored
		// byte, and that takes a byte after it: Write holds a chunk's trailing '\r'
		// back in case the "\r\n" it belongs to splits across PTY reads. So a single
		// write of "…\r" cannot exhibit §3d at all, and a fixture built that way
		// would report green because it is the wrong shape, not because the class is
		// closed — storedCR is asserted below so that stays visible.
		for _, c := range []struct {
			name     string
			writes   []string
			storedCR bool
			scanned  string
			idle     bool
		}{{
			// ONLCR puts "\r\r\n" on the wire for a line the program ended with a
			// '\r' of its own; history keeps the '\r' the terminator did not account
			// for. The line is on screen and the reduction made it vanish.
			name:     "a line ending in a carriage return",
			writes:   []string{"still working\r\r\n"},
			storedCR: true,
			scanned:  "still working",
		}, {
			// The same shape with nothing terminating the line, which is what a
			// program part-way through redrawing its status line leaves in history.
			name:     "an unfinished line whose last byte is a carriage return",
			writes:   []string{"still working\r", "\r"},
			storedCR: true,
			scanned:  "still working",
		}, {
			// The reduction dropped everything before the last '\r' too, so a line
			// with more than one of them lost more than its tail.
			name:     "a status line redrawn twice and left mid-write",
			writes:   []string{"step 1\rstep 2\r", "\r"},
			storedCR: true,
			scanned:  "step 2",
		}, {
			name:    "a line the program erased",
			writes:  []string{"still working\x1b[2K"},
			scanned: "",
			idle:    true,
		}} {
			t.Run(pattern+"/"+c.name, func(t *testing.T) {
				sb := NewScrollback(100)
				for _, w := range c.writes {
					sb.Write([]byte(w))
				}
				if got := strings.HasSuffix(sb.LastN(5), "\r"); got != c.storedCR {
					t.Fatalf("premise: history %q ends in a carriage return = %v, want %v",
						sb.LastN(5), got, c.storedCR)
				}
				lines := idleScanLines(sb, d.ScanLines())
				// The verdict has to come from the text here: a refusal would make
				// every case not-idle for a reason §3d is not about.
				if u := untrusted(lines); len(u) > 0 {
					t.Fatalf("premise: the scan refuses lines %v, so this case does not test the render (scan %q)",
						u, scanShow(lines))
				}
				if got := scanText(lines); got != c.scanned {
					t.Errorf("the idle scan sees %q, want %q (history holds %q)", got, c.scanned, sb.LastN(5))
				}
				if got := d.IsIdleScan(lines); got != c.idle {
					t.Errorf("IsIdleScan = %v, want %v (scan %q)", got, c.idle, scanShow(lines))
				}
			})
		}
	}
}

func TestStripANSI(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[?2004h$ ":                            "$ ", // bash 5.2 readline before each prompt
		"\x1b[?25l$ \x1b[?25h":                     "$ ", // ConPTY around repaints
		"\x1b]0;t\x1b\\$ ":                         "$ ", // OSC ended by ST
		"\x1b]0;t\x07$ ":                           "$ ",
		"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1bP1$r\x1b\\x":                          "x", // DCS
		"\x1b_Gf=100\x1b\\x":                       "x", // APC
		"\x1b(B$ ":                                 "$ ",
		"\x1b[0 q❯":                                "❯", // DECSCUSR has an intermediate
		"\x1b[2~x":                                 "x",
		"\x1b[38:2:1:2:3mx":                        "x",
		"\x1b[>4;1mx":                              "x",
		"a\x1b[1;31mb\x1b[0m":                      "ab",
		"\x1b=\x1b>\x1b7\x1b8\x1bM":                "",
		"\x1b\\$ ":                                 "$ ", // a lone ST
		// Malformed input loses idle verdicts only in the safe direction:
		// an ESC inside an OSC ends it (the ESC b pair is stripped, the
		// rest stays), and an unterminated DCS runs to a later ST.
		"\x1b]0;a\x1bb\x07$ ": "\x1b]0;a\x07$ ",
		"\x1bPq\n❯\n\x1b\\$ ": "$ ",
		// Unterminated sequences stay whole.
		"\x1b]0;unterminated": "\x1b]0;unterminated",
		"\x1bPq#0;2":          "\x1bPq#0;2",
		"\x1b[":               "\x1b[",
	} {
		if got := StripANSI(in); got != want {
			t.Errorf("StripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRustRegexFishPattern(t *testing.T) {
	re := regexp.MustCompile(RustRegex(IdlePatternForShell("/usr/bin/fish")))
	if !re.MatchString("~/projéct ❯ ") || !re.MatchString("user@host>") || re.MatchString("echo hi > file") {
		t.Fatal("fish prompt pattern with Unicode \\w")
	}
}

func TestStash(t *testing.T) {
	s := &StashState{}
	s.AccumulateInput([]byte("ignored"))
	s.Begin("partial input")
	s.AccumulateInput([]byte(" more"))
	s.AccumulateInput([]byte(" text"))
	if !s.IsActive() || s.Complete() != "partial input more text" || s.IsActive() {
		t.Fatal("stash lifecycle")
	}
}

// ── Shell ──────────────────────────────────────────────────────────────────

func TestShellPatterns(t *testing.T) {
	if DefaultShell() == "" {
		t.Fatal("default shell")
	}
	for shell, want := range map[string]string{
		"pwsh.exe":              `(?m)^PS [^>]+>\s*$`,
		"/usr/bin/powershell":   `(?m)^PS [^>]+>\s*$`,
		`C:\Windows\cmd.exe /K`: `(?m)^[A-Za-z]:\\.*>\s*$`,
		"/bin/bash":             `(?m)^\s*(?:❯|[$%#>])\s*$`,
		"/usr/bin/zsh":          `(?m)^\s*(?:❯|[$%#>])\s*$`,
		`"C:\Program Files\PowerShell\7\pwsh.exe" -NoLogo`: `(?m)^PS [^>]+>\s*$`,
		`"C:\Program Files\Git\bin\bash.exe" --login`:      `(?m)^\s*(?:❯|[$%#>])\s*$`,
	} {
		if got := IdlePatternForShell(shell); got != want {
			t.Errorf("%s: %s", shell, got)
		}
	}
	// A program already split out keeps its spaces (WindowOpts.Command[0]).
	for prog, want := range map[string]string{
		`C:\Program Files\PowerShell\7\pwsh.exe`: `(?m)^PS [^>]+>\s*$`,
		`/opt/my tools/fish`:                     `(?m)^\s*[\w@/~\-.]+\s*[$❯>]\s*$`,
		`C:\Windows\System32\cmd.exe`:            `(?m)^[A-Za-z]:\\.*>\s*$`,
	} {
		if got := IdlePatternForProgram(prog); got != want {
			t.Errorf("IdlePatternForProgram(%q) = %s", prog, got)
		}
	}
	for in, want := range map[string]string{"/usr/bin/Bash": "bash", `C:\Windows\PWSH.exe`: "pwsh.exe", "plain": "plain", `C:\Program Files\x\Pwsh.exe`: "pwsh.exe"} {
		if got := basenameLower(in); got != want {
			t.Errorf("basenameLower(%q) = %q", in, got)
		}
	}
	got, _ := splitCommandLine(`pwsh.exe -Command "Set-X -Y SaveNothing"`)
	if strings.Join(got, "|") != "pwsh.exe|-Command|Set-X -Y SaveNothing" {
		t.Fatalf("split: %q", got)
	}
}

// ── Manager ────────────────────────────────────────────────────────────────

func TestMoveWindow(t *testing.T) {
	m := NewManager()
	_ = m.CreateSession("src")
	_ = m.CreateSession("dst")
	m.Session("src").InsertFakeWindow("w1")
	if err := m.MoveWindow("src", "w1", "dst", ""); err != nil {
		t.Fatal(err)
	}
	if m.Session("dst").Window("w1") == nil || m.Session("src").Window("w1") != nil {
		t.Fatal("move")
	}

	_ = m.CreateSession("a")
	m.Session("a").InsertFakeWindow("x")
	if err := m.MoveWindow("a", "x", "b", "y"); err != nil || m.Session("b").Window("y").Name() != "y" {
		t.Fatal("rename + auto-create dst")
	}

	m.Session("a").InsertFakeWindow("x")
	m.Session("b").InsertFakeWindow("x")
	if err := m.MoveWindow("a", "x", "b", ""); err == nil {
		t.Fatal("conflict must fail")
	}
	if w := m.Session("a").Window("x"); w == nil || w.Name() != "x" {
		t.Fatal("conflict rolls back")
	}
	if err := m.CreateSession("a"); err == nil || err.Error() != `session "a" already exists` {
		t.Fatalf("duplicate session: %v", err)
	}
}

// TestMoveWindowChecksTheSourceBeforeAnsweringOrCreating pins the source window's
// lookup ahead of the other two decisions in MoveWindow: ahead of the no-op's
// answer, and ahead of the destination's creation. One ordering was live here
// before -- create, then no-op, then lookup -- and it carried both of the defects
// below. Two claims about one ordering, not two orderings.
//
// The daemon's move endpoint maps a nil error to 200 OK, so a no-op that returns
// before the source window is looked up reports success for a window the daemon
// does not have. What the caller is told instead is a 409: routes.go maps every
// MoveWindow error to Conflict, so a mistyped window name is not distinguishable
// by status from a real collision in the destination. That is the reference
// daemon's mapping too, so it is left alone here; the point of this test is that
// the caller is told at all. And a destination created before the source is
// checked is a session nobody asked for that nothing removes, left behind by a
// move that then failed.
//
// The census subtest asserts on the number of sessions rather than a lookup of
// the name, because the claim is that the failed move created nothing, not that it
// created nothing under one particular name.
//
// The split into subtests is for independent fixtures and named failures, and it
// is deliberately NOT load-bearing: every assertion that fires against the old
// ordering is an Errorf, so a single body reports all four too. That was measured
// rather than assumed -- an earlier draft of this comment claimed a single body
// would stop at the first failure, which is what would happen if those were
// Fatalf, and they are not. The three fatal-class calls here -- one Fatalf, two
// Fatal, so grep for the shorter name -- are for states the old
// ordering does not produce, so none of them fires in that run and none of them
// can mask anything in it.
func TestMoveWindowChecksTheSourceBeforeAnsweringOrCreating(t *testing.T) {
	t.Run("no-op does not answer for a missing window", func(t *testing.T) {
		m := NewManager()
		_ = m.CreateSession("s")
		m.Session("s").InsertFakeWindow("real")

		// Both arms of the no-op condition: an omitted dstName, and one echoing
		// srcName. Neither may answer for a window that is not there.
		for _, dstName := range []string{"", "ghost"} {
			err := m.MoveWindow("s", "ghost", "s", dstName)
			if err == nil {
				t.Errorf("MoveWindow(s, ghost, s, %q) = nil; nil here is a 200 OK for a window that does not exist", dstName)
				continue
			}
			if !strings.Contains(err.Error(), "ghost") {
				t.Errorf("MoveWindow(s, ghost, s, %q) error %q does not name the window asked for", dstName, err)
			}
		}

		// The real no-op still succeeds, and leaves the window alone.
		if err := m.MoveWindow("s", "real", "s", ""); err != nil {
			t.Fatalf("no-op move of an existing window: %v", err)
		}
		if w := m.Session("s").Window("real"); w == nil || w.Name() != "real" {
			t.Fatal("the no-op move lost or renamed the window")
		}
	})

	t.Run("a move that fails on the source creates no destination", func(t *testing.T) {
		m := NewManager()
		_ = m.CreateSession("s")
		m.Session("s").InsertFakeWindow("real")

		before := len(m.ListSessions())
		if err := m.MoveWindow("s", "ghost", "fresh", ""); err == nil {
			t.Fatal("move of a nonexistent window into a new session must fail")
		}
		if got := len(m.ListSessions()); got != before {
			t.Errorf("session count %d, want %d: the failed move left a session behind", got, before)
		}
		if m.SessionExists("fresh") {
			t.Error(`session "fresh" exists after the move that would have created it failed`)
		}
	})
}

// ── Window (fake) ──────────────────────────────────────────────────────────

func TestFakeWindowBasics(t *testing.T) {
	w := NewFakeWindow("w1")
	if w.Name() != "w1" || w.IsAlive() || w.ScrollbackLen() != 0 || w.PasteMode() != "auto" {
		t.Fatal("basics")
	}
	if err := w.SendKeys("hi", true, nil); err == nil || err.Error() != `window "w1" is dead` {
		t.Fatalf("dead: %v", err)
	}
}

func advanceFake(w *Window, s string) {
	g := w.LockVT()
	g.Advance([]byte(s))
	w.UnlockVT()
}

func TestCaptureVisible(t *testing.T) {
	w := NewFakeWindow("w")
	advanceFake(w, "old line\r\x1b[2Knew line")
	if v := w.CaptureVisible(0); !strings.Contains(v, "new line") || strings.Contains(v, "old line") {
		t.Fatalf("redraw: %q", v)
	}
	w = NewFakeWindow("w")
	advanceFake(w, "\x1b[2GDo\x1b[5Gyou\x1b[9Gwant\x1b[14Gto\x1b[17Gproceed?")
	if v := w.CaptureVisible(0); !strings.Contains(v, "Do you want to proceed?") {
		t.Fatalf("CHA padding: %q", v)
	}
	w = NewFakeWindow("w")
	advanceFake(w, "hello   \r\n")
	if v := w.CaptureVisible(0); v != "hello" {
		t.Fatalf("trim: %q", v)
	}
}

// Visible capture reads the snapshot, so a wide character that lost its
// spacer used to swallow the next character and an orphan spacer used to drop
// a column; both halves now read as a blank.
func TestCaptureVisibleUnpairedWideHalves(t *testing.T) {
	for in, want := range map[string]string{
		"中文x\x1b[1;2H\x1b[P":    " 文x",    // DCH on the spacer leaves 中 alone before 文
		"ab中cd\x1b[1;3H\x1b[1K": "    cd", // EL 1 ends on 中, leaving its spacer
	} {
		w := NewFakeWindow("w")
		advanceFake(w, in)
		if v := w.CaptureVisible(0); v != want {
			t.Errorf("%q: got %q, want %q", in, v, want)
		}
	}
}

// size reads both axes in one acquisition, as the production readers do. Built
// from Cols and Rows it would model the very defect
// TestEffectiveSizeIsNeverAMixOfTwoDeclarations exists to keep out.
func size(w *Window) [2]uint16 { c, r := w.Size(); return [2]uint16{c, r} }

func TestAttacherGeometry(t *testing.T) {
	w := NewFakeWindow("w")
	a := w.RegisterAttacher(100, 30)
	if size(w) != [2]uint16{100, 30} {
		t.Fatal("register shrinks")
	}
	b := w.RegisterAttacher(80, 20)
	if size(w) != [2]uint16{80, 20} || a.Cols() != 100 || a.Rows() != 30 {
		t.Fatal("min of two")
	}
	b.Release()
	if w.AttacherCount() != 1 || size(w) != [2]uint16{100, 30} {
		t.Fatal("release recomputes")
	}
	a.Release()
	a.Release()
	if size(w) != [2]uint16{100, 30} {
		t.Fatal("no attachers freezes")
	}

	w = NewFakeWindow("w")
	a = w.RegisterAttacher(100, 30)
	_ = w.RegisterAttacher(80, 20)
	a.SetSize(50, 10)
	if size(w) != [2]uint16{50, 10} {
		t.Fatal("set size")
	}

	w = NewFakeWindow("w")
	_ = w.RegisterAttacher(0, 0)
	if size(w) != [2]uint16{20, 5} {
		t.Fatal("floor")
	}

	w = NewFakeWindow("w")
	_ = w.RegisterAttacher(100, 30)
	_ = w.Resize(150, 40)
	if size(w) != [2]uint16{150, 40} {
		t.Fatal("pin beats min")
	}
	w.UnpinSize()
	if _, _, ok := w.PinnedSize(); ok || size(w) != [2]uint16{100, 30} {
		t.Fatal("unpin")
	}

	w = NewFakeWindow("w")
	_ = w.Resize(80, 24)
	if size(w) != [2]uint16{80, 24} {
		t.Fatal("pin without attachers")
	}
}

// TestEffectiveSizeIsThePerAxisMinimum covers clients whose sizes cross: no one
// of them is the smallest on both axes, so the width and the height come from
// different clients and the PTY ends up at a size no client asked for. That is
// tmux's window-size smallest (clients_calculate_size in resize.c takes the
// minimum of sx and sy independently), and it is the shape the rule has to have
// for a full-screen program: a client must never be sent a line longer or a
// screen taller than its own, and picking one client's geometry wholesale would
// overflow the other on one axis.
//
// TestAttacherGeometry's pairs are all dominated on both axes, where a per-axis
// minimum and "use the smaller client's size" agree. The crossed two-client case
// is asserted elsewhere as a side effect of counting reflows
// (TestReflowCounterCountsUnpinnedAttacherChanges: 120x30 with 200x10 gives
// 120x10), and three voters appear in TestObserverDoesNotVoteOnEffectiveSize; a
// mutant that takes both axes from the narrowest client, and one that lets only
// the first two attachers vote, are each killed by those as well as by this
// test. So this is the rule stated as its own subject rather than new reach:
// what it adds is the third and fourth voter, a client that is smallest on
// neither axis, and a release that raises one axis alone.
func TestEffectiveSizeIsThePerAxisMinimum(t *testing.T) {
	w := NewFakeWindow("w")
	wide := w.RegisterAttacher(200, 24) // widest, shortest
	tall := w.RegisterAttacher(80, 50)  // narrowest, tallest
	mid := w.RegisterAttacher(120, 30)  // smallest on neither axis
	if got := size(w); got != [2]uint16{80, 24} {
		t.Fatalf("size %v, want [80 24]: width from the narrow client, height from the short one", got)
	}
	for _, a := range []*Attacher{wide, tall, mid} {
		if a.Cols() < w.Cols() || a.Rows() < w.Rows() {
			t.Errorf("client %dx%d is smaller than the effective %dx%d on one axis", a.Cols(), a.Rows(), w.Cols(), w.Rows())
		}
	}

	short := w.RegisterAttacher(300, 10)
	if got := size(w); got != [2]uint16{80, 10} {
		t.Fatalf("size %v after a 300x10 client, want [80 10]: it lowers the height only", got)
	}

	tall.Release()
	if got := size(w); got != [2]uint16{120, 10} {
		t.Fatalf("size %v after releasing the narrow client, want [120 10]: the width rises to the next narrowest", got)
	}

	short.SetSize(300, 40)
	if got := size(w); got != [2]uint16{120, 24} {
		t.Fatalf("size %v after that client grew to 300x40, want [120 24]: growing one axis releases that axis alone", got)
	}
}

// ── Grid render / diff ─────────────────────────────────────────────────────

func grid(cols, rows int, s string) *VtGrid {
	g := NewVtGrid(cols, rows)
	g.Advance([]byte(s))
	return g
}

func TestRenderFrame(t *testing.T) {
	f := string(NewVtGrid(80, 24).RenderFrame())
	if !strings.HasPrefix(f, "\x1b[?25l\x1b[2J\x1b[H") || !strings.HasSuffix(f, "\x1b[?25h") || !strings.Contains(f, "\x1b[1;1H") {
		t.Fatalf("empty frame: %q", f)
	}
	if f := string(grid(40, 10, "\x1b[3;5HX").RenderFrame()); !strings.Contains(f, "X") || !strings.Contains(f, "\x1b[3;6H") {
		t.Fatalf("cup: %q", f)
	}
	if f := string(grid(20, 4, "main\x1b[?1049halt\x1b[?1049l").RenderFrame()); !strings.Contains(f, "main") || strings.Contains(f, "alt") {
		t.Fatalf("alt screen: %q", f)
	}
	if f := string(grid(20, 3, "\x1b[38;2;10;20;30mZ\x1b[1;4mX").RenderFrame()); !strings.Contains(f, "\x1b[38;2;10;20;30m") || !strings.Contains(f, "\x1b[1m\x1b[4m") {
		t.Fatalf("sgr: %q", f)
	}
	if f := string(grid(10, 3, "\x1b[99;99H").RenderFrame()); !strings.Contains(f, "\x1b[3;10H") {
		t.Fatalf("clamp: %q", f)
	}
}

func TestOSCColorQueries(t *testing.T) {
	g := NewVtGrid(40, 10)
	g.Advance([]byte("\x1b]11;?\x07"))
	if g.TakeReplies() != nil {
		t.Fatal("unset color must not reply")
	}
	g.Colors().Set(IdxBackground, vt.RGB{R: 0x1e, G: 0x1e, B: 0x2e})
	g.Colors().Set(IdxForeground, vt.RGB{R: 0xc0, G: 0xc0, B: 0xc0})
	g.Colors().Set(IdxCursor, vt.RGB{R: 0xff})
	g.Advance([]byte("\x1b]11;?\x07\x1b]10;?\x07\x1b]12;?\x1b\\"))
	got := string(g.TakeReplies())
	want := "\x1b]11;rgb:1e1e/1e1e/2e2e\x07\x1b]10;rgb:c0c0/c0c0/c0c0\x07\x1b]12;rgb:ffff/0000/0000\x1b\\"
	if got != want || g.TakeReplies() != nil {
		t.Fatalf("replies %q", got)
	}
}

func TestRenderDiff(t *testing.T) {
	g := NewVtGrid(40, 8)
	out, _ := g.RenderDiff(g.Snapshot())
	if len(out) != 0 {
		t.Fatal("identical state must be empty")
	}

	g = grid(20, 4, "\x1b[31mhello\r\n\x1b[0mworld")
	out, snap := g.RenderDiff(EmptySnapshot())
	if string(out) != string(g.RenderFrame()) {
		t.Fatal("first paint equals frame")
	}
	if replay := grid(20, 4, string(out)); !slotsEqual(replay.Snapshot().Cells, snap.Cells) {
		t.Fatal("first paint round trip")
	}

	g = grid(40, 10, "hello")
	prev := g.Snapshot()
	g.Advance([]byte("\x1b[1;3HX"))
	out, _ = g.RenderDiff(prev)
	if !strings.Contains(string(out), "X") || strings.Contains(string(out), "\x1b[2J") || len(out) > 40 {
		t.Fatalf("single cell diff: %q", out)
	}

	g = NewVtGrid(20, 3)
	prev = g.Snapshot()
	g.Advance([]byte("\x1b[31mAB"))
	out, _ = g.RenderDiff(prev)
	if strings.Count(string(out), "\x1b[31m") != 1 {
		t.Fatalf("sgr carried: %q", out)
	}

	g = NewVtGrid(20, 3)
	prev = g.Snapshot()
	g.Advance([]byte("\x1b[?25l"))
	if out, _ = g.RenderDiff(prev); !strings.Contains(string(out), "\x1b[?25l") {
		t.Fatalf("cursor hide: %q", out)
	}

	g = grid(20, 4, "hi")
	prev = g.Snapshot()
	g.Resize(40, 8)
	if out, _ = g.RenderDiff(prev); !strings.Contains(string(out), "\x1b[2J") {
		t.Fatal("resize repaints")
	}

	g = NewVtGrid(20, 3)
	prev = g.Snapshot()
	g.Advance([]byte("中"))
	out, snap = g.RenderDiff(prev)
	if replay := grid(20, 3, string(out)); !slotsEqual(replay.Snapshot().Cells, snap.Cells) {
		t.Fatal("wide round trip")
	}
}

func TestRenderAtOffset(t *testing.T) {
	g := NewVtGrid(40, 4)
	var sb strings.Builder
	for i := range 100 {
		fmt.Fprintf(&sb, "line%d\r\n", i)
	}
	g.Advance([]byte(sb.String()))
	if strings.Contains(string(g.RenderFrame()), "line0") || g.ScrollbackLen() < 50 {
		t.Fatal("history")
	}
	top := string(g.RenderFrameAt(g.ScrollbackLen()))
	if !strings.Contains(top, "line0") || strings.Contains(top, "\x1b[?25h") {
		t.Fatalf("top of history: %q", top)
	}
	if string(g.RenderFrameAt(g.ScrollbackLen()+1000)) != top || string(g.RenderFrameAt(0)) != string(g.RenderFrame()) {
		t.Fatal("clamping")
	}
	if out, _ := g.RenderDiffAt(g.SnapshotAt(5), 5); len(out) != 0 {
		t.Fatal("identical paused state")
	}
	if out, _ := g.RenderDiffAt(g.SnapshotAt(5), 20); len(out) == 0 || strings.Contains(string(out), "\x1b[?25h") {
		t.Fatalf("offset change: %q", out)
	}
	if s := g.SnapshotAt(5); s.CursorVisible {
		t.Fatal("paused snapshot hides cursor")
	}
}

func TestCropped(t *testing.T) {
	g := grid(30, 8, "\x1b[31mhello\r\nworld")
	if string(g.RenderFrameCropped(30, 8)) != string(g.RenderFrame()) {
		t.Fatal("same-size crop is identical")
	}
	g = grid(20, 4, "hello")
	if out, _ := g.RenderDiffCropped(EmptySnapshot(), 10, 3); string(out) != string(g.RenderFrameCropped(10, 3)) {
		t.Fatal("cropped first paint")
	}
	if out, _ := g.RenderDiffCropped(g.SnapshotCropped(10, 3), 10, 3); len(out) != 0 {
		t.Fatal("cropped stable")
	}
	g = grid(10, 3, "top")
	if f := string(g.RenderFrameCropped(20, 6)); !strings.Contains(f, "top") {
		t.Fatal("pad larger")
	}
	g = NewVtGrid(10, 3)
	prev := g.SnapshotCropped(20, 6)
	g.Advance([]byte("xy"))
	if out, snap := g.RenderDiffCropped(prev, 20, 6); len(out) == 0 || snap.Cols != 20 || snap.Rows != 6 {
		t.Fatal("cropped larger diff")
	}
}

// A full repaint used to end with ?25h whatever the cursor mode, while the
// snapshot recorded the cursor as hidden, so later diffs never hid it again.
func TestFullFrameKeepsHiddenCursorHidden(t *testing.T) {
	g := NewVtGrid(20, 5)
	g.Advance([]byte("hello\x1b[?25l"))
	for _, f := range [][]byte{g.RenderFrame(), g.RenderFrameCropped(10, 3)} {
		if bytes.Contains(f, []byte("\x1b[?25h")) {
			t.Fatalf("hidden cursor shown: %q", f)
		}
	}
	if g.Snapshot().CursorVisible {
		t.Fatal("snapshot says visible")
	}
	g.Advance([]byte("\x1b[?25h"))
	if !bytes.HasSuffix(g.RenderFrame(), []byte("\x1b[?25h")) {
		t.Fatal("visible cursor not shown")
	}
}

// clientView is what a client terminal shows, cell by cell, without the
// snapshot's wide-character pairing.
func clientView(term *vt.Term) *GridSnapshot {
	grid := term.Grid()
	s := &GridSnapshot{Cols: grid.Columns(), Rows: grid.ScreenLines()}
	for r := 0; r < s.Rows; r++ {
		for c := 0; c < s.Cols; c++ {
			s.Cells = append(s.Cells, slotFromCell(grid.Row(r).Cell(c)))
		}
	}
	s.marks = term.MarkTable()
	return s
}

// visibleDiff describes the first cell where a client's view and a snapshot
// look different. A space shows only its background and the decorations drawn
// across it; a spacer is drawn by the wide character it follows, which gives it
// its style. Marks compare by content, since the two sides number them in
// tables of their own; a control character's are not drawn.
func visibleDiff(got, want *GridSnapshot) string {
	blank := func(s CellSlot) bool {
		return s.Ch == ' ' && s.Attrs&(attrInverse|attrUnderline|attrStrike|attrWide|attrWideSpacer) == 0
	}
	for i, w := range want.Cells {
		g := got.Cells[i]
		gm, wm := string(got.Marks(g)), string(want.Marks(w))
		if w.Ch < 0x20 || w.Ch == 0x7f { // padding outside the grid (0), or a tab cell
			w.Ch, wm = ' ', ""
		}
		g.Attrs &^= attrMarks
		w.Attrs &^= attrMarks
		if gm == wm && (g == w || blank(g) && blank(w) && g.Bg == w.Bg) || g.WideSpacer() && w.WideSpacer() {
			continue
		}
		return fmt.Sprintf("row %d col %d: client %q%+q %+v, snapshot %q%+q %+v", i/want.Cols, i%want.Cols, g.Ch, gm, g, w.Ch, wm, w)
	}
	return ""
}

// A client cannot hold half of a wide character. Sent alone, the left half
// wrapped at the client's last column (scrolling the client on its bottom
// row) or wrote a spacer the snapshot did not record, and full repaints
// shifted the rest of the row; diffs against that baseline never repaired the
// screen. The half is now sent as a blank in the wide character's style.
func TestRenderPairsWideHalves(t *testing.T) {
	green := "\x1b[42m中\x1b[0m"
	cases := []struct {
		name          string
		gridCols      int
		before, after string
		blankRow      int
		blankCol      int // -1: no blank expected
	}{
		{"cut by the crop", 11, "", "\x1b[1;10H" + green, 0, 9},
		{"cut by the crop on the bottom row", 11, "top", "\x1b[3;10H" + green, 2, 9},
		{"right half erased (EL)", 10, "", green + "ab\x1b[1;2H\x1b[K\x1b[1;4Hxy", 0, 0},
		{"right half erased (ECH)", 10, "", green + "x\x1b[1;2H\x1b[X", 0, 0},
		{"left half erased (EL 1)", 10, "", "ab" + green + "cd\x1b[1;3H\x1b[1K", 0, -1},
		{"right half deleted (DCH)", 10, "", green + "文x\x1b[1;2H\x1b[P", 0, 0},
		{"spacer pushed off the row (ICH)", 10, "", "abcdefg" + green + "\x1b[1;1H\x1b[2@", 0, 9},
		// 文's spacer now follows 中: the grid pairs them and so does the snapshot.
		{"spacer of the next character (DCH 2)", 10, "", green + "文x\x1b[1;2H\x1b[2P", 0, -1},
	}
	for _, tc := range cases {
		for _, full := range []bool{true, false} {
			g := grid(tc.gridCols, 3, tc.before)
			client := vt.NewTerm(vt.Config{}, 10, 3, nil)
			p := vt.NewProcessor()
			p.Advance(client, g.RenderFrameCropped(10, 3))
			prev := g.SnapshotCropped(10, 3)
			g.Advance([]byte(tc.after))
			cur := g.SnapshotCropped(10, 3)
			if full {
				p.Advance(client, g.RenderFullInto(nil, 0, 10, 3))
			} else {
				p.Advance(client, g.RenderDiffSnapshots(nil, prev, cur, 0))
			}
			if d := visibleDiff(clientView(client), cur); d != "" {
				t.Errorf("%s (full=%v): %s", tc.name, full, d)
			}
			if tc.blankCol >= 0 {
				// Pinned: the blank keeps the wide character's background.
				s := cur.Cells[tc.blankRow*10+tc.blankCol]
				if s.Ch != ' ' || s.Wide() || s.Bg != vt.NewNamedColor(vt.Green) {
					t.Errorf("%s: blanked half %+v", tc.name, s)
				}
			}
		}
	}
}

// A client fed every frame shows exactly its snapshot, through random output
// with wide characters, erases, insertions and deletions, resizes and crops.
func TestRenderClientMatchesSnapshot(t *testing.T) {
	text := []string{"ab", "中文", "🙂", "é", "x", "\x1b[31m", "\x1b[7m", "\x1b[0m", "\x1b[42m", "\r\n", "\x1b[4h中x\x1b[4l",
		"\t", "a\tb", "\x1b[3g\x1b[5G\x1bH\t", "\b", "\x07", "\x0b", "\x0e", "\x0f", "\x7f", "\x1b[2I", "\x1b[Z"}
	edits := []string{"K", "1K", "2K", "J", "@", "2@", "P", "2P", "X", "3X", "L", "M"}
	const cols, rows = 12, 5
	for seed := int64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := NewVtGrid(cols, rows)
		client := vt.NewTerm(vt.Config{}, cols, rows, nil)
		p := vt.NewProcessor()
		p.Advance(client, g.RenderFrameCropped(cols, rows))
		last := g.SnapshotCropped(cols, rows)
		for step := 0; step < 200; step++ {
			switch k := rng.Intn(10); {
			case k == 0:
				g.Resize(cols-3+rng.Intn(7), rows-2+rng.Intn(5))
			case k < 3:
				g.Advance(fmt.Appendf(nil, "\x1b[%d;%dH", 1+rng.Intn(rows+2), 1+rng.Intn(cols+3)))
			case k < 4:
				g.Advance([]byte("\x1b[" + edits[rng.Intn(len(edits))]))
			case k < 8:
				g.Advance([]byte(text[rng.Intn(len(text))]))
			default:
				cur := g.SnapshotCropped(cols, rows)
				if rng.Intn(4) == 0 {
					p.Advance(client, g.RenderFullInto(nil, 0, cols, rows))
				} else {
					p.Advance(client, g.RenderDiffSnapshots(nil, last, cur, 0))
				}
				last = cur
				if d := visibleDiff(clientView(client), cur); d != "" {
					t.Fatalf("seed %d step %d: %s", seed, step, d)
				}
			}
		}
	}
}
