package vt

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// randomPrefix builds output that leaves a term in a varied state: wide
// characters and spacers on the rows, attributes in the template, wrap
// pending or not, line wrap mode on or off, some history.
func randomPrefix(rng *rand.Rand, cols, lines int) string {
	var b strings.Builder
	for range 5 + rng.Intn(40) {
		switch rng.Intn(10) {
		case 0:
			b.WriteString(strings.Repeat("中", rng.Intn(cols+2)))
		case 1:
			fmt.Fprintf(&b, "\x1b[%d;%dH", 1+rng.Intn(lines), 1+rng.Intn(cols))
		case 2:
			fmt.Fprintf(&b, "\x1b[%d;%d;4m\x1b[48;2;%d;1;2m\x1b[58;5;%dm", 30+rng.Intn(8), 1+rng.Intn(9), rng.Intn(256), rng.Intn(256))
		case 3:
			b.WriteString("\x1b[0m")
		case 4:
			b.WriteString([]string{"\x1b[?7l", "\x1b[?7h"}[rng.Intn(2)])
		case 5:
			b.WriteString("\r\n")
		case 6:
			b.WriteString(strings.Repeat("x", rng.Intn(2*cols)))
		case 7:
			b.WriteString("é") // a zero-width mark on the previous cell
		default:
			b.WriteString("ab cd")
		}
	}
	return b.String()
}

func sameGrid(t *testing.T, what string, a, b *Term) {
	t.Helper()
	ga, gb := a.Grid(), b.Grid()
	if ga.Cursor != gb.Cursor || ga.HistorySize() != gb.HistorySize() {
		t.Fatalf("%s: cursor %+v vs %+v, history %d vs %d", what, ga.Cursor, gb.Cursor, ga.HistorySize(), gb.HistorySize())
	}
	for line := -ga.HistorySize(); line < ga.ScreenLines(); line++ {
		for col := 0; col < ga.Columns(); col++ {
			if ca, cb := *ga.Row(line).Cell(col), *gb.Row(line).Cell(col); ca != cb {
				t.Fatalf("%s: cell %d,%d: %+v vs %+v", what, line, col, ca, cb)
			}
		}
	}
}

// inputASCII must leave exactly the grid that printing each character with
// input does.
func TestInputASCIIMatchesInput(t *testing.T) {
	for seed := int64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cols, lines := 2+rng.Intn(24), 1+rng.Intn(6) // a wide character needs two columns
		prefix := randomPrefix(rng, cols, lines)
		text := strings.Repeat("the quick brown fox ", 4)[:1+rng.Intn(60)]
		a, b := NewTerm(Config{ScrollingHistory: 8}, cols, lines, nil), NewTerm(Config{ScrollingHistory: 8}, cols, lines, nil)
		NewProcessor().Advance(a, []byte(prefix))
		NewProcessor().Advance(b, []byte(prefix))
		a.inputASCII([]byte(text))
		for _, c := range []byte(text) {
			b.input(rune(c))
		}
		sameGrid(t, fmt.Sprintf("seed %d", seed), a, b)
	}
}

func TestCellFieldsHandledByWriters(t *testing.T) {
	if n := reflect.TypeFor[Cell]().NumField(); n != 6 {
		t.Fatalf("Cell has %d fields; update writeAtCursor's field assignments (and check inputASCII's template copy) before changing this test", n)
	}
}
