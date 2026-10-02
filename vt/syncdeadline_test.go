package vt

import (
	"testing"
	"time"
)

// TestSyncDeadlineMovesWithEachBSU: the deadline is counted from the newest
// BSU of the update, as vte re-arms it, and is gone once the update ends.
func TestSyncDeadlineMovesWithEachBSU(t *testing.T) {
	term, p := NewTerm(Config{}, 20, 2, nil), NewProcessor()
	if _, ok := p.SyncDeadline(); ok {
		t.Fatal("a deadline with no update pending")
	}
	before := time.Now()
	feed(term, p, "\x1b[?2026ha")
	d1, ok := p.SyncDeadline()
	if !ok || d1.Before(before.Add(SyncUpdateTimeout)) || d1.After(time.Now().Add(SyncUpdateTimeout)) {
		t.Fatalf("deadline after a BSU: %v ok=%v, want SyncUpdateTimeout from the BSU", d1, ok)
	}
	time.Sleep(20 * time.Millisecond)
	feed(term, p, "b\x1b[?2026hc")
	d2, ok := p.SyncDeadline()
	if !ok || !d2.After(d1) {
		t.Fatalf("a BSU inside the update left the deadline at %v (first %v)", d2, d1)
	}
	feed(term, p, "\x1b[?2026l")
	if _, ok := p.SyncDeadline(); ok {
		t.Fatal("a deadline after the ESU")
	}
}
