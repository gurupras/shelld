package shelld

import (
	"testing"
	"unicode/utf8"
)

// hasControl's contract is a claim about renderCR: "anything renderCR would treat as
// other than a printable character". renderScanLines relies on it in one direction
// only — a false means the line is trusted VERBATIM and renderCR is never consulted,
// so a rune renderCR could not have modelled must never answer false. The converse
// is harmless: hasControl true on a rune renderCR models exactly (a '\r', '\b',
// '\t', '\a') only costs the slow path, which then decides.
//
// So the property is the implication, and it is checked over every rune rather than
// over a table of the ones I thought of. That is the point: the original defect was
// U+0085, which is not a rune anyone writes a case for, and a hand-picked table is a
// list of the author's own guesses about where the disagreement is. An exhaustive
// sweep has no guesses in it — it derives the needle from renderCR itself.
//
// This is deliberately not the same test as TestHasControlCoversEveryRuneRenderCRGivesUp,
// which pins named examples and the reason each one matters. That one documents; this
// one searches.
func TestHasControlNeverTrustsALineRenderCRCannotModel(t *testing.T) {
	var checked, slower int
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue // the surrogate range: no line can hold one
		}
		s := string(r)
		_, exact, _ := renderCR(s)
		got := hasControl(s)
		checked++
		switch {
		case !exact && !got:
			// The defect's whole shape: the fast path hands this line to
			// IsIdleScan as trusted text, and renderCR — the only thing that
			// knows it cannot place the rune — is never asked.
			t.Errorf("hasControl(%q) = false but renderCR is not exact for U+%04X: "+
				"a line holding it is trusted verbatim and never rendered", s, r)
		case exact && got:
			slower++ // allowed: a modelled control, costing only the slow path
		}
		if t.Failed() && checked > 0 && testing.Short() {
			break
		}
	}
	// A liveness half, because every assertion above is inside a conditional: a
	// sweep that decoded nothing, or that found every rune printable, would report
	// exactly the same silence as a sweep that passed.
	if checked < 1_000_000 {
		t.Fatalf("the sweep checked only %d runes, so it proved nothing", checked)
	}
	if slower == 0 {
		t.Errorf("no rune was found that hasControl reports and renderCR models "+
			"exactly; '\\r' and '\\t' are two, so the sweep is not reaching "+
			"renderCR's modelled controls (checked %d)", checked)
	}
	t.Logf("swept %d runes; %d take the slow path and render exactly", checked, slower)
}
