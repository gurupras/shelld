package shelld

import (
	"encoding/json"
	"testing"
	"time"
)

// ExpireOlderThan measures each message's age from its own CreatedAt, so a
// message that has waited out the deadline behind a younger one goes and the
// younger one stays. A per-window timer reset on each delivery could not do
// this: it would keep the oldest payload alive for as long as traffic kept
// arriving, which is the unbounded wait the deadline exists to end.
func TestExpireOlderThanIsPerMessage(t *testing.T) {
	q := NewInjectQueue(8)
	old, err := q.Enqueue("stale", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Age the first message by moving its timestamp back rather than sleeping:
	// the deadline is arithmetic on CreatedAt, so backdating exercises the same
	// comparison in microseconds.
	backdate(q, old.ID, 5*time.Second)
	fresh, err := q.Enqueue("fresh", 0)
	if err != nil {
		t.Fatal(err)
	}

	expired := q.ExpireOlderThan(time.Second)
	if len(expired) != 1 || expired[0].ID != old.ID {
		t.Fatalf("expected only %s to expire, got %+v", old.ID, expired)
	}
	pending := q.Pending()
	if len(pending) != 1 || pending[0].ID != fresh.ID {
		t.Fatalf("expected only %s to remain, got %+v", fresh.ID, pending)
	}
}

// A dequeue the delivery loop could not complete puts the message back, and that
// must not buy it a fresh deadline: Requeue re-inserts the same struct, so the
// payload the caller is waiting on keeps aging. Without this, a window that
// refused delivery every poll would requeue its way past any deadline.
func TestRequeueDoesNotResetTheDeadline(t *testing.T) {
	q := NewInjectQueue(8)
	msg, err := q.Enqueue("payload", 0)
	if err != nil {
		t.Fatal(err)
	}
	backdate(q, msg.ID, 5*time.Second)

	got, ok := q.Dequeue()
	if !ok {
		t.Fatal("Dequeue found nothing")
	}
	q.Requeue(got)

	if expired := q.ExpireOlderThan(time.Second); len(expired) != 1 {
		t.Fatalf("a requeued message stopped aging: expired %d, pending %+v",
			len(expired), q.Pending())
	}
}

// Zero is the opt-out and has to stay reachable, which is why WindowOpts takes a
// pointer: a window configured with no deadline waits as it did before the
// deadline existed.
func TestExpireOlderThanZeroExpiresNothing(t *testing.T) {
	q := NewInjectQueue(8)
	msg, err := q.Enqueue("payload", 0)
	if err != nil {
		t.Fatal(err)
	}
	backdate(q, msg.ID, time.Hour)
	for _, age := range []time.Duration{0, -time.Second} {
		if expired := q.ExpireOlderThan(age); expired != nil {
			t.Fatalf("age %v expired %+v", age, expired)
		}
	}
	if q.Len() != 1 {
		t.Fatalf("queue length %d", q.Len())
	}
}

// The log is bounded because it is fed by a timer: a window whose scan never
// recovers expires every payload a caller sends it, so an unbounded log would be
// the same unbounded growth the deadline removes from the queue.
func TestExpiryLogKeepsTheNewestEntriesOnly(t *testing.T) {
	var l expiryLog
	for i := 0; i < injectExpiryLogSize*3; i++ {
		l.add(InjectExpiry{Message: InjectMessage{ID: string(rune('a' + i%26))}, Reason: "not-idle"})
	}
	got := l.all()
	if len(got) != injectExpiryLogSize {
		t.Fatalf("log holds %d entries, want %d", len(got), injectExpiryLogSize)
	}
	// Newest last: the final entry is the final add.
	last := injectExpiryLogSize*3 - 1
	if want := string(rune('a' + last%26)); got[len(got)-1].Message.ID != want {
		t.Fatalf("newest entry is %q, want %q", got[len(got)-1].Message.ID, want)
	}
}

// The bound is a number, not whatever the constant happens to say. Asserting
// against injectExpiryLogSize alone — which is what the test above does, because
// it also has to build the input — holds for any value of it, so it would pass
// with the log resized to 8 or 32 and the documented depth silently wrong.
func TestTheExpiryLogHoldsSixteenEntries(t *testing.T) {
	var l expiryLog
	for i := 0; i < 17; i++ {
		l.add(InjectExpiry{Message: InjectMessage{ID: string(rune('a' + i))}})
	}
	got := l.all()
	if len(got) != 16 {
		t.Fatalf("17 expiries left %d in the log, want 16", len(got))
	}
	// Dropped from the front: 'a' is the one of the seventeen that went.
	if got[0].Message.ID != "b" || got[15].Message.ID != "q" {
		t.Errorf("the log holds %q…%q, want \"b\"…\"q\"", got[0].Message.ID, got[15].Message.ID)
	}
}

// all returns a copy, because its caller is Window.InjectExpiries and the value
// goes to an HTTP handler on another goroutine while the drain loop keeps adding
// to the log. Returning the backing slice would hand that handler a window into
// memory add writes through — a data race, and one a reader would see as an entry
// changing under it rather than as a crash.
// The log has to be at capacity for the aliasing to be observable, and that is
// also the state a window in this condition is in: add appends while there is room,
// which reallocates and hides the sharing, but once full it shifts the entries down
// inside the one array it keeps. A caller holding that array sees its entries
// change identity under it.
func TestTheExpiryLogReturnsACopy(t *testing.T) {
	var l expiryLog
	for i := 0; i < injectExpiryLogSize; i++ {
		l.add(InjectExpiry{Reason: expiryNotIdle, Message: InjectMessage{ID: string(rune('a' + i))}})
	}
	got := l.all()
	if got[0].Message.ID != "a" {
		t.Fatalf("the full log starts at %q, want \"a\"; the fixture is wrong", got[0].Message.ID)
	}

	// The shift a full log does on its next add must not reach a slice already
	// handed out.
	l.add(InjectExpiry{Reason: expiryScanRefused, Message: InjectMessage{ID: "new"}})
	if got[0].Message.ID != "a" {
		t.Errorf("an entry in a slice handed out earlier became %q after one more add",
			got[0].Message.ID)
	}
	// And writing through that slice must not reach the log.
	got[0].Message.ID = "clobbered"
	if again := l.all(); again[0].Message.ID == "clobbered" {
		t.Error("writing to the returned slice changed the log")
	}
}

// The three reasons are the three ways a delivery never happened, and they are
// not interchangeable to whoever reads the log: "scan-refused" means the screen
// held output the scan could not place, which can persist with no further output
// and is the case the deadline exists for; "not-idle" means the scan read the
// screen and it was not a prompt — a program still running, the deadline working
// as designed against a caller who injected into a busy window; and "outranked"
// means the window was delivering and higher priorities kept going first, so
// nothing was wrong with the window at all.
//
// Driven through expireStalledInjections directly: the mapping is a property of
// the poll's verdict, and a live window cannot hold any verdict still.
func TestExpiryReasonFollowsThePollsVerdict(t *testing.T) {
	for _, reason := range []string{expiryScanRefused, expiryNotIdle, expiryOutranked} {
		w := newWindowState("reasons", DefaultInjectConfig(), 80, 24, 0)
		msg, err := w.injectQueue.Enqueue("payload", 0)
		if err != nil {
			t.Fatal(err)
		}
		backdate(w.injectQueue, msg.ID, 5*time.Second)

		w.expireStalledInjections(time.Second, reason)

		got := w.InjectExpiries()
		if len(got) != 1 {
			t.Fatalf("%s: recorded %d expiries, want 1", reason, len(got))
		}
		if got[0].Reason != reason {
			t.Errorf("%s: reason recorded as %q", reason, got[0].Reason)
		}
		if got[0].Message.ID != msg.ID {
			t.Errorf("%s: recorded %q, want %q", reason, got[0].Message.ID, msg.ID)
		}
		if w.injectQueue.Len() != 0 {
			t.Errorf("%s: the expired payload is still queued", reason)
		}
	}
	// The strings themselves are the contract: they go out on GET …/inject/queue
	// and into logs an operator greps. Renaming one is a wire change.
	if expiryNotIdle != "not-idle" || expiryScanRefused != "scan-refused" || expiryOutranked != "outranked" {
		t.Errorf("the reason strings changed: %q %q %q",
			expiryNotIdle, expiryScanRefused, expiryOutranked)
	}
}

// Nothing to expire records nothing: the log is for payloads that were actually
// discarded, so a window polled for hours with a healthy queue keeps it empty.
func TestExpireStalledRecordsNothingWhenNothingIsStale(t *testing.T) {
	w := newWindowState("fresh", DefaultInjectConfig(), 80, 24, 0)
	if _, err := w.injectQueue.Enqueue("payload", 0); err != nil {
		t.Fatal(err)
	}
	w.expireStalledInjections(time.Minute, expiryScanRefused)
	if got := w.InjectExpiries(); len(got) != 0 {
		t.Errorf("recorded %+v with nothing stale", got)
	}
	if w.injectQueue.Len() != 1 {
		t.Errorf("the payload left the queue; length %d", w.injectQueue.Len())
	}
}

// The drain loop calls ExpireOlderThan on every poll of every window with
// anything queued, and almost every one of those calls finds nothing. That is
// what the first pass buys, and it is only worth its second loop if the finding-
// nothing path really is free — so the claim is pinned rather than asserted in a
// comment. A single pass building the kept slice unconditionally would allocate
// here on every poll.
func TestExpireOlderThanAllocatesNothingWhenNothingIsStale(t *testing.T) {
	q := NewInjectQueue(64)
	for i := 0; i < 8; i++ {
		if _, err := q.Enqueue("payload", 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := testing.AllocsPerRun(100, func() { q.ExpireOlderThan(time.Hour) }); got != 0 {
		t.Errorf("a poll that expires nothing allocated %v times, want 0", got)
	}
	if q.Len() != 8 {
		t.Fatalf("the probe expired something: length %d", q.Len())
	}
}

// An expiry is only useful to the client reading it off the route, so the keys
// are part of the contract. Order is asserted too: the struct's fields are
// declared in JSON key order to match serde_json, which sorts them, so a field
// added in the wrong place here is a divergence from shelld-rs that no other test
// would see.
func TestInjectExpiryMarshalsTheDocumentedShape(t *testing.T) {
	b, err := json.Marshal(InjectExpiry{
		ExpiredAt: 1700000000123,
		Message:   InjectMessage{CreatedAt: 1700000000000, ID: "inj-1", Payload: "clear"},
		Reason:    "scan-refused",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"expiredAt":1700000000123,` +
		`"message":{"createdAt":1700000000000,"id":"inj-1","payload":"clear","priority":0},` +
		`"reason":"scan-refused"}`
	if string(b) != want {
		t.Errorf("marshalled\n %s\nwant\n %s", b, want)
	}
}

// backdate moves a queued message's CreatedAt back by age, reaching into the
// queue the way only a same-package test can. It exists so the deadline tests
// run in microseconds instead of sleeping out a real timeout.
func backdate(q *InjectQueue, id string, age time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.messages {
		if q.messages[i].ID == id {
			q.messages[i].CreatedAt -= age.Milliseconds()
		}
	}
}
