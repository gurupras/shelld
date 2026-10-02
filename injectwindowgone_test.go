package shelld

import (
	"testing"
	"time"
)

// A window that dies with payloads still queued used to leave them in the queue
// for ever: drainLoop returned on the first push that failed with the input
// queue's close error, and the queue outlives the goroutine. Nothing else removes
// them, so /inject/queue kept reporting them as `pending` -- not "we lost it", but
// the stronger and more misleading "it is still waiting its turn" -- while
// InjectExpiries stayed empty.
//
// StallTimeoutMS is 0 here, which means wait for ever, so the deadline path cannot
// account for anything this test sees. That is deliberate: every other expiry
// reason needs a deadline to have run out, and a test that left one set would pass
// on a loop that merely swept the queue a bit sooner.
func TestAWindowThatDiesRecordsTheWholeQueueRatherThanLeavingItPending(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 0
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(idleScreen))

	// Distinct priorities so which payload the loop dequeues is decided rather than
	// observed: Dequeue takes the highest, so "high" is the one push refuses and the
	// other two are the queue left behind. Without that the ordering assertion below
	// would be asserting the storage order.
	low, err := w.injectQueue.Enqueue("low", 0)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := w.injectQueue.Enqueue("mid", 1)
	if err != nil {
		t.Fatal(err)
	}
	high, err := w.injectQueue.Enqueue("high", 2)
	if err != nil {
		t.Fatal(err)
	}

	// Premise, in three parts, because this test's whole subject is the arm reached
	// only when the gate PASSES and the push then fails. If the window did not look
	// deliverable the loop would take the not-idle branch instead and, with a
	// deadline of zero, expire nothing -- the queue would stay put and the test
	// would fail with the same symptom it is looking for, for an unrelated reason.
	if idle, refused := w.idleVerdict(); !idle || refused {
		t.Fatalf("premise: fixture is not idle: idle=%v refused=%v", idle, refused)
	}
	if !w.inputTracker.IsQuiet() {
		t.Fatal("premise: fixture is not quiet, so the gate would hold the payload back instead of trying it")
	}
	if w.injectQueue.Len() != 3 {
		t.Fatalf("premise: queued %d payloads, want 3", w.injectQueue.Len())
	}

	// The window dies. close clears the queued bytes, so pending() goes false and
	// the gate opens -- which is how the loop gets as far as a push at all.
	w.input.close(w.deadErr())
	if w.input.pending() {
		t.Fatal("premise: input still pending after close, so the gate will not reach a push")
	}
	go w.drainLoop()

	pollUntil(t, 3*time.Second, func() bool { return len(w.InjectExpiries()) >= 3 })

	got := w.InjectExpiries()
	if len(got) != 3 {
		t.Fatalf("recorded %d expiries, want 3 (the payload the loop held plus the two behind it): %+v", len(got), got)
	}
	// Every payload, not just the one the loop was holding. Recording only the
	// dequeued payload leaves the other two in the queue and reported as pending,
	// which is the original defect narrowed rather than fixed.
	seen := map[string]string{}
	for _, e := range got {
		seen[e.Message.ID] = e.Reason
	}
	for _, want := range []InjectMessage{high, mid, low} {
		reason, ok := seen[want.ID]
		if !ok {
			t.Errorf("payload %q (%q) was not recorded", want.ID, want.Payload)
			continue
		}
		if reason != expiryWindowGone {
			t.Errorf("payload %q recorded as %q, want %q: the other reasons all claim a deadline ran out or the screen was not ready, and neither happened here",
				want.ID, reason, expiryWindowGone)
		}
	}
	// The dequeued payload keeps its place ahead of the ones queued behind it.
	if got[0].Message.ID != high.ID {
		t.Errorf("first expiry is %q, want the dequeued payload %q", got[0].Message.ID, high.ID)
	}

	// The queue is empty, which is what makes the record true rather than merely
	// present: a payload must be in `pending` or in `expired`, never in both.
	if n := w.injectQueue.Len(); n != 0 {
		t.Errorf("%d payloads still queued on a dead window, so /inject/queue still reports them as pending", n)
	}
	if p := w.injectQueue.Pending(); len(p) != 0 {
		t.Errorf("Pending still reports %d payloads: %+v", len(p), p)
	}

	// Nothing was typed. The window's input queue was closed before the loop
	// started, so a delivery here would mean push accepted bytes for a dead window.
	if typed := delivered(); len(typed) != 0 {
		t.Errorf("typed %q into a window whose input queue was closed", typed)
	}
}

// The same property on the exit that actually happens. The test above reaches
// drainLoop's push-error return, which needs a window that died with an injection in
// flight; the ordinary way a window dies is drainStop, and the queue outlives the
// goroutine just as completely there. Recording on one exit and not the others left
// the common case reporting `pending` for ever -- the narrower defect underneath the
// one above, and invisible to it.
//
// stopDrain is what both callers reach: Window.Kill, and the reader loop once it has
// set alive false. So closing drainStop directly is the shared tail of every way a
// window ends, without a PTY.
//
// The fixture is deliberately NOT idle -- no idleScreen written -- with
// StallTimeoutMS zero. So the loop cannot deliver and cannot expire by deadline: if
// anything is recorded here, the exit recorded it.
func TestADrainStopExitRecordsTheQueueTheSameWayADeadPushDoes(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 0
	w, delivered := drainWindow(t, cfg)

	if idle, _ := w.idleVerdict(); idle {
		t.Fatal("premise: fixture looks idle, so the loop may deliver a payload instead of holding it")
	}
	first, err := w.injectQueue.Enqueue("first", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.injectQueue.Enqueue("second", 0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.drainLoop()
	}()
	w.stopDrain()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainLoop did not return after drainStop closed")
	}

	// Read after the goroutine has returned rather than polled for, because the
	// record is written by a deferred call inside it: polling could pass on a loop
	// that recorded nothing and happened to be raced by a later write.
	got := w.InjectExpiries()
	if len(got) != 2 {
		t.Fatalf("recorded %d expiries after a drainStop exit, want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Reason != expiryWindowGone {
			t.Errorf("payload %q recorded as %q, want %q", e.Message.ID, e.Reason, expiryWindowGone)
		}
	}
	seen := map[string]bool{got[0].Message.ID: true, got[1].Message.ID: true}
	for _, want := range []InjectMessage{first, second} {
		if !seen[want.ID] {
			t.Errorf("payload %q (%q) was not recorded", want.ID, want.Payload)
		}
	}
	if n := w.injectQueue.Len(); n != 0 {
		t.Errorf("%d payloads still queued, so /inject/queue reports them as pending with no loop left to deliver them", n)
	}
	if p := w.injectQueue.Pending(); len(p) != 0 {
		t.Errorf("Pending still reports %d payloads: %+v", len(p), p)
	}
	if typed := delivered(); len(typed) != 0 {
		t.Errorf("typed %q on a window that was never idle", typed)
	}
}

// DrainAll's contract on an empty queue, which the caller above relies on to
// distinguish "nothing else was queued" from "something was and we dropped it
// silently": a nil answer means there is nothing to report, so the loop over it
// records nothing rather than recording a zero-valued message.
func TestDrainAllReportsNothingForAnEmptyQueue(t *testing.T) {
	q := NewInjectQueue(4)
	if got := q.DrainAll(); got != nil {
		t.Errorf("DrainAll on an empty queue = %+v, want nil", got)
	}
	if _, err := q.Enqueue("one", 0); err != nil {
		t.Fatal(err)
	}
	got := q.DrainAll()
	if len(got) != 1 || got[0].Payload != "one" {
		t.Fatalf("DrainAll = %+v, want the one queued payload", got)
	}
	if q.Len() != 0 {
		t.Errorf("queue still holds %d after DrainAll", q.Len())
	}
	// Draining twice is not an error and does not re-report: the second caller of a
	// drained queue has nothing to record.
	if again := q.DrainAll(); again != nil {
		t.Errorf("a second DrainAll = %+v, want nil", again)
	}
}
