package shelld

import (
	"math"
	"testing"
	"time"
)

// msDuration is the conversion four configured millisecond fields now share, so
// its boundary is asserted here once rather than four times. The tables per field
// do not become redundant: what they add is that the field reaches this function
// at all, which is the part that was wrong. SendKeysDelayMS was already clamped on
// SendKeys' path and multiplied raw on PasteWith's, and no test of the conversion
// could have seen that.
//
// The values past the limit are measured, not derived. Past the range the product
// depends on the input's low bits, so which of the two failures a given input
// produces does not follow from its size: maxStallMS+1 is negative and
// 18446744073710, five million times larger, is positive and sub-millisecond.
func TestMsDurationClampsInsteadOfWrapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		// Zero and ordinary values are untouched: every field's documented meaning
		// for them predates the clamp, and zero is the opt-out for two of the four.
		{"zero converts to zero", 0, 0},
		{"an ordinary count", 250, 250 * time.Millisecond},
		{"the largest value that converts", maxStallMS, time.Duration(maxStallMS) * time.Millisecond},
		// Unclamped: about -2562047h.
		{"just past the limit", maxStallMS + 1, math.MaxInt64},
		// Unclamped: 448.384µs. The one case that stays positive, so it is the one
		// a `> 0` check at a consumer cannot catch.
		{"a value that wraps to a usable-looking duration", 18_446_744_073_710, math.MaxInt64},
		// Unclamped: -1ms.
		{"the largest uint64", math.MaxUint64, math.MaxInt64},
	} {
		if got := msDuration(tc.ms); got != tc.want {
			t.Errorf("%s: msDuration(%d) = %v, want %v", tc.name, tc.ms, got, tc.want)
		}
	}
	// Monotone across the boundary, in both wrap directions. This is the property
	// the wrap broke and the one every consumer reads, whichever side of zero its
	// own comparison sits on.
	for _, ms := range []uint64{maxStallMS + 1, 18_446_744_073_710, math.MaxUint64} {
		if msDuration(ms) < msDuration(maxStallMS) {
			t.Errorf("msDuration(%d) = %v is shorter than msDuration(%d) = %v", ms, msDuration(ms), maxStallMS, msDuration(maxStallMS))
		}
	}
}

// clampWindow is a window with a live input queue that nothing drains, so a push
// can be read back as the chunks it queued. It has no PTY and starts no writer:
// writeInput is what waits out a gap, and what these tests queue is a gap of some
// 292 years.
//
// Its own budget rather than the default, because nothing here ever calls done:
// the bytes stay reserved for the life of the process, and on the shared budget
// that is a debit against every other test in the package.
func clampWindow(t *testing.T, sendKeysDelayMS uint64) *Window {
	t.Helper()
	cfg := DefaultInjectConfig()
	cfg.SendKeysDelayMS = sendKeysDelayMS
	w := newWindowState("clamp", cfg, 80, 24, 0)
	w.input = newInputQueue(0, NewInputBudget(1<<20))
	w.alive = true
	t.Cleanup(func() { w.input.close(w.deadErr()) })
	return w
}

func queuedChunks(w *Window) []inputChunk {
	w.input.mu.Lock()
	defer w.input.mu.Unlock()
	return append([]inputChunk(nil), w.input.chunks...)
}

// The two paths that write text and then Enter read the same SendKeysDelayMS, and
// until this change only one of them clamped it. So the assertion is on the gap
// each path actually queued, not on the conversion: a test of enterDelay was
// already green while PasteWith multiplied the same field raw.
//
// Both paths are exercised against the same window, and the last check compares
// them to each other. That comparison is what would have caught the original
// divergence without anyone having to guess which of the two sites was wrong.
func TestPasteAndSendKeysQueueTheSameClampedEnterGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		{"the shipped default", 50, 50 * time.Millisecond},
		// Past MaxInputGap the window holds MaxInputGap, as shelld-rs's enter_gap
		// does, and the wrapping rows below land there too.
		{"past MaxInputGap", 600_000, MaxInputGap},
		// Negative unclamped, which writeInput's `c.gap > 0` reads as no gap at all:
		// the Enter lands immediately behind the text, which is the outcome the
		// delay exists to prevent.
		{"a value that wraps negative", maxStallMS + 1, MaxInputGap},
		// Positive unclamped, and only 448.384µs of it. This row is why the checks
		// below compare against the clamped duration rather than asking whether the
		// gap is positive: this input passes a positivity check while wrapped.
		{"a value that wraps to a usable-looking gap", 18_446_744_073_710, MaxInputGap},
		{"the largest uint64", math.MaxUint64, MaxInputGap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := clampWindow(t, tc.ms)

			// "raw" rather than the window's mode: a bracketed paste puts the CR in
			// the same write as the text and has no gap to get wrong, so the
			// unbracketed path is the one under test and the mode is pinned instead
			// of inherited.
			if err := w.PasteWith("hello", PasteOpts{Mode: "raw"}); err != nil {
				t.Fatal(err)
			}
			if err := w.SendKeys("hello", true, nil); err != nil {
				t.Fatal(err)
			}

			got := queuedChunks(w)
			if len(got) != 4 {
				t.Fatalf("queued %d chunks, want 4 (text and Enter from each path): %+v", len(got), got)
			}
			// The Enter chunks, identified by their content rather than by position,
			// so a path that stopped queueing Enter separately fails here instead of
			// silently moving the index the gap is read from.
			for i, c := range got {
				wantEnter := i == 1 || i == 3
				if isEnter := string(c.data) == "\r"; isEnter != wantEnter {
					t.Fatalf("chunk %d is %q, so the gap is not where this test reads it", i, c.data)
				}
			}
			if got[0].gap != 0 || got[2].gap != 0 {
				t.Errorf("the text chunks carry gaps (%v, %v), so the delay is applied before the text rather than before the Enter", got[0].gap, got[2].gap)
			}
			if got[1].gap != tc.want {
				t.Errorf("PasteWith queued a gap of %v for SendKeysDelayMS=%d, want %v", got[1].gap, tc.ms, tc.want)
			}
			if got[3].gap != tc.want {
				t.Errorf("SendKeys queued a gap of %v for SendKeysDelayMS=%d, want %v", got[3].gap, tc.ms, tc.want)
			}
			if got[1].gap != got[3].gap {
				t.Errorf("the two paths disagree on the same field: PasteWith %v, SendKeys %v", got[1].gap, got[3].gap)
			}
		})
	}
}

// DebounceMS is the one of the four fields whose wrap is not a shorter wait but an
// inverted verdict, so this asserts the verdict and not only the duration. A
// negative debounce makes IsQuiet true the instant after a keystroke, and IsQuiet
// is drainLoop's gate: the window a human is typing into is the window an
// injection is typed over.
func TestInputTrackerDebounceClampsSoAKeystrokeStillCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		// Zero is not a zero window here, it is the documented default, and the
		// clamp must not reach past it.
		{"zero is the 500ms default", 0, 500 * time.Millisecond},
		{"the shipped default", 500, 500 * time.Millisecond},
		{"just past the limit", maxStallMS + 1, math.MaxInt64},
		{"a value that wraps to a usable-looking window", 18_446_744_073_710, math.MaxInt64},
		{"the largest uint64", math.MaxUint64, math.MaxInt64},
	} {
		if got := NewInputTracker(tc.ms).debounce; got != tc.want {
			t.Errorf("%s: NewInputTracker(%d).debounce = %v, want %v", tc.name, tc.ms, got, tc.want)
		}
	}

	// The control comes first: it shows this fixture can reach a quiet verdict at
	// all, so the "not quiet" below is the debounce holding and not a tracker that
	// never goes quiet.
	short := NewInputTracker(1)
	short.Touch()
	time.Sleep(20 * time.Millisecond)
	if !short.IsQuiet() {
		t.Fatal("a 1ms debounce is not quiet 20ms after a keystroke, so the assertion below cannot be read as the clamp working")
	}

	// maxStallMS+1 and MaxUint64 both, because they wrap to different negatives and
	// the comparison is against a clock reading, not against the constant.
	for _, ms := range []uint64{maxStallMS + 1, math.MaxUint64} {
		tr := NewInputTracker(ms)
		tr.Touch()
		if tr.IsQuiet() {
			t.Errorf("NewInputTracker(%d) reports quiet immediately after a keystroke (debounce %v), so drainLoop would type over a human", ms, tr.debounce)
		}
	}
}

// The cooldown is the field whose wrap is visible only through the loop that reads
// it, so this runs the real drainLoop. What a negative cooldown costs is the
// throttle itself: `cooldown > 0` is false, the poll continues, and queued
// payloads are typed one 200ms poll apart into a window that is meant to be given
// two seconds of quiet between deliveries.
//
// The arms differ in one factor and each is the other's control. Without the
// zero arm, "the second payload did not arrive" is also what a fixture that can
// only ever deliver one looks like; without the wrapped arm, the throttle is
// never tested at all.
func TestDeliveryCooldownClampsSoASecondInjectionStaysThrottled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ms        uint64
		throttled bool
	}{
		{"no cooldown delivers both", 0, false},
		{"a cooldown that wraps negative", math.MaxUint64, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultInjectConfig()
			cfg.DeliveryCooldownMS = tc.ms
			// No deadline, so the payload that waits out the cooldown waits rather
			// than expiring: an expiry would empty the queue and make the throttled
			// arm green for the wrong reason.
			cfg.StallTimeoutMS = 0
			w, delivered := drainWindow(t, cfg)
			w.scrollback.Write([]byte(idleScreen))

			for _, payload := range []string{"one", "two"} {
				if _, err := w.injectQueue.Enqueue(payload, 0); err != nil {
					t.Fatal(err)
				}
			}
			go w.drainLoop()

			pollUntil(t, 3*time.Second, func() bool { return len(delivered()) >= 1 })
			if got := delivered(); len(got) == 0 {
				t.Fatal("nothing was delivered at all, so neither arm measures the cooldown")
			} else if got[0] != "one\r" {
				t.Fatalf("first delivery was %q, want \"one\\r\"", got[0])
			}

			if tc.throttled {
				// Comfortably past the 200ms poll the loop would otherwise deliver
				// on: unclamped, the second payload lands on the first poll after
				// the first delivery.
				pollUntil(t, 1500*time.Millisecond, func() bool { return len(delivered()) >= 2 })
				if got := delivered(); len(got) != 1 {
					t.Fatalf("delivered %q: the clamped cooldown did not hold the second payload, so a caller asking for the longest throttle got none", got)
				}
				// Still queued rather than gone, which is what says it is waiting on
				// the cooldown and not on something that consumed it.
				if n := w.injectQueue.Len(); n != 1 {
					t.Errorf("%d payloads left in the queue, want 1 waiting out the cooldown", n)
				}
				return
			}
			pollUntil(t, 3*time.Second, func() bool { return len(delivered()) >= 2 })
			if got := delivered(); len(got) != 2 {
				t.Fatalf("delivered %q, want both payloads: with no cooldown nothing here should hold the second one, and the arm above rests on that", got)
			}
		})
	}
}

// The stall deadline's clamp at the consumer rather than at the conversion, which
// is the only place the cost of getting it wrong is visible: a caller writing an
// enormous StallTimeoutMS to mean "do not give up on this" has its payload thrown
// away instead.
//
// 18446744073710 is the input for it. maxStallMS+1 and math.MaxUint64 both wrap
// negative, and a negative deadline is ExpireOlderThan's opt-out, so unclamped
// they leave the payload alone and this test could not tell them from the fix.
// This one wraps to 448.384µs, so unclamped the first poll finds the payload older
// than its deadline and expires it.
//
// The arms are the two directions of the same assertion because the claim in the
// clamped arm is a SURVIVAL -- nothing expired -- and a survival is a green that a
// fixture which simply cannot expire anything would also print. The ordinary arm
// is that positive control: same fixture, same backdating, an expiry the deadline
// does name, and it has to happen.
func TestAnEnormousStallTimeoutDoesNotDiscardThePayloadItWasMeantToWaitFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ms      uint64
		expires bool
	}{
		// Unclamped this is 448.384µs, which the backdating below is far past.
		{"a deadline that wraps to sub-millisecond", 18_446_744_073_710, false},
		// The control: a deadline the payload really is past, so the sweep this
		// fixture reaches is the same one in both arms.
		{"an ordinary deadline the payload is past", 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultInjectConfig()
			cfg.DeliveryCooldownMS = 0
			cfg.StallTimeoutMS = tc.ms
			w, delivered := drainWindow(t, cfg)
			// A screen the gate refuses, so the payload stays queued and the deadline
			// is what decides its fate. Delivery would empty the queue and both arms
			// would agree for a reason that has nothing to do with the clamp.
			w.scrollback.Write([]byte(refusedScreen))

			msg, err := w.injectQueue.Enqueue("one", 0)
			if err != nil {
				t.Fatal(err)
			}
			// Ten seconds of age: past the 1000ms control deadline and past the
			// 448µs the wrap produces, and nowhere near the 292 years the clamp
			// gives. One number separates all three.
			backdate(w.injectQueue, msg.ID, 10*time.Second)
			go w.drainLoop()

			if tc.expires {
				pollUntil(t, 3*time.Second, func() bool { return len(w.InjectExpiries()) >= 1 })
				if e := w.InjectExpiries(); len(e) != 1 || e[0].Message.ID != msg.ID {
					t.Fatalf("the payload did not expire against a %dms deadline it was 10s past: %+v -- so this fixture cannot expire anything and the other arm proves nothing", tc.ms, e)
				}
				return
			}
			// Several polls at the loop's 200ms cadence, so "not expired" means the
			// sweep ran and declined rather than that it had not run yet.
			pollUntil(t, 1500*time.Millisecond, func() bool { return len(w.InjectExpiries()) >= 1 })
			if e := w.InjectExpiries(); len(e) != 0 {
				t.Fatalf("the payload expired against a deadline of %d ms: %+v -- unclamped that is 448.384µs, so the longest wait a caller can ask for became the shortest", tc.ms, e)
			}
			if n := w.injectQueue.Len(); n != 1 {
				t.Errorf("%d payloads queued, want the one still waiting out its deadline", n)
			}
			if got := delivered(); len(got) != 0 {
				t.Errorf("delivered %q: the gate was meant to refuse this screen, so the deadline is not what this arm measured", got)
			}
		})
	}
}
