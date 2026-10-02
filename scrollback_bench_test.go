package shelld

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// benchWrites is the shapes of output the store has to be fast for, in the
// 4 KiB reads the PTY hands the daemon rather than in whole lines, because that
// is what Window.readLoop calls Write with.
func benchWrites() []struct {
	name string
	data []byte
} {
	var short, wide, bar, none []byte
	for i := range 4096 {
		short = append(short, fmt.Sprintf("[%04d] a line of ordinary build output\n", i)...)
	}
	for range 256 {
		wide = append(wide, bytes.Repeat([]byte("x"), 1<<12)...)
		wide = append(wide, '\n')
	}
	for i := range 8192 {
		bar = append(bar, fmt.Sprintf("\r  %3d%% [%-20s]", i%101, bytes.Repeat([]byte("#"), i%21))...)
	}
	// A full-screen redraw addresses the cursor and never writes '\n', so the
	// whole of it is one line until the cap cuts it.
	for range 4096 {
		none = append(none, "\x1b[H\x1b[2Kredrawn row of a full-screen application"...)
	}
	return []struct {
		name string
		data []byte
	}{
		{"short-lines", short},
		{"lines-of-4k", wide},
		{"progress-bar", bar},
		{"no-newline", none},
	}
}

// Write is on the PTY read path, so it runs for every byte a window produces.
// Reported per byte of output, which is the rate that matters and is
// comparable across the shapes.
func BenchmarkScrollbackWrite(b *testing.B) {
	for _, w := range benchWrites() {
		b.Run(w.name, func(b *testing.B) {
			b.SetBytes(int64(len(w.data)))
			b.ReportAllocs()
			s := NewScrollback(defaultScrollbackCapacity)
			// Fill history first: the steady state is what a long-lived window
			// spends its time in, and it is the one where storing a line must
			// not allocate.
			for range 2 {
				for off := 0; off < len(w.data); off += 4096 {
					s.Write(w.data[off:min(off+4096, len(w.data))])
				}
			}
			b.ResetTimer()
			for range b.N {
				for off := 0; off < len(w.data); off += 4096 {
					s.Write(w.data[off:min(off+4096, len(w.data))])
				}
			}
		})
	}
}

// The reads Window.Capture and idle detection make, over a full history.
func BenchmarkScrollbackRead(b *testing.B) {
	s := NewScrollback(defaultScrollbackCapacity)
	var line []byte
	for i := range 20_000 {
		line = append(line[:0], fmt.Sprintf("[%05d] a line of ordinary build output\n", i)...)
		s.Write(line)
	}
	for _, n := range []int{1, 100, 10_000} {
		b.Run(fmt.Sprint("last-", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				sinkString = s.LastN(n)
			}
		})
	}
}

// The bounded reads are a different measurement from BenchmarkScrollbackRead:
// LastN validates the joined payload once, LastNBytes measures every candidate
// line first and then joins what fit, so it is the path /capture and /scroll
// take and the one a per-line measurement can make slower.
//
// Both histories are run because the two arms of the measurement are different
// work. A valid history answers "is this valid?" for every line and then must
// not ask again about the join; an invalid one cannot skip anything and pays a
// validity scan before the byte loop that rebuilds it, so it is where a fast
// path for the valid case shows up as a cost rather than a saving.
func BenchmarkScrollbackReadBounded(b *testing.B) {
	build := func(mangle bool) *Scrollback {
		s := NewScrollback(defaultScrollbackCapacity)
		var line []byte
		for i := range 20_000 {
			line = append(line[:0], fmt.Sprintf("[%05d] a line of ordinary build output", i)...)
			// A stray byte in one line of eight: a run of bytes the store keeps
			// and Lossy rewrites, which is what output from a program writing a
			// non-UTF-8 locale's bytes looks like.
			if mangle && i%8 == 0 {
				line = append(line, 0xff, 0xfe)
			}
			s.Write(append(line, '\n'))
		}
		// The premise the two arms rest on: they have to be different work. The
		// text is valid either way — that is what Lossy is for — so what
		// separates them is whether the read replaced anything, and if the
		// mangled history came back untouched the "invalid" numbers would be the
		// valid path measured twice.
		text, _, _, _ := s.LastNBytes(20_000, 1<<30)
		if strings.ContainsRune(text, '�') != mangle {
			b.Fatalf("history mangle=%v reads back with replacements=%v; the arms are the same work",
				mangle, !mangle)
		}
		return s
	}
	for _, h := range []struct {
		name string
		s    *Scrollback
	}{{"valid", build(false)}, {"invalid", build(true)}} {
		for _, n := range []int{1, 100, 10_000} {
			// A bound above what the lines need, so every line is kept and the
			// join is the full payload: the bound's own trimming is a separate
			// path, and this is measuring the measure-then-join one.
			b.Run(fmt.Sprint(h.name, "/last-", n), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					sinkString, _, _, _ = h.s.LastNBytes(n, 1<<30)
				}
			})
		}
	}
}

var sinkString string
