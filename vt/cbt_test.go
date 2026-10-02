package vt

import (
	"strings"
	"testing"
	"time"
)

// TestBackwardTabWithNoStopsStopsCounting: with every stop cleared, CBT
// leaves the cursor where it is, and must not spend its whole count (up to
// 65535) rescanning the row to find that out. A program's output holds the
// window's lock while it is parsed.
func TestBackwardTabWithNoStopsStopsCounting(t *testing.T) {
	term, p := NewTerm(Config{}, 200, 5, nil), NewProcessor()
	feed(term, p, "\x1b[3g\x1b[200G")
	start := time.Now()
	feed(term, p, strings.Repeat("\x1b[65535Z", 2000))
	elapsed := time.Since(start)
	if col := term.Grid().Cursor.Point.Column; col != 199 {
		t.Errorf("CBT with no stops moved the cursor to column %d, want 199", col)
	}
	// 2000 sequences: about 2.6e10 row-scan steps (8 s here) before the fix,
	// microseconds after it.
	if elapsed > 2*time.Second {
		t.Errorf("2000 CBT sequences took %v", elapsed)
	}
	// A stop still stops it, and a count past the only stop stays on it.
	feed(term, p, "\x1b[50G\x1bH\x1b[100G\x1b[Z")
	if col := term.Grid().Cursor.Point.Column; col != 49 {
		t.Errorf("CBT to the one stop: column %d, want 49", col)
	}
	feed(term, p, "\x1b[100G\x1b[5Z")
	if col := term.Grid().Cursor.Point.Column; col != 49 {
		t.Errorf("CBT 5 with one stop to the left: column %d, want 49", col)
	}
}
