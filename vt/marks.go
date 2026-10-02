package vt

import "unicode/utf8"

// Zero-width characters (combining marks, ZWJ, variation selectors) are kept
// with the cell whose glyph they combine with, as alacritty keeps them in
// CellExtra::zerowidth, so a renderer can draw the whole glyph.
//
// alacritty boxes them per cell. Here a cell holds a 16-bit id, in the two
// bytes of padding Cell already had, into a table of mark sequences owned by
// the Term; an unmarked cell holds 0 and costs nothing. Ids are interned, so
// every "é" on the screen shares one entry, and never reused for a different
// sequence, so a snapshot taken earlier still resolves its ids after later
// output. Entries are immutable once added, which is what lets a reader holding
// a copy of the table's slice header (MarkTable) read it while the Term
// appends.
//
// Both bounds are this package's, not the reference's, which has neither
// (see the package doc):
//
//   - maxCellBytes of UTF-8 per cell, the base character and its marks
//     together: a mark that would take the cell past it is dropped. This is
//     tmux's bound (UTF8_SIZE, tmux.h, checked in screen_write_combine), and
//     shelld-rs applies the same one. The longest real cluster a cell gets, a
//     subdivision flag (a base and six tag characters), is 28 bytes. Interning
//     a sequence of n marks interns its n prefixes too, so without a bound one
//     long run would cost O(n²).
//   - maxMarkSequences distinct sequences per Term: once full, a mark that
//     would need a new entry is dropped. Real text needs few (one per accented
//     letter in use); the bound is for output built to exhaust it.
//
// A dropped mark still sets FlagZerowidth, so whether a cell counts as empty
// matches the reference either way; only the drawing loses the mark.
const (
	maxCellBytes     = 32
	maxMarkSequences = 4096
)

type markTable struct {
	seqs [][]rune // seqs[0] is the empty sequence: id 0 is "no marks"
	ids  map[string]uint16
}

// push returns the id of sequence id followed by r, on a cell whose character
// is base, adding it if needed, or id unchanged when a bound refuses it.
func (m *markTable) push(base rune, id uint16, r rune) uint16 {
	if m.seqs == nil {
		m.seqs = [][]rune{nil}
		m.ids = make(map[string]uint16)
	}
	prev := m.seqs[id]
	n := utf8.RuneLen(base) + utf8.RuneLen(r)
	for _, p := range prev {
		n += utf8.RuneLen(p)
	}
	if n > maxCellBytes {
		return id
	}
	seq := make([]rune, len(prev)+1)
	copy(seq, prev)
	seq[len(prev)] = r
	key := string(seq)
	if next, ok := m.ids[key]; ok {
		return next
	}
	if len(m.seqs) > maxMarkSequences {
		return id
	}
	next := uint16(len(m.seqs))
	m.seqs = append(m.seqs, seq)
	m.ids[key] = next
	return next
}

// MarkID is the cell's zero-width sequence, 0 when it has none; Term.Marks
// resolves it. A cell can carry FlagZerowidth with id 0, when a bound dropped
// its marks.
func (c *Cell) MarkID() uint16 { return c.zw }

// Marks returns the zero-width characters of mark sequence id, nil for 0. The
// slice must not be modified.
func (t *Term) Marks(id uint16) []rune {
	if int(id) >= len(t.marks.seqs) {
		return nil
	}
	return t.marks.seqs[id]
}

// MarkTable returns the mark sequences by id, for resolving ids without the
// Term (a copy of the screen taken earlier, read without the lock that
// guards the Term). Entries never change, so the returned slice stays valid
// while the Term goes on adding entries past its length. It must not be
// modified.
func (t *Term) MarkTable() [][]rune {
	if t.marks.seqs == nil {
		return [][]rune{nil}
	}
	return t.marks.seqs
}
