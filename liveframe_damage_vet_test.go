package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
)

// The damage stamps must never miss a change. The
// framer test above compares one damage framer with one full framer; these
// drive several consumers at different paces off the one shared epoch
// (clients behind, clients joining mid-stream, clients cropped), check every
// visible line's raw cells against a copy taken when the line was last
// reported unchanged, and run on grids with no history, a tiny history that
// evicts, and the default one.

// dmgSource is what a scenario draws its choices from: a seeded rng, or the
// bytes a fuzzer hands over (exhausted means done).
type dmgSource interface {
	intn(n int) int
	done() bool
}

type rngSource struct {
	r    *rand.Rand
	left int
}

func (s *rngSource) intn(n int) int { s.left--; return s.r.Intn(n) }
func (s *rngSource) done() bool     { return s.left <= 0 }

type byteSource struct {
	b []byte
	i int
}

func (s *byteSource) intn(n int) int {
	if s.i >= len(s.b) {
		s.i++
		return 0
	}
	v := int(s.b[s.i])
	s.i++
	if n > 256 {
		if s.i < len(s.b) {
			v = v<<8 | int(s.b[s.i])
			s.i++
		}
	}
	return v % n
}
func (s *byteSource) done() bool { return s.i >= len(s.b) }

var dmgPieces = []string{
	// printing, wide, combining, zero-width, invalid
	"a", "bc", "hello", "中", "🙂", "é", "́", "‍", "\xff", "\xe4\xb8", "x中y",
	// cursor and line motion
	"\r", "\n", "\r\n", "\b", "\x1b[H", "\x1b[2A", "\x1b[3B", "\x1b[4C", "\x1b[2D",
	"\x1b[2E", "\x1b[F", "\x1b[7G", "\x1b[3d", "\x1b[99;99H",
	// tabs and tab stops
	"\t", "\x1bH", "\x1b[g", "\x1b[3g", "\x1b[2I", "\x1b[Z",
	// erases
	"\x1b[K", "\x1b[1K", "\x1b[2K", "\x1b[J", "\x1b[1J", "\x1b[2J", "\x1b[3J", "\x1b[5X",
	// insert / delete / repeat
	"\x1b[2L", "\x1b[3M", "\x1b[4@", "\x1b[2P", "\x1b[3b", "\x1b[4h", "\x1b[4l",
	// scroll regions and scrolling
	"\x1b[3;8r", "\x1b[r", "\x1b[2;3r", "\x1b[2S", "\x1b[3T", "\x1bM", "\x1bM\x1bM\x1bM", "\x1bD", "\x1bE",
	// screens, modes
	"\x1b[?1049h", "\x1b[?1049l", "\x1b[?25l", "\x1b[?25h", "\x1b[?7l", "\x1b[?7h",
	"\x1b[?6h", "\x1b[?6l", "\x1b[?3h", "\x1b[?3l", "\x1b#8", "\x1bc", "\x1b[!p",
	// pen
	"\x1b[31m", "\x1b[1;4m", "\x1b[0m", "\x1b[7m", "\x1b[38;5;196m", "\x1b[48;2;1;2;3m",
	"\x1b[4:3m", "\x1b[58;5;9m", "\x1b[9m", "\x1b[41m",
	// saved cursor, charsets
	"\x1b7", "\x1b8", "\x1b[s", "\x1b[u", "\x1b(0", "\x1b(B", "\x0e", "\x0f", "lqk",
	// strings and sync
	"\x1b]0;title\x07", "\x1bP1$r0m\x1b\\", "\x1b[?2026h", "\x1b[?2026l",
}

var dmgCorpus [][]byte

// dmgSkipped and dmgChecked count lines a damaged refresh skipped and
// recopied: a run that skips none tested nothing B saves.
var dmgSkipped, dmgChecked int

func loadDmgCorpus(tb testing.TB) [][]byte {
	if dmgCorpus != nil {
		return dmgCorpus
	}
	for _, name := range []string{"UTF-8-test.txt", "cursor-test.txt", "combine-test.bin", "capture-pane-sgr0.bin"} {
		b, err := os.ReadFile(filepath.Join("vt", "testdata", "tmux", name))
		if err != nil {
			tb.Fatal(err)
		}
		dmgCorpus = append(dmgCorpus, bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")))
	}
	return dmgCorpus
}

// newDmgGrid is NewVtGrid with a chosen history limit.
func newDmgGrid(cols, rows, hist int) *VtGrid {
	colors := &ColorState{}
	l := &oscListener{colors: colors}
	g := &VtGrid{parser: vt.NewProcessor(), colors: colors, listener: l}
	cfg := vt.Config{ScrollingHistory: hist, BeforeHistoryClear: g.freezePausedViews}
	g.term = vt.NewTerm(cfg, cols, rows, l)
	return g
}

// dmgConsumer is one reader of the stamps: a grid-sized snapshot and raw row
// copies, both as of epoch e.
type dmgConsumer struct {
	name  string
	buf   *GridSnapshot
	raw   [][]vt.Cell
	e     uint64
	ok    bool
	term  *vt.Term
	pace  int // refresh every pace ops; 0: at random, 1 in 7
	count int
	// ccols, crows crop the consumer's view (0: the grid's size), so its
	// geometry can survive a grid resize and a damaged refresh spans it.
	ccols, crows int
	resizes      int // the grid's resize count at the last refresh
}

func viewRow(g *vt.Grid, line, cols int) []vt.Cell {
	var scratch []vt.Cell
	v := g.Row(line).View(&scratch)
	out := make([]vt.Cell, cols)
	copy(out, v[:min(len(v), cols)])
	return out
}

func (c *dmgConsumer) refresh(t testing.TB, g *VtGrid, st *dmgStats, where string) {
	term := g.term
	cols, rows := resolveDeclared(c.ccols, g.Cols()), resolveDeclared(c.crows, g.Lines())
	want := snapshotInto(nil, term, 0, cols, rows)
	if c.ok && c.term == term && c.buf.Cols == cols && c.buf.Rows == rows {
		grid := term.Grid()
		// Raw: every line reported unchanged holds exactly the cells it held.
		for r := 0; r < rows; r++ {
			if term.LineChangedSince(r, c.e) {
				dmgChecked++
				continue
			}
			dmgSkipped++
			now := viewRow(grid, r, cols)
			for i := range now {
				if now[i] != c.raw[r][i] {
					t.Fatalf("%s: consumer %s: line %d reported unchanged since %d but col %d changed: %+v -> %+v",
						where, c.name, r, c.e, i, c.raw[r][i], now[i])
				}
			}
		}
		if c.resizes != st.resizes {
			st.crossResize++
		}
		prev := &GridSnapshot{Cols: c.buf.Cols, Rows: c.buf.Rows, Cells: append([]CellSlot(nil), c.buf.Cells...)}
		got := snapshotDamaged(c.buf, term, cols, rows, c.e)
		if !snapshotsEqual(got, want) {
			for r := 0; r < rows; r++ {
				a, b := got.Cells[r*cols:(r+1)*cols], want.Cells[r*cols:(r+1)*cols]
				if !slotsEqual(a, b) {
					t.Fatalf("%s: consumer %s: damage snapshot row %d differs from full (changedSince=%v)",
						where, c.name, r, term.LineChangedSince(r, c.e))
				}
			}
			t.Fatalf("%s: consumer %s: damage snapshot cursor/visibility differs from full", where, c.name)
		}
		if n, m := countChangedSince(prev, got, term, c.e), CountChanged(prev, want); n != m {
			t.Fatalf("%s: consumer %s: countChangedSince %d, CountChanged %d", where, c.name, n, m)
		}
	} else {
		c.buf = want
	}
	c.raw = c.raw[:0]
	for r := 0; r < rows; r++ {
		c.raw = append(c.raw, viewRow(term.Grid(), r, cols))
	}
	c.e, c.ok, c.term, c.resizes = term.DamageEpoch(), true, term, st.resizes
}

// framerPair is one client: a full framer and a damage framer fed the same.
type framerPair struct {
	full, dmg    LiveFramer
	ccols, crows int
	pace, count  int
	resizes      int
}

func (p *framerPair) frame(t testing.TB, g *VtGrid, st *dmgStats, dirty bool, where string) {
	f := &p.dmg
	if cols, rows := resolveDeclared(p.ccols, g.Cols()), resolveDeclared(p.crows, g.Lines()); f.ok && f.lastOK && f.scratchOK &&
		f.last.Cols == cols && f.last.Rows == rows && f.scratch.Cols == cols && f.scratch.Rows == rows &&
		p.resizes != st.resizes && g.Gen() != f.gen {
		st.crossResizeFrames++
	}
	p.resizes = st.resizes
	old := damageFrames
	defer func() { damageFrames = old }()
	damageFrames = false
	want, wr, wf := p.full.Frame(nil, g, p.ccols, p.crows, dirty)
	damageFrames = true
	got, gr, gf := p.dmg.Frame(nil, g, p.ccols, p.crows, dirty)
	if !bytes.Equal(got, want) || gr != wr || gf != wf {
		t.Fatalf("%s: client %dx%d: damage frame %q (%v %v), full frame %q (%v %v)",
			where, p.ccols, p.crows, got, gr, gf, want, wr, wf)
	}
	if wr && !snapshotsEqual(p.dmg.last, p.full.last) {
		t.Fatalf("%s: client %dx%d: damage snapshot differs from full", where, p.ccols, p.crows)
	}
}

func (p *framerPair) settle(t testing.TB, g *VtGrid, where string) {
	old := damageFrames
	defer func() { damageFrames = old }()
	damageFrames = false
	want := p.full.Settle(nil, g, p.ccols, p.crows)
	damageFrames = true
	got := p.dmg.Settle(nil, g, p.ccols, p.crows)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: settle differs: damage %q, full %q", where, got, want)
	}
}

// dmgStats counts what a run exercised, so a run that never reached an
// operation cannot read as having covered it.
type dmgStats struct {
	crossResize, crossResizeFrames                                                              int
	advances, resizes, refreshes, frames, settles, invalidates, joins, endSyncs, corpus, noPack int
}

func runDamageScenario(t testing.TB, src dmgSource, st *dmgStats) {
	corpus := loadDmgCorpus(t)
	hists := []int{0, 3, 40, 10_000}
	cols, rows := 2+src.intn(60), 1+src.intn(25)
	g := newDmgGrid(cols, rows, hists[src.intn(len(hists))])
	consumers := []*dmgConsumer{{name: "every", pace: 1}, {name: "third", pace: 3}, {name: "random", pace: 0},
		{name: "crop-every", pace: 1}, {name: "crop-random", pace: 0}}
	for _, c := range consumers[3:] {
		c.ccols, c.crows = 1+src.intn(12), 1+src.intn(6)
	}
	pairs := []*framerPair{{pace: 1}, {pace: 2}, {pace: 0}}
	for _, p := range pairs[1:] {
		p.ccols, p.crows = 1+src.intn(70), 1+src.intn(30)
	}
	step := 0
	for !src.done() {
		step++
		where := "step " + strconv.Itoa(step)
		var b bytes.Buffer
		switch k := src.intn(20); {
		case k < 14:
			for n := src.intn(6) + 1; n > 0; n-- {
				b.WriteString(dmgPieces[src.intn(len(dmgPieces))])
			}
		case k < 15:
			fmt.Fprintf(&b, "\x1b[%d;%dH", src.intn(rows+3), src.intn(cols+3))
		case k < 16:
			fmt.Fprintf(&b, "\x1b[%d;%dr", src.intn(rows+2), src.intn(rows+2))
		case k < 17:
			fmt.Fprintf(&b, "%0*d\r\n", src.intn(3*cols+1), step)
		case k < 18:
			c := corpus[src.intn(len(corpus))]
			off := src.intn(len(c))
			b.Write(c[off:min(len(c), off+1+src.intn(400))])
			st.corpus++
		default:
			for n := src.intn(24); n > 0; n-- {
				b.WriteByte(byte(src.intn(256)))
			}
		}
		g.Advance(b.Bytes())
		st.advances++
		switch src.intn(30) {
		case 0:
			cols, rows = 2+src.intn(60), 1+src.intn(25)
			g.Resize(cols, rows)
			st.resizes++
		case 6:
			// A resize and back between two refreshes: the geometry a consumer
			// sees is unchanged, and the content was reflowed twice.
			g.Resize(2+src.intn(60), 1+src.intn(25))
			if src.intn(2) == 0 {
				g.Advance([]byte(dmgPieces[src.intn(len(dmgPieces))]))
			}
			g.Resize(cols, rows)
			st.resizes += 2
		case 1:
			p := pairs[1+src.intn(2)]
			p.ccols, p.crows = src.intn(70), src.intn(30)
		case 2:
			p := pairs[src.intn(3)]
			p.full.Invalidate()
			p.dmg.Invalidate()
			st.invalidates++
		case 3:
			// A client joins mid-stream: a fresh framer pair and a fresh consumer.
			i := src.intn(3)
			pairs[i] = &framerPair{ccols: pairs[i].ccols, crows: pairs[i].crows, pace: pairs[i].pace}
			consumers[i].ok = false
			st.joins++
		case 4:
			if g.EndSync() {
				st.endSyncs++
			}
		case 5:
			g.term.Grid().SetNoPack(src.intn(2) == 0)
			st.noPack++
		}
		for _, c := range consumers {
			c.count++
			if c.pace == 0 && src.intn(7) == 0 || c.pace > 0 && c.count%c.pace == 0 {
				c.refresh(t, g, st, where)
				st.refreshes++
			}
		}
		dirty := src.intn(4) != 0
		for _, p := range pairs {
			p.count++
			if p.pace == 0 && src.intn(5) == 0 || p.pace > 0 && p.count%p.pace == 0 {
				p.frame(t, g, st, dirty, where)
				st.frames++
				if src.intn(10) == 0 {
					p.settle(t, g, where)
					st.settles++
				}
			}
		}
	}
}

func dmgSeeds() (lo, hi int64) {
	lo, hi = 1, 300
	if v := os.Getenv("DMG_SEED_LO"); v != "" {
		lo, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := os.Getenv("DMG_SEED_HI"); v != "" {
		hi, _ = strconv.ParseInt(v, 10, 64)
	}
	return lo, hi
}

func TestDamageStampsMultiConsumer(t *testing.T) {
	old := damageFrames
	t.Cleanup(func() { damageFrames = old })
	lo, hi := dmgSeeds()
	var st dmgStats
	var ps vt.PackStats
	oldPS := vt.SetPackStats(&ps)
	t.Cleanup(func() { vt.SetPackStats(oldPS) })
	start := time.Now()
	for seed := lo; seed <= hi; seed++ {
		src := &rngSource{r: rand.New(rand.NewSource(seed)), left: 6000}
		t.Run("", func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("seed %d: panic %v", seed, r)
				}
			}()
			runDamageScenario(&seedTB{t, seed}, src, &st)
		})
		if t.Failed() {
			return
		}
	}
	t.Logf("seeds %d..%d in %v: %+v skipped=%d recopied=%d", lo, hi, time.Since(start), st, dmgSkipped, dmgChecked)
	t.Logf("pack stats: %+v", ps)
	// Packed history rows must have been made, brought back on screen
	// (unpacked) and replaced, or the packed paths went untested.
	if ps.Packed == 0 || ps.Unpacks == 0 || ps.Replaced == 0 || ps.Settles == 0 {
		t.Fatalf("packed history paths not exercised: %+v", ps)
	}
	if dmgSkipped == 0 {
		t.Fatal("no damaged refresh skipped a line: the stamps were never trusted")
	}
	// Coverage floor: each kind of event must have happened.
	if st.crossResize == 0 || st.crossResizeFrames == 0 || st.resizes == 0 || st.joins == 0 || st.endSyncs == 0 || st.corpus == 0 || st.invalidates == 0 || st.settles == 0 {
		t.Fatalf("scenario did not exercise every event: %+v", st)
	}
}

// seedTB prefixes failures with the seed.
type seedTB struct {
	testing.TB
	seed int64
}

func (s *seedTB) Fatalf(f string, a ...any) {
	s.TB.Helper()
	s.TB.Fatalf("seed %d: "+f, append([]any{s.seed}, a...)...)
}

func FuzzDamageStamps(f *testing.F) {
	for seed := int64(1); seed <= 8; seed++ {
		r := rand.New(rand.NewSource(seed))
		b := make([]byte, 600)
		r.Read(b)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		old := damageFrames
		defer func() { damageFrames = old }()
		var st dmgStats
		runDamageScenario(t, &byteSource{b: b}, &st)
	})
}

// The framer keys its damage epochs to the term they came from. VtGrid never
// replaces its term today, so this swaps one in by hand: a framer that
// trusted the old term's epochs against the new term's stamps would keep the
// old screen.
func TestLiveFramerDamageTermReplaced(t *testing.T) {
	old := damageFrames
	t.Cleanup(func() { damageFrames = old })
	g := newDmgGrid(20, 5, 100)
	var p framerPair
	var st dmgStats
	for i := 0; i < 40; i++ {
		g.Advance([]byte("\x1b[H" + strconv.Itoa(i)))
		p.frame(t, g, &st, false, "before")
	}
	fresh := newDmgGrid(20, 5, 100)
	fresh.Advance([]byte("\x1b[3;3Hother"))
	g.term = fresh.term
	g.gen++
	p.frame(t, g, &st, false, "after replacing the term")
	g.Advance([]byte("x"))
	p.frame(t, g, &st, false, "after replacing the term, then a write")
}
