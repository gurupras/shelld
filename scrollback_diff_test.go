package shelld

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/gurupras/shelld/internal/utf8x"
)

// awkwardWrite returns output that stresses line splitting and chunk edges:
// empty lines, lone and doubled CRs, lines of exactly a chunk, lines longer
// than two, invalid UTF-8, and CR and LF split across writes.
func awkwardWrite(rng *rand.Rand) []byte {
	var d []byte
	for range rng.IntN(6) {
		switch rng.IntN(14) {
		case 0:
			d = append(d, '\n')
		case 1:
			d = append(d, '\r')
		case 2:
			d = append(d, "\r\r\n"...)
		case 3:
			d = append(d, bytes.Repeat([]byte{'x'}, sbMaxChunk)...)
		case 4:
			d = append(d, bytes.Repeat([]byte{'y'}, sbMaxChunk-1)...)
		case 5:
			d = append(d, bytes.Repeat([]byte{'z'}, sbMinChunk+1)...)
		case 6:
			d = append(d, 0xff, 0xe2, 0x82)
		case 7:
			d = append(d, "\n\n\n"...)
		case 8:
			d = append(d, bytes.Repeat([]byte{'w'}, 2*sbMaxChunk+rng.IntN(5))...)
		default:
			for range rng.IntN(300) {
				d = append(d, byte(32+rng.IntN(90)))
			}
			if rng.IntN(3) != 0 {
				d = append(d, '\n')
			}
		}
	}
	return d
}

// compareScrollbacks checks a's reads at every argument against the model,
// including the ones past the ends, where a count or an offset is clamped
// rather than rejected.
func compareScrollbacks(t *testing.T, a *Scrollback, m *lineModel) {
	t.Helper()
	checkAgainstModel(t, "final", a, m)
	lines := m.lines[len(m.lines)-a.Len():]
	join := func(ls [][]byte) string { return utf8x.Lossy(bytes.Join(ls, []byte("\n"))) }
	for n := range a.Capacity() + 2 {
		want := ""
		if n > 0 {
			want = join(lines[len(lines)-min(n, len(lines)):])
		}
		if got := a.LastN(n); got != want {
			t.Fatalf("LastN(%d) of %d lines: %d bytes, want %d", n, a.Len(), len(got), len(want))
		}
	}
	for off := range a.Capacity() + 1 {
		want := ""
		if off < len(lines) {
			want = join(lines[off:])
		}
		if got := a.Range(off, a.Capacity()); got != want {
			t.Fatalf("Range(%d, %d) of %d lines: %d bytes, want %d",
				off, a.Capacity(), a.Len(), len(got), len(want))
		}
	}
}

// scrollbackIters scales a randomized scrollback test's iterations down under
// -short and the race detector, which instruments Write's byte scan over lines
// of up to 128 KiB and made these tests most of the root package's race run.
func scrollbackIters(n int) int {
	if testing.Short() || raceDetector {
		return n / 10
	}
	return n
}

func TestScrollbackMatchesPerLineBuffers(t *testing.T) {
	writes := scrollbackIters(3000)
	for _, capacity := range []int{1, 2, 3, 5, 64} {
		for seed := range uint64(4) {
			rng := rand.New(rand.NewPCG(seed, 99))
			a, m := NewScrollback(capacity), newLineModel(capacity+4)
			for i := range writes {
				d := awkwardWrite(rng)
				a.Write(d)
				m.Write(d)
				if i%13 != 0 {
					continue
				}
				tag := fmt.Sprintf("cap %d seed %d write %d", capacity, seed, i)
				checkAgainstModel(t, tag, a, m)
				checkJoinedReads(t, tag, a, m)
			}
			compareScrollbacks(t, a, m)
			if used, live := a.retainedBytes(), a.liveBytes(); used > 2*live+2*sbMaxChunk+sbSpares*sbMaxChunk {
				t.Fatalf("cap %d seed %d: %d bytes held for %d live", capacity, seed, used, live)
			}
		}
	}
}

func FuzzScrollbackMatchesPerLineBuffers(f *testing.F) {
	f.Add([]byte("a\r\nb\n\n\rc"), uint8(3))
	f.Add([]byte("\r\r\n\n\r"), uint8(1))
	f.Add([]byte{0x8f, 'x', 1, '\n', 0x83, 'y', 2, '\r', '\n'}, uint8(40))
	f.Fuzz(func(t *testing.T, data []byte, capb uint8) {
		capacity := int(capb%64) + 1
		a, m := NewScrollback(capacity), newLineModel(capacity+4)
		write := func(d []byte) {
			for len(d) > 0 {
				k := min(len(d), 4096)
				a.Write(d[:k])
				m.Write(d[:k])
				d = d[k:]
			}
		}
		// Start from a history already in full-size chunks, so the fuzz bytes
		// release chunks and reuse spares instead of only filling small chunks.
		line := append(bytes.Repeat([]byte{'p'}, min(48<<10, max(1<<10, (512<<10)/capacity))), '\n')
		for range 2*capacity + 8 {
			write(line)
		}
		for len(data) > 0 {
			c := data[0]
			if c&0x80 != 0 && len(data) > 1 {
				// A byte with the high bit set repeats the next byte 4–128 KiB
				// times, reaching chunk boundaries and lines longer than a chunk.
				write(bytes.Repeat(data[1:2], (int(c&0x1f)+1)<<12))
				data = data[2:]
				continue
			}
			// Otherwise it picks the length of the next write, varying where
			// writes split lines and CRLF pairs.
			k := min(len(data), 1+int(c)%7)
			a.Write(data[:k])
			m.Write(data[:k])
			data = data[k:]
		}
		// Every retained line on its own, which is enough to catch aliasing and
		// much cheaper than compareScrollbacks with long lines.
		checkAgainstModel(t, "fuzz", a, m)
	})
}
