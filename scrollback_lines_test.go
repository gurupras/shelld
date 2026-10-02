package shelld

import (
	"strings"
	"testing"
)

// A PTY read ends wherever the kernel's buffer does, not at a line break, so a
// line is often delivered across several writes.
func TestScrollbackLineSpansWrites(t *testing.T) {
	line := strings.Repeat("x", 9000)
	b := NewScrollback(100)
	data := []byte(line + "\r\nnext\r\n")
	for off := 0; off < len(data); off += 4096 {
		b.Write(data[off:min(off+4096, len(data))])
	}
	if b.Len() != 2 || b.TotalLines() != 2 || b.LastN(2) != line+"\nnext" {
		t.Fatalf("got %d lines (total %d), lengths %v", b.Len(), b.TotalLines(), lineLens(b.LastN(10)))
	}
}

func lineLens(s string) []int {
	var n []int
	for _, l := range strings.Split(s, "\n") {
		n = append(n, len(l))
	}
	return n
}
