package shelld

import "testing"

// reflowStepper differences the process-global counters two ways for one test:
// once per step, so a change is attributable to the request that caused it, and
// once over the whole test, so a mistake in one step's expectation cannot be
// cancelled by a mistake in another's. Shared by both counter tests because the
// difference between them is the window's configuration, not the accounting.
//
// Neither caller may call t.Parallel(): paused parallel tests are resumed only
// after the serial ones finish, and a difference across one step is attributable
// to that step only while no other test is reflowing anything.
func reflowStepper(t *testing.T, w *Window) (
	step func(what string, reflows, cells uint64, want [2]uint16, do func()),
	total func(wantSteps uint64, why string),
) {
	t.Helper()
	reflows0, cells0 := ReflowStats()
	var wantReflows, wantCells uint64

	step = func(what string, reflows, cells uint64, want [2]uint16, do func()) {
		t.Helper()
		r0, c0 := ReflowStats()
		do()
		r1, c1 := ReflowStats()
		if got := r1 - r0; got != reflows {
			t.Fatalf("%s: %d reflows, want %d", what, got, reflows)
		}
		if got := c1 - c0; got != cells {
			t.Fatalf("%s: %d cells, want %d", what, got, cells)
		}
		if got := size(w); got != want {
			t.Fatalf("%s: window is %v, want %v", what, got, want)
		}
		wantReflows += reflows
		wantCells += cells
	}

	total = func(wantSteps uint64, why string) {
		t.Helper()
		if r, c := ReflowStats(); r-reflows0 != wantReflows || c-cells0 != wantCells {
			t.Fatalf("over the whole test: %d reflows / %d cells, want %d / %d",
				r-reflows0, c-cells0, wantReflows, wantCells)
		}
		if wantReflows != wantSteps {
			t.Fatalf("the steps expect %d reflows in total, want %d: %s",
				wantReflows, wantSteps, why)
		}
	}
	return step, total
}

// The counter's whole claim is stated in reflow_stats.go: a count taken inside
// setEffectiveSize counts effective-size CHANGES and never resize requests. E1
// differences these counters across a resize storm to answer "how many reflows
// did that workload actually cause", and `resizes_sent` — the number of
// requests — is precisely the number E1 exists to distrust. A counter one line
// earlier, in RecomputeEffectiveSize or in Window.Resize, would answer with the
// wrong quantity while looking right on every test that only resizes to new
// geometries.
//
// So every step below is either a request that must not count, or a request
// whose effective size differs from the one asked for. The two that discriminate
// hardest are the 10x2 and 15x3 pins: they are different requests that both
// floor to 20x5, so a counter in Window.Resize sees two changes where the grid
// saw one, and no equality check on the request can tell them apart.
//
// This test pins, and every reflow it counts happens with a pin live; that is a
// stated limit and not a complete account of the counter. The configuration the
// metric's own scenario produces — attachers only, no pin at all — is
// TestReflowCounterCountsUnpinnedAttacherChanges.
func TestReflowCounterSkipsNoOps(t *testing.T) {
	w := NewFakeWindow("w")
	var a *Attacher
	t.Cleanup(func() {
		if a != nil {
			a.Release()
		}
	})

	// The premise the first two steps rest on: a fresh fake window has no
	// effective size while its vt grid is already 80x24, so "the size is left
	// alone" is a claim with content. Asserted rather than described, because a
	// creation path that sized the window would not leave those steps holding for
	// a different reason — it would make step 1's own size expectation wrong, and
	// a broken premise should read as a broken premise.
	if got := size(w); got != [2]uint16{0, 0} {
		t.Fatalf("premise: a fresh fake window reports %v, want 0x0", got)
	}
	if c, r := w.vt.Cols(), w.vt.Lines(); c != 80 || r != 24 {
		t.Fatalf("premise: the fake window's grid is %dx%d, want 80x24", c, r)
	}

	step, total := reflowStepper(t, w)

	// Neither a pin nor an attacher: the recompute returns early through its
	// `default` branch and the size is left alone. A count placed at the top of
	// the recompute would count this.
	step("recompute with no pin and no attacher", 0, 0, [2]uint16{0, 0}, func() {
		if err := w.RecomputeEffectiveSize(); err != nil {
			t.Fatal(err)
		}
	})
	step("unpin an unpinned window", 0, 0, [2]uint16{0, 0}, w.UnpinSize)

	step("first pin 100x30", 1, 100*30, [2]uint16{100, 30}, func() {
		if err := w.Resize(100, 30); err != nil {
			t.Fatal(err)
		}
	})
	step("the same pin three more times", 0, 0, [2]uint16{100, 30}, func() {
		for i := 0; i < 3; i++ {
			if err := w.Resize(100, 30); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Below the 20x5 floor: the request is honoured as a pin but the grid becomes
	// 20x5, so the cells recorded have to be the grid's and not the request's.
	step("pin 10x2, floored to 20x5", 1, minEffectiveCols*minEffectiveRows, [2]uint16{minEffectiveCols, minEffectiveRows}, func() {
		if err := w.Resize(10, 2); err != nil {
			t.Fatal(err)
		}
	})
	step("pin 15x3, a different request that floors to the same 20x5", 0, 0, [2]uint16{minEffectiveCols, minEffectiveRows}, func() {
		if err := w.Resize(15, 3); err != nil {
			t.Fatal(err)
		}
	})

	step("back to 100x30", 1, 100*30, [2]uint16{100, 30}, func() {
		if err := w.Resize(100, 30); err != nil {
			t.Fatal(err)
		}
	})

	// An attacher smaller than the pin loses on both axes, so registering it and
	// then shrinking it further changes nothing: max(pin, min over attachers).
	step("register an attacher below the pin", 0, 0, [2]uint16{100, 30}, func() {
		a = w.RegisterAttacher(80, 24)
	})
	step("shrink that attacher further", 0, 0, [2]uint16{100, 30}, func() {
		a.SetSize(60, 20)
	})

	step("grow the attacher past the pin", 1, 120*40, [2]uint16{120, 40}, func() {
		a.SetSize(120, 40)
	})
	step("set the attacher to the size it already has", 0, 0, [2]uint16{120, 40}, func() {
		a.SetSize(120, 40)
	})

	// A shrink is a reflow too, and Release is the path that produces it.
	step("release the attacher, dropping back to the pin", 1, 100*30, [2]uint16{100, 30}, func() {
		a.Release()
		a = nil
	})

	total(5, "14 requests reached the window and 5 of them changed its size")
}

// The counter's placement is only half its claim. The other half is that it
// counts on the windows the metric will actually be read from, and every reflow
// TestReflowCounterSkipsNoOps counts happens with a pin live — so a count guarded
// on w.pinned keeps that test green while publishing zero for the scenario E1
// runs. `resizestorm` never pins: it creates its windows with POST /windows and
// drives their size entirely through attachers (ws `resize` frames reaching
// Attacher.SetSize), so an unpinned, attacher-driven window is not an edge case
// here, it is the only configuration the metric's own workload produces.
//
// So this test pins nothing. It asserts that at the start and at the end,
// because a stray pin would make it a second copy of the test above.
//
// The steps still discriminate changes from requests, now with the pin term of
// max(pin, min over attachers) absent throughout: two different SetSize requests
// that floor to the same 20x5, an attacher whose declared size loses the min on
// both axes, and a release that grows the window rather than shrinking it.
func TestReflowCounterCountsUnpinnedAttacherChanges(t *testing.T) {
	w := NewFakeWindow("w")
	var a, b *Attacher
	t.Cleanup(func() {
		for _, x := range []*Attacher{a, b} {
			if x != nil {
				x.Release()
			}
		}
	})
	unpinned := func(when string) {
		t.Helper()
		if c, r, ok := w.PinnedSize(); ok {
			t.Fatalf("%s the window is pinned to %dx%d; this test's whole point is that "+
				"nothing here pins", when, c, r)
		}
	}
	unpinned("before the first step")

	step, total := reflowStepper(t, w)

	// 0x0 to 200x50: the first attacher alone sets the size, with no pin to lose
	// to. A count reached only through the pin sees nothing here.
	step("register the only attacher at 200x50", 1, 200*50, [2]uint16{200, 50}, func() {
		a = w.RegisterAttacher(200, 50)
	})

	// The 20x5 floor applies to the attacher term as much as to a pin, and it is
	// the grid's size that must be recorded, not the request's.
	step("shrink it to 10x2, floored to 20x5", 1, minEffectiveCols*minEffectiveRows,
		[2]uint16{minEffectiveCols, minEffectiveRows}, func() {
			a.SetSize(10, 2)
		})
	step("a different request that floors to the same 20x5", 0, 0,
		[2]uint16{minEffectiveCols, minEffectiveRows}, func() {
			a.SetSize(15, 3)
		})

	step("grow it to 120x30", 1, 120*30, [2]uint16{120, 30}, func() {
		a.SetSize(120, 30)
	})

	// A second attacher larger on both axes loses the min on both, so it changes
	// nothing; shrinking it below the first on one axis only changes that axis.
	step("register a second attacher, larger on both axes", 0, 0, [2]uint16{120, 30}, func() {
		b = w.RegisterAttacher(200, 50)
	})
	step("shrink the second attacher below the first on rows only", 1, 120*10,
		[2]uint16{120, 10}, func() {
			b.SetSize(200, 10)
		})

	// Release is a reflow path in both directions: dropping the smaller attacher
	// raises the min back to the survivor's declaration.
	step("release the second attacher, growing back", 1, 120*30, [2]uint16{120, 30}, func() {
		b.Release()
		b = nil
	})

	// With no attachers and no pin the recompute has no term to apply, so the size
	// freezes where it was rather than falling to the floor.
	step("release the last attacher, freezing the size", 0, 0, [2]uint16{120, 30}, func() {
		a.Release()
		a = nil
	})

	unpinned("after the last step")
	total(5, "8 requests reached the window with no pin ever set, and 5 of them changed its size")
}
