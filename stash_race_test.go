//go:build linux || darwin

// This file matches the constraint on input_pty_test.go, which defines the
// startInputEcho and receivedInput helpers below: they are the only way to see
// what actually reached the program, and they need a pty. Without the constraint
// the test package does not compile for windows, which it otherwise does -- there
// is a window_windows_test.go.

package shelld

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// stashRaceRounds drives rounds of "one keystroke racing the stash's own
// termination" against w, and reports how many of those bytes the stash handed back
// and how many the program actually read.
//
// write is the input path under test. It runs in its own goroutine, released at the
// same instant as the Complete it races, and must either divert its byte into the
// stash or type it at the program -- which is the whole property under test, so it
// is the part each caller supplies.
//
// Both tests below share this harness deliberately. The control is only a control if
// it exercises the same rounds, the same release mechanism and the same accounting as
// the test it vouches for; two similar-looking harnesses would let the control pass
// while the real test's harness was the broken one.
func stashRaceRounds(t *testing.T, w *Window, rounds int, write func() error) (handedBack, typed int, settled bool) {
	t.Helper()
	for i := range rounds {
		w.Stash().Begin("")

		// Both goroutines are spawned, then released together, so the write and the
		// Complete overlap instead of the first finishing before the second is
		// scheduled.
		start := make(chan struct{})
		var wg sync.WaitGroup
		var writeErr error
		var back string
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			writeErr = write()
		}()
		go func() {
			defer wg.Done()
			<-start
			back = w.Stash().Complete()
		}()
		close(start)
		wg.Wait()

		if writeErr != nil {
			t.Fatalf("round %d: the write reported %v; these tests are about bytes accepted with a nil error", i, writeErr)
		}
		handedBack += strings.Count(back, "x")
		// Complete closes the stash unconditionally and both goroutines are joined
		// above, so the stash is always closed by here. This is an assertion and not
		// a drain: what it replaced was a second Complete guarded by IsActive, whose
		// comment claimed Complete could have run before the write diverted and left
		// the stash open with the byte inside it. That state does not exist -- a
		// write that found the stash closed typed its byte rather than leaving it
		// anywhere -- so the branch never ran and the count it added was dead. Kept
		// as a check rather than deleted so that a Complete which stopped closing
		// unconditionally would be caught here instead of silently reopening the
		// two-Complete path.
		if w.Stash().IsActive() {
			t.Fatalf("round %d: the stash is still open after Complete was joined", i)
		}
	}

	wantTyped := rounds - handedBack

	// receivedInput returns as soon as its own 5s deadline passes, so a short count
	// there is ambiguous between "lost" and "not flushed yet". Wait for the count to
	// stop changing instead, and report whether it settled rather than expired -- an
	// expired wait would make a slow pipe look like a dropped byte.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		n := bytes.Count(receivedInput(t, w, wantTyped), []byte("x"))
		if n == typed && n > 0 {
			settled = true
			break
		}
		typed = n
		if n >= wantTyped {
			settled = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return handedBack, typed, settled
}

// A keystroke racing the stash's own termination must end up somewhere: either
// handed back by Complete, or typed at the program. The input paths used to ask
// IsActive and then hand the bytes to AccumulateInput as two acquisitions, so a
// Complete landing in between closed the stash, the append was discarded by
// AccumulateInput's own active re-check, and the caller -- already committed to
// the stash's side of the branch -- returned nil for a keystroke that was never
// typed and is no longer held anywhere.
//
// The assertion is a conservation count: every byte written is either returned by
// Complete or read by the program, and the sum has to come out whatever the
// scheduler did.
//
// What this test's own green does NOT establish is that it can see the defect. The
// window the defect needs is the few tens of nanoseconds between two uncontended
// acquisitions, and nothing here forces it. A run in which Complete always lands
// outside that window -- before the IsActive or after the append -- conserves every
// byte under the OLD code too, and would be green against the bug. So the green is
// only half the evidence, and on its own it says only that the fixed code conserves
// bytes. TestTheTwoStepShapeLosesKeystrokes below is the other half, and it is a
// test rather than a note in a comment for exactly that reason.
//
// The sensitivity was measured, and it is worth knowing before treating a green here
// as decisive. Against the pre-fix window.go this test lost 1, 1, 2 keystrokes in
// three runs, and an earlier version of this harness lost 2, 3, 2 in three of its
// own: detection in 6 of 6. The two are reported separately because the rate is a
// property of the harness, not of the defect, and this file ships the first one. Its
// mean is 1.33 losses per run, so its per-run miss rate is on the order of e^-1.33,
// about one run in four; the earlier harness's mean of 2.33 gives one in ten. Three
// runs each cannot pin either number more tightly than that. It is a tripwire that
// fired every time it was tried, not a deterministic guard. The instrument matters as
// much as the figure: 24-core Linux box, plain `go test` at default GOMAXPROCS, 5000
// rounds. Fewer cores or a loaded box moves the rate, and it cannot be re-measured
// once the defect is gone, which is why it is recorded here.
//
// `-race` is the more sensitive mode and by a wide margin. The control below lost 131
// of 5000 under `-race` against a plain median of 8.5 -- the detector instruments
// every access, which widens the very gap the defect needs. Note what that figure is:
// the control under `-race`, not the real pre-fix path under `-race`, which was never
// measured. So it indicates the direction for this test and does not quantify it.
// handedBack moves the same way, from 11 to 18 of 5000 plain to roughly 2030 to 2085
// under `-race` over four runs, which is also what keeps the gate below far from its
// bound.
//
// handedBack is logged, and bounded at one end only. Every round being typed is not
// a broken run: it asserts that all 5000 arrived, which is the non-vacuous
// direction. The other end is the empty one -- if every round were handed back,
// wantTyped would be 0 and the comparison below would say only that nothing was
// typed either, which is not what this test is for -- so only that end fails. No
// value in between is weak in that way: for any handedBack strictly inside the range
// the comparison is exact conservation, and growing handedBack costs detection power
// rather than content. Measured handedBack on this box is 11 to 18 of 5000 plain and 2030 to 2085
// under -race, so the end runs actually approach is the end that is allowed.
func TestStashRaceLosesNoKeystroke(t *testing.T) {
	w := startInputEcho(t, "", WindowOpts{})
	const rounds = 5000

	handedBack, typed, settled := stashRaceRounds(t, w, rounds, func() error {
		return w.WriteInput([]byte("x"))
	})
	wantTyped := rounds - handedBack

	t.Logf("LIVENESS: rounds=%d handedBack=%d wantTyped=%d typed=%d settled=%v", rounds, handedBack, wantTyped, typed, settled)
	if !settled {
		t.Fatalf("LIVENESS: the byte count never stopped changing, so %d vs %d says nothing about loss", typed, wantTyped)
	}
	if handedBack == rounds {
		t.Fatalf("LIVENESS: every round was handed back (handedBack=%d of %d), so wantTyped is 0 and the comparison below only checks that nothing was typed either, which is not the property this test is for",
			handedBack, rounds)
	}
	if typed != wantTyped {
		t.Errorf("wrote %d keystrokes; the stash handed back %d, so %d should have reached the program, but it read %d.\n"+
			"%d keystroke(s) were accepted with a nil error, diverted into a stash that closed before the append, and never typed.",
			rounds, handedBack, wantTyped, typed, wantTyped-typed)
	}
}

// TestTheTwoStepShapeLosesKeystrokes is a negative control on the test above. It is
// not a test of shipped behaviour -- nothing in the tree does what it does any more.
// It reproduces the two-acquisition shape the fix removed, drives it through the same
// harness and the same accounting, and requires that keystrokes go missing.
//
// It exists because the conservation test's green is not self-validating. That test's
// detection margin against the real defect was one to three lost keystrokes in 5000
// across six measured runs: real, but thin enough that an unrelated change to
// scheduling, to pty buffering or to the capture helper could take it to zero without
// anyone noticing the test had gone blind. A green conservation test next to a green
// control says nothing. A green conservation test next to a RED control is the pair
// that means something, and this is the half that fails when the harness stops being
// able to see the defect at all.
//
// So a failure here is not a bug in shelld, and it should not be fixed by relaxing
// the floor. It means this file can no longer detect what it was written for, and the
// conservation test above should not be trusted until that is understood.
//
// The floor is one keystroke, because one is the minimum that was measured against
// the real pre-fix path: losses of 1, 1 and 2 in three runs of the current harness
// against the pre-fix window.go, and 2, 3 and 2 in three runs of an earlier version.
// It is deliberately not the mean -- a floor above the minimum observed is a flake,
// and a control has to be reliable in the direction it asserts or it is just another
// test. A floor of 1 is not flake-free either, it is just much less likely to fire: at
// this control's mean of about 8 losses a zero-loss run is somewhere between one in
// several hundred and one in a few thousand, since the losses are overdispersed and
// that pushes the zero probability above e^-8. If it does fire once, read the next
// paragraph before concluding anything.
//
// Be careful what this control does and does not vouch for. Omitting the IsAlive
// check and the Touch means this write reaches s.mu sooner than the real typeInput
// did, so it straddles the window more often and is USUALLY the more sensitive of the
// two: loss over eight runs on this box was 2, 4, 5, 8, 9, 9, 11 and 15 of 5000,
// against the 1 to 2 the real pre-fix path gave. But the spread is wide and its low
// end is not above the real path's high end, so "more sensitive" is a tendency across
// runs and not a property of any one run -- do not read a green here as a margin this
// control holds in reserve.
//
// What a green here does establish is that the harness can still OBSERVE a lost
// keystroke: the rounds, the release, the pty capture, the settle loop and the
// accounting all still work. It does not establish that a window as narrow as the real
// one would still be caught. It is a guard on the instrument, not on the margin. The
// margin itself cannot be re-measured once the defect is gone, which is why the
// numbers above are recorded rather than recomputed.
//
// Rounds cannot simply be raised to widen the margin, and the binding limit is not
// the line count receivedInput asks Capture for. It is the window's scrollback
// capacity: defaultScrollbackCapacity (scrollback.go) is 10_000 lines, passed at
// window.go's NewScrollbackWithBudget, and eviction happens at write time, so past
// that the oldest lines are gone before any capture can ask for them. The two 10_000s
// are a coincidence -- raising Capture's argument buys nothing. The helper emits one
// line per read rather than per byte, and a read can return several bytes, which only
// ever produces fewer lines, so the ceiling argument is conservative either way.
func TestTheTwoStepShapeLosesKeystrokes(t *testing.T) {
	w := startInputEcho(t, "", WindowOpts{})
	const rounds = 5000

	handedBack, typed, settled := stashRaceRounds(t, w, rounds, func() error {
		// typeInput's body as it stood before the fix, reproduced rather than
		// referenced so that fixing the real one cannot silently fix this. The
		// IsAlive check and the inputTracker.Touch that precede it in the real
		// method are omitted: neither touches s.mu, so neither participates in the
		// race, and the window here is alive by construction.
		if w.stash.IsActive() {
			w.stash.AccumulateInput([]byte("x"))
			return nil
		}
		return w.input.push(inputChunk{data: []byte("x")})
	})
	wantTyped := rounds - handedBack

	t.Logf("CONTROL: rounds=%d handedBack=%d wantTyped=%d typed=%d settled=%v lost=%d",
		rounds, handedBack, wantTyped, typed, settled, wantTyped-typed)
	if !settled {
		t.Fatalf("CONTROL: the byte count never stopped changing, so %d vs %d says nothing about loss and this control proves nothing",
			typed, wantTyped)
	}
	if lost := wantTyped - typed; lost < 1 {
		t.Errorf("CONTROL: the two-step shape lost %d keystrokes of %d written; want at least 1.\n"+
			"This harness can no longer see the defect TestStashRaceLosesNoKeystroke exists to catch, so that test's green is not evidence. "+
			"Do not lower this floor: find out what stopped the loss from being observable.",
			lost, rounds)
	}
}
