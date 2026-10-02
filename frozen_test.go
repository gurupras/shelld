package shelld

import (
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// A PausedView's promise is that the content a client is reading stays readable,
// so these tests are about which events hand a view a copy and what the copy then
// serves. They read the copy through its own API — SnapshotInto and RenderDiff —
// because that is what a client reads it through.

// historyGrid fills a grid with rows of two distinguishable halves, each row
// filling the width. Two halves so that halving the width splits a row into two
// rows that are still tellable apart, and full width so that it splits at all: a
// reflow only rearranges rows that do not fit, and a short row narrows to itself.
func historyGrid(t *testing.T, cols, rows, lines int) *VtGrid {
	t.Helper()
	if cols%2 != 0 {
		t.Fatalf("historyGrid needs an even width, got %d", cols)
	}
	g := NewVtGrid(cols, rows)
	var b strings.Builder
	for i := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(strings.Repeat(string(rune('A'+2*i%26)), cols/2))
		b.WriteString(strings.Repeat(string(rune('A'+(2*i+1)%26)), cols/2))
	}
	g.Advance([]byte(b.String()))
	if g.ScrollbackLen() == 0 {
		t.Fatalf("setup: no scrollback, so there is nothing for a paused view to hold")
	}
	return g
}

// snapRow is row r of a snapshot as text, trailing blanks trimmed.
func snapRow(s *GridSnapshot, r int) string {
	var b strings.Builder
	for _, c := range s.Cells[r*s.Cols : (r+1)*s.Cols] {
		if c.Ch == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(c.Ch)
	}
	return strings.TrimRight(b.String(), " ")
}

// frozenTop is the first row a client at offset reads out of a copy, at the
// copy's own width and one row tall: enough to identify the row, and nothing
// about the client's size that a separate test is not already about.
func frozenTop(f *FrozenHistory, offset int) string {
	return snapRow(f.SnapshotInto(nil, offset, f.cols, 1), 0)
}

// gridTop is the same row read from the live grid, for a before-and-after pair.
func gridTop(g *VtGrid, offset int) string {
	return snapRow(g.SnapshotAt(offset), 0)
}

// enterAlt takes the alternate screen and checks that it did. The alternate-screen
// tests all turn on which of the two grids a read reaches, so a DECSET that
// quietly stopped switching would leave them testing the primary screen and
// passing for the wrong reason.
func enterAlt(t *testing.T, g *VtGrid) {
	t.Helper()
	g.Advance([]byte("\x1b[?1049h"))
	if !g.AltScreenActive() {
		t.Fatalf("setup: DECSET 1049 did not take the alternate screen")
	}
	if n := g.ScrollbackLen(); n != 0 {
		t.Fatalf("setup: the active grid holds %d scrollback rows, so it is not the alternate one", n)
	}
}

// TestAClearHandsAPausedViewTheHistoryItWasReading is the ESC[3J event. The
// clear drops every history row at once, so a view that is still reading them
// has to be handed them first or they are gone.
func TestAClearHandsAPausedViewTheHistoryItWasReading(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()
	if v.Frozen() != nil {
		t.Fatalf("a view is handed a copy by an event, not by registering")
	}
	want := gridTop(g, 4)

	g.Advance([]byte("\x1b[3J"))

	f := v.Frozen()
	if f == nil {
		t.Fatalf("a clear handed the view no copy, so what it was reading is gone")
	}
	if got := frozenTop(f, 4); got != want {
		t.Errorf("the copy reads %q at offset 4, want %q — it is not the history that was cleared", got, want)
	}
	if g.ScrollbackLen() != 0 {
		t.Errorf("live history is %d rows; this test needs the clear to have actually emptied it", g.ScrollbackLen())
	}
}

// TestAFullResetHandsAPausedViewTheHistoryItWasReading is the other wholesale
// drop: RIS resets the terminal, history included, by a different path than
// ESC[3J and with no history-clear escape in the stream to recognise it by.
func TestAFullResetHandsAPausedViewTheHistoryItWasReading(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()
	want := gridTop(g, 4)

	g.Advance([]byte("\x1bc"))

	f := v.Frozen()
	if f == nil {
		t.Fatalf("a reset handed the view no copy, so what it was reading is gone")
	}
	if got := frozenTop(f, 4); got != want {
		t.Errorf("the copy reads %q at offset 4, want %q", got, want)
	}
	if g.ScrollbackLen() != 0 {
		t.Errorf("live history is %d rows; this test needs the reset to have emptied it", g.ScrollbackLen())
	}
}

// TestAResetUnderTheAlternateScreenCopiesThePrimarysCount is the reset arriving
// while a program owns the alternate screen. The rows a viewer is reading are in
// the primary grid, and the reset drops them, so a copy is still owed — but a copy
// records the count its holder then follows from, and the alternate grid has a
// count of its own.
//
// Term.resetState leaves the alternate screen by swapping the two grids, and the
// mode bit has to go with the swap: HistoryPushed picks its grid by that bit, so
// with the bit still set it reads the grid the swap just moved aside and reports
// the alternate screen's count. A viewer handed a copy stamped with that count
// applies the whole of the primary's count as an outstanding correction on its next
// tick, which drives its offset to the far end of what it holds.
func TestAResetUnderTheAlternateScreenCopiesThePrimarysCount(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	want := g.HistoryPushed()
	if want == 0 {
		t.Fatalf("setup: nothing was pushed, so any count would pass")
	}
	v := g.NewPausedView()

	enterAlt(t, g)
	g.Advance([]byte("\x1bc")) // RIS, arriving while the alternate screen is active

	f := v.Frozen()
	if f == nil {
		t.Fatalf("a reset under the alternate screen handed out no copy, though it dropped the history")
	}
	if f.pushed != want {
		t.Errorf("the copy records %d pushes, want the primary screen's %d: it read the alternate grid's count",
			f.pushed, want)
	}
}

// TestAWidthChangeUnderTheAlternateScreenCopiesThePrimary is the other half of the
// same confusion, and the one that loses the content rather than the count. A
// resize reflows the primary screen whichever screen is active, so a viewer holding
// a position in that history has to be copied for; the alternate grid has no
// scrollback at all, so copying it hands out an empty view.
//
// This is the site where the copy's two halves can disagree. Its rows and its
// Pushed both have to name the primary screen, and unlike the reset above, here
// the mode bit is still set when the copy is taken — so a count read from the
// active grid is the alternate screen's zero while the rows are the primary's.
// Both axes are asserted for that reason.
func TestAWidthChangeUnderTheAlternateScreenCopiesThePrimary(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	hist := g.ScrollbackLen()
	wantPushed := g.HistoryPushed()
	want := gridTop(g, 4)
	v := g.NewPausedView()
	if wantPushed == 0 {
		t.Fatalf("setup: nothing was pushed, so any count would pass")
	}

	enterAlt(t, g)
	g.Resize(10, 3)

	f := v.Frozen()
	if f == nil {
		t.Fatalf("a width change under the alternate screen handed out no copy, though it reflowed the primary")
	}
	if f.HistoryLen() != hist {
		t.Fatalf("the copy holds %d history rows, want the primary's %d: it copied the alternate grid",
			f.HistoryLen(), hist)
	}
	if got := frozenTop(f, 4); got != want {
		t.Errorf("the copy reads %q at offset 4, want %q", got, want)
	}
	if f.pushed != wantPushed {
		t.Errorf("the copy records %d pushes, want the primary screen's %d: its rows and its count name different grids",
			f.pushed, wantPushed)
	}
}

// TestAClearOnTheAlternateScreenHandsOutNoCopy is the third alternate-screen case,
// and the one where doing nothing is correct. ESC[3J clears the history of the
// screen that is active, and the alternate screen has none — so a paused viewer's
// rows are untouched, no copy is owed, and none is taken.
//
// It is worth pinning because the safety of that nothing rests on where the clear
// reaches. The freeze is driven by the same guard the clear is, so a change that
// let ESC[3J reach the primary's history from the alternate screen would drop a
// viewer's rows with no copy taken and no test to say so. Giving the screen back
// and finding the rows still there is what makes the claim, rather than just the
// absence of a copy.
func TestAClearOnTheAlternateScreenHandsOutNoCopy(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()
	hist, want := g.ScrollbackLen(), gridTop(g, 4)

	enterAlt(t, g)
	g.Advance([]byte("\x1b[3J"))

	if f := v.Frozen(); f != nil {
		t.Errorf("a clear on the alternate screen copied %d rows; there is no history there to clear", f.rows)
	}
	g.Advance([]byte("\x1b[?1049l")) // give the screen back

	if got := g.ScrollbackLen(); got != hist {
		t.Fatalf("the primary holds %d history rows after the clear, want the %d it had: the clear reached it and nothing was copied first",
			got, hist)
	}
	if got := gridTop(g, 4); got != want {
		t.Errorf("offset 4 reads %q, want the %q it read before", got, want)
	}
}

// TestAWidthChangeHandsAPausedViewTheHistoryAtTheOldWidth is the reflow event.
// Nothing is destroyed — every character is still in the grid — but the rows are
// rewritten, so the row at a given offset is different text afterwards and the
// view has to keep the arrangement it was reading.
func TestAWidthChangeHandsAPausedViewTheHistoryAtTheOldWidth(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()
	want := gridTop(g, 4)

	g.Resize(10, 3)

	f := v.Frozen()
	if f == nil {
		t.Fatalf("a width change handed the view no copy, so its rows were rewritten under it")
	}
	if f.cols != 20 {
		t.Errorf("the copy is %d columns wide, want the 20 it was taken at", f.cols)
	}
	if got := frozenTop(f, 4); got != want {
		t.Errorf("the copy reads %q at offset 4, want %q", got, want)
	}
	if live := gridTop(g, 4); live == want {
		t.Fatalf("the reflow left offset 4 reading %q either way, so this test would pass without a copy", live)
	}
}

// TestARowOnlyResizeHandsOutNoCopy is the negative control, and the reason the
// copy is affordable. A change in rows moves whole rows between the screen and
// history, which is exactly what HistoryPushed reports, so a view can still be
// followed through it and nothing is copied.
func TestARowOnlyResizeHandsOutNoCopy(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()

	g.Resize(20, 8) // rows grow, pulling history back onto the screen
	if f := v.Frozen(); f != nil {
		t.Errorf("a row-only grow copied %d rows of history; reanchoring covers this case", f.rows)
	}
	g.Resize(20, 2) // and shrink, pushing screen rows into history
	if f := v.Frozen(); f != nil {
		t.Errorf("a row-only shrink copied %d rows of history; reanchoring covers this case", f.rows)
	}
}

// TestOrdinaryOutputHandsOutNoCopy is the other half of that control, and the
// stronger one: output is the path every line a program prints takes, including
// the line that pushes the oldest row out of a full history. Copying there would
// put the cost on the hot path, and the row that ages out is gone for every
// reader anyway.
func TestOrdinaryOutputHandsOutNoCopy(t *testing.T) {
	g := NewVtGrid(20, 3)
	g.Advance([]byte(strings.Repeat("\r\nfill", 10_050))) // past the 10,000-line limit
	v := g.NewPausedView()
	before := g.ScrollbackLen()

	g.Advance([]byte(strings.Repeat("\r\nmore", 50)))

	if g.ScrollbackLen() != before {
		t.Fatalf("history moved from %d to %d rows; this test needs it pinned at the limit, evicting", before, g.ScrollbackLen())
	}
	if f := v.Frozen(); f != nil {
		t.Errorf("eviction copied %d rows of history; it must not be on the path output takes", f.rows)
	}
}

// TestASecondEventLeavesAnAlreadyFrozenViewAlone pins what a copy being private
// means. Two views pause at different times; one is frozen by the first event and
// the other by the second, and each keeps what it was reading rather than both
// ending up on the newer copy.
func TestASecondEventLeavesAnAlreadyFrozenViewAlone(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	early := g.NewPausedView()
	late := g.NewPausedView()
	first := gridTop(g, 4)

	g.Advance([]byte("\x1b[3J"))
	g.Advance([]byte("\r\n" + strings.Repeat("\r\nsecond", 10)))
	second := gridTop(g, 4)
	if second == first {
		t.Fatalf("both events left offset 4 reading %q, so this test cannot tell the two copies apart", first)
	}
	// The late view catches up to the live grid before the second event, which is
	// what a client that paused after the clear would have.
	late.Release()
	late = g.NewPausedView()

	g.Advance([]byte("\x1b[3J"))

	if got := frozenTop(early.Frozen(), 4); got != first {
		t.Errorf("the view frozen first reads %q, want the %q it was reading", got, first)
	}
	if got := frozenTop(late.Frozen(), 4); got != second {
		t.Errorf("the view frozen second reads %q, want the %q it was reading", got, second)
	}
	if early.Frozen() == late.Frozen() {
		t.Errorf("both views hold one copy, so one of them is reading rows it never saw")
	}
}

// TestViewsFrozenTogetherShareOneCopy is the allocation that argument buys: two
// views reading the same grid at the same instant want the same rows, and a copy
// is never written to, so one serves both.
func TestViewsFrozenTogetherShareOneCopy(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	a, b := g.NewPausedView(), g.NewPausedView()

	g.Advance([]byte("\x1b[3J"))

	if a.Frozen() == nil || a.Frozen() != b.Frozen() {
		t.Errorf("two views frozen by one event hold %p and %p, want one copy between them", a.Frozen(), b.Frozen())
	}
}

// TestAReleasedViewIsNoLongerCopiedFor is the leak. A client that returns to the
// live view, or goes away, must stop being a reason to copy history: a grid that
// keeps a released view goes on paying for a reader that is not there.
func TestAReleasedViewIsNoLongerCopiedFor(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	kept, released := g.NewPausedView(), g.NewPausedView()
	released.Release()
	if released.Frozen() != nil {
		t.Errorf("a released view still holds a copy, pinning history it no longer reads")
	}

	g.Advance([]byte("\x1b[3J"))

	if released.Frozen() != nil {
		t.Errorf("a released view was handed a copy by a later event")
	}
	if kept.Frozen() == nil {
		t.Fatalf("releasing one view stopped the other from being copied for")
	}
	kept.Release()
	if n := len(g.pausedViews); n != 0 {
		t.Errorf("%d views still registered after releasing both", n)
	}
	released.Release() // a second release is a no-op, not a panic
}

// TestAFrozenViewScrollsWithinItsCopy is the question a client asks next: having
// been handed a copy because a peer cleared, can it still page further up? It can,
// and only within the copy — HistoryLen is what bounds it, not the live history,
// which is empty.
func TestAFrozenViewScrollsWithinItsCopy(t *testing.T) {
	g := historyGrid(t, 20, 3, 10)
	v := g.NewPausedView()
	oldest := gridTop(g, g.ScrollbackLen())

	g.Advance([]byte("\x1b[3J"))

	f := v.Frozen()
	if f.HistoryLen() == 0 {
		t.Fatalf("the copy holds no history, so there is nothing to scroll through")
	}
	if got := frozenTop(f, f.HistoryLen()); got != oldest {
		t.Errorf("the top of the copy reads %q, want the oldest row %q", got, oldest)
	}
	// Past the end clamps to the oldest row rather than reading off the rectangle.
	if got := frozenTop(f, f.HistoryLen()+50); got != oldest {
		t.Errorf("an offset past the copy reads %q, want the oldest row %q", got, oldest)
	}
}

// TestARepaintFromACopyDrawsWhatTheClientWasReading is the render path: the
// frame a frozen client receives has to put its rows on the client's screen.
// Replaying the frame into a terminal is how a client sees it.
func TestARepaintFromACopyDrawsWhatTheClientWasReading(t *testing.T) {
	const cols, rows = 20, 4
	g := historyGrid(t, cols, rows, 10)
	v := g.NewPausedView()
	want := make([]string, rows)
	view := g.SnapshotAt(4)
	for r := range rows {
		want[r] = snapRow(view, r)
	}

	g.Advance([]byte("\x1b[3J"))

	f := v.Frozen()
	cur := f.SnapshotInto(nil, 4, cols, rows)
	frame := f.RenderDiff(nil, EmptySnapshot(), cur)
	term := replayFrame(t, frame, cols, rows)
	for r := range rows {
		got := strings.TrimRight(termRow(term, r), " ")
		if got != want[r] {
			t.Errorf("row %d of the client's screen reads %q, want %q", r, got, want[r])
		}
	}
}

// TestADiffMovesAFrozenViewToANewOffset is the same path on the second frame: a
// client scrolling inside its copy gets a diff, not a repaint, and the diff has to
// land it on the rows the new offset addresses.
func TestADiffMovesAFrozenViewToANewOffset(t *testing.T) {
	const cols, rows = 20, 4
	g := historyGrid(t, cols, rows, 10)
	v := g.NewPausedView()
	g.Advance([]byte("\x1b[3J"))
	f := v.Frozen()

	at4 := f.SnapshotInto(nil, 4, cols, rows)
	term := replayFrame(t, f.RenderDiff(nil, EmptySnapshot(), at4), cols, rows)

	at6 := f.SnapshotInto(nil, 6, cols, rows)
	diff := f.RenderDiff(nil, at4, at6)
	if len(diff) == 0 {
		t.Fatalf("scrolling from offset 4 to 6 emitted nothing, so the client is still on the old rows")
	}
	vt.NewProcessor().Advance(term, diff)

	for r := range rows {
		want := snapRow(at6, r)
		if got := strings.TrimRight(termRow(term, r), " "); got != want {
			t.Errorf("row %d reads %q after the diff, want %q", r, got, want)
		}
	}
}

// TestADiffWithinACopyIsEmptyWhenNothingMoved is what makes a frozen client free
// to serve: its copy cannot change, so a window printing at full speed produces
// no frames for it at all.
func TestADiffWithinACopyIsEmptyWhenNothingMoved(t *testing.T) {
	const cols, rows = 20, 4
	g := historyGrid(t, cols, rows, 10)
	v := g.NewPausedView()
	g.Advance([]byte("\x1b[3J"))
	f := v.Frozen()

	prev := f.SnapshotInto(nil, 4, cols, rows)
	cur := f.SnapshotInto(nil, 4, cols, rows)
	if diff := f.RenderDiff(nil, prev, cur); len(diff) != 0 {
		t.Errorf("an unmoved view emitted %d bytes: %q", len(diff), diff)
	}
}

// TestACopyCropsAndPadsToTheClientsDeclaredSize is the per-client sizing the
// live path already has. One copy is taken at the terminal's width and serves
// every client whatever size it declared, so it has to crop for a narrower one
// and pad for a wider one exactly as a live capture does.
func TestACopyCropsAndPadsToTheClientsDeclaredSize(t *testing.T) {
	const cols, rows = 20, 4
	g := historyGrid(t, cols, rows, 10)
	v := g.NewPausedView()
	full := snapRow(g.SnapshotAt(4), 0)

	g.Advance([]byte("\x1b[3J"))
	f := v.Frozen()

	narrow := f.SnapshotInto(nil, 4, 8, rows)
	if got, want := snapRow(narrow, 0), strings.TrimRight(full[:8], " "); got != want {
		t.Errorf("a client 8 columns wide reads %q, want the first 8 columns %q", got, want)
	}
	wide := f.SnapshotInto(nil, 4, cols+6, rows+2)
	if got := snapRow(wide, 0); got != full {
		t.Errorf("a client wider than the copy reads %q, want %q padded", got, full)
	}
	for _, c := range wide.Cells[cols : cols+6] {
		if c != padSlot {
			t.Errorf("column past the copy is %+v, want the pad slot a live capture uses", c)
			break
		}
	}
	if r := rows; snapRow(wide, r) != "" {
		t.Errorf("row %d past the copy reads %q, want blank", r, snapRow(wide, r))
	}
}

// TestACopyKeepsAWideCharacterWithItsSpacer pins the one thing the copy has to
// normalise. A double-width character occupies two cells and is only drawable as
// the pair; a crop that splits the pair leaves a half the renderers must not
// emit. freezeHistory pairs the rows it copies for the same reason snapshotInto
// pairs the rows it captures.
func TestACopyKeepsAWideCharacterWithItsSpacer(t *testing.T) {
	const cols, rows = 6, 2
	g := NewVtGrid(cols, rows)
	// 漢 spans columns 0-1 of each line; the scrolled-off lines are the history.
	g.Advance([]byte("漢ab\r\n漢cd\r\n漢ef\r\n漢gh"))
	if g.ScrollbackLen() == 0 {
		t.Fatalf("setup: no scrollback")
	}
	v := g.NewPausedView()
	g.Advance([]byte("\x1b[3J"))
	f := v.Frozen()

	row := f.cells[:f.cols]
	if !row[0].Wide() || !row[1].WideSpacer() {
		t.Fatalf("the copy's first row is %v/%v, want a wide character and its spacer", row[0].Attrs, row[1].Attrs)
	}
	// Cropped to one column the pair cannot be drawn, so the wide half becomes a
	// blank rather than a character the client would render at the wrong width.
	half := f.SnapshotInto(nil, f.HistoryLen(), 1, 1)
	if c := half.Cells[0]; c.Wide() || c.Ch == '漢' {
		t.Errorf("a one-column crop kept %q as wide, which a client cannot draw", c.Ch)
	}
	// Replayed at full width the character is back on the client's screen.
	full := f.SnapshotInto(nil, f.HistoryLen(), cols, 1)
	term := replayFrame(t, f.RenderDiff(nil, EmptySnapshot(), full), cols, 1)
	if got := termRow(term, 0); !strings.HasPrefix(got, "漢") {
		t.Errorf("the client's row reads %q, want it to start with the wide character", got)
	}
}

// termRow is row r of a terminal's viewport as text, which is how a replayed
// frame is read back.
func termRow(term *vt.Term, r int) string {
	grid := term.Grid()
	var b strings.Builder
	row := grid.Row(r)
	for c := 0; c < grid.Columns(); c++ {
		ch := row.Cell(c).Rune()
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}
