package shelld

import "testing"

// The size half of the observer change is platform-neutral and NewFakeWindow
// needs no PTY, so this runs on Windows too — unlike the spike's test file,
// which is `//go:build linux || darwin` and leaves the changed arithmetic with
// no coverage on the maintainer's primary platform.
func TestObserverDoesNotVoteOnEffectiveSize(t *testing.T) {
	w := NewFakeWindow("w")
	if err := w.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	w.UnpinSize()
	if got := size(w); got != [2]uint16{100, 30} {
		t.Fatalf("after unpin: %v", got)
	}

	// An observer alone must not move the size, and must not collapse it to the
	// 20x5 floor by making the min run over an empty set.
	obs := w.RegisterObserver(40, 12)
	if got := size(w); got != [2]uint16{100, 30} {
		t.Fatalf("observer-only: size %v, want the frozen [100 30]", got)
	}
	if obs.Voting() {
		t.Fatal("RegisterObserver produced a voting attacher")
	}
	if w.AttacherCount() != 1 {
		t.Fatalf("AttacherCount %d, want 1", w.AttacherCount())
	}

	// A voter sets the size; the observer's smaller declaration is ignored.
	vote := w.RegisterAttacher(90, 28)
	if got := size(w); got != [2]uint16{90, 28} {
		t.Fatalf("voter present: size %v, want [90 28]", got)
	}
	obs.SetSize(30, 8)
	if got := size(w); got != [2]uint16{90, 28} {
		t.Fatalf("observer SetSize moved the size to %v", got)
	}
	// A second voter does enter the min.
	vote2 := w.RegisterAttacher(70, 20)
	if got := size(w); got != [2]uint16{70, 20} {
		t.Fatalf("second voter: size %v, want [70 20]", got)
	}
	vote2.Release()
	if got := size(w); got != [2]uint16{90, 28} {
		t.Fatalf("after the second voter left: size %v, want [90 28]", got)
	}
	// The last voter leaves: the size freezes where it was, not at the
	// observer's 30x8 and not at the floor.
	vote.Release()
	if got := size(w); got != [2]uint16{90, 28} {
		t.Fatalf("after the last voter left: size %v, want the frozen [90 28]", got)
	}
	// A pin still applies with only an observer attached.
	if err := w.Resize(150, 50); err != nil {
		t.Fatal(err)
	}
	if got := size(w); got != [2]uint16{150, 50} {
		t.Fatalf("pin with only an observer: %v", got)
	}
	obs.Release()
	if got := size(w); got != [2]uint16{150, 50} {
		t.Fatalf("after the observer left: %v", got)
	}
	w.Kill()
}

// Release must be idempotent for an observer too (serveAttach defers it while
// the ticker can also exit).
func TestObserverReleaseIdempotent(t *testing.T) {
	w := NewFakeWindow("w")
	obs := w.RegisterObserver(40, 12)
	obs.Release()
	obs.Release()
	if w.AttacherCount() != 0 {
		t.Fatalf("AttacherCount %d after a double release", w.AttacherCount())
	}
	w.Kill()
}
