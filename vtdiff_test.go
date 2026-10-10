package shelld

// Differential test: the Go VtGrid against shelld-rs's VtGrid
// (alacritty_terminal 0.26). Both sides execute the same op stream and print
// grid state and render output; any divergence fails with the first
// differing line.
//
//	SHELLD_VTDIFF=1 SHELLD_RS_DIR=/path/to/shelld-rs go test . -run TestVtDiff
//
// Requires cargo and a shelld-rs checkout (the directory holding the shelld-core
// crate).

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// ops builds an op stream (see testdata/rsdump/src/main.rs).
type ops struct{ bytes.Buffer }

func newOps(cols, rows int) *ops {
	o := &ops{}
	o.u16(cols)
	o.u16(rows)
	return o
}

func (o *ops) u16(v int) { _ = binary.Write(o, binary.LittleEndian, uint16(v)) }
func (o *ops) u32(v int) { _ = binary.Write(o, binary.LittleEndian, uint32(v)) }

func (o *ops) feed(b []byte) {
	o.WriteByte('F')
	o.u32(len(b))
	o.Write(b)
}
func (o *ops) resize(c, r int) { o.WriteByte('R'); o.u16(c); o.u16(r) }
func (o *ops) dump()           { o.WriteByte('D') }
func (o *ops) frameAt(off int) { o.WriteByte('A'); o.u32(off) }
func (o *ops) cropped(c, r int) {
	o.WriteByte('C')
	o.u16(c)
	o.u16(r)
}
func (o *ops) diff(slot, off, c, r int) {
	o.WriteByte('P')
	o.WriteByte(byte(slot))
	o.u32(off)
	o.u16(c)
	o.u16(r)
}
func (o *ops) osc() { o.WriteByte('Q') }
func (o *ops) setColor(idx int, r, g, b byte) {
	o.WriteByte('K')
	o.u16(idx)
	o.Write([]byte{r, g, b})
}

func colorStr(c vt.Color) string {
	switch c.Kind() {
	case vt.KindNamed:
		return fmt.Sprintf("N%d", c.Named())
	case vt.KindIndexed:
		return fmt.Sprintf("I%d", c.Index())
	case vt.KindRGB:
		rgb := c.RGB()
		return fmt.Sprintf("R%02x%02x%02x", rgb.R, rgb.G, rgb.B)
	}
	return "-"
}

func goDump(g *VtGrid, w *bufio.Writer) {
	term := g.Term()
	grid := term.Grid()
	cur, saved := grid.Cursor, grid.SavedCursor
	wrap := 0
	if cur.InputNeedsWrap {
		wrap = 1
	}
	fmt.Fprintf(w, "grid %d %d hist=%d cur=%d,%d,%d saved=%d,%d mode=%x\n",
		grid.Columns(), grid.ScreenLines(), grid.HistorySize(),
		cur.Point.Line, cur.Point.Column, wrap, saved.Point.Line, saved.Point.Column,
		uint32(term.Mode())&0x3ffff)
	defFg, defBg := vt.NewNamedColor(vt.Foreground), vt.NewNamedColor(vt.Background)
	for l := -grid.HistorySize(); l < grid.ScreenLines(); l++ {
		row := grid.Row(l)
		fmt.Fprintf(w, "L%d", l)
		for c := 0; c < grid.Columns(); c++ {
			cell := row.Cell(c)
			if cell.Rune() == ' ' && cell.Flags() == 0 && cell.UnderlineColor() == vt.NoColor && cell.Fg() == defFg && cell.Bg() == defBg {
				continue
			}
			fmt.Fprintf(w, " %d:%x.%s.%s.%x.%s", c, cell.Rune(), colorStr(cell.Fg()), colorStr(cell.Bg()), uint16(cell.Flags()), colorStr(cell.UnderlineColor()))
		}
		w.WriteByte('\n')
	}
	fmt.Fprintf(w, "frame %s\n", hex.EncodeToString(g.RenderFrame()))
}

func snapSummary(s *GridSnapshot) string {
	var sum uint64 = 1469598103934665603
	mix := func(v uint64) {
		sum ^= v
		sum *= 1099511628211
	}
	for _, c := range s.Cells {
		mix(uint64(c.Ch))
		mix(b2u(c.Wide()))
		mix(b2u(c.WideSpacer()))
	}
	return fmt.Sprintf("%dx%d cur=%d,%d vis=%d cells=%x", s.Cols, s.Rows, s.CursorRow, s.CursorCol, b2u(s.CursorVisible), sum)
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// runGo executes an op stream against the Go VtGrid.
func runGo(stream []byte) string {
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	rd := bytes.NewReader(stream)
	u16 := func() int { var v uint16; _ = binary.Read(rd, binary.LittleEndian, &v); return int(v) }
	u32 := func() int { var v uint32; _ = binary.Read(rd, binary.LittleEndian, &v); return int(v) }
	g := NewVtGrid(u16(), u16())
	slots := make([]*GridSnapshot, 4)
	for i := range slots {
		slots[i] = EmptySnapshot()
	}
	for {
		op, err := rd.ReadByte()
		if err != nil {
			break
		}
		switch op {
		case 'F':
			buf := make([]byte, u32())
			_, _ = rd.Read(buf)
			g.Advance(buf)
		case 'R':
			g.Resize(u16(), u16())
		case 'D':
			goDump(g, w)
		case 'A':
			off := u32()
			fmt.Fprintf(w, "frame_at %d %s\n", off, hex.EncodeToString(g.RenderFrameAt(off)))
		case 'C':
			c, r := u16(), u16()
			fmt.Fprintf(w, "cropped %dx%d %s\n", c, r, hex.EncodeToString(g.RenderFrameCropped(c, r)))
		case 'P':
			slot, _ := rd.ReadByte()
			off := u32()
			c, r := u16(), u16()
			s, snap := g.RenderDiffAtCropped(slots[slot], off, c, r)
			fmt.Fprintf(w, "diff %d %d %dx%d %s %s\n", slot, off, c, r, hex.EncodeToString(s), snapSummary(snap))
			slots[slot] = snap
		case 'Q':
			fmt.Fprintf(w, "osc %s\n", hex.EncodeToString(g.TakeReplies()))
		case 'K':
			idx := u16()
			var rgb [3]byte
			_, _ = rd.Read(rgb[:])
			g.Colors().Set(idx, vt.RGB{R: rgb[0], G: rgb[1], B: rgb[2]})
		default:
			panic(fmt.Sprintf("bad op %q", op))
		}
	}
	w.Flush()
	return out.String()
}

// referencePatch is a one-line fix to a crate the reference is built from, kept
// here because the defect is in the oracle rather than in either terminal: a
// differential test whose reference is wrong on a case reports a divergence
// where there is no disagreement about what a terminal should do.
//
// The argument for each is the entry's own defect field below, next to the diff it
// justifies, so the case for a patch and the patch travel together. What is not
// here is the long form -- a minimal reproduction per entry and the reference's own
// self-contradiction on advance_partial_utf8 -- which was written up outside this
// repo; a reader who needs more than the defect field has to re-derive it from the
// diff rather than follow a link. Every one is an upstream
// defect that should be reported upstream from a machine with network access;
// until they are fixed there, a patch that only lives here is a debt this
// comment is naming. Run with SHELLD_VTDIFF_STOCK=1 to build against the crates
// as published and see the divergences. Do not count the entries from this
// comment: the slice below is the enumeration.
//
// cursor-scroll-region is the one whose expectations did not come from reading
// either implementation: the answers are measured from tmux 3.8-rc, and
// vt.TestCursorMovesWithinTheScrollingRegionAsTmuxDoes holds the whole table with
// how it was taken. Against the stock crate 13 of those 26 inputs disagree.
//
// The last three arrived together, from a review that found the defects in the Go
// port and fixed them there before anyone checked the reference — where all three
// turned out to be reference-exact transcriptions. They are here because the Go
// fixes are right, not because the port drifted: without them TestVtDiffFuzz
// reports a divergence on sequences fuzzChunk generates freely (DCH and ED come
// out of the letter set at its CSI case, DECSTBM out of its own, and num() reaches
// 65535 against screens of 1 to 40 rows).
type referencePatch struct {
	crate, version, diff, defect string
}

var referencePatches = []referencePatch{{
	crate:   "alacritty_terminal",
	version: "0.26.0",
	diff:    "testdata/rsdump/patches/alacritty_terminal-0.26.0-row-grow-occ.diff",
	defect: "Row::grow appends default cells without extending occ, so a row whose own tail " +
		"was painted by back-colour erase keeps those cells through a reset after a widen",
}, {
	crate:   "vte",
	version: "0.15.0",
	diff:    "testdata/rsdump/patches/vte-0.15.0-advance-partial-utf8.diff",
	defect: "advance_partial_utf8 prints one rune and returns every well-formed byte in the " +
		"buffer, so bytes after that rune are consumed unprinted (the defect 6fd707a fixed in Go)",
}, {
	crate:   "alacritty_terminal",
	version: "0.26.0",
	diff:    "testdata/rsdump/patches/alacritty_terminal-0.26.0-cursor-scroll-region.diff",
	defect: "the relative cursor moves and CHA go through goto, which applies DECOM's " +
		"translation of absolute addressing to all of them and bounds none of them by the " +
		"scrolling region, so CUU and CUD leave a region they started inside",
}, {
	crate:   "alacritty_terminal",
	version: "0.26.0",
	diff:    "testdata/rsdump/patches/alacritty_terminal-0.26.0-dch-clamp.diff",
	defect: "delete_chars clamps the count to the whole row rather than to the cells after " +
		"the cursor, and the trailing fill starts at columns-count, so a count past the " +
		"cells remaining blanks cells to the left of the cursor",
}, {
	crate:   "alacritty_terminal",
	version: "0.26.0",
	diff:    "testdata/rsdump/patches/alacritty_terminal-0.26.0-ed-above-line-one.diff",
	defect: "ED 1 guards its reset_region with cursor.line > 1, so with the cursor on line 1 " +
		"line 0 — the only line above it — is left unerased",
}, {
	crate:   "alacritty_terminal",
	version: "0.26.0",
	diff:    "testdata/rsdump/patches/alacritty_terminal-0.26.0-decstbm-top-clamp.diff",
	defect: "DECSTBM clamps the region's top to screen_lines rather than to the last line, so " +
		"a top at or below the last line yields an empty region and scroll_up_relative's " +
		"min(lines, end-start) is 0 from then on: the screen never scrolls again",
}}

// crateTarget is every patch that one published crate needs, gathered because
// [patch.crates-io] can name a crate once: a crate with two defects has to be
// served from a single directory holding both fixes, not from one directory per
// defect. referencePatches stays one entry per defect, which is what the argument
// for each is attached to.
type crateTarget struct {
	crate, version string
	patches        []referencePatch
}

// crateTargets groups referencePatches by the crate they patch, in the order the
// crates first appear, so that the digest a directory is keyed by is stable.
//
// It groups by crate wherever the entries are rather than only where they are
// adjacent, because where they are is how they get declared: a defect is added to
// the end of the list, and grouping that only noticed neighbours would name its
// crate in the block twice.
func crateTargets() []crateTarget {
	var out []crateTarget
	at := map[string]int{}
	for _, p := range referencePatches {
		key := p.crate + " " + p.version
		if i, ok := at[key]; ok {
			out[i].patches = append(out[i].patches, p)
			continue
		}
		at[key] = len(out)
		out = append(out, crateTarget{crate: p.crate, version: p.version, patches: []referencePatch{p}})
	}
	return out
}

// patchedCrates materialises the patched crates under root and returns the
// [patch.crates-io] block naming them, along with a line per defect identifying
// the reference for the test's output.
//
// Each crate is copied out of cargo's registry and patched in a directory keyed by
// the digest of all of its patches in order, so the stock reference binary is never
// overwritten by a patched build, a changed patch cannot be served from a stale
// copy, and adding a second patch for a crate rebuilds it rather than reusing the
// directory the first one made.
func patchedCrates(t *testing.T, root string) (block, reference string) {
	t.Helper()
	var b, ref strings.Builder
	b.WriteString("[patch.crates-io]\n")
	for _, tg := range crateTargets() {
		diffs := make([][]byte, len(tg.patches))
		sum := sha256.New()
		for i, p := range tg.patches {
			diffs[i] = mustRead(t, p.diff)
			sum.Write(diffs[i])
		}
		digest := fmt.Sprintf("%x", sum.Sum(nil))
		dir := filepath.Join(root, "crates", tg.crate+"-"+tg.version+"-"+digest[:12])
		stamp := filepath.Join(dir, ".shelld-patched")
		if _, err := os.Stat(stamp); err != nil {
			src := registryCrate(t, tg.crate, tg.version)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			// The registry copy is read-only, so -a's mode preservation has to
			// be undone before patch can write.
			if out, err := exec.Command("cp", "-a", src, dir).CombinedOutput(); err != nil {
				t.Fatalf("copying %s: %v\n%s", src, err, out)
			}
			if out, err := exec.Command("chmod", "-R", "u+w", dir).CombinedOutput(); err != nil {
				t.Fatalf("chmod %s: %v\n%s", dir, err, out)
			}
			for i, p := range tg.patches {
				cmd := exec.Command("patch", "-p1", "-d", dir)
				cmd.Stdin = bytes.NewReader(diffs[i])
				// patch writes a temporary file, so it needs somewhere writable to
				// put it rather than wherever the caller's environment points.
				cmd.Env = append(os.Environ(), "TMPDIR="+root)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("applying %s to %s: %v\n%s", p.diff, src, err, out)
				}
			}
			if err := os.WriteFile(stamp, []byte(digest), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&b, "%s = { path = %q }\n", tg.crate, dir)
		for _, p := range tg.patches {
			fmt.Fprintf(&ref, "\n  %s %s + %s (%s): %s",
				tg.crate, tg.version, filepath.Base(p.diff), digest[:12], p.defect)
		}
	}
	return b.String(), ref.String()
}

// registryCrate is where cargo unpacked a published crate. Building against the
// registry copy is what makes a patch a patch: the reference is the published
// crate plus a named diff, not a checkout of something else.
func registryCrate(t *testing.T, crate, version string) string {
	t.Helper()
	dir, pattern := unpackedCrate(t, crate, version)
	if dir == "" {
		t.Fatalf("no unpacked %s %s under %s: run the differential once against stock "+
			"crates (SHELLD_VTDIFF_STOCK=1) to populate the registry", crate, version, pattern)
	}
	return dir
}

// unpackedCrate is where cargo has unpacked crate at version, with the glob it
// looked under so a caller can name it; dir is empty when nothing matches.
//
// Split out from registryCrate so the absence can be answered two ways. For the
// differential, which the caller asked for with SHELLD_VTDIFF=1, an unpopulated
// registry is a failure. For a test that only inspects what the patches produce
// and runs unasked, it is a prerequisite that machine does not have, and
// reporting that as a failure makes a machine with no Rust work on it look
// broken -- which is how TestAPatchedCrateHoldsEveryPatchDeclaredForIt read the
// first time this suite ran on one.
func unpackedCrate(t *testing.T, crate, version string) (dir, pattern string) {
	t.Helper()
	home := os.Getenv("CARGO_HOME")
	if home == "" {
		d, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(d, ".cargo")
	}
	pattern = filepath.Join(home, "registry", "src", "*", crate+"-"+version)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", pattern
	}
	return matches[0], pattern
}

func buildRsdump(t *testing.T) string {
	t.Helper()
	if os.Getenv("SHELLD_VTDIFF") == "" {
		t.Skip("set SHELLD_VTDIFF=1 to run the Rust differential test")
	}
	rsDir, err := filepath.Abs(os.Getenv("SHELLD_RS_DIR"))
	if os.Getenv("SHELLD_RS_DIR") == "" || err != nil {
		t.Fatal("set SHELLD_RS_DIR to a shelld-rs checkout")
	}
	if _, err := os.Stat(filepath.Join(rsDir, "crates", "shelld-core", "Cargo.toml")); err != nil {
		t.Fatalf("SHELLD_RS_DIR: %v", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cache, "shelld-go-rsdump")
	patches, patched := "", ""
	if os.Getenv("SHELLD_VTDIFF_STOCK") == "" {
		// A patched reference gets its own target directory: the stock binary is
		// the one the round's published numbers were measured against.
		root := target + "-patched"
		patches, patched = patchedCrates(t, root)
		target = root
	}
	// A differential test that silently patches its oracle is worth little. Say
	// which reference this run compared against, so a green or a red can be read
	// against the right one.
	t.Logf("reference: shelld-rs %s%s", rsDir, cmp.Or(patched, ", stock crates"))
	// Stage the crate with its manifest pointing at SHELLD_RS_DIR.
	src := filepath.Join(target, "src-crate")
	if err := os.MkdirAll(filepath.Join(src, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile("testdata/rsdump/Cargo.toml.in")
	if err != nil {
		t.Fatal(err)
	}
	manifest := bytes.ReplaceAll(tmpl, []byte("@SHELLD_RS_DIR@"), []byte(rsDir))
	manifest = bytes.ReplaceAll(manifest, []byte("@REFERENCE_PATCHES@\n"), []byte(patches))
	for name, data := range map[string][]byte{
		"Cargo.toml":  manifest,
		"Cargo.lock":  mustRead(t, "testdata/rsdump/Cargo.lock"),
		"src/main.rs": mustRead(t, "testdata/rsdump/src/main.rs"),
	} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("cargo", "build", "--offline", "--release", "-q", "--manifest-path", filepath.Join(src, "Cargo.toml"))
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building rsdump: %v\n%s", err, out)
	}
	return filepath.Join(target, "release", "rsdump")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runRust(t *testing.T, bin string, stream []byte) string {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rsdump: %v\n%s", err, stderr.String())
	}
	return string(out)
}

func compare(t *testing.T, name string, stream []byte, bin string) {
	t.Helper()
	want := runRust(t, bin, stream)
	got := runGo(stream)
	if want == got {
		return
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(wl), len(gl)); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			dir := t.TempDir()
			p := filepath.Join(dir, "ops.bin")
			_ = os.WriteFile(p, stream, 0o644)
			t.Fatalf("%s: divergence at output line %d (stream saved to %s)\nrust: %s\ngo:   %s", name, i+1, p, trunc(w), trunc(g))
		}
	}
}

func trunc(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

// probe appends a mix of render/diff operations exercising every variant.
func probe(o *ops, rng *rand.Rand, cols, rows int) {
	o.dump()
	o.diff(0, 0, cols, rows)
	o.diff(1, rng.Intn(40), cols, rows)
	c, r := 2+rng.Intn(cols+20), 1+rng.Intn(rows+10)
	o.diff(2, 0, c, r)
	o.diff(3, rng.Intn(60), c, r)
	o.frameAt(rng.Intn(200))
	o.cropped(c, r)
	o.osc()
}

// fuzzChunk generates escape-sequence-heavy terminal output.
func fuzzChunk(rng *rand.Rand) []byte {
	var b bytes.Buffer
	n := 1 + rng.Intn(60)
	num := func() string {
		switch rng.Intn(6) {
		case 0:
			return ""
		case 1:
			return "0"
		case 2:
			return fmt.Sprint(rng.Intn(3000))
		case 3:
			return "65535"
		default:
			return fmt.Sprint(1 + rng.Intn(30))
		}
	}
	for i := 0; i < n; i++ {
		switch rng.Intn(40) {
		case 0, 1, 2, 3, 4, 5, 6, 7:
			words := []string{"hello", "world ", "❯ ", "中文字", "🙂", "á", "é̂", "‍", "️", "x", "   ", "ok", "▶", "⏺", "៘", "𝒳"}
			for k := rng.Intn(8); k >= 0; k-- {
				b.WriteString(words[rng.Intn(len(words))])
			}
		case 8:
			b.WriteString([]string{"\r\n", "\n", "\r", "\t", "\b", "\x0b", "\x0c", "\x0e", "\x0f", "\x07", "\x00", "\x7f"}[rng.Intn(12)])
		case 9, 10, 11:
			fmt.Fprintf(&b, "\x1b[%s%s", num(), string("ABCDEFGHIJKLMPSTXZ@abdeghI`"[rng.Intn(27)]))
		case 12:
			fmt.Fprintf(&b, "\x1b[%s;%sH", num(), num())
		case 13:
			fmt.Fprintf(&b, "\x1b[%s;%sr", num(), num())
		case 14:
			modes := []string{"1049", "25", "7", "6", "1000", "1002", "1003", "1006", "1005", "2004", "3", "12", "47", "1047", "1004", "2026"}
			fmt.Fprintf(&b, "\x1b[?%s%s", modes[rng.Intn(len(modes))], string("hl"[rng.Intn(2)]))
		case 15:
			fmt.Fprintf(&b, "\x1b[%s%s", []string{"4", "20", "4;20"}[rng.Intn(3)], string("hl"[rng.Intn(2)]))
		case 16, 17:
			sgrs := []string{"0", "1", "2", "3", "4", "4:3", "4:0", "4:2:1", "5", "7", "8", "9", "21", "22", "23", "24", "27", "28", "29",
				"31", "42", "39", "49", "38;5;123", "48;2;10;20;30", "38:2:1:2:3", "38:2::4:5:6", "58;5;9", "58:2:1:2:3", "59", "95", "104", "38;5;300", "38;2;1", "4:4", "4:5", "48;5;200"}
			parts := []string{}
			for k := rng.Intn(4); k >= 0; k-- {
				parts = append(parts, sgrs[rng.Intn(len(sgrs))])
			}
			fmt.Fprintf(&b, "\x1b[%sm", strings.Join(parts, ";"))
		case 18:
			b.WriteString([]string{"\x1b7", "\x1b8", "\x1bM", "\x1bD", "\x1bE", "\x1bH", "\x1b#8", "\x1b(0", "\x1b(B", "\x1b)0", "\x1b=", "\x1b>", "\x1b[s", "\x1b[u", "\x1bZ", "\x1b[c", "\x1b[>c", "\x1b[6n", "\x1b[5n", "\x1b[?25$p", "\x1b[4$p", "\x1b[18t", "\x1b[3 q", "\x1b[0 q"}[rng.Intn(24)])
		case 19:
			if rng.Intn(10) == 0 {
				b.WriteString("\x1bc")
			}
		case 20:
			b.WriteString([]string{"\x1b]11;?\x07", "\x1b]10;?\x1b\\", "\x1b]12;?\x07", "\x1b]4;1;?;2;?\x07", "\x1b]10;?;?;?\x07", "\x1b]0;title\x07", "\x1b]8;;http://x\x1b\\", "\x1b]52;c;aGk=\x07", "\x1b]4;300;?\x07"}[rng.Intn(9)])
		case 21:
			b.WriteString([]string{"\x1bP1$qm\x1b\\", "\x1bPq#0;2;0;0;0\x1b\\", "\x1b_apc\x1b\\", "\x1bX sos \x1b\\"}[rng.Intn(4)])
		case 22:
			// invalid / split UTF-8
			b.Write([]byte{[]byte{0x80, 0x9b, 0xc3, 0xe2, 0xf0, 0xff, 0xed, 0xa0, 0xe0}[rng.Intn(9)]})
			if rng.Intn(2) == 0 {
				k := rng.Intn(3)
				b.Write([]byte{0x82, 0xa9, 0x28}[k : k+1])
			}
		case 23:
			b.WriteString("\x1b[?2026h")
		case 24:
			b.WriteString("\x1b[?2026l")
		case 25:
			fmt.Fprintf(&b, "\x1b[%s;%s;%sm", num(), num(), num())
		case 26:
			fmt.Fprintf(&b, "\x1b[%sJ", []string{"", "0", "1", "2", "3"}[rng.Intn(5)])
		case 27:
			fmt.Fprintf(&b, "\x1b[%sK", []string{"", "0", "1", "2"}[rng.Intn(4)])
		case 28:
			fmt.Fprintf(&b, "\x1b[%sg", []string{"", "0", "3"}[rng.Intn(3)])
		case 29:
			// long lines to force wrapping
			b.WriteString(strings.Repeat(string("abc中"[rng.Intn(4)]), rng.Intn(300)))
		case 30:
			fmt.Fprintf(&b, "\x1b[%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%s;%sm", num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num(), num())
		case 31:
			fmt.Fprintf(&b, "\x1b[%s%s", string("<=>?"[rng.Intn(4)]), num())
			b.WriteByte("hlmpucq"[rng.Intn(7)])
		case 32:
			b.WriteString(strings.Repeat("line of text\r\n", rng.Intn(40)))
		case 33:
			fmt.Fprintf(&b, "\x1b[%s b", num())
		case 34, 35:
			b.Write(csiChunk(rng))
		default:
			b.WriteString("text ")
		}
	}
	return b.Bytes()
}

// csiChunk generates CSI shapes the fixed lists in fuzzChunk miss: SGR with
// random parameter and colon subparameter lists, truncated and out-of-range
// colors, values past 65535, private markers and intermediates on assorted
// final bytes, controls inside a sequence, and mixed synchronized-update mode
// lists. (Contributed by a review of the parser proposal, so the shapes were
// chosen by someone who had not written fuzzChunk's lists.)
func csiChunk(rng *rand.Rand) []byte {
	var b bytes.Buffer
	num := func() string {
		switch rng.Intn(10) {
		case 0:
			return ""
		case 1:
			return "0"
		case 2:
			return strconv.Itoa(rng.Intn(300))
		case 3:
			return []string{"65535", "65536", "99999", "655350", "0000000000012", "4294967297"}[rng.Intn(6)]
		case 4:
			return strconv.Itoa(250 + rng.Intn(10))
		default:
			return strconv.Itoa(rng.Intn(10))
		}
	}
	sgrWords := []string{"38", "48", "58", "2", "5", "4", "1", "0", "39", "49", "59", "7", "22", "24"}
	param := func() string {
		// A parameter with 0-6 colon subparameters.
		var parts []string
		switch rng.Intn(3) {
		case 0:
			parts = append(parts, sgrWords[rng.Intn(len(sgrWords))])
		default:
			parts = append(parts, num())
		}
		if rng.Intn(3) == 0 {
			for k := rng.Intn(7); k > 0; k-- {
				if rng.Intn(3) == 0 {
					parts = append(parts, sgrWords[rng.Intn(len(sgrWords))])
				} else {
					parts = append(parts, num())
				}
			}
		}
		return strings.Join(parts, ":")
	}
	n := 1 + rng.Intn(40)
	for i := 0; i < n; i++ {
		switch rng.Intn(12) {
		case 0:
			b.WriteString([]string{"x", "hello ", "中", "\r\n", " "}[rng.Intn(5)])
		case 1, 2, 3, 4:
			// SGR with a random parameter list (0..40 params)
			var ps []string
			cnt := rng.Intn(8)
			if rng.Intn(5) == 0 {
				cnt = rng.Intn(41)
			}
			for k := cnt; k > 0; k-- {
				ps = append(ps, param())
			}
			b.WriteString("\x1b[" + strings.Join(ps, ";") + "m")
		case 5:
			// semicolon colors, sometimes truncated or out of range, then attributes
			kind := []string{"38", "48", "58"}[rng.Intn(3)]
			sub := []string{"2", "5", "7", ""}[rng.Intn(4)]
			var ps []string
			for k := rng.Intn(5); k > 0; k-- {
				ps = append(ps, num())
			}
			b.WriteString("\x1b[" + strings.Join(append([]string{kind, sub}, ps...), ";") + ";" + num() + "m")
		case 6:
			// colon colors with colorspace ids and odd lengths
			kind := []string{"38", "48", "58", "4"}[rng.Intn(4)]
			var ps []string
			for k := rng.Intn(8); k > 0; k-- {
				ps = append(ps, num())
			}
			b.WriteString("\x1b[" + kind + ":" + []string{"2", "5", "3", ""}[rng.Intn(4)] + ":" + strings.Join(ps, ":") + "m")
		case 7:
			// private markers / intermediates on assorted finals
			pre := []string{"", "?", ">", "<", "=", "?$"}[rng.Intn(6)]
			mid := []string{"", "$", " ", "!", "\"", "$$", " $", "#%", "'"}[rng.Intn(9)]
			fin := "@ABCDEFGHIJKLMPSTXZabcdefghlmnpqrstu`"[rng.Intn(37)]
			var ps []string
			for k := rng.Intn(4); k > 0; k-- {
				ps = append(ps, param())
			}
			b.WriteString("\x1b[" + pre + strings.Join(ps, ";") + mid + string(fin))
		case 8:
			// cursor moves, erases, modes with odd params
			fmt.Fprintf(&b, "\x1b[%s;%s%c", param(), param(), "HfrJKXPL@"[rng.Intn(9)])
		case 9:
			b.WriteString([]string{"\x1b[?2026h", "\x1b[?2026l", "\x1b[?25;2026h", "\x1b[?2026;25l", "\x1b[?2026$p", "\x1b[?1049h", "\x1b[?1049l", "\x1b[4h", "\x1b[4l", "\x1b[?7l", "\x1b[?7h", "\x1b[c", "\x1b[>c", "\x1b[6n", "\x1b[18t", "\x1b[?2026", "\x1b["}[rng.Intn(17)])
		case 10:
			// C0 or ESC inside a CSI, CAN/SUB aborts
			fmt.Fprintf(&b, "\x1b[%s%c%s%s", num(), []byte{0x08, 0x0a, 0x18, 0x1a, 0x1b, 0x7f, 0x0d}[rng.Intn(7)], num(), "mH"[rng.Intn(2):][:1])
		default:
			b.WriteString("text ")
		}
	}
	return b.Bytes()
}

// referencePartialUTF8FixState logs whether the reference carries the
// advance_partial_utf8 fix.
//
// vte 0.15.0 completes a rune held across a write boundary, prints that one rune,
// and then reports every well-formed byte in its 4-byte buffer as consumed — so an
// ASCII byte sitting between the printed rune and the start of the next multi-byte
// one is skipped without ever being parsed. It swallows a space, a tab, or a
// newline, in which case the following rune lands on the wrong row. The Go port
// fixes it; an unpatched reference does not, so the two disagree.
//
// Measured end to end against an unpatched reference, and the two tests differ:
// TestVtDiffFuzz diverges on 14 of its 400 seeds, the first at seed 14, so a full run
// fails rather than fails sometimes. A replay of 127 recorded sessions passed —
// 0 of 127 — because it seeded each recording from its own index, and the write boundaries that fixes produces
// happen not to land inside a multi-byte rune with two more bytes behind it. That is
// luck, not immunity: swept fix-against-revert in Go, 4 of 256 seed offsets diverge, so
// a change to the seeding or the corpus turns this into a 1.6% flake whose failure will
// read as a port bug. Probing more often does not help — over 8128 trajectories every
// divergence that appeared at a probe also appeared in the final grid, none only in
// between.
//
// Both counts are a (Go tree, reference checkout) pair, not a property of the fix: a
// tree carrying other port fixes, or a SHELLD_RS_DIR whose shelld-rs has merged some of
// them, moves them. Quote them with both sides named, and use the probe below for the
// one axis it settles.
//
// And note how such a count has to be taken, because two sessions got this wrong in
// opposite directions: compare() ends in t.Fatalf, so TestVtDiffFuzz's loop dies at its
// FIRST diverging seed and one run can only ever name one. "N of 400" from an unmodified
// run is a first-divergence index, not a population. The 14 above came from a harness
// that collects instead of failing (kept out of the repo so a second copy of this
// loop cannot drift from it), and on this tree
// the population is 14 while the first diverging seed is also 14 — which is exactly what
// makes the mistake invisible.
//
// Reverting just this fix takes the fuzz count to 0 of 400, so every one of those 14 is
// this and none of them is some other divergence riding along.
//
// The tempting response is a whitelist, and it is the wrong one: the reference is the
// side that is wrong, and vte's own comment above the line says so ("we just ignore
// the rest" — the rest is not ignored, it is skipped).
// Patch: testdata/rsdump/patches/vte-0.15.0-advance-partial-utf8.diff, which
// patchedCrates applies, so on this tree the probe below should report that the
// reference carries the fix. It saying otherwise means the reference was not patched
// — a stale target dir, or SHELLD_VTDIFF_STOCK=1. Do NOT whitelist.
//
// The probe asks for the property the fix defines rather than for a known dump, so
// it cannot rot as the dump format changes: a write boundary inside a rune must not
// change the result. "· ↑X" is the shortest stream that reaches the arm — the buffer
// fills to C2 B7 20 E2 and ends mid-sequence. Note that uniform chunking does not
// reach it at all, including one byte at a time, since that never leaves the buffer
// holding a complete rune plus the start of another.
func referencePartialUTF8FixState(t *testing.T, bin string) {
	t.Helper()
	stream := func(chunks ...[]byte) []byte {
		o := newOps(20, 3)
		for _, c := range chunks {
			o.feed(c)
		}
		o.dump()
		return o.Bytes()
	}
	split := runRust(t, bin, stream([]byte{0xc2}, []byte{0xb7, 0x20, 0xe2, 0x86, 0x91, 0x58}))
	whole := runRust(t, bin, stream([]byte{0xc2, 0xb7, 0x20, 0xe2, 0x86, 0x91, 0x58}))
	if split == whole {
		t.Logf("reference carries the advance_partial_utf8 fix (probe: split write == single write)")
		return
	}
	t.Logf("reference does NOT carry the advance_partial_utf8 fix: splitting \"· ↑X\" after "+
		"its first byte drops the space, so a split write and a single write disagree. "+
		"Any divergence below is that and not the Go side being wrong: 14 of "+
		"TestVtDiffFuzz's 400 seeds, from seed 14 on. Patch: "+
		"testdata/rsdump/patches/vte-0.15.0-advance-partial-utf8.diff, which this tree "+
		"applies through patchedCrates — seeing this line means it did not reach the "+
		"build (stale target dir, or SHELLD_VTDIFF_STOCK=1). Do NOT whitelist."+
		"\nsplit:  %s\nsingle: %s",
		trunc(split), trunc(whole))
}

func TestVtDiffFuzz(t *testing.T) {
	bin := buildRsdump(t)
	referencePartialUTF8FixState(t, bin)
	seeds := 400
	if v := os.Getenv("SHELLD_VTDIFF_SEEDS"); v != "" {
		fmt.Sscan(v, &seeds)
	}
	if testing.Short() {
		seeds = 40
	}
	for seed := 1; seed <= seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		cols, rows := 2+rng.Intn(120), 1+rng.Intn(40)
		o := newOps(cols, rows)
		o.setColor(IdxBackground, 0x1e, 0x1e, 0x2e)
		o.setColor(IdxForeground, 0xc0, 0xc0, 0xc0)
		o.setColor(1, 0xff, 0, 0)
		steps := 20 + rng.Intn(60)
		for s := 0; s < steps; s++ {
			data := fuzzChunk(rng)
			// Feed in random splits so partial UTF-8 and sync buffering see
			// every boundary.
			for len(data) > 0 {
				k := 1 + rng.Intn(len(data))
				o.feed(data[:k])
				data = data[k:]
			}
			if rng.Intn(8) == 0 {
				cols, rows = 2+rng.Intn(150), 1+rng.Intn(50)
				o.resize(cols, rows)
			}
			if rng.Intn(3) == 0 {
				probe(o, rng, cols, rows)
			}
		}
		probe(o, rng, cols, rows)
		compare(t, fmt.Sprintf("fuzz seed %d", seed), o.Bytes(), bin)
	}
}
