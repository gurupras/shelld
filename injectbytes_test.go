package shelld

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// H16: the inject queue's enqueue refusals in shelld-rs's order
// (shelld-core inject.rs:765-820): closed, a payload larger than the whole cap,
// the message count, the bytes retained beside it, then the budget windows
// share. Bytes are given back when a message leaves the queue, which is what
// keeps a 429 retryable.
func TestInjectEnqueueBoundsBytesInRustsOrder(t *testing.T) {
	pay := func(n int) string { return strings.Repeat("a", n) }

	t.Run("a payload larger than the cap names both numbers", func(t *testing.T) {
		q := newInjectQueue(64, 1000, NewInputBudget(1<<20))
		_, err := q.Enqueue(pay(1001), 0)
		if !errors.Is(err, ErrPayloadTooLarge) || errors.Is(err, ErrQueueFull) {
			t.Fatalf("1001 bytes into a 1000-byte queue: %v, want ErrPayloadTooLarge alone", err)
		}
		if want := "payload of 1001 bytes exceeds the 1000-byte inject queue"; err.Error() != want {
			t.Errorf("text %q, want %q", err, want)
		}
		if _, err := q.Enqueue(pay(1000), 0); err != nil {
			t.Errorf("exactly the cap into an empty queue: %v", err)
		}
		// The budget's limit where it is smaller, as rs's min(max_bytes, budget).
		small := newInjectQueue(64, 1<<20, NewInputBudget(500))
		if _, err := small.Enqueue(pay(501), 0); err == nil || err.Error() != "payload of 501 bytes exceeds the 500-byte inject queue" {
			t.Errorf("501 bytes against a 500-byte budget: %v", err)
		}
	})

	t.Run("closed comes before size, and size before count", func(t *testing.T) {
		q := newInjectQueue(1, 1000, NewInputBudget(1<<20))
		if _, err := q.Enqueue("x", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Enqueue(pay(2000), 0); !errors.Is(err, ErrPayloadTooLarge) {
			t.Errorf("too large into a queue at its count: %v, want ErrPayloadTooLarge", err)
		}
		if _, err := q.Enqueue("y", 0); !errors.Is(err, ErrQueueFull) {
			t.Errorf("one message past the count: %v, want ErrQueueFull", err)
		}
		q.Close()
		if _, err := q.Enqueue(pay(2000), 0); !errors.Is(err, ErrQueueClosed) {
			t.Errorf("too large into a closed queue: %v, want ErrQueueClosed", err)
		}
	})

	t.Run("retained bytes, given back when a message leaves", func(t *testing.T) {
		b := NewInputBudget(1 << 20)
		q := newInjectQueue(64, 1000, b)
		if _, err := q.Enqueue(pay(600), 0); err != nil {
			t.Fatal(err)
		}
		_, err := q.Enqueue(pay(600), 0)
		if !errors.Is(err, ErrQueueFull) || errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("600 beside 600 in a 1000-byte queue: %v, want ErrQueueFull", err)
		}
		// Delivered: the delivery loop dequeues.
		msg, _ := q.Dequeue()
		if b.used.Load() != 0 {
			t.Errorf("budget holds %d bytes after the only message was dequeued", b.used.Load())
		}
		if _, err := q.Enqueue(pay(600), 0); err != nil {
			t.Errorf("600 after the first was dequeued: %v", err)
		}
		// A requeue cannot be refused, so it is counted even past the cap.
		q.Requeue(msg)
		if q.Len() != 2 || b.used.Load() != 1200 {
			t.Errorf("after a requeue: %d messages, %d budget bytes; want 2 and 1200", q.Len(), b.used.Load())
		}
		// Expired: everything here is older than a nanosecond once a millisecond passes.
		time.Sleep(2 * time.Millisecond)
		if got := q.ExpireOlderThan(time.Nanosecond); len(got) != 2 {
			t.Fatalf("expired %d, want 2", len(got))
		}
		if b.used.Load() != 0 {
			t.Errorf("budget holds %d bytes after every message expired", b.used.Load())
		}
		if _, err := q.Enqueue(pay(1000), 0); err != nil {
			t.Errorf("the whole cap after the queue expired: %v", err)
		}
		// Removed with the window.
		q.Close()
		if b.used.Load() != 0 {
			t.Errorf("budget holds %d bytes after the queue closed", b.used.Load())
		}
	})

	t.Run("the budget windows share", func(t *testing.T) {
		b := NewInputBudget(1000)
		q1 := newInjectQueue(64, 1000, b)
		q2 := newInjectQueue(64, 1000, b)
		if _, err := q1.Enqueue(pay(600), 0); err != nil {
			t.Fatal(err)
		}
		// Fits q2's own cap, empty as it is, but not the budget q1 holds most of.
		_, err := q2.Enqueue(pay(600), 0)
		if !errors.Is(err, ErrQueueFull) || errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("600 against a budget with 400 left: %v, want ErrQueueFull", err)
		}
		if q2.Len() != 0 {
			t.Errorf("the refused payload was queued")
		}
		q1.DrainAll()
		if _, err := q2.Enqueue(pay(600), 0); err != nil {
			t.Errorf("600 once the other window's queue drained: %v", err)
		}
	})
}
