// Package vt is a terminal emulator core: a VT500-series escape-sequence
// parser driving a scrollback-backed cell grid.
//
// It is a behavioral port of the Rust crates shelld-rs is built on —
// alacritty_terminal 0.26 (Term, Grid, reflowing resize) and vte 0.15 (the
// Paul Williams state machine plus the alacritty ANSI handler) — so the grid
// a Go program observes after feeding a byte stream matches the grid
// alacritty would hold, cell for cell. Rendering, selection, vi mode, damage
// tracking and hyperlink storage are deliberately out of scope: callers read
// the grid directly.
//
// Six sequences deliberately do not match, and each site names the reference's
// own expression so that a later diff against alacritty does not read the
// difference as a transcription slip and undo it. Five are defects in the
// reference; the sixth is a threat model the reference does not have.
//
//   - DECSTBM with a top at or below the last line (term.go, setScrollingRegion).
//     The reference clamps the top to lines (term/mod.rs:2174), yielding the
//     empty region [lines, lines); every later linefeed then scrolls zero lines
//     and the screen never scrolls again.
//   - DCH with a count past the cells remaining after the cursor (term.go,
//     deleteChars). The reference clamps to columns (term/mod.rs:1544), so the
//     trailing fill, which starts at columns-count, blanks cells to the left of
//     the cursor.
//   - ED 1 with the cursor on line 1 (term.go, clearScreen). The reference's
//     guard is cursor.line > 1 (term/mod.rs:1761), which leaves line 0 — the
//     only line above the cursor — unerased.
//   - An 8-bit ST (0x9C) inside SOS, PM, APC or a DCS the parser gave up on
//     (processor.go, advanceIgnoredString). The reference routes those states to
//     its anywhere (lib.rs:175 and :182), whose arms are 0x18 | 0x1A, 0x1B and
//     _ => () (lib.rs:438-450), so the string never ends: every later byte, which
//     is all program output, is discarded until some ESC resynchronizes the parser.
//     Alone among the three defects above, this one carries no referencePatches
//     entry, because the differential test cannot reach it — fuzzChunk emits no
//     bare 0x9C at all (its lone C1 bytes are 0x80 and 0x9B, and its SOS, PM, APC
//     and DCS cases all terminate with ESC \), so the reference would agree with a
//     port that had the defect too. The gap is the generator's, not the patch
//     set's, and it is why this divergence is carried by argument here rather than
//     by a green fuzz run.
//   - A stray 0x80-0x9F byte in ground state (processor.go, advanceGround). vte
//     executes it as an 8-bit C1 control (its ground error path calls execute for
//     a byte <= 0x9F), alacritty implements none, and the byte vanishes. It prints
//     U+FFFD instead, as tmux does and as shelld-rs does with its substitute_c1.
//   - An unterminated OSC (processor.go, maxOSCRaw). vte bounds the payload only
//     in its no_std build; alacritty_terminal asks for std (its Cargo.toml sets
//     features = ["std", "ansi"]), where osc_raw is an unbounded Vec. A daemon
//     holding one parser per window for the life of the process cannot carry
//     that, so the payload is capped and abandoned.
//
// One stored quantity is bounded where the reference's is not, on the same
// threat model as the OSC cap: the zero-width characters a cell keeps. A cell
// holds at most 32 bytes of UTF-8, its base character and marks together, as
// tmux bounds a cell (UTF8_SIZE), and a Term at most 4096 distinct sequences of
// marks; a mark past either bound is dropped, though it still marks the cell as
// having absorbed one (marks.go).
//
// Divergences are held to this list on purpose: a port whose exceptions are
// undocumented cannot be checked against its reference at all.
package vt

import (
	"fmt"
	"strconv"
	"strings"
)

// RGB is a 24-bit color, as carried by an SGR 38;2 / 48;2 sequence or an OSC
// color reply.
type RGB struct{ R, G, B uint8 }

// String formats the color as #rrggbb.
func (c RGB) String() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// NamedColor indexes the terminal color table. 0..15 are the ANSI colors;
// the dynamic colors live above the 256-entry palette, matching alacritty's
// numbering (OSC 10/11/12 address 256/257/258).
type NamedColor uint16

// The sixteen ANSI colors, in the order SGR 30..37 and 90..97 select them.
const (
	Black NamedColor = iota
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White
	BrightBlack
	BrightRed
	BrightGreen
	BrightYellow
	BrightBlue
	BrightMagenta
	BrightCyan
	BrightWhite
)

// The dynamic colors, which sit above the 256-entry palette so a NamedColor
// can name both a palette entry and a role the program may redefine. The grid
// stores the role, not a resolved value: whoever renders it decides what each
// one looks like.
//
// Foreground, Background and CursorColor are what OSC 10, 11 and 12 address.
// Nothing in this package ever writes the rest. They are here because the
// palette they belong to has entries for them and a renderer may want them, not
// because a sequence selects them: SGR 1 and SGR 2 set FlagBold and FlagDim on
// the cell and leave its color alone, so it is the renderer that decides whether
// a bold red cell is drawn in BrightRed and a dim one in DimRed. Only the plain
// bright ANSI colors above are ever selected by a sequence, by SGR 90-97 and
// 100-107.
const (
	Foreground NamedColor = 256 + iota
	Background
	CursorColor
	DimBlack
	DimRed
	DimGreen
	DimYellow
	DimBlue
	DimMagenta
	DimCyan
	DimWhite
	BrightForeground
	DimForeground
)

// String names the color, or formats an unrecognised index as "NamedColor(n)".
func (n NamedColor) String() string {
	if int(n) < len(ansiColorNames) {
		return ansiColorNames[n]
	}
	if i := int(n) - int(Foreground); i >= 0 && i < len(dynamicColorNames) {
		return dynamicColorNames[i]
	}
	return "NamedColor(" + strconv.Itoa(int(n)) + ")"
}

var ansiColorNames = [...]string{
	"Black", "Red", "Green", "Yellow", "Blue", "Magenta", "Cyan", "White",
	"BrightBlack", "BrightRed", "BrightGreen", "BrightYellow",
	"BrightBlue", "BrightMagenta", "BrightCyan", "BrightWhite",
}

var dynamicColorNames = [...]string{
	"Foreground", "Background", "CursorColor",
	"DimBlack", "DimRed", "DimGreen", "DimYellow",
	"DimBlue", "DimMagenta", "DimCyan", "DimWhite",
	"BrightForeground", "DimForeground",
}

// ColorKind discriminates a Color: which of the three payloads it carries, or
// none at all.
type ColorKind uint8

// The color kinds. Each names the accessor that reads a Color of that kind:
// KindNamed goes with Color.Named, KindIndexed with Color.Index, KindRGB with
// Color.RGB. KindNone carries no payload; the color is unset.
const (
	KindNone ColorKind = iota
	KindNamed
	KindIndexed
	KindRGB
)

// String names the kind.
func (k ColorKind) String() string {
	switch k {
	case KindNone:
		return "None"
	case KindNamed:
		return "Named"
	case KindIndexed:
		return "Indexed"
	case KindRGB:
		return "RGB"
	}
	return "ColorKind(" + strconv.Itoa(int(k)) + ")"
}

// Color is a packed tagged union: kind in the top byte, payload below
// (named index, palette index, or 0xRRGGBB). The zero value is "no color",
// used only for an unset underline color.
type Color uint32

// NoColor is the absent color.
const NoColor Color = 0

// NewNamedColor is the color naming a table entry or a dynamic role.
func NewNamedColor(n NamedColor) Color { return Color(uint32(KindNamed)<<24 | uint32(n)) }

// NewIndexedColor is the color at index i of the 256-entry palette.
func NewIndexedColor(i uint8) Color { return Color(uint32(KindIndexed)<<24 | uint32(i)) }

// NewRGBColor is the 24-bit color with the given components.
func NewRGBColor(r, g, b uint8) Color {
	return Color(uint32(KindRGB)<<24 | uint32(r)<<16 | uint32(g)<<8 | uint32(b))
}

// Kind reports which payload the color carries. Read only the accessor that
// goes with it: on a color of another kind the rest return the packed bits
// reinterpreted, not a converted color.
func (c Color) Kind() ColorKind { return ColorKind(c >> 24) }

// Named is the table entry or dynamic role of a KindNamed color.
func (c Color) Named() NamedColor { return NamedColor(c & 0xffff) }

// Index is the palette index of a KindIndexed color.
func (c Color) Index() uint8 { return uint8(c) }

// RGB is the components of a KindRGB color.
func (c Color) RGB() RGB { return RGB{uint8(c >> 16), uint8(c >> 8), uint8(c)} }

// String renders the kind and payload: "Named(Red)", "Indexed(171)",
// "#1e1e2e" or "None" for KindNone. A kind outside the four, which none of the
// constructors can produce, renders as "Color(0x...)" rather than as "None", so
// that it cannot be mistaken for an unset color.
//
// Over every color the constructors can make — all 65536 values NewNamedColor
// accepts, 256 indexed, the RGB cube and NoColor — the result is unique, so it is
// usable as a map key or a comparison key. Most of the named range names nothing;
// those render as "Named(NamedColor(n))", which is still unique. That matters because fmt calls this for %x and %q as well as
// for %v: formatting a Color with %x prints the hex of this string, not the
// packed bits. Convert to uint32 first when the bits are what is wanted.
func (c Color) String() string {
	switch c.Kind() {
	case KindNone:
		return "None"
	case KindNamed:
		return "Named(" + c.Named().String() + ")"
	case KindIndexed:
		return "Indexed(" + strconv.Itoa(int(c.Index())) + ")"
	case KindRGB:
		return c.RGB().String()
	}
	return "Color(0x" + strconv.FormatUint(uint64(c), 16) + ")"
}

// The default colors are the XOR keys of Cell's storage encoding. As
// constants they fold into the accessors instead of being loaded per cell.
const (
	defaultFg = Color(uint32(KindNamed)<<24 | uint32(Foreground))
	defaultBg = Color(uint32(KindNamed)<<24 | uint32(Background))
)

// Flags are per-cell attributes. Bit values match alacritty's cell::Flags.
type Flags uint16

// The cell attributes. Most are the SGR styles a renderer applies directly.
// Four are structural and describe the cell's place in the grid rather than
// its appearance:
//
//   - FlagWrapline: the line continues on the row below because it ran past
//     the last column, not because the program wrote a newline. A reflowing
//     resize joins the two rows back together on this flag alone.
//   - FlagWideChar: the cell holds a double-width character and owns the
//     column to its right, which carries FlagWideCharSpacer.
//   - FlagWideCharSpacer: the right half of a double-width character, or the
//     padding a resize left behind. It has no glyph of its own.
//   - FlagLeadingWideCharSpacer: the last column of a row too narrow to hold
//     a double-width character, which therefore starts on the row below.
//
// See AllUnderlines for the underline styles, which are mutually exclusive in
// practice but stored as independent bits, as alacritty stores them.
const (
	FlagInverse Flags = 1 << iota
	FlagBold
	FlagItalic
	FlagUnderline
	FlagWrapline
	FlagWideChar
	FlagWideCharSpacer
	FlagDim
	FlagHidden
	FlagStrikeout
	FlagLeadingWideCharSpacer
	FlagDoubleUnderline
	FlagUndercurl
	FlagDottedUnderline
	FlagDashedUnderline
	// FlagZerowidth marks a cell that has absorbed zero-width characters,
	// which is what decides whether it counts as empty (reflow trimming,
	// clearing the viewport). The characters themselves are Cell.MarkID's.
	FlagZerowidth
)

// AllUnderlines is every underline style, for testing whether a cell is
// underlined at all without naming the five bits.
const AllUnderlines = FlagUnderline | FlagDoubleUnderline | FlagUndercurl | FlagDottedUnderline | FlagDashedUnderline

// String lists the set bits, separated by "|", or "0" when none are set.
func (f Flags) String() string {
	if f == 0 {
		return "0"
	}
	var b strings.Builder
	for i, name := range flagNames {
		if f&(1<<uint(i)) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(name)
	}
	return b.String()
}

// flagNames is indexed by bit position, so it must stay in the order the
// constants above declare. It covers all sixteen bits of Flags, which is why
// String needs no branch for unnamed ones; widening Flags would change that.
var flagNames = [...]string{
	"Inverse", "Bold", "Italic", "Underline", "Wrapline",
	"WideChar", "WideCharSpacer", "Dim", "Hidden", "Strikeout",
	"LeadingWideCharSpacer", "DoubleUnderline", "Undercurl",
	"DottedUnderline", "DashedUnderline", "Zerowidth",
}

// Cell is one grid cell. Fields are stored so that the zero Cell is the
// default blank cell (space, default foreground/background, no flags); that
// lets fresh rows come straight from zeroed allocations and lets row resets
// compile to memclr.
//
// The character and the two colors pay for that by being stored XOR'd against
// their defaults, which is why every field is unexported and read through an
// accessor: the accessors are where the encoding is undone, and a field read
// directly gives the stored value, which is the real one only for a cell that
// happens to hold the default. Rune, Fg and Bg and their setters are one XOR
// each; UnderlineColor and Flags are stored as they are, so their accessors
// return the field unchanged. All of them inline away.
type Cell struct {
	ch    rune  // character XOR ' '
	fg    Color // foreground XOR defaultFg
	bg    Color // background XOR defaultBg
	ul    Color // underline color, NoColor when unset
	flags Flags
	zw    uint16 // zero-width sequence id, 0 for none; see marks.go
}

// Rune is the character in the cell. A cell that has never been written holds
// a space.
func (c *Cell) Rune() rune { return c.ch ^ ' ' }

// Fg is the cell's foreground color.
func (c *Cell) Fg() Color { return c.fg ^ defaultFg }

// Bg is the cell's background color.
func (c *Cell) Bg() Color { return c.bg ^ defaultBg }

// UnderlineColor is the color of the cell's underline, or NoColor when the
// underline takes the foreground color. Unlike Fg and Bg this is stored
// directly, because "unset" and "default" are the same thing here.
func (c *Cell) UnderlineColor() Color { return c.ul }

// Flags is the cell's attribute set.
func (c *Cell) Flags() Flags { return c.flags }

// SetRune sets the character in the cell.
func (c *Cell) SetRune(r rune) { c.ch = r ^ ' ' }

// SetFg sets the cell's foreground color.
func (c *Cell) SetFg(col Color) { c.fg = col ^ defaultFg }

// SetBg sets the cell's background color.
func (c *Cell) SetBg(col Color) { c.bg = col ^ defaultBg }

// SetFlags replaces the cell's attribute set.
func (c *Cell) SetFlags(f Flags) { c.flags = f }

// isEmpty reports whether the cell holds nothing a reflow or a viewport clear
// has to preserve, mirroring alacritty's GridCell::is_empty. It is not the
// same as equalling the zero Cell: a tab and a space are both empty, and so is
// a cell carrying bold or dim, because neither is visible on a blank.
//
// The color tests read the stored fields rather than Fg and Bg on purpose. A
// stored zero is exactly the default color under the XOR encoding, so `c.bg ==
// 0` is `c.Bg() == NewNamedColor(Background)` with the XOR left out — the same
// test, one instruction shorter, on a predicate the reflow runs per cell.
func (c *Cell) isEmpty() bool {
	ch := c.Rune()
	return (ch == ' ' || ch == '\t') &&
		c.bg == 0 && c.fg == 0 &&
		c.flags&(FlagInverse|AllUnderlines|FlagStrikeout|FlagWrapline|FlagWideCharSpacer|FlagLeadingWideCharSpacer|FlagZerowidth) == 0
}

// clearWide mirrors Cell::clear_wide: drop the wide flag and any zero-width
// content, keep colors and underline color. It runs when something overwrites
// one half of a double-width character, which leaves the other half a normal
// blank rather than a stray half-glyph.
func (c *Cell) clearWide() {
	c.flags &^= FlagWideChar | FlagZerowidth
	c.zw = 0
	c.ch = 0 // stored zero is a space
}
