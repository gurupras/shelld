package daemon

import (
	"math/rand"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A reused pause timer that fired before it was stopped must not end the
// next wait at once. That only happens with asynchronous timer channels, so
// the check reruns this test in a child with GODEBUG=asynctimerchan=1.
func TestTickerArmIgnoresFiredTimer(t *testing.T) {
	if os.Getenv("SHELLD_TEST_ASYNC_TIMERS") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTickerArmIgnoresFiredTimer$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SHELLD_TEST_ASYNC_TIMERS=1", "GODEBUG=asynctimerchan=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("with asynctimerchan=1: %v\n%s", err, out)
		}
	}
	tk := &ticker{pause: time.NewTimer(time.Millisecond)}
	time.Sleep(20 * time.Millisecond)
	tk.pause.Stop()
	const d = 200 * time.Millisecond
	start := time.Now()
	tk.arm(d)
	<-tk.pause.C
	if waited := time.Since(start); waited < d/2 {
		t.Fatalf("armed for %v, woke after %v", d, waited)
	}
}

// A frame arms no timer of its own; a burst's settle check keeps the timing
// it had when every frame armed a maxInterval tick, so the cursor shows
// maxInterval+cursorSettle after the last frame.
func TestTickerNextWait(t *testing.T) {
	now := idleEpoch.Add(5*idlePoll + 300*time.Millisecond)
	cases := []struct {
		name         string
		first, burst bool
		sinceFrame   time.Duration
		wait         time.Duration
		armed        waitTimer
	}{
		{"first tick", true, false, 0, 0, armFirst},
		{"just after a frame", false, false, time.Millisecond, idlePoll - 300*time.Millisecond, armIdle},
		{"long after a frame", false, false, time.Hour, idlePoll - 300*time.Millisecond, armIdle},
		{"burst, just after its frame", false, true, 5 * time.Millisecond, maxInterval - 5*time.Millisecond, armSettle},
		{"burst, at maxInterval", false, true, maxInterval, minInterval, armSettle},
		{"burst, settle interrupted", false, true, 50 * time.Millisecond, minInterval, armSettle},
	}
	for _, tc := range cases {
		wait, armed := nextWait(tc.first, tc.burst, now.Add(-tc.sinceFrame), now)
		if wait != tc.wait || armed != tc.armed {
			t.Errorf("%s: wait %v armed %d, want %v armed %d", tc.name, wait, armed, tc.wait, tc.armed)
		}
	}
	if maxInterval != 33*time.Millisecond || cursorSettle != 33*time.Millisecond || minInterval != 8*time.Millisecond {
		t.Errorf("settle timing changed: maxInterval %v, cursorSettle %v, minInterval %v", maxInterval, cursorSettle, minInterval)
	}
}

// The frame carrying the tail of a burst must not wait out a quantum the burst
// has already spent. A 500-line command has all its output in the grid a few
// hundred microseconds after its first frame; under a bare minimum spacing that
// tail frame — the one carrying the prompt back — was held until the quantum
// expired, which put frame arrivals on an 8 ms grid and made a 2,000-line
// command finish sooner than a 500-line one.
func TestPaceReleasesBurstTail(t *testing.T) {
	start := time.Unix(0, 0)
	var sched time.Time

	first := start.Add(1620 * time.Microsecond)
	if w := paceWait(sched, first); w != 0 {
		t.Fatalf("first frame of a burst held %v, want 0", w)
	}
	sched = paceSent(sched, first)

	// The rest of the output lands while that frame is being written.
	tail := first.Add(280 * time.Microsecond)
	if w := paceWait(sched, tail); w != 0 {
		t.Fatalf("burst tail held %v, want 0", w)
	}
	sched = paceSent(sched, tail)

	// A third frame is paced, and from the burst's FIRST frame rather than its
	// last: two frames in a quantum have used the quantum.
	if w, want := paceWait(sched, tail), minInterval-tail.Sub(first); w != want {
		t.Fatalf("third frame held %v, want %v", w, want)
	}
}

// The allowance is a burst, not a balance: a client that has been quiet for ten
// seconds may still send only paceBurst frames before it is paced. Without the
// clamp in paceSent an idle client would bank a quantum per idle second and
// then empty its whole backlog at once.
func TestPaceDoesNotBankIdleCredit(t *testing.T) {
	start := time.Unix(0, 0)
	sched := paceSent(time.Time{}, start)
	now := start.Add(10 * time.Second)

	for i := 0; i < paceBurst; i++ {
		if w := paceWait(sched, now); w != 0 {
			t.Fatalf("frame %d after a long idle held %v, want 0", i+1, w)
		}
		sched = paceSent(sched, now)
	}
	if w := paceWait(sched, now); w != minInterval {
		t.Fatalf("frame %d after a long idle held %v, want %v", paceBurst+1, w, minInterval)
	}
}

// The burst allowance must not raise the sustained frame rate, which is the
// whole reason the pace exists. With output always pending, frames must leave
// paceBurst at a time and then settle to one per minInterval, and n of them
// may not be delivered faster than the schedule allows. Driven on a virtual
// clock: the property is about the schedule, not about how fast this machine
// renders.
//
// The window check below is half-open on purpose. A *closed* minInterval
// window legitimately holds paceBurst+1 frames — sends at 0, 0.3 and 8.0 are
// all inside [0, 8] — so a bound stated over closed windows looks violated
// when nothing is wrong. TestPaceHoldsSustainedRateOnRandomArrivals states the
// rate as the leaky bucket's own inequality instead, which has no such edge.
func TestPaceSustainsOneFramePerInterval(t *testing.T) {
	const (
		frames     = 200
		renderCost = 300 * time.Microsecond
	)
	start := time.Unix(0, 0)
	var sched time.Time
	now := start
	sends := make([]time.Time, 0, frames)
	for range frames {
		// Output is always pending, so the loop's wait returns at once and the
		// pace is the only thing spacing the frames.
		now = now.Add(paceWait(sched, now) + renderCost)
		sends = append(sends, now)
		sched = paceSent(sched, now)
	}

	for i, s := range sends {
		j := i
		for j < len(sends) && sends[j].Sub(s) < minInterval {
			j++
		}
		if n := j - i; n > paceBurst {
			t.Fatalf("%d frames inside one %v window at +%v, want at most %d",
				n, minInterval, s.Sub(start), paceBurst)
		}
	}
	if span, floor := sends[frames-1].Sub(start), (frames-paceBurst)*minInterval; span < floor {
		t.Fatalf("%d frames delivered in %v, faster than the sustained rate allows (%v)", frames, span, floor)
	}

	if paceBurst != 2 || paceCredit != minInterval {
		t.Errorf("pace policy changed: paceBurst %d, paceCredit %v, minInterval %v", paceBurst, paceCredit, minInterval)
	}
}

// The sustained rate is what the burst allowance must not break, and the test
// above shows it only for the one arrival pattern continuous output produces.
// This drives the pace with random gaps straddling minInterval and asserts the
// leaky bucket's defining inequality over every pair of sends: n frames span at
// least (n-paceBurst)·minInterval.
//
// That bound is not a tolerance, it is the algorithm's guarantee. paceSent
// advances sched by minInterval per frame and paceWait never releases one more
// than paceCredit early, so sched grows by at least minInterval per send and
// the span of n sends is at least (n-1)·minInterval - paceCredit, which is
// (n-paceBurst)·minInterval exactly. It holds with equality under saturation,
// so a regression that let even one extra frame through anywhere fails it.
func TestPaceHoldsSustainedRateOnRandomArrivals(t *testing.T) {
	// Gaps on both sides of the quantum: some arrive while the bucket is still
	// draining, some after it has refilled.
	gaps := []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 5 * time.Millisecond,
		minInterval, 12 * time.Millisecond, 40 * time.Millisecond}
	for seed := int64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		start := time.Unix(0, 0)
		var sched, lastSend time.Time
		ready := start
		sends := make([]time.Time, 0, 300)
		for k := range 300 {
			// Output becomes available, then waits only for the pace.
			ready = ready.Add(gaps[rng.Intn(len(gaps))])
			wait := paceWait(sched, ready)

			// No frame may be held longer than the bare minimum spacing this
			// replaced: the change must not cost latency anywhere, only save
			// it. Provable, so asserted rather than sampled — a send is
			// released no earlier than sched-paceCredit, which at paceBurst 2
			// puts the previous send within minInterval of the new schedule.
			if k > 0 {
				was := max(minInterval-ready.Sub(lastSend), 0)
				if wait > was {
					t.Fatalf("seed %d send %d: held %v, longer than the old minimum spacing would have (%v)",
						seed, k, wait, was)
				}
			}

			at := ready.Add(wait)
			sends = append(sends, at)
			sched, lastSend = paceSent(sched, at), at
		}
		for i := range sends {
			for j := i + 1; j < len(sends); j++ {
				n := j - i + 1
				if span, floor := sends[j].Sub(sends[i]), time.Duration(n-paceBurst)*minInterval; span < floor {
					t.Fatalf("seed %d: sends %d..%d are %d frames in %v, below the sustained floor %v",
						seed, i, j, n, span, floor)
				}
			}
		}
	}
}
