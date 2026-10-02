package vt

import (
	"testing"
	"time"
)

// A wide character needs two columns. At one column, writing one indexed
// past the row (a panic) and reflowing one pushed it into the next row
// forever (a resize that never returned, holding the window's grid lock), so
// a terminal is never narrower than MinColumns.
func TestTerminalIsAtLeastMinColumns(t *testing.T) {
	run := func(name string, f func() *Term) {
		t.Helper()
		done := make(chan *Term, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", name, r)
					done <- nil
				}
			}()
			done <- f()
		}()
		select {
		case term := <-done:
			if term != nil && term.Columns() != MinColumns {
				t.Errorf("%s: %d columns, want %d", name, term.Columns(), MinColumns)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: did not return", name) // the goroutine keeps spinning; fail the binary
		}
	}
	run("resize to 1 column with a wide character in history and on screen", func() *Term {
		term := NewTerm(Config{ScrollingHistory: 100}, 80, 24, nil)
		feed(term, NewProcessor(), "漢字\r\n漢")
		term.Resize(1, 24)
		return term
	})
	run("wide character written at 1 column", func() *Term {
		term := NewTerm(Config{ScrollingHistory: 100}, 1, 24, nil)
		feed(term, NewProcessor(), "漢字\r\na漢\r\n")
		return term
	})
	run("resize to 0 columns", func() *Term {
		term := NewTerm(Config{}, 80, 24, nil)
		feed(term, NewProcessor(), "漢")
		term.Resize(0, 0)
		if term.ScreenLines() < 1 {
			t.Errorf("resize to 0 lines: %d lines", term.ScreenLines())
		}
		return term
	})
}
