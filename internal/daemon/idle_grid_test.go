package daemon

import (
	"testing"
	"time"
)

// Every idle deadline is on the shared grid and no more than idlePoll away,
// whatever phase the loop re-arms at. That is the lost-wake-up repair bound, and
// the bound is the sentence above rather than a document: it is what a
// platform whose timers round deadlines up or read a coarse clock could break,
// so this runs on every OS. The end-to-end repair is in idle_repair_test.go,
// which needs the Unix attach harness.
func TestIdleWaitBound(t *testing.T) {
	// Phases across a period, in steps that are not a divisor of idlePoll,
	// plus the exact boundary and one nanosecond either side of it.
	var phases []time.Duration
	for d := time.Duration(0); d < idlePoll; d += 7*time.Millisecond + 13*time.Microsecond {
		phases = append(phases, d)
	}
	phases = append(phases, idlePoll-time.Nanosecond, idlePoll, idlePoll+time.Nanosecond, 3*idlePoll, 3*idlePoll+time.Nanosecond)
	for _, d := range phases {
		now := idleEpoch.Add(d)
		wait := idleWait(now)
		if wait <= 0 || wait > idlePoll {
			t.Fatalf("phase %v: wait %v, want in (0, %v]", d, wait, idlePoll)
		}
		if off := now.Add(wait).Sub(idleEpoch) % idlePoll; off != 0 {
			t.Fatalf("phase %v: deadline is %v off the grid", d, off)
		}
	}
	// Loops that re-arm at different phases of the same period share one
	// deadline, which is what makes their wake-ups coalesce.
	base := idleEpoch.Add(5 * idlePoll)
	want := base.Add(idleWait(base))
	for _, d := range []time.Duration{time.Microsecond, time.Millisecond, 250 * time.Millisecond, idlePoll - time.Millisecond} {
		at := base.Add(d)
		if got := at.Add(idleWait(at)); !got.Equal(want) {
			t.Fatalf("re-armed %v later: deadline %v, want %v", d, got, want)
		}
	}
}
