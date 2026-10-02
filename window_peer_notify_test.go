package shelld

import (
	"testing"
	"time"
)

// requireWoke fails unless armed closes within 500 ms.
func requireWoke(t *testing.T, armed <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-armed:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("%s did not wake dirty waiters", what)
	}
}

// A client renders a crop of the shared grid, so every effective-size change
// resizes that grid under it: registering and releasing a peer attacher both
// have to wake the clients already attached. They have no timer of their own
// to notice it (internal/daemon's idleWait is the only fallback, and it is
// bounded by idlePoll).
//
// The wake is what is new here, not the size. TestEffectiveSizeIsThePerAxisMinimum
// and TestAttacherGeometry already assert the sizes these arms assert, and
// TestResizeNotifiesWhenPTYResizeFails already asserts a wake -- but from
// Window.Resize, which pins. Every path meets at setEffectiveSize, so deleting
// the notify there reds all of them together and says nothing about which test
// covers which caller. What separates them is narrowing rather than deleting:
// with the notify made conditional on a pin being set, the whole of the root
// package and the whole of internal/daemon stay green except this test. So the
// attacher-driven half of that site is guarded here and nowhere else.
func TestPeerAttacherSizeChangesWakeDirtyWaiters(t *testing.T) {
	w := NewFakeWindow("peers")

	armed := w.Dirty().Wait()
	a := w.RegisterAttacher(80, 24)
	requireWoke(t, armed, "the first attacher's effective size")
	if c, r := w.Cols(), w.Rows(); c != 80 || r != 24 {
		t.Fatalf("effective size %dx%d, want 80x24", c, r)
	}

	// A smaller peer shrinks the shared grid under the first client.
	armed = w.Dirty().Wait()
	b := w.RegisterAttacher(40, 10)
	requireWoke(t, armed, "a peer attacher shrinking the effective size")
	if c, r := w.Cols(), w.Rows(); c != 40 || r != 10 {
		t.Fatalf("effective size %dx%d, want 40x10", c, r)
	}

	// The peer growing its own declaration grows the grid again.
	armed = w.Dirty().Wait()
	b.SetSize(60, 20)
	requireWoke(t, armed, "a peer attacher growing the effective size")
	if c, r := w.Cols(), w.Rows(); c != 60 || r != 20 {
		t.Fatalf("effective size %dx%d, want 60x20", c, r)
	}

	// The peer leaving restores the size the remaining client declared.
	armed = w.Dirty().Wait()
	b.Release()
	requireWoke(t, armed, "a peer attacher releasing")
	if c, r := w.Cols(), w.Rows(); c != 80 || r != 24 {
		t.Fatalf("effective size %dx%d, want 80x24", c, r)
	}

	// A size pin (POST /resize) and clearing it again.
	armed = w.Dirty().Wait()
	if err := w.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	requireWoke(t, armed, "a size pin")
	armed = w.Dirty().Wait()
	w.UnpinSize()
	requireWoke(t, armed, "clearing the size pin")

	a.Release()
}
