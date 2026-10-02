package shelld

import (
	"errors"
	"testing"
	"time"
)

// H13's queued-gap cap, at the queue: shelld-rs refuses a push whose gaps would
// take the window's queued pacing past MaxQueuedGap (shelld-core input.rs:344-347)
// with the retryable full-queue refusal, since the total falls as the writer
// waits the gaps ahead out. No writer goroutine here, so nothing drains.
func TestPushRefusesPacingPastTheQueuedGapCap(t *testing.T) {
	q := newInputQueue(1<<20, NewInputBudget(1<<20))
	first := enterAfter(MaxInputGap)
	if err := q.push(first); err != nil {
		t.Fatalf("one clamped gap: %v", err)
	}
	// Exactly the cap fits: two clamped gaps are the ordinary shape of a client
	// pacing input with one gap waited and the next queued behind it.
	if err := q.push(enterAfter(MaxQueuedGap - MaxInputGap)); err != nil {
		t.Fatalf("pacing up to exactly the cap: %v", err)
	}
	err := q.push(enterAfter(time.Millisecond))
	if !errors.Is(err, ErrInputQueueFull) || errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("a gap past the cap: %v, want ErrInputQueueFull and not ErrInputTooLarge", err)
	}
	// The refusal queued nothing, and a gap without bytes is still refused, so
	// the cap is on time and not on bytes.
	if q.gaps != MaxQueuedGap {
		t.Errorf("queued gaps after the refusal = %v, want %v", q.gaps, MaxQueuedGap)
	}
	// Writing a chunk gives its gap back, so the window paces again.
	c, _ := q.next()
	q.done(c)
	if err := q.push(enterAfter(time.Millisecond)); err != nil {
		t.Errorf("a gap after the first was written: %v", err)
	}
	// Close drops what is queued, and with it the gaps, leaving none counted.
	q.close(errors.New("closed"))
	if q.gaps != 0 {
		t.Errorf("queued gaps after close = %v, want 0", q.gaps)
	}
}
