// Package uniwidth reports how many terminal cells a rune occupies.
//
// The table is generated from the Rust unicode-width 0.2.2 crate — the exact
// version alacritty_terminal 0.26 uses — so a Go terminal emulator built on
// this package lays out text identically to shelld-rs, codepoint for
// codepoint.
package uniwidth

//go:generate sh -c "cargo run --quiet --release --manifest-path gen/Cargo.toml > table.go && gofmt -w table.go"

// None is returned by Width for runes that have no printable width
// (C0/C1 control characters and DEL).
const None = -1

// khmerBeyyal (U+17D8) is the only scalar unicode-width 0.2.2 reports as three
// cells wide. The packed table reserves its 2-bit value 3 for None, so this
// one codepoint is special-cased (it is stored as None in the table).
const khmerBeyyal = 0x17d8

// Width returns the cell width of r: 0 (combining / zero-width), 1, 2 (wide /
// fullwidth / emoji presentation), 3 (U+17D8, the single codepoint
// unicode-width assigns three cells), or None for control characters.
func Width(r rune) int {
	if r >= 0x20 && r < 0x7f {
		return 1
	}
	if r < 0 || r > 0x10ffff {
		return 1
	}
	if r == khmerBeyyal {
		return 3
	}
	blk := int(stage1[r>>8])
	lo := int(r & 0xff)
	v := (stage2[blk*64+(lo>>2)] >> (uint(lo&3) * 2)) & 3
	if v == 3 {
		return None
	}
	return int(v)
}
