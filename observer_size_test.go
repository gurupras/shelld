package shelld

import "testing"

// TestObserverSizeNeverEntersEffectiveSize pins the invariant that actually
// makes observers safe: RecomputeEffectiveSize's output is a function of the
// VOTING attachers' sizes, the pin and the current size, and of nothing an
// observer declares.
//
// It exists because two mutants survived the observer suite — removing the
// `if !a.voting { return }` guard in Attacher.SetSize and the `if a.voting`
// guard in Attacher.Release, so that an observer triggers a recomputation.
// Both survived because they are *equivalent* mutants: an observer's SetSize or
// Release changes none of the three inputs above, so the recomputation it
// triggers returns the size already in place. Those two guards are therefore
// performance guards, not correctness guards, and the exclusion inside
// RecomputeEffectiveSize is what carries the correctness.
//
// Two things about the SHAPE of these subtests, both of which a review caught
// after the first version and neither of which is obvious:
//
// Every subtest asserts on BOTH SIDES of the forced recompute. Forcing the
// recompute is what buys power over the exclusion in the walk — but it also
// launders anything SetSize did before the assertion could see it, because the
// recompute restores the voter's or the pin's size. Three of four subtests were
// green under a mutant that made an observer's SetSize write the effective size
// directly (the naive form of confusing a client's crop with the shared size);
// the one that failed did so only because with no voter and no pin the recompute
// hits `default: return nil` and *cannot* restore. So the pre-recompute
// assertion is not redundant with the post-recompute one: they see different
// mutations, and only together do they cover both the exclusion and SetSize.
//
// Every pin here is BELOW some declared size. A pin above every declared size
// makes `max(pin, min over voters)` the pin whatever the min is, so counting an
// observer in the min changes nothing and the subtest cannot fail under the very
// class it was written for. The first version pinned 150x40 over sizes of 100x30
// and below, and its two pinned subtests were decorative.
func TestObserverSizeNeverEntersEffectiveSize(t *testing.T) {
	t.Run("voting attacher present", func(t *testing.T) {
		w := NewFakeWindow("w")
		v := w.RegisterAttacher(100, 30)
		o := w.RegisterObserver(40, 12)
		if got := size(w); got != [2]uint16{100, 30} {
			t.Fatalf("registering an observer moved the size to %v", got)
		}
		o.SetSize(30, 8)
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("an observer's SetSize moved the grid immediately: %v, want [100 30]", got)
		}
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("an observer's declared size reached the grid: %v, want [100 30]", got)
		}
		o.Release()
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("an observer's release moved the grid: %v, want [100 30]", got)
		}
		// The voter still governs, so the assertions above were not passing
		// because nothing can move this window at all.
		v.SetSize(70, 21)
		if got := size(w); got != [2]uint16{70, 21} {
			t.Fatalf("the voter cannot move the window either: %v", got)
		}
	})

	t.Run("observers only", func(t *testing.T) {
		w := NewFakeWindow("w")
		// Reach the observers-only state the way production does, through a
		// voter that leaves. NewFakeWindow's *effective* size is 0x0 until
		// something sets it (only its vt grid is 80x24), and a window that has
		// never had a size is not a state the daemon holds — asserting "0x0 did
		// not move" would be asserting about a shape the system cannot produce.
		w.RegisterAttacher(100, 30).Release()
		before := size(w)
		if before != [2]uint16{100, 30} {
			t.Fatalf("premise: expected a frozen 100x30 after the voter left, got %v", before)
		}
		o := w.RegisterObserver(40, 12)
		o2 := w.RegisterObserver(200, 60)
		o.SetSize(30, 8)
		if got := size(w); got != before {
			t.Errorf("an observer's SetSize moved the grid immediately: %v, want %v", got, before)
		}
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != before {
			t.Errorf("a window with only observers moved to %v, want %v (no voter, so it freezes)", got, before)
		}
		o.Release()
		o2.Release()
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != before {
			t.Errorf("releasing the last observer moved the window to %v, want %v", got, before)
		}
	})

	t.Run("pin below an observer's size, observers only", func(t *testing.T) {
		w := NewFakeWindow("w")
		// 30x8 is below every size the observer declares, so an observer that
		// reached `min over voters` would RAISE the result above the pin. A pin
		// above the observer could not be undercut by construction, which is why
		// the earlier 150x40 version of this subtest could not fail.
		if err := w.Resize(30, 8); err != nil {
			t.Fatal(err)
		}
		o := w.RegisterObserver(40, 12)
		if got := size(w); got != [2]uint16{30, 8} {
			t.Errorf("registering an observer moved the pinned grid: %v, want [30 8]", got)
		}
		o.SetSize(50, 20)
		if got := size(w); got != [2]uint16{30, 8} {
			t.Errorf("an observer's SetSize moved the pinned grid immediately: %v, want [30 8]", got)
		}
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != [2]uint16{30, 8} {
			t.Errorf("an observer raised a window that has only the pin: %v, want [30 8]", got)
		}
	})

	t.Run("pin below both a voter and an observer", func(t *testing.T) {
		w := NewFakeWindow("w")
		v := w.RegisterAttacher(100, 30)
		if err := w.Resize(30, 8); err != nil {
			t.Fatal(err)
		}
		// max(pin 30x8, min over voters 100x30) is the voter's size. Asserted as
		// a premise: if this ever became the pin, the subtest would be measuring
		// a pin that dominates and would stop discriminating.
		if got := size(w); got != [2]uint16{100, 30} {
			t.Fatalf("premise: max(pin, voter) should be [100 30], got %v", got)
		}
		o := w.RegisterObserver(40, 12)
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("registering an observer moved the grid: %v, want [100 30]", got)
		}
		// Between the pin and the voter, so an observer inside `min over voters`
		// moves the result to 35x9 — neither the pin nor the voter's size, so
		// nothing else can produce it.
		o.SetSize(35, 9)
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("an observer's SetSize moved the grid immediately: %v, want [100 30]", got)
		}
		_ = w.RecomputeEffectiveSize()
		if got := size(w); got != [2]uint16{100, 30} {
			t.Errorf("an observer entered min over voters: %v, want [100 30]", got)
		}
		v.SetSize(80, 24)
		if got := size(w); got != [2]uint16{80, 24} {
			t.Fatalf("the voter cannot move the window either: %v", got)
		}
	})

	// UnpinSize is a fifth way into RecomputeEffectiveSize, and the only one the
	// four subtests above never take: each of them forces the recompute directly
	// with the pin still in place, so `pinned` is true in every walk they run.
	// Taking the pin away while an observer is attached is the one state where the
	// exclusion decides between two DIFFERENT sizes rather than between a size and
	// itself — with the pin gone and no voter, `haveMin` false is what reaches
	// `default: return nil`, and an observer inside the min would instead produce
	// its own size.
	t.Run("unpin with only an observer attached", func(t *testing.T) {
		w := NewFakeWindow("w")
		if err := w.Resize(30, 8); err != nil {
			t.Fatal(err)
		}
		// The observer's size is ABOVE the pin, so an observer that entered the min
		// would raise the result rather than be masked by max(pin, ...) — the same
		// reason the pinned subtests above pin below every declared size.
		o := w.RegisterObserver(100, 30)
		if got := size(w); got != [2]uint16{30, 8} {
			t.Fatalf("premise: pinned 30x8 with one observer should be [30 8], got %v", got)
		}
		w.UnpinSize()
		if got := size(w); got != [2]uint16{30, 8} {
			t.Errorf("unpinning let an observer set the size: %v, want the frozen [30 8]", got)
		}
		// Not frozen for an unrelated reason: a voter arriving now does move it, so
		// the assertion above is about the observer rather than about an unpinned
		// window being immovable.
		v := w.RegisterAttacher(70, 20)
		if got := size(w); got != [2]uint16{70, 20} {
			t.Fatalf("a voter cannot move the unpinned window either: %v", got)
		}
		// And with the pin gone the observer still does not vote against a voter's
		// min — 45x15 is below 70x20, so an observer inside the min would shrink it.
		o.SetSize(45, 15)
		// Checked here, where the other calls in this file discard it: SetSize on a
		// non-voting attacher returns before recomputing (window.go:232), so this is
		// the only walk that sees 45x15 — and the size it has to leave behind is the
		// one already there, so a recompute that failed and changed nothing would
		// pass the assertion below.
		if err := w.RecomputeEffectiveSize(); err != nil {
			t.Fatal(err)
		}
		if got := size(w); got != [2]uint16{70, 20} {
			t.Errorf("after the unpin an observer entered min over voters: %v, want [70 20]", got)
		}
		v.Release()
	})
}
