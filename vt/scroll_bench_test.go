package vt

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkScrollHistory is the steady state of a busy window: a full 10,000
// line history at 200 columns, where every line printed pushes one into
// history and evicts the oldest. An op is one line. The arms are what history
// rows are made of: indexed colours, which pack; truecolor on every word, which
// stays plain; and lines longer than the screen, which wrap into full rows.
func BenchmarkScrollHistory(b *testing.B) {
	const lines = 64
	words := strings.Fields("the quick brown fox jumps over the lazy dog while the build runs")
	arms := map[string][]string{}
	for i := range lines {
		var plain, tc strings.Builder
		fmt.Fprintf(&plain, "\x1b[3%dm[%05d]\x1b[0m ", i%8, i)
		for j, w := range words {
			plain.WriteString(w + " ")
			fmt.Fprintf(&tc, "\x1b[38;2;%d;%d;%dm%s ", (i*7+j*13)%256, (i*11+j*3)%256, (i*5+j*17)%256, w)
		}
		arms["plain"] = append(arms["plain"], plain.String()+"\r\n")
		arms["truecolor"] = append(arms["truecolor"], tc.String()+"\x1b[0m\r\n")
		arms["wrapped"] = append(arms["wrapped"], strings.Repeat(plain.String(), 5)[:300]+"\r\n")
	}
	for _, name := range []string{"plain", "truecolor", "wrapped"} {
		b.Run(name, func(b *testing.B) {
			data := make([][]byte, lines)
			for i, l := range arms[name] {
				data[i] = []byte(l)
			}
			term, p := NewTerm(Config{ScrollingHistory: 10000}, 200, 50, nil), NewProcessor()
			for i := range 10050 {
				p.Advance(term, data[i%lines])
			}
			if h := term.Grid().HistorySize(); h != 10000 {
				b.Fatalf("history %d, want it full", h)
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				p.Advance(term, data[i%lines])
				i++
			}
		})
	}
}
