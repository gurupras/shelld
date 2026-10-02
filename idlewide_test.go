package shelld

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gurupras/shelld/uniwidth"
)

// hasWide reports whether s shows a glyph the terminal gives more than one cell.
func hasWide(s string) bool {
	for _, r := range s {
		if uniwidth.Width(r) > 1 {
			return true
		}
	}
	return false
}

// A wide glyph takes two columns, so a line that redraws itself over one lands at
// a column the rune count does not give: "😀a\r❯ " still shows the 'a', at column
// 2, and a render that spent one column per rune reduced the line to a bare
// prompt — an injection into a command that is still running.
//
// The grid is the oracle here, not a hand-written expectation: each case writes
// the same bytes to raw history and to a VtGrid and the render has to produce
// what the grid shows. shows records the claim so a reader sees it, and it is
// asserted against the grid rather than believed.
func TestIdleScanRendersWideGlyphsAtTheGridsColumns(t *testing.T) {
	d := NewIdleDetector("", 5)
	// Which cases carry the property, counted by fixture: a wide glyph that
	// survived a redraw, a wide glyph the redraw covered (so the fix is not
	// "call every wide line busy"), and a line refused for a rune the column
	// model cannot place.
	survived, covered := map[string]bool{}, map[string]bool{}
	refused := map[string]bool{}
	for _, c := range []struct {
		name    string
		line    string
		shows   string // what the grid shows for it, trailing blanks trimmed
		blocked bool   // ...and the render cannot vouch for it
		idle    bool
	}{{
		// This and the next two came from review rather than from this table's
		// author. The emoji holds columns 0 and 1, so the 'a' is at column 2 and
		// the two-column redraw cannot reach it.
		name:  "an emoji under a shorter redraw",
		line:  "😀a\r❯ ",
		shows: "❯ a",
	}, {
		name:  "two CJK glyphs under a redraw",
		line:  "漢字\r❯ ",
		shows: "❯ 字",
	}, {
		name:  "two emoji under a redraw",
		line:  "🔴🔴\r❯ ",
		shows: "❯ 🔴",
	}, {
		// The other half of the same arithmetic: two columns of redraw do cover
		// one wide glyph, and this line is a prompt. A model that marked every
		// wide-glyph line unrenderable would fail here, which is the cost the
		// reviewer named — agent TUIs draw spinners and status glyphs on the row
		// the prompt returns to.
		name:  "a wide glyph the redraw covers",
		line:  "漢\r❯ ",
		shows: "❯",
		idle:  true,
	}, {
		name:  "wide glyphs erased to the end of the line",
		line:  "漢字\r\x1b[K❯ ",
		shows: "❯",
		idle:  true,
	}, {
		// Overwriting either half of a wide glyph blanks both cells: the 漢 is
		// gone entirely and the 字 after it is not shifted.
		name:  "a redraw that lands on the first half",
		line:  "漢字\rx❯",
		shows: "x❯字",
	}, {
		// A backspace onto the second half of a wide glyph, which is the cell the
		// terminal holds as a spacer. Writing there erases the glyph, and the
		// column it leaves behind is blank rather than closed up.
		name:  "a write onto the second half",
		line:  "漢\bx",
		shows: " x",
	}, {
		// U+17D8 is the one rune uniwidth gives three columns. A grid cell holds a
		// rune and at most one spacer, so vt.input advances two for it, and the
		// render has to advance two as well: sharing the width table is not the
		// same as using its answer the same way. Unclamped, the 'x' lands one
		// column further right here than the terminal puts it.
		name:  "a three-column rune the grid gives two",
		line:  "៘x\r❯ ",
		shows: "❯ x",
	}, {
		// A reviewer's counterexample to the first version of the erase-left
		// branch, which blanked one cell past the cursor whatever it held. No wide
		// glyph anywhere: the terminal keeps the 'g' of "running" at column 6, and
		// a render that eats it turns a running command's line into a bare prompt. The grid is a sound oracle here — the alacritty spacer quirk
		// needs a wide glyph under the erase.
		name:  "an erase-left beside an ordinary character",
		line:  "running❯ \x1b[0m\rbuild\x1b[1K",
		shows: "      g❯",
	}, {
		// Tab stops are columns, not runes: four CJK glyphs reach column 8, so
		// the tab goes to 16, where a rune count would have said 8.
		name:  "a tab after wide glyphs",
		line:  "漢漢漢漢\tx\r❯ ",
		shows: "❯ 漢漢漢" + strings.Repeat(" ", 8) + "x",
	}, {
		// A combining mark composes into the cell before it. A buffer of one rune
		// per column cannot hold that, and dropping the mark would change the
		// line's text, so the line is refused rather than guessed at — even
		// though what the render did produce happens to match the grid, which is
		// why blocked is asserted separately from the text.
		name:    "a combining mark before a redraw",
		line:    "é\r❯ ",
		shows:   "❯",
		blocked: true,
	}, {
		// The erase sequences this deliberately does not model. alacritty's
		// stale-spacer quirk was measured on ECH 1 and ED 1 as well as EL 1, so the
		// reason these are refused rather than called harmless is load-bearing: a
		// render that ignored them would vouch for a line the terminal had erased
		// under it. classifyEscape sends every CSI but K, m, h and l to escUnknown,
		// and these two rows are what would notice if one of them moved.
		name:    "an erase-character the render does not model",
		line:    "running\r\x1b[2X\x1b[K❯ ",
		shows:   "❯",
		blocked: true,
	}, {
		name:    "an erase-to-top the render does not model",
		line:    "running\r\x1b[1J\x1b[K❯ ",
		shows:   "❯",
		blocked: true,
	}, {
		// DEL and the C1 controls have no width the table can give, and the C1s a
		// terminal acts on move the cursor the way an escape does.
		name:    "a delete before a redraw",
		line:    "a\x7fb\r❯ ",
		shows:   "❯",
		blocked: true,
	}} {
		t.Run(c.name, func(t *testing.T) {
			sb := NewScrollback(100)
			sb.Write([]byte(c.line))
			g := NewVtGrid(80, 4)
			g.Advance([]byte(c.line))
			g.TakeReplies()

			// The premise: the grid shows what the case says it shows. Without it
			// the comparison below is between two things this test does not pin,
			// and a change in either would go green by agreeing.
			grid := idleGridText(g, 1)
			if grid != c.shows {
				t.Fatalf("the terminal shows %q for %q, the case says %q", grid, c.line, c.shows)
			}

			lines := idleScanLines(sb, d.ScanLines())
			if got := strings.TrimRight(scanText(lines), " "); got != grid {
				t.Errorf("the idle scan sees %q, the terminal shows %q", got, grid)
			}
			if blocked := len(untrusted(lines)) > 0; blocked != c.blocked {
				t.Errorf("the scan marks lines %v as unmodelled, want blocked = %v (scan %q)",
					untrusted(lines), c.blocked, scanShow(lines))
			}
			if got := d.IsIdleScan(lines); got != c.idle {
				t.Errorf("IsIdleScan = %v, want %v (scan %q)", got, c.idle, scanShow(lines))
			}
			// A blocked case has to be one the text alone would call idle, or the
			// refusal is doing nothing here and the case is only pinning text.
			if c.blocked && !d.IsIdleScan(trustAll(lines)) {
				t.Errorf("scan %q is not idle even with every line trusted, so this case "+
					"does not show the refusal doing anything", scanShow(lines))
			}

			switch {
			case c.blocked:
				refused[c.name] = true
			case hasWide(grid):
				survived[c.name] = true
			case hasWide(c.line):
				covered[c.name] = true
			}
		})
	}
	// Each class needs two fixtures: with one, the sweep pins that fixture's
	// shape rather than the property, and the class the width fix exists for is
	// the first of the three.
	for _, class := range []struct {
		what string
		seen map[string]bool
	}{
		{"a wide glyph still showing after a redraw", survived},
		{"a wide glyph the redraw covered", covered},
		{"a line refused for a rune with no column", refused},
	} {
		if len(class.seen) < 2 {
			t.Errorf("%d cases of %s; this test needs two, or it pins a fixture rather "+
				"than the property", len(class.seen), class.what)
		}
	}
}

// One class where the render and the grid disagree, and the render is the one a
// terminal agrees with.
//
// `CSI 1 K` erases from the start of the line through the cursor, so with the
// cursor on the left half of a wide glyph it clears that cell and leaves the
// right half still flagged as the glyph's spacer. A wide glyph written there next
// takes the cleared cell, and writing its own spacer over the stale one runs
// alacritty's overwrite rule — `write_at_cursor` sees a spacer, decides it is
// overwriting the right half of some other glyph, and clears the cell to its left:
// the glyph it just wrote. alacritty_terminal 0.26 does this
// (`src/term/mod.rs:994-1002` with `clear_line` at `:1651`), vt reproduces it
// faithfully (`vt/term.go:209-217`), and the differential tests cannot see it
// because both sides do the same thing.
//
// So this is pinned rather than followed. The render places the glyph, which is
// what a terminal shows; if the grid ever agrees, this test fails and CSI 1 K goes
// back into the sweep's alphabet.
func TestIdleScanKeepsAWideGlyphAlacrittyDrops(t *testing.T) {
	const line = "字\r\x1b[1K😀❯"

	g := NewVtGrid(20, 3)
	g.Advance([]byte(line))
	g.TakeReplies()
	if got, want := idleGridText(g, 1), "  ❯"; got != want {
		t.Errorf("the grid shows %q for %q, want %q — if it now holds the glyph, take "+
			"CSI 1 K out of this test and put it back in the sweep's alphabet", got, line, want)
	}

	text, exact, _ := renderCR(line)
	if got, want := text, "😀❯"; got != want || !exact {
		t.Errorf("renderCR(%q) = %q, exact %v; want %q and exact, which is what a "+
			"terminal shows", line, got, exact, want)
	}
}

// The table above names the shapes; this sweeps them. Every line is built from
// runes of each width the table reports and from the redraws a shell and a TUI
// emit, and the render has to produce what the grid shows for it. The grid is an
// independent implementation of the same question — it lays out cells from
// uniwidth and is checked against alacritty by the differential tests — so
// agreement here is not the render agreeing with itself.
//
// Lines the render refuses are excluded and counted, and the count is asserted
// from both sides: a sweep where nothing is refused is not exercising the refusal,
// and one where most lines are refused would agree by declining to answer.
func TestIdleScanRenderMatchesTheGridOverRedrawnLines(t *testing.T) {
	tokens := []string{
		"a", "b", "x", "#", " ", "❯", "⠋", "é", // one column
		"漢", "字", "😀", "🔴", "▔", // two
		"៘", // three in the width table, two on the screen
		"\r", "\b", "\t", "\x1b[K", "\x1b[2K", "\x1b[m",
	}
	// What the render refuses. Drawn one token in forty, so that a quarter or so
	// of the lines are refused and the rest are compared: a sweep built mostly
	// from these would agree by declining to answer nearly every line.
	unmodelled := []string{"́", "​", "\x7f", "\x1b[H", "\x1b[3B"}
	// CSI 1 K is left out, and TestIdleScanKeepsAWideGlyphAlacrittyDrops is why:
	// the grid loses a wide glyph written where an erase-left ended on the lead of
	// an earlier one, which is alacritty's behaviour and not a terminal's. That
	// test pins the divergence, so this alphabet grows again if it is ever fixed.
	const cols = 256
	rng := rand.New(rand.NewSource(1))
	var exact, refused, wide int
	for i := 0; i < 20_000; i++ {
		var b strings.Builder
		for n := rng.Intn(24) + 1; n > 0; n-- {
			if rng.Intn(40) == 0 {
				b.WriteString(unmodelled[rng.Intn(len(unmodelled))])
				continue
			}
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		line := b.String()

		text, ok, _ := renderCR(line)
		if !ok {
			refused++
			continue
		}
		exact++
		if hasWide(text) {
			wide++
		}

		g := NewVtGrid(cols, 4)
		g.Advance([]byte(line))
		g.TakeReplies()
		if got, want := strings.TrimRight(text, " "), idleGridText(g, 1); got != want {
			t.Fatalf("line %d %q renders to %q, the terminal shows %q", i, line, got, want)
		}
		if utf8.RuneCountInString(text) > cols {
			t.Fatalf("line %d %q reaches %d columns, past the %d the grid has, so the "+
				"comparison is against a wrapped row", i, line, utf8.RuneCountInString(text), cols)
		}
	}
	switch {
	case wide < exact/10:
		t.Errorf("only %d of the %d compared lines still show a wide glyph; this sweep "+
			"is not about widths", wide, exact)
	case refused == 0:
		t.Error("no line was refused, so the refusal this sweep excuses itself with never happened")
	case exact < refused:
		t.Errorf("%d lines compared against %d refused: the sweep mostly agrees by "+
			"declining to answer", exact, refused)
	}
	t.Logf("%d lines compared (%d still showing a wide glyph), %d refused", exact, wide, refused)
}

// columnsOf lays a rendered line back out on the columns a terminal puts it on:
// one entry per column, with a wide glyph's continuation column holding
// wideSpacer, the same shape renderCR builds internally. The mapping is exact
// because a render's text is its visible glyphs in column order.
func columnsOf(text string) []rune {
	var cols []rune
	for _, r := range text {
		w := min(max(uniwidth.Width(r), 1), 2)
		cols = append(cols, r)
		for c := 1; c < w; c++ {
			cols = append(cols, wideSpacer)
		}
	}
	return cols
}

func cellAt(cols []rune, c int) rune {
	if c < 0 || c >= len(cols) {
		return ' '
	}
	if cols[c] == wideSpacer {
		return wideSpacer
	}
	return cols[c]
}

// The erase-in-line branches, checked against the render itself rather than
// against the grid.
//
// TestIdleScanKeepsAWideGlyphAlacrittyDrops is why CSI 1 K is not in the sweep
// above: the grid is the wrong oracle for it. But dropping the input dropped the
// only coverage of the branch that handles it, and that is where a defect was
// found — an unconditional blank one cell past the cursor, which erased an
// ordinary character the terminal was still showing and could turn a running
// command's line into a bare prompt. So the input comes back with a different
// oracle, which is a property of the render's own output: an erase blanks exactly
// the cells the sequence names and no others.
//
// The reference is renderCR on the same line without the final erase, so the
// comparison is against a run of the function that never entered the erase
// branch. The cursor column needs no model: after a '\r' the sweep writes a
// counted run of single-column characters, so the erase happens at a column the
// test chose.
func TestIdleScanErasesExactlyTheCellsTheSequenceNames(t *testing.T) {
	tokens := []string{
		"a", "b", "x", "#", " ", "❯", "⠋", "é",
		"漢", "字", "😀", "🔴", "▔", "៘",
		"\r", "\b", "\t", "\x1b[K", "\x1b[1K", "\x1b[2K", "\x1b[m",
	}
	erases := []struct {
		seq  string
		mode int
	}{{"\x1b[K", 0}, {"\x1b[1K", 1}, {"\x1b[2K", 2}}
	rng := rand.New(rand.NewSource(7))
	// The classes this has to reach: an erase-left that ends on the lead of a wide
	// glyph (so the glyph's far half must go too), and one that ends with an
	// ordinary character in the next cell (which must survive — the defect).
	var straddled, neighbour, checked int
	for i := 0; i < 20_000; i++ {
		var b strings.Builder
		for n := rng.Intn(20) + 1; n > 0; n-- {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		cursor := rng.Intn(12)
		prefix := b.String() + "\r" + strings.Repeat("x", cursor)
		e := erases[rng.Intn(len(erases))]

		before, okBefore, _ := renderCR(prefix)
		after, okAfter, _ := renderCR(prefix + e.seq)
		if !okBefore || !okAfter {
			t.Fatalf("line %d %q: renderCR refused a line built only from modelled tokens "+
				"(before %v, after %v)", i, prefix, okBefore, okAfter)
		}
		checked++
		bc, ac := columnsOf(before), columnsOf(after)
		width := max(len(bc), len(ac))

		// erased reports whether the sequence names column c; straddle is the
		// column of the far half of a glyph the erase boundary cuts, which goes
		// with the half that was named.
		straddle := -1
		switch e.mode {
		case 0:
			if cellAt(bc, cursor) == wideSpacer {
				straddle = cursor - 1
			}
		case 1:
			if cellAt(bc, cursor+1) == wideSpacer {
				straddle = cursor + 1
			}
		}
		if straddle >= 0 {
			straddled++
		} else if e.mode == 1 {
			if r := cellAt(bc, cursor+1); r != ' ' && r != wideSpacer && cursor+1 < len(bc) {
				neighbour++
			}
		}
		for c := 0; c < width; c++ {
			erased := (e.mode == 0 && c >= cursor) || (e.mode == 1 && c <= cursor) || e.mode == 2
			got, want := cellAt(ac, c), cellAt(bc, c)
			switch {
			case erased || c == straddle:
				if got != ' ' && got != wideSpacer {
					t.Fatalf("line %d %q + %q: column %d shows %q after an erase that names it "+
						"(cursor %d, before %q, after %q)", i, prefix, e.seq, c, got, cursor, before, after)
				}
			case got != want:
				t.Fatalf("line %d %q + %q: column %d was %q and is %q, and the erase does not "+
					"name it (cursor %d, before %q, after %q)",
					i, prefix, e.seq, c, want, got, cursor, before, after)
			}
		}
	}
	if straddled < 2 {
		t.Errorf("%d erases ended inside a wide glyph; this sweep is about the cells an "+
			"erase takes with it", straddled)
	}
	if neighbour < 2 {
		t.Errorf("%d erase-lefts had an ordinary character in the cell past the cursor, so "+
			"the sweep never asked whether that character survives", neighbour)
	}
	t.Logf("%d lines checked, %d erases ending inside a wide glyph, %d erase-lefts with a "+
		"character in the next cell", checked, straddled, neighbour)
}

// idleGridText is the last n non-blank rows of the visible grid, which is what a
// terminal is showing and what a capture-based detector would match against.
func idleGridText(g *VtGrid, n int) string {
	snap := g.Snapshot()
	var out []string
	for r := snap.Rows - 1; r >= 0 && len(out) < n; r-- {
		var sb strings.Builder
		for c := 0; c < snap.Cols; c++ {
			cell := snap.Cells[r*snap.Cols+c]
			if cell.WideSpacer() {
				continue
			}
			ch := cell.Ch
			if ch < 0x20 || ch == 0x7f {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
		row := strings.TrimRight(sb.String(), " ")
		if strings.TrimSpace(row) != "" {
			out = append([]string{row}, out...)
		}
	}
	return strings.Join(out, "\n")
}
