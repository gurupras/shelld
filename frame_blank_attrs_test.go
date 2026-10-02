package shelld

import (
	"fmt"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// The converge scenario compares a client's screen (built from the diff stream)
// with a replay of /frame, cell by cell, on ch+fg+bg+flags. A few states per
// round never converge within 2.5 s. These tests pin the mechanism: the two
// renderers do not agree about a blank cell's attributes, so a state where one
// exists is a difference that no amount of waiting resolves.
//
// isBlank, which renderFull uses to find each row's last emitted column,
// accepts a space carrying a foreground colour or bold/italic/dim/hidden. So a
// row whose trailing run is styled blanks is truncated before them and the
// attributes are gone from the frame — while renderDiff, which compares whole
// cell slots, sends them. The client has them; /frame does not.
//
// Whether that matters to a viewer is a separate question, and the answer is
// no: every attribute in the lossy set is invisible on a space (a foreground
// colour needs a glyph, and bold/italic/dim/hidden are glyph properties). It
// matters to a comparison of full state, which is what the converge scenario is.

// cellAttrKey is the converge scenario's cellKey (scenarios_corr.go), reproduced
// here so these tests compare on exactly what the measurement compares on. A
// test written against a looser key would pass while the scenario diverged.
func cellAttrKey(c *vt.Cell) string {
	const flagMask = vt.FlagInverse | vt.FlagBold | vt.FlagItalic | vt.FlagUnderline | vt.FlagWideChar |
		vt.FlagWideCharSpacer | vt.FlagDim | vt.FlagHidden | vt.FlagStrikeout
	ch := c.Rune()
	if ch == 0 {
		ch = ' '
	}
	// Integers for the same reason the scenario converts them: fmt runs a Stringer
	// for %x too, so a Color or Flags formatted directly keys on the hex of a name.
	return fmt.Sprintf("%c/%x/%x/%x", ch, uint32(c.Fg()), uint32(c.Bg()), uint16(c.Flags()&flagMask))
}

// replayFrame parses a frame the way the converge scenario does: into a fresh
// terminal of the frame's own size.
func replayFrame(t *testing.T, frame []byte, cols, rows int) *vt.Term {
	t.Helper()
	term := vt.NewTerm(vt.Config{}, cols, rows, nil)
	vt.NewProcessor().Advance(term, frame)
	return term
}

// gridOf builds a viewport by feeding b to a grid of the given size.
func gridOf(cols, rows int, b string) *VtGrid {
	g := NewVtGrid(cols, rows)
	g.Advance([]byte(b))
	return g
}

// TestRenderFullDropsTrailingBlankAttributesThatRenderDiffSends is the
// never-converge mechanism, both halves in one test: the frame loses a trailing
// styled blank's attributes and the diff stream carries them, so a client that
// was attached when the blank was painted holds a cell /frame cannot describe.
func TestRenderFullDropsTrailingBlankAttributesThatRenderDiffSends(t *testing.T) {
	const cols, rows = 10, 2
	// "ab" then three bold red spaces: the row's trailing run is styled blanks.
	const paint = "ab\x1b[1;31m   \x1b[0m"

	g := gridOf(cols, rows, paint)
	src := g.Term().Grid().Row(0).Cell(2) // the first trailing blank
	if src.Flags()&vt.FlagBold == 0 {
		t.Fatalf("premise: the source cell is not bold, so this test is not about a styled blank: %s", cellAttrKey(src))
	}

	// The /frame side: what the harness fetches and replays.
	got := replayFrame(t, g.RenderFrame(), cols, rows).Grid().Row(0).Cell(2)
	if cellAttrKey(got) == cellAttrKey(src) {
		t.Errorf("renderFull round-tripped a trailing styled blank (%s); the never-converge mechanism this test documents is gone, and the converge scenario's classification must be revisited", cellAttrKey(src))
	}

	// The client side: the same paint delivered as a diff from an empty
	// baseline, replayed into a client terminal.
	blank := NewVtGrid(cols, rows)
	base := blank.Snapshot()
	g2 := NewVtGrid(cols, rows)
	cl := replayFrame(t, g2.RenderFrame(), cols, rows) // the client's initial frame
	g2.Advance([]byte(paint))
	diff, _ := g2.RenderDiff(base)
	vt.NewProcessor().Advance(cl, diff)
	if k := cellAttrKey(cl.Grid().Row(0).Cell(2)); k != cellAttrKey(src) {
		t.Errorf("the diff stream did not deliver the styled blank: client=%s source=%s; then both sides agree and this is not the mechanism", k, cellAttrKey(src))
	}
}

// TestRenderFullKeepsInteriorBlankAttributes bounds the class: only a row's
// trailing run is lost. An interior styled blank is emitted with its SGR, so a
// state that has one converges.
func TestRenderFullKeepsInteriorBlankAttributes(t *testing.T) {
	const cols, rows = 10, 2
	g := gridOf(cols, rows, "ab\x1b[1;31m   \x1b[0mz")
	src := g.Term().Grid().Row(0).Cell(2)
	if src.Flags()&vt.FlagBold == 0 {
		t.Fatalf("premise: cell 2 is not bold: %s", cellAttrKey(src))
	}
	got := replayFrame(t, g.RenderFrame(), cols, rows).Grid().Row(0).Cell(2)
	if cellAttrKey(got) != cellAttrKey(src) {
		t.Errorf("an interior styled blank was lost: frame=%s source=%s", cellAttrKey(got), cellAttrKey(src))
	}
}

// TestRenderFullKeepsTrailingBlanksIsBlankRejects is the other half of the
// bound: the attributes isBlank does check survive even in a row's trailing run,
// because a cell carrying them is not blank and moves `last`. Without this, "the
// frame loses trailing blank attributes" would read as a claim about all of them.
func TestRenderFullKeepsTrailingBlanksIsBlankRejects(t *testing.T) {
	const cols, rows = 10, 2
	for _, tc := range []struct{ name, sgr string }{
		{"background", "\x1b[41m"},
		{"inverse", "\x1b[7m"},
		{"underline", "\x1b[4m"},
		{"strikeout", "\x1b[9m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gridOf(cols, rows, "ab"+tc.sgr+"   \x1b[0m")
			src := g.Term().Grid().Row(0).Cell(2)
			got := replayFrame(t, g.RenderFrame(), cols, rows).Grid().Row(0).Cell(2)
			if cellAttrKey(got) != cellAttrKey(src) {
				t.Errorf("a trailing blank isBlank rejects was still lost: frame=%s source=%s",
					cellAttrKey(got), cellAttrKey(src))
			}
		})
	}
}

// TestIsBlankLossySet names the set exactly, on the cell primitive rather than
// through a render, so the set cannot drift without a failure here: these are
// the attributes a space can carry and still be dropped.
func TestIsBlankLossySet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sgr   string
		lossy bool
	}{
		{"foreground", "\x1b[31m", true},
		{"bold", "\x1b[1m", true},
		{"italic", "\x1b[3m", true},
		{"dim", "\x1b[2m", true},
		{"hidden", "\x1b[8m", true},
		{"background", "\x1b[41m", false},
		{"inverse", "\x1b[7m", false},
		{"underline", "\x1b[4m", false},
		{"underline colour", "\x1b[4m\x1b[58;5;9m", false},
		{"strikeout", "\x1b[9m", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gridOf(10, 2, tc.sgr+" \x1b[0m")
			c := g.Term().Grid().Row(0).Cell(0)
			if got := isBlank(c); got != tc.lossy {
				t.Errorf("isBlank(space with %s) = %v, want %v (cell %s)", tc.name, got, tc.lossy, cellAttrKey(c))
			}
		})
	}
}
