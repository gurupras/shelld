package vt

import (
	"fmt"
	"testing"
)

// The alternative forms of the colored fill, so that comparing them is not also comparing
// two builds. Every earlier round of this comparison built one form per binary, and that
// cannot work at these sizes: a fill of a few cells takes 6 to 19 ns, where two builds of
// one arm differ by more than any form does.
//
//	shipped     min(len, seed) stores, then double with copy: the landed form
//	plain       one store per cell, which is what the doubling replaced
//	lentestseed a length test choosing between plain and doubling, at the shipped seed
//	lentest1    the same length test, doubling from a single cell
//	seedN       the shipped form's body with a seed of N, counter omitted
//	seedtwin    seedN at the shipped seed a second time, as the placement control
//
// The arms are reached through a function value, which is the one thing this harness does
// that a caller does not: fillBlank is inlined into all seven of its call sites. That is
// deliberate, since inlining each form into its own benchmark arm would measure eleven
// separate layouts rather than the forms.
//
// # Reading a difference between two arms
//
// Two arms can differ without differing in what they compute, and how much they can
// depends on how they differ. Three kinds of pair are here, and every gap this sweep
// reports has to be read against the widest gap of its own kind at its own width. The
// tables below are medians of six interleaved reps, as a share of the slower arm, and a
// star marks a pair whose min-max ranges are disjoint.
//
// Identical source. seedtwin against seed16 is the same text twice, so the only thing
// between them is where the linker put them: 0.37% at worst, overlapping at all twelve
// widths. This bounds placement and nothing else. It is not the floor for any other
// comparison here, and reading a form gap against it was the error two drafts of
// fillBlank's comment made.
//
// One immediate apart. Two seedN arms share a body and differ in the constant passed to
// it, and at a width at or below both seeds the min clamps both to the span, so they also
// execute the same instructions. The widest such pair per width:
//
//	 1: 0.59%*  2: 0.58%   8: 0.97%*  17: 0.45%   24: 1.25%   32: 0.47%
//	33: 1.41%  48: 0.28%  64: 0.14%   120+: no such pair exists
//
// Seven of them separate at all; the widest that does is seed32 against seed128 at 24
// cells, 1.07%. Past 64 cells no two seeds are both at least the span, so this kind gives
// no floor there and the class maximum of 1.41% is the conservative stand-in. lentest1
// against lentestseed is also one immediate apart, but only at a single width — n=1, where
// both take the length test's short branch — and there it is 0.27%, overlapping.
//
// Separate source. Two forms, one carrying code the other has no equivalent of, compared
// over a range where they still execute the same stores. Which pairings qualify follows
// from what each form does at a span of n: the plain loop writes n cells; a min arm with
// seed N writes min(n, N) and doubles the rest; a length-test arm with seed N writes n if
// n <= N and otherwise writes N and doubles. So the pairs are the plain loop against any
// seed at least n, a length-test arm below its own seed against any seed at least n, and a
// length-test arm above its own seed against a min arm carrying that same seed. The widest
// of them at each width is the floor, and this is the expensive kind, because unexecuted
// code is not free to the loop that does run:
//
//	 1: 10.62%*   2: 11.76%*   8: 10.01%*  17: 4.09%*  24: 8.59%*  32: 6.30%*
//	33:  2.02%*  48:  4.17%*  64:  4.58%*  120: 0.83%  200: 0.26%  400: 0.17%
//
// So a seed comparison answers to about a point and a form comparison to as much as
// eleven, and neither answers to the 0.37%. What that decides, in the sweep's own output:
// the seed is resolved from 48 cells up; the min against the length test is resolved at no
// width at all; and the doubling against the plain store loop is resolved at seven of the
// twelve, the store loop ahead at 17 and 33 cells and the doubling ahead from 48 up. See
// fillBlank's comment for the figures that survive.
func fillPlain(cells []Cell, bg Color) {
	if bg == 0 {
		clear(cells)
		return
	}
	blank := Cell{bg: bg}
	for i := range cells {
		cells[i] = blank
	}
}

// fillLenTestN is the other form: a length test choosing between the plain loop and
// doubling, rather than the min the shipped form uses. The seed is an argument for the
// same reason fillSeedN's is, so that the form and the seed are separate axes.
func fillLenTestN(cells []Cell, bg Color, seedN int) {
	if bg == 0 {
		clear(cells)
		return
	}
	blank := Cell{bg: bg}
	if len(cells) <= seedN {
		for i := range cells {
			cells[i] = blank
		}
		return
	}
	for i := range cells[:seedN] {
		cells[i] = blank
	}
	for filled := seedN; filled < len(cells); {
		filled += copy(cells[filled:], cells[:filled])
	}
}

func fillLenTestSeed(cells []Cell, bg Color) { fillLenTestN(cells, bg, fillBlankSeed) }
func fillLenTest1(cells []Cell, bg Color)    { fillLenTestN(cells, bg, 1) }

// fillSeedN is the shipped form with the seed as an argument, so that the seed can be
// swept without a second build. The argument is a constant at each call below, so each
// wrapper compiles to the same code a build with that constant would.
func fillSeedN(cells []Cell, bg Color, seedN int) {
	if bg == 0 {
		clear(cells)
		return
	}
	blank := Cell{bg: bg}
	seed := min(len(cells), seedN)
	for i := range cells[:seed] {
		cells[i] = blank
	}
	for filled := seed; filled < len(cells); {
		filled += copy(cells[filled:], cells[:filled])
	}
}

// The seeds swept. None of them carries the shipped body's counter, so that the sweep is
// one family of arms measured alike rather than one arm plus five, and so that the
// counter's own cost is the difference between two arms of this same binary.
func fillSeed8(cells []Cell, bg Color)   { fillSeedN(cells, bg, 8) }
func fillSeed16(cells []Cell, bg Color)  { fillSeedN(cells, bg, 16) }
func fillSeed24(cells []Cell, bg Color)  { fillSeedN(cells, bg, 24) }
func fillSeed32(cells []Cell, bg Color)  { fillSeedN(cells, bg, 32) }
func fillSeed64(cells []Cell, bg Color)  { fillSeedN(cells, bg, 64) }
func fillSeed128(cells []Cell, bg Color) { fillSeedN(cells, bg, 128) }

// fillSeedTwin is character for character the shipped seed's arm above. Whatever the two
// of them differ by is placement and only placement, since there is no other difference
// left for it to be: not the algorithm, not the seed, not the counter, only where the
// linker put the code. That makes it the first of the three controls in the header, and
// the narrowest — it is not the floor the other two kinds of comparison answer to. Its
// partner is whichever seedN arm equals the shipped seed — seed16 today; 8, 24 and 32 are
// there as well so that the pairing survives a change to it.
func fillSeedTwin(cells []Cell, bg Color) { fillSeedN(cells, bg, fillBlankSeed) }

// BenchmarkFillForms is where fillBlank's comment gets its comparisons between forms
// and between seeds. The widths start at one cell and end twenty-five times past the
// seed; a form is only distinguishable from another at a width where the two execute
// different instructions, which for a seed is a width strictly greater than it. Two of
// the widths are one cell past a seed, 17 and 33, because that is where a seed is at its
// worst: the fill writes the whole seed and then calls memmove to place one cell.
func BenchmarkFillForms(b *testing.B) {
	buf := make([]Cell, 400)
	bg := NewIndexedColor(200)
	for _, arm := range []struct {
		name string
		fill func([]Cell, Color)
	}{
		{"shipped", fillBlank},
		{"plain", fillPlain},
		{"lentestseed", fillLenTestSeed},
		{"lentest1", fillLenTest1},
		{"seed8", fillSeed8},
		{"seed16", fillSeed16},
		{"seedtwin", fillSeedTwin},
		{"seed24", fillSeed24},
		{"seed32", fillSeed32},
		{"seed64", fillSeed64},
		{"seed128", fillSeed128},
	} {
		for _, n := range []int{1, 2, 8, 17, 24, 32, 33, 48, 64, 120, 200, 400} {
			b.Run(fmt.Sprintf("%s/cells=%d", arm.name, n), func(b *testing.B) {
				span := buf[:n]
				f := arm.fill
				for b.Loop() {
					f(span, bg)
				}
			})
		}
	}
}

// Every form has to agree with the shipped one, or a benchmark of them is timing a
// different function rather than a different implementation of the same one. The
// widths are the benchmark's, plus the boundaries each form's own test is around.
func TestFillFormsAgree(t *testing.T) {
	bg := NewIndexedColor(200)
	for _, arm := range []struct {
		name string
		fill func([]Cell, Color)
	}{
		{"plain", fillPlain},
		{"lentestseed", fillLenTestSeed},
		{"lentest1", fillLenTest1},
		{"seed8", fillSeed8},
		{"seed16", fillSeed16},
		{"seedtwin", fillSeedTwin},
		{"seed24", fillSeed24},
		{"seed32", fillSeed32},
		{"seed64", fillSeed64},
		{"seed128", fillSeed128},
	} {
		for _, n := range []int{0, 1, 2, 8, 15, 16, 17, 23, 24, 31, 32, 33, 48, 63, 64, 65, 120, 127, 128, 129, 200, 400} {
			want := make([]Cell, n)
			fillBlank(want, bg)
			got := make([]Cell, n)
			arm.fill(got, bg)
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s at %d cells: cell %d is %+v, want %+v", arm.name, n, i, got[i], want[i])
				}
			}
			// And with the default background, which every form shortcuts.
			wantZero, gotZero := make([]Cell, n), make([]Cell, n)
			for i := range gotZero {
				gotZero[i] = Cell{ch: 'x'}
			}
			arm.fill(gotZero, 0)
			for i := range gotZero {
				if gotZero[i] != wantZero[i] {
					t.Fatalf("%s at %d cells with the default background: cell %d is %+v, want the zero cell", arm.name, n, i, gotZero[i])
				}
			}
		}
	}
}
