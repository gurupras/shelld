package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// buildSplitSeed replays TestVtDiffFuzz's generator for one seed and returns
// the op stream, the concatenation of every byte it feeds, and how many chunks
// it split into more than one write. When whole is true each chunk is fed in a
// single write; the split points are drawn either way, so both streams see the
// same generator state and differ only in how the same bytes are written.
//
// Chunks mentioning mode 2026 are dropped from both arms — see
// TestVtGridDoesNotSeeWriteBoundaries for why a synchronized update is the one
// place where a write boundary is allowed to show.
func buildSplitSeed(seed int, whole bool) (stream, fed []byte, splits int) {
	rng := rand.New(rand.NewSource(int64(seed)))
	cols, rows := 2+rng.Intn(120), 1+rng.Intn(40)
	o := newOps(cols, rows)
	o.setColor(IdxBackground, 0x1e, 0x1e, 0x2e)
	o.setColor(IdxForeground, 0xc0, 0xc0, 0xc0)
	o.setColor(1, 0xff, 0, 0)
	var payload bytes.Buffer
	steps := 20 + rng.Intn(60)
	for s := 0; s < steps; s++ {
		data := fuzzChunk(rng)
		if bytes.Contains(data, []byte("2026")) {
			data = nil
		}
		var pieces [][]byte
		for rest := data; len(rest) > 0; {
			k := 1 + rng.Intn(len(rest))
			pieces = append(pieces, rest[:k])
			rest = rest[k:]
		}
		if len(pieces) > 1 {
			splits++
		}
		if whole {
			if len(data) > 0 {
				o.feed(data)
				payload.Write(data)
			}
		} else {
			for _, piece := range pieces {
				o.feed(piece)
				payload.Write(piece)
			}
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
	return o.Bytes(), payload.Bytes(), splits
}

// A PTY read boundary is not a terminal event: the same bytes must land on the
// same screen however they are chopped up. This is TestVtDiffFuzz's corpus with
// the reference removed, so it runs everywhere and a failure is the Go parser's
// own rather than a disagreement about which side is right. It is the property
// that decides a split-consumption bug: when the fuzz differential reddens on a
// split feed, whichever side moves when only the write boundaries move is the
// side that is wrong.
//
// One exception, and it is deliberate. Inside a synchronized update
// (Processor.advanceSyncCSI) the BSU/ESU scan only searches up to
// syncEscapeLen-1 bytes short of the buffer's end, so an update ends a write
// later than the bytes alone would say — vte does the same, and the
// differential holds the port to vte. Chunks mentioning mode 2026 are therefore
// dropped from the fixture rather than asserted over; without that filter this
// property is false on both sides, which is how the exception was found.
func TestVtGridDoesNotSeeWriteBoundaries(t *testing.T) {
	seeds := 400
	if testing.Short() {
		seeds = 40
	}
	var withSplits, withPartialRunes int
	for seed := 1; seed <= seeds; seed++ {
		splitStream, splitFed, splits := buildSplitSeed(seed, false)
		wholeStream, wholeFed, _ := buildSplitSeed(seed, true)
		// Premise: the two op streams differ only in framing. If the split arm
		// stopped splitting, or the generator drifted between the two replays,
		// every seed would agree for a reason that has nothing to do with the
		// parser.
		if !bytes.Equal(splitFed, wholeFed) {
			t.Fatalf("seed %d: premise: the two arms feed different bytes (%d vs %d)",
				seed, len(splitFed), len(wholeFed))
		}
		if splits > 0 {
			withSplits++
			if bytes.Equal(splitStream, wholeStream) {
				t.Fatalf("seed %d: premise: %d chunks were split yet the op streams are identical",
					seed, splits)
			}
		}
		if !utf8.Valid(splitFed) {
			withPartialRunes++
		}
		if got, want := runGo(splitStream), runGo(wholeStream); got != want {
			t.Errorf("seed %d: the screen depends on where the writes were split\n%s",
				seed, firstDumpDiff(got, want))
		}
	}
	// Breadth: a sweep that never split a chunk, or one whose bytes are all
	// whole runes, proves less than it looks. The second is the class this
	// property exists for — a multi-byte rune cut in half by a write boundary.
	if withSplits == 0 {
		t.Fatalf("premise: none of the %d seeds split a chunk across writes", seeds)
	}
	if withPartialRunes == 0 {
		t.Fatalf("premise: none of the %d seeds fed a byte sequence that is not whole runes", seeds)
	}
	t.Logf("%d seeds, %d with a split chunk, %d carrying incomplete runes", seeds, withSplits, withPartialRunes)
}

// firstDumpDiff describes the first differing line of two dumps.
func firstDumpDiff(a, b string) string {
	if a == b {
		return "(identical)"
	}
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(al), len(bl)); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n  split: %s\n  whole: %s", i+1, trunc(x), trunc(y))
		}
	}
	return "line counts differ"
}
