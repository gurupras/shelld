//go:build linux || darwin

package shelld

// The other half of the drain loop's exits. injectstall_pty_test.go covers the
// window that is never ready; these two are the window that IS ready and whose
// input queue will not take the bytes anyway, which is the one path to a push and
// therefore the one path where the gate's own deadline sweep has not run.
//
// Both cases are reached through a real program that has stopped reading its
// input, because that is what fills an input queue: there is no way to pin bytes
// in the queue from the API side without one.

import (
	"strings"
	"testing"
	"time"
)

// idlePrompt is the default idle pattern, printed once so the scan has a prompt
// to recognise. startNotReading's program never reads its input afterwards.
const idlePrompt = `printf '\342\235\257\r\n'` // ❯

func waitForPrompt(t *testing.T, w *Window) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(5)
		return strings.Contains(out, "❯")
	}, "prompt never shown, so nothing establishes that the window is idle")
}

// A payload bigger than the whole input queue can never be typed, and the drain
// loop used to answer that by putting it back and trying again 200 ms later, for
// ever. Requeue puts a message at the front, ahead of others of its priority, so
// it was not one payload that was stuck: everything queued at its priority or
// below was, behind a head that never moved. That is what the two priorities
// below are for -- the payload that fits is queued lower, so the old behaviour
// starved it.
//
// StallTimeoutMS is zero here on purpose. The deadline is not what should end
// this -- waiting cannot make the payload smaller, and a ready window is never
// scanned into the branch that sweeps -- so with the deadline switched off, the
// expiry recorded below can only have come from the loop recognising that these
// bytes will not fit any state of the queue.
//
// The reason has to be too-large and not the no-room the other case records: a
// client told no-room is being told the payload might go through next time, which
// for this one is never true.
func TestAnInjectionTooLargeForTheInputQueueIsDroppedAndDoesNotBlockTheRest(t *testing.T) {
	const limit = 1 << 16
	stall := uint64(0)
	w := startNotReading(t, WindowOpts{
		Name:            "too-large",
		InputQueueLimit: limit,
		StallTimeoutMS:  &stall,
	}, idlePrompt)
	waitForPrompt(t, w)
	// The inject queue's byte cap follows InputQueueLimit, so a payload of the
	// whole limit leaves no room beside it for the one this test needs queued
	// behind. Widened here so both are queued at once; the arm under test is
	// still the one a payload within a CR of the limit reaches in production.
	w.InjectQueue().mu.Lock()
	w.InjectQueue().maxBytes = 2 * limit
	w.InjectQueue().mu.Unlock()

	// Higher priority, so Dequeue reaches it first and the small payload behind it
	// is what the old behaviour starved. One byte over the limit would do; a whole
	// limit's worth is over it by the CR the loop appends either way.
	big, err := w.InjectQueue().Enqueue(strings.Repeat("y", limit), 1)
	if err != nil {
		t.Fatal(err)
	}
	small, err := w.InjectQueue().Enqueue("echo small", 0)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, func() bool { return w.InjectQueue().Len() == 0 },
		"the queue never emptied: a payload that cannot fit the input queue is still being dequeued, refused and requeued at the front, and every payload behind it waits on a head that never moves")

	// One expiry, not two: the small payload left the queue by being typed.
	expiries := w.InjectExpiries()
	if len(expiries) != 1 {
		t.Fatalf("recorded %+v, want exactly the over-large payload", expiries)
	}
	if got := expiries[0]; got.Message.ID != big.ID {
		t.Errorf("recorded %s (%q), want the over-large %s: the payload that fit is the one that should have been delivered",
			got.Message.ID, got.Message.Payload, big.ID)
	}
	if got := expiries[0].Reason; got != expiryTooLarge {
		t.Errorf("reason %q, want %q: %q is the recoverable case, and a client that reads it retries a payload no retry can deliver",
			got, expiryTooLarge, expiryNoRoom)
	}
	// A bound on promptness, not on provenance: StallTimeoutMS is zero, so no
	// deadline can expire anything and nothing but the too-large arm could have
	// recorded the entry above whatever the timing. What this catches is that arm
	// dropping the payload only after it has sat -- the poll interval is 200 ms and
	// the measured drop is ~201 ms, so the bound is an order of magnitude of slack.
	if waited := expiries[0].ExpiredAt - big.CreatedAt; waited > 2000 {
		t.Errorf("the over-large payload sat for %d ms before being dropped, against a 200 ms poll interval and no deadline configured: it is not being dropped on the poll that first tries it", waited)
	}
	for _, e := range expiries {
		if e.Message.ID == small.ID {
			t.Errorf("the payload that fits was expired as %q instead of being typed", e.Reason)
		}
	}

	// And the drain loop is still running rather than having returned on the way
	// out: a later payload still delivers.
	if _, err := w.InjectQueue().Enqueue("echo after", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return w.InjectQueue().Len() == 0 },
		"nothing was delivered after the over-large payload was dropped, so the loop stopped draining")
}

// The transient half: a payload that fits the window's own queue limit but not the
// budget it shares. This is the requeue that IS a retry -- whoever holds the budget
// could release it and the payload would go through -- and StallTimeoutMS is what
// is supposed to bound the wait. It did not: the requeue arm returned to the top of
// the loop without sweeping, and every poll that reaches a push has passed a gate
// whose own sweep therefore did not run, so no poll ever tested the deadline and
// the payload waited with no bound at all.
//
// The window's own limit cannot produce this case, only the shared budget can: the
// window's own queued bytes make input.pending() true, that is a term in the gate,
// and so the loop never reaches the push. Anything the window itself is holding
// stops the loop earlier than the push does.
//
// The reason assertion is what proves where the expiry came from, and it does not
// rest on anything outside this file. Each of the four sweep and expire call sites
// in drainLoop passes a different reason -- the gate not-idle or scan-refused, the
// delivery loop outranked, this arm no-room -- so no-room has exactly one producer.
// Had the gate been true on the poll that expired this payload, the test would fail
// on the reason rather than pass for the wrong cause.
//
// The budget is held by the test rather than by a second window, which is where it
// would come from in a running daemon. A window holding it is not a fixed amount:
// its writer releases bytes as its PTY absorbs them, so the room left moves, and a
// payload sized against a reading taken a moment earlier is delivered on some polls
// and refused on others. An earlier version of this test did that and passed
// without -race and failed with it. reserve is the same call the other window's
// push would make, and nothing here releases it.
func TestAnInjectionRefusedByASharedBudgetStillHitsItsDeadline(t *testing.T) {
	const free = 1024
	budget := NewInputBudget(1 << 20)
	if !budget.reserve(budget.limit - free) {
		t.Fatalf("a fresh budget of %d refused a reservation of %d", budget.limit, budget.limit-free)
	}

	stall := uint64(600)
	w := startNotReading(t, WindowOpts{
		Name:           "starved",
		InputBudget:    budget,
		StallTimeoutMS: &stall,
	}, idlePrompt)
	waitForPrompt(t, w)

	// Bigger than the room left in the budget, far smaller than either bound on its
	// own, so this is the queue-full case and not the too-large one. Checked before
	// the enqueue, not after: a payload that was simply delivered raises used by its
	// own length, which makes a reading taken afterwards look exactly like a refusal.
	payload := strings.Repeat("z", 64<<10)
	if room := budget.limit - budget.used.Load(); room >= int64(len(payload)) {
		t.Fatalf("premise: %d bytes of budget are free and the payload is %d, so it would simply be delivered",
			room, len(payload))
	}
	if int64(len(payload)) > budget.limit || len(payload) >= DefaultInputQueueLimit {
		t.Fatalf("premise: a %d-byte payload is past a bound on its own, so this is the too-large case", len(payload))
	}
	if w.input.pending() {
		t.Fatal("premise: the window has input of its own queued, so the gate stops the loop before the push and this case is not reached")
	}

	msg, err := w.InjectQueue().Enqueue(payload, 0)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, func() bool { return w.InjectQueue().Len() == 0 },
		"a payload the shared budget refused waited past its deadline: the requeue arm never swept, and it is the only branch a push is reached from")

	expiries := w.InjectExpiries()
	if len(expiries) != 1 {
		t.Fatalf("recorded %+v, want one expiry", expiries)
	}
	if got := expiries[0]; got.Message.ID != msg.ID || got.Reason != expiryNoRoom {
		t.Errorf("recorded %s reason %q, want %s reason %q", got.Message.ID, got.Reason, msg.ID, expiryNoRoom)
	}
	if waited := expiries[0].ExpiredAt - msg.CreatedAt; waited < int64(stall) {
		t.Errorf("expired after %d ms with a %d ms deadline: this payload could have been delivered once the budget freed up, so dropping it early is a delivery lost",
			waited, stall)
	}
	// Not typed: the bytes were refused, so nothing of this payload is queued for
	// the program or counted against the budget.
	if w.input.pending() {
		t.Error("the starved window has input queued, so some of the refused payload was accepted after all")
	}
}
