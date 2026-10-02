package shelld

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// cellIsEmpty mirrors vt's unexported Cell.isEmpty, which is what reflow uses to
// decide where a row's content ends. A trailing blank carrying a background is
// content: a program that painted a region and cleared it left blanks the
// terminal still shows in that colour, and reflow has to move them.
//
// Reconstructed through the public accessors rather than exported from vt, so
// that a change to the emptiness rule shows up here as a divergence to explain
// instead of being silently adopted. It deliberately ignores the underline
// colour, as isEmpty does.
func cellIsEmpty(c *vt.Cell) bool {
	ch := c.Rune()
	const attrs = vt.FlagInverse | vt.AllUnderlines | vt.FlagStrikeout | vt.FlagWrapline |
		vt.FlagWideCharSpacer | vt.FlagLeadingWideCharSpacer | vt.FlagZerowidth
	return (ch == ' ' || ch == '\t') &&
		c.Fg() == vt.NewNamedColor(vt.Foreground) &&
		c.Bg() == vt.NewNamedColor(vt.Background) &&
		c.Flags()&attrs == 0
}

// logicalCells is logicalLines with the attributes: the same joining across the
// wrapline flag, but each cell rendered as its glyph and every attribute that
// travels with it, so an assertion built on it can see what a text comparison
// cannot — a blank carrying a background, an inverse cell, an underline colour.
//
// Two kinds of cell are dropped rather than compared, because their presence is
// a function of the width and not of the program's output:
//
//   - FlagWrapline, which marks where a row continues and so moves with every
//     rewrap by definition;
//   - the wide-character spacers. A wide glyph that does not fit at the end of a
//     row is pushed to the next one behind a leading spacer, so at one width a
//     spacer exists and at another it does not. The glyph itself is compared;
//     the placeholder for where it did not fit is not.
func logicalCells(g *vt.Grid) []string {
	const spacers = vt.FlagWideCharSpacer | vt.FlagLeadingWideCharSpacer
	var out []string
	var cur []*vt.Cell
	flush := func() {
		for len(cur) > 0 && cellIsEmpty(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		var b strings.Builder
		for _, c := range cur {
			ch := c.Rune()
			if ch == 0 {
				ch = ' '
			}
			fmt.Fprintf(&b, "%c/%x/%x/%x/%x ", ch, uint32(c.Fg()), uint32(c.Bg()),
				uint32(c.UnderlineColor()), uint32(c.Flags()&^(vt.FlagWrapline|spacers)))
		}
		out = append(out, b.String())
		cur = cur[:0]
	}
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		for c := 0; c < g.Columns(); c++ {
			if cell := row.Cell(c); cell.Flags()&spacers == 0 {
				cur = append(cur, cell)
			}
		}
		if row.Cell(g.Columns()-1).Flags()&vt.FlagWrapline != 0 {
			continue // this row continues into the next
		}
		flush()
	}
	if len(cur) > 0 {
		flush()
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// emptyIgnoringWrap is cellIsEmpty with the wrap marker excused: a blank whose
// only attribute is FlagWrapline.
//
// vt's isEmpty counts such a cell as content, and for its purpose that is right —
// it is deciding where a row's content ends, and the marker says the row
// continues. For a comparison across a resize it is exactly wrong: the marker
// records where a logical line was broken, so it moves with every rewrap by
// definition, which is why logicalCells masks it out of the cells it compares.
func emptyIgnoringWrap(c *vt.Cell) bool {
	ch := c.Rune()
	const attrs = vt.FlagInverse | vt.AllUnderlines | vt.FlagStrikeout |
		vt.FlagWideCharSpacer | vt.FlagLeadingWideCharSpacer | vt.FlagZerowidth
	return (ch == ' ' || ch == '\t') &&
		c.Fg() == vt.NewNamedColor(vt.Foreground) &&
		c.Bg() == vt.NewNamedColor(vt.Background) &&
		c.Flags()&attrs == 0
}

// contentStream is every content cell in order with the row boundaries thrown
// away. Reshaping a logical line, splitting it, or inserting a blank row does not
// perturb it, so comparing it across a resize separates the two things a reflow bug
// can do: move content around, or change what the content is.
//
// This is what bounds the defect lists below: measured at every width of every case
// here, and in a separate sweep over 127 recordings and 9 794 resize cases, no
// resize either drops a cell from this stream or invents one. That is the difference
// between a cosmetic bug and losing a line of an agent's output, so it is asserted
// on its own and without a defect list.
//
// Both halves of the observable are load-bearing, each checked here by reverting it
// alone. Mask the wrap marker out of the key but keep the marked blank in the
// stream, and narrowing reads as inventing one or two blanks whose key is a space
// with default colours — which is also the key a real invented cell would have, so
// the two are indistinguishable. Skip the marked blank but keep the marker in the
// key, and a marker landing on a content cell reads as that cell dropped and a
// differently-flagged copy invented. Only both together conserve.
func contentStream(g *vt.Grid) []string {
	const spacers = vt.FlagWideCharSpacer | vt.FlagLeadingWideCharSpacer
	var out []string
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		for c := 0; c < g.Columns(); c++ {
			cell := row.Cell(c)
			if cell.Flags()&spacers != 0 || emptyIgnoringWrap(cell) {
				continue
			}
			ch := cell.Rune()
			if ch == 0 {
				ch = ' '
			}
			out = append(out, fmt.Sprintf("%c/%x/%x/%x", ch, uint32(cell.Fg()),
				uint32(cell.Bg()), uint32(cell.Flags()&^(vt.FlagWrapline|spacers))))
		}
	}
	return out
}

// streamDelta reports which cells a resize invented and which it dropped, as
// multiset differences, so "the stream changed" can be read as one or the other.
func streamDelta(before, after []string) (gained, lost []string) {
	n := map[string]int{}
	for _, c := range before {
		n[c]++
	}
	for _, c := range after {
		n[c]--
	}
	for c, k := range n {
		for ; k < 0; k++ {
			gained = append(gained, c)
		}
		for ; k > 0; k-- {
			lost = append(lost, c)
		}
	}
	sort.Strings(gained)
	sort.Strings(lost)
	return gained, lost
}

// paintedTail is a line of text followed by blanks painted with a background,
// which is what "clear to end of line" writes while a background is set. The
// blanks are content the terminal shows in that colour, and they fill the row,
// so the logical line is exactly cols cells wide.
func paintedTail(text string, colour int) string {
	return fmt.Sprintf("%s\x1b[4%dm\x1b[K", text, colour)
}

// attrReflowCase is one input to the width sweep. The geometry and the line count
// are the obvious axes; the final cursor movement is the one that is easy to miss,
// and it is here because appending a single cursor sequence to a byte-identical
// stream changes which widths break — in both directions, hiding defects as well
// as revealing them. A table keyed on width alone records a clean pass for an
// input whose defect a cursor movement away is plainly there.
type attrReflowCase struct {
	cols, rows, lines int
	tail              string // appended after the last line, before any resize
}

// attrReflowDefect is the three observables of one case, which have to be
// recorded separately because they come apart:
//
//   - narrow: narrowing alone already loses cells. A round-trip assertion cannot
//     see this, and at two cases below the round trip is clean *while* the narrow
//     is broken — the widen restores what the narrow lost, so the two bugs cancel
//     and the only assertion that catches either is one that looks in the middle.
//   - cells: the round trip changes attributed cells.
//   - glyphs: the round trip changes glyphs too, so reflowRoundTripDefects' own
//     assertion would catch it — at a cursor variant it does not currently sweep.
type attrReflowDefect struct {
	narrow, cells, glyphs []int
}

// attrReflowDefects records what reflow does to attributed cells at every width of
// each case. Like reflowRoundTripDefects it is a defect list and not a
// specification: every non-empty field is a bug, reported to the session that owns
// resize. Measured 2026-09-16.
//
// Characterised as follows, which is in the report rather than left for the owner
// to rediscover:
//
//   - No cells are lost, they are unjoined. At cols 20 rows 4 width 2 the 20-cell
//     painted logical line comes back as 14 cells, a wholly blank row, then 6
//     cells — 14 + 6 = 20, every cell present and in order. The wrapline count is
//     0 before the narrow and 0 after the widen: nothing was dropped, because at
//     cols 20 the line filled exactly one row and had no wrapline flag to lose.
//     What the widen fails to do is *set* the flags that would rejoin the
//     fragments it created, and it inserts a blank row between them.
//   - Mostly invisible to a glyph comparison. logicalLines passes at almost every
//     listed width, because the split falls inside the painted blanks its
//     TrimRight erases. The exceptions are in the glyphs field, and they only
//     appear under a cursor tail.
//   - For the no-tail arm the breaking widths are exactly those where the painted
//     logical line rewraps to more rows than the screen has in use:
//     ceil(cols/w) > min(rows, lines). That holds for all ten no-tail cases here.
//     A cursor tail perturbs it, which is the second axis in one sentence.
var attrReflowDefects = map[attrReflowCase]attrReflowDefect{
	// Cursor-inert: the same widths break under every tail. The minimal reproducer.
	{20, 4, 4, ""}:        {cells: []int{2, 3, 4}},
	{20, 4, 4, "\x1b[3B"}: {cells: []int{2, 3, 4}},
	{20, 4, 4, "\x1b[H"}:  {cells: []int{2, 3, 4}},
	{20, 4, 8, ""}:        {cells: []int{2, 3, 4}},
	{20, 4, 8, "\x1b[3B"}: {cells: []int{2, 3, 4}},
	{20, 4, 8, "\x1b[H"}:  {cells: []int{2, 3, 4}},

	{40, 4, 4, ""}:        {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 4, 4, "\x1b[3B"}: {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 4, 4, "\x1b[H"}:  {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 4, 8, ""}:        {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 4, 8, "\x1b[3B"}: {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 4, 8, "\x1b[H"}:  {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},

	{20, 2, 4, ""}:        {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{20, 2, 4, "\x1b[3B"}: {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{20, 2, 4, "\x1b[H"}:  {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{20, 2, 8, ""}:        {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{20, 2, 8, "\x1b[3B"}: {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{20, 2, 8, "\x1b[H"}:  {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},

	// The two cases where the round trip is clean and the narrow is not: the
	// widen puts back what the narrow lost. Recorded as a defect with an empty
	// cells field, which is the point — a round-trip-only assertion reads these
	// as correct.
	{20, 8, 4, ""}:        {cells: []int{2, 3, 4}},
	{20, 8, 4, "\x1b[3B"}: {narrow: []int{2, 3, 4}},
	{20, 8, 4, "\x1b[H"}:  {cells: []int{2, 3, 4}},
	{40, 6, 4, ""}:        {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},
	{40, 6, 4, "\x1b[3B"}: {narrow: []int{2, 3, 4}},
	{40, 6, 4, "\x1b[H"}:  {cells: []int{2, 3, 4, 5, 6, 7, 8, 9}},

	// The two cases where a cursor tail makes the defect glyph-visible, so the
	// existing glyph-level assertion would catch it if it swept cursor variants.
	{20, 8, 8, ""}:        {cells: []int{2}},
	{20, 8, 8, "\x1b[3B"}: {cells: []int{2}},
	{20, 8, 8, "\x1b[H"}:  {cells: []int{2, 4, 7, 8, 9}, glyphs: []int{4, 7, 8, 9}},
	{40, 6, 8, ""}:        {cells: []int{2, 3, 4, 5, 6}},
	{40, 6, 8, "\x1b[3B"}: {cells: []int{2, 3, 4, 5, 6}},
	{40, 6, 8, "\x1b[H"}: {cells: []int{2, 3, 4, 5, 6, 10, 11, 12, 13},
		glyphs: []int{10, 11, 12, 13}},
}

// logicalLines compares glyphs, so it cannot see an attribute at all, and a
// blank carrying a background it cannot see even as a character — TrimRight takes
// it for absent. That makes every reflow assertion built on it blind to a whole
// class of defect: reflow could drop, duplicate or recolour attributed cells and
// the assertion would pass.
//
// This first proves that blindness rather than asserting it in a comment, so the
// round trip below is known to be testing something the existing assertions do
// not, and then sweeps every width at four geometries.
func TestReflowCarriesCellAttributes(t *testing.T) {
	t.Run("a text comparison cannot see a painted blank", func(t *testing.T) {
		bare := NewVtGrid(20, 4)
		bare.Advance([]byte("hello"))
		paint := NewVtGrid(20, 4)
		paint.Advance([]byte(paintedTail("hello", 1)))

		if a, b := logicalLines(bare.Term().Grid()), logicalLines(paint.Term().Grid()); //
		strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Fatalf("logicalLines already distinguishes a painted blank (%q vs %q); "+
				"the round trip below is no longer covering a blind spot", a, b)
		}
		a := strings.Join(logicalCells(bare.Term().Grid()), "\n")
		b := strings.Join(logicalCells(paint.Term().Grid()), "\n")
		if a == b {
			t.Fatalf("logicalCells cannot see a painted blank either, so it adds nothing: %q", a)
		}
	})

	cases := make([]attrReflowCase, 0, len(attrReflowDefects))
	for c := range attrReflowDefects {
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool {
		a, b := cases[i], cases[j]
		if a.cols != b.cols {
			return a.cols < b.cols
		}
		if a.rows != b.rows {
			return a.rows < b.rows
		}
		if a.lines != b.lines {
			return a.lines < b.lines
		}
		return a.tail < b.tail
	})

	for _, c := range cases {
		want := attrReflowDefects[c]
		name := fmt.Sprintf("cols%d_rows%d_lines%d_tail%q", c.cols, c.rows, c.lines, c.tail)
		t.Run(name, func(t *testing.T) {
			var gotNarrow, gotCells, gotGlyphs []int
			for w := 2; w <= c.cols; w++ {
				g := NewVtGrid(c.cols, c.rows)
				// Several painted lines, so a rewrap has more than one to lose.
				for i := range c.lines - 1 {
					g.Advance([]byte(paintedTail(fmt.Sprintf("line%d", i), i%6+1) + "\r\n"))
				}
				g.Advance([]byte(paintedTail("hello", 1)))
				if c.tail != "" {
					g.Advance([]byte(c.tail))
				}
				grid := g.Term().Grid()
				wantCells := strings.Join(logicalCells(grid), "\n")
				wantGlyphs := strings.Join(logicalLines(grid), "\n")
				wantStream := contentStream(grid)

				g.Resize(w, c.rows)
				if strings.Join(logicalCells(grid), "\n") != wantCells {
					gotNarrow = append(gotNarrow, w)
				}
				checkConserved(t, w, "narrowing", wantStream, contentStream(grid))
				g.Resize(c.cols, c.rows)
				if strings.Join(logicalCells(grid), "\n") != wantCells {
					gotCells = append(gotCells, w)
				}
				if strings.Join(logicalLines(grid), "\n") != wantGlyphs {
					gotGlyphs = append(gotGlyphs, w)
				}
				checkConserved(t, w, "a round trip", wantStream, contentStream(grid))
			}
			checkWidths(t, "narrowing alone lost attributed cells", want.narrow, gotNarrow)
			checkWidths(t, "a round trip changed attributed cells", want.cells, gotCells)
			checkWidths(t, "a round trip changed glyphs", want.glyphs, gotGlyphs)
		})
	}
}

// checkConserved is the invariant that separates a cosmetic reflow bug from a
// destructive one. Reflow may reshape a logical line, split it, rejoin it or push
// it into history; it may not change which cells exist or the order they come in.
//
// The sequence is compared rather than the multiset: the order is what a reader of
// the history depends on, and it costs one join. streamDelta then describes the
// difference, so a failure reads as an insertion, a loss or a reordering instead of
// as two long lists.
//
// Both phases are asserted, with no defect list. With the wrap marker excused
// (emptyIgnoringWrap) no case here or in reflowRoundTripDefects perturbs this
// stream in either direction at any width, so the width lists above are provably
// lists of reshapings.
func checkConserved(t *testing.T, w int, phase string, before, after []string) {
	t.Helper()
	if strings.Join(before, "\x00") == strings.Join(after, "\x00") {
		return
	}
	gained, lost := streamDelta(before, after)
	switch {
	case len(gained) == 0 && len(lost) == 0:
		t.Errorf("width %d: %s reordered the content: the same %d cells came back in a "+
			"different order", w, phase, len(before))
	case len(lost) == 0:
		t.Errorf("width %d: %s invented content cells %v", w, phase, gained)
	case len(gained) == 0:
		t.Errorf("width %d: %s dropped content cells %v — reflow lost output a program "+
			"wrote", w, phase, lost)
	default:
		t.Errorf("width %d: %s changed the content itself, not just its shape: invented "+
			"%v, dropped %v", w, phase, gained, lost)
	}
}

// checkWidths reports a defect list drifting in either direction: a width that
// broke and is not listed is a regression, and a listed width that no longer
// breaks is a stale entry that has to come off the list, since leaving it there
// would let the defect come back unnoticed.
func checkWidths(t *testing.T, what string, want, got []int) {
	t.Helper()
	in := func(v []int, w int) bool {
		for _, x := range v {
			if x == w {
				return true
			}
		}
		return false
	}
	var broke, fixed []int
	for _, w := range got {
		if !in(want, w) {
			broke = append(broke, w)
		}
	}
	for _, w := range want {
		if !in(got, w) {
			fixed = append(fixed, w)
		}
	}
	if len(broke) > 0 {
		t.Errorf("%s at widths %v, which attrReflowDefects does not list: a regression "+
			"in reflow (listed: %v)", what, broke, want)
	}
	if len(fixed) > 0 {
		t.Errorf("attrReflowDefects lists widths %v where %s, but they are now correct: "+
			"remove them from the list", fixed, what)
	}
}

// logicalLines joins grid rows across the wrapline flag, as tmux's
// capture-pane -J does, and strips each logical line's trailing blanks. It
// compares glyphs only; a companion assertion is needed for cell attributes,
// which this cannot see.
func logicalLines(g *vt.Grid) []string {
	var out []string
	var cur strings.Builder
	for line := -g.HistorySize(); line <= g.ScreenLines()-1; line++ {
		row := g.Row(line)
		for c := 0; c < g.Columns(); c++ {
			ch := row.Cell(c).Rune()
			if ch == 0 {
				ch = ' '
			}
			cur.WriteRune(ch)
		}
		if row.Cell(g.Columns()-1).Flags()&vt.FlagWrapline != 0 {
			continue // this row continues into the next
		}
		out = append(out, strings.TrimRight(cur.String(), " "))
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimRight(cur.String(), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
