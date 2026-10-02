package shelld

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The deadline is enforced from inside drainLoop, and where in the poll it sits
// decides three things a unit test on ExpireOlderThan cannot see: whether a
// payload that is both past its deadline and deliverable is typed or discarded,
// which of several queued payloads goes when only one can, and whether the log's
// stall latch survives a queue emptied by expiry rather than by delivery. These
// tests run the real loop for that reason.

// drainWindow is a window with a live input queue and an unclosed drainStop, so
// the real drainLoop can be started against it, plus a reader of everything the
// loop wrote. NewFakeWindow cannot be used here: it closes both.
//
// The reader drains the queue without echoing anything back into scrollback, so
// the screen the scan sees stays whatever the test wrote — which is what lets a
// test hold one verdict still for the length of the run.
func drainWindow(t *testing.T, cfg InjectConfig) (*Window, func() []string) {
	t.Helper()
	w := newWindowState("stall", cfg, 80, 24, 0)
	w.input = newInputQueue(0, nil)
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			c, ok := w.input.next()
			if !ok {
				return
			}
			mu.Lock()
			got = append(got, string(c.data))
			mu.Unlock()
			w.input.done(c)
		}
	}()
	t.Cleanup(func() {
		w.stopDrain()
		w.input.close(w.deadErr())
	})
	return w, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// pollUntil polls until cond holds or d elapses, so a test asserts on the loop's
// outcome rather than on how many 200 ms polls it took to get there.
func pollUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// refusedScreen is output the scan cannot place: CSI J is not one of the four
// sequences classifyEscape models, so this screen refuses for ever. It is the
// state the deadline exists for — an idle program writes nothing further, so the
// unreadable line stays the newest one.
const refusedScreen = "\x1b[J\xe2\x9d\xaf "

// idleScreen is a bare prompt, which the scan reads as idle.
const idleScreen = "\xe2\x9d\xaf "

// On a screen the scan refuses, an expiry is the only way the queue can empty:
// nothing is deliverable, so no other path removes a payload. The expiry
// therefore has to hand that transition to logScanRefusal,
// or the latch stays set on an empty queue and the next payload to stall on the
// same unreadable screen is never reported — and the stall line is the only
// report a waiting payload gets.
//
// The reset must also stay silent: the screen did not become readable, so
// logging recovery here would be false. That rules out fixing the latch by
// calling the recovery path.
func TestExpiryResetsTheStallLatchSoTheNextStallIsLogged(t *testing.T) {
	h := &countHandler{count: map[string]int{}, attrs: map[string]string{}}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })

	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 2000
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(refusedScreen))

	first, err := w.injectQueue.Enqueue("one", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Backdated past the deadline so the first poll expires it, keeping the test
	// to a few polls instead of the two seconds the deadline names.
	backdate(w.injectQueue, first.ID, 10*time.Second)
	go w.drainLoop()

	pollUntil(t, 3*time.Second, func() bool { return len(w.InjectExpiries()) >= 1 })
	if e := w.InjectExpiries(); len(e) != 1 || e[0].Message.ID != first.ID {
		t.Fatalf("first payload did not expire: %+v", e)
	}
	if got := delivered(); len(got) != 0 {
		t.Fatalf("the refused screen was typed into: %q", got)
	}
	if got := h.get(msgScanRefused); got != 1 {
		t.Fatalf("first stall logged %d times, want 1", got)
	}

	// A second payload against the same unreadable screen is a new stall, and the
	// operator has to be told: nothing else reports that it is waiting.
	if _, err := w.injectQueue.Enqueue("two", 0); err != nil {
		t.Fatal(err)
	}
	// Bounded well inside the second payload's own 2 s deadline, so a failure here
	// is the missing log line and never the payload expiring underneath the wait.
	pollUntil(t, time.Second, func() bool { return h.get(msgScanRefused) >= 2 })
	if got := h.get(msgScanRefused); got != 2 {
		t.Errorf("a second stall on the same refused screen logged %d refusals in total,"+
			" want 2 (scanStalled=%v)", got, w.scanStalled.Load())
	}
	if got := h.get(msgScanCleared); got != 0 {
		t.Errorf("logged %d recoveries; the screen never became readable", got)
	}
	if w.injectQueue.Len() != 1 {
		t.Errorf("the second payload left the queue before its own deadline; len %d",
			w.injectQueue.Len())
	}
}

// The deadline bounds how long a payload waits on a window that is not ready, so
// a poll that is going to deliver must not expire anything: a payload being typed
// is not waiting. Expiring first discarded a payload on the very poll that would
// have delivered it, and recorded "not-idle" against a scan that had just said
// idle — the one field a caller cannot reconstruct.
//
// A payload reaches a ready window already past its deadline whenever the window
// was busy for the deadline and then came back, or when the delivery cooldown
// (2 s by default) outlasts a shorter deadline.
func TestAPastDeadlinePayloadIsDeliveredWhenTheWindowIsReady(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 1000
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(idleScreen))
	// Premise: without a deliverable window this test would pass on an expiry-only
	// loop for the wrong reason.
	if idle, refused := w.idleVerdict(); !idle || refused {
		t.Fatalf("fixture is not idle: idle=%v refused=%v", idle, refused)
	}
	if !w.inputTracker.IsQuiet() || w.input.pending() {
		t.Fatalf("fixture is not quiet: quiet=%v pending=%v",
			w.inputTracker.IsQuiet(), w.input.pending())
	}

	msg, err := w.injectQueue.Enqueue("echo hi", 0)
	if err != nil {
		t.Fatal(err)
	}
	backdate(w.injectQueue, msg.ID, 1500*time.Millisecond)
	go w.drainLoop()

	pollUntil(t, 3*time.Second, func() bool {
		return len(delivered()) > 0 || len(w.InjectExpiries()) > 0
	})
	if e := w.InjectExpiries(); len(e) != 0 {
		t.Fatalf("expired %+v on a ready window: reason %q contradicts the poll's own scan",
			e, e[0].Reason)
	}
	if got := delivered(); len(got) != 1 || got[0] != "echo hi\r" {
		t.Fatalf("delivered %q, want [%q]", got, "echo hi\r")
	}
}

// Delivery order is by priority and expiry order is by age, so expiring before
// the delivery gate let the two disagree on one poll: the highest-priority
// payload was discarded while the lower-priority one it was ahead of was typed.
// Dequeue on that same poll would have chosen the high one.
func TestTheDeadlineDoesNotInvertPriority(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 1000
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(idleScreen))

	hi, err := w.injectQueue.Enqueue("HIGH", 99)
	if err != nil {
		t.Fatal(err)
	}
	// Only the high-priority payload is past the deadline, which is the whole
	// disagreement: by age it goes first, by priority it is delivered first.
	backdate(w.injectQueue, hi.ID, 1500*time.Millisecond)
	if _, err := w.injectQueue.Enqueue("low", 0); err != nil {
		t.Fatal(err)
	}
	go w.drainLoop()

	pollUntil(t, 3*time.Second, func() bool {
		return len(delivered())+len(w.InjectExpiries()) >= 2
	})
	if e := w.InjectExpiries(); len(e) != 0 {
		t.Fatalf("expired %+v on a ready window", e)
	}
	got := delivered()
	if len(got) != 2 || got[0] != "HIGH\r" || got[1] != "low\r" {
		t.Fatalf("delivered %q, want [%q %q] — priority decides the order", got, "HIGH\r", "low\r")
	}
}

// The other half of the same placement: because the deadline is checked inside the
// not-ready branch, a window that keeps delivering would never check it at all.
// The queue orders by priority, so a stream of higher-priority payloads is typed
// past a low-priority one for as long as the stream lasts, and every one of those
// polls takes the delivery branch. Without a sweep after the write, the low
// payload has no bound of any kind — the adversarial probe for this ran one 4 s
// with a 300 ms deadline and recorded zero expiries.
//
// The sweep sits after the write so the payload the poll chose is never the one it
// drops, which is what the two tests above pin from the other side.
func TestTheDeadlineBitesOnAWindowThatKeepsDelivering(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	// Long enough that the higher-priority payloads outlive the polls it takes to
	// deliver them all — one per 200 ms poll — so the only thing past its deadline
	// here is the one that was backdated. A deadline shorter than others*200ms
	// would expire the tail of the queue too, correctly but for a different reason.
	cfg.StallTimeoutMS = 3000
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(idleScreen))

	// Past its deadline and outranked by everything else in the queue, so every
	// poll delivers one of the others and none of them ever finds the window
	// unready.
	low, err := w.injectQueue.Enqueue("LOW", 0)
	if err != nil {
		t.Fatal(err)
	}
	backdate(w.injectQueue, low.ID, 10*time.Second)
	const others = 3
	for i := 0; i < others; i++ {
		if _, err := w.injectQueue.Enqueue("high", 9); err != nil {
			t.Fatal(err)
		}
	}
	go w.drainLoop()

	pollUntil(t, 5*time.Second, func() bool { return len(w.InjectExpiries()) > 0 })
	e := w.InjectExpiries()
	if len(e) != 1 || e[0].Message.ID != low.ID {
		t.Fatalf("the outranked payload did not expire: %+v (delivered %q)", e, delivered())
	}
	// "outranked" rather than "not-idle": nothing was wrong with the window, and a
	// client told its payload met a busy window would look in the wrong place.
	if e[0].Reason != expiryOutranked {
		t.Errorf("reason %q, want %q", e[0].Reason, expiryOutranked)
	}
	// Dropped, never typed — the deadline must not turn into a delivery.
	for _, d := range delivered() {
		if d == "LOW\r" {
			t.Errorf("the expired payload was typed anyway: %q", delivered())
		}
	}
	// And the higher priorities were unaffected: a sweep that dropped what the poll
	// had just chosen would show up as fewer than all of them arriving.
	pollUntil(t, 5*time.Second, func() bool { return len(delivered()) >= others })
	if got := len(delivered()); got != others {
		t.Errorf("delivered %d of %d higher-priority payloads: %q", got, others, delivered())
	}
}

// Every accepted payload must end up either written or recorded as expired: the
// signal channel holds one token, so a lost wakeup would leave one queued for
// ever with the enqueue already answered 202.
func TestNoPayloadIsLostBetweenSignalAndExpiry(t *testing.T) {
	cfg := DefaultInjectConfig()
	cfg.DeliveryCooldownMS = 0
	cfg.StallTimeoutMS = 400
	w, delivered := drainWindow(t, cfg)
	w.scrollback.Write([]byte(idleScreen))
	go w.drainLoop()

	const n = 12
	for i := 0; i < n; i++ {
		if _, err := w.injectQueue.Enqueue("p", 0); err != nil {
			t.Fatal(err)
		}
		// Staggered unevenly so enqueues land both inside a poll's sleep and in
		// the window where the loop has just seen an empty queue.
		time.Sleep(time.Duration(17*(i%7)) * time.Millisecond)
	}
	pollUntil(t, 10*time.Second, func() bool {
		return w.injectQueue.Len() == 0 && len(delivered())+len(w.InjectExpiries()) >= n
	})
	if q := w.injectQueue.Len(); q != 0 {
		t.Errorf("%d payloads still queued", q)
	}
	if acct := len(delivered()) + len(w.InjectExpiries()); acct != n {
		t.Errorf("accounted for %d of %d payloads (delivered %d, expired %d)",
			acct, n, len(delivered()), len(w.InjectExpiries()))
	}
}
