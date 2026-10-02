package shelld

import "testing"

// TestMoveWindow already covers the end state of a colliding move, and the version
// that detached the window first and undid the detach on collision satisfied it:
// the rollback put the window back, so afterwards everything looked right. What no
// end-state test can see is the interval between the two, which is what this
// covers.
//
// The interval is reachable because m.mu does not cover it. Session.CreateWindow
// takes only s.mu — the daemon's create endpoint calls it on a *Session directly
// and never goes through this Manager — so a create of the source name could land
// inside the interval, and the undo, a plain map write of src.windows[srcName]
// performed after src.mu had been released, would overwrite it. That drops a live
// window with its shell, PTY, cgroup and four goroutines out of every map that can
// reach it, while the client that created it holds a name answering 404.
//
// A sampler through Session.Window is the cheapest witness: it takes s.mu, so it
// sees exactly what a concurrent create or lookup would, and the window it is
// looking for must be reachable at every instant.
func TestAFailedMoveIsNotObservableFromTheSourceSession(t *testing.T) {
	m := NewManager()
	if err := m.CreateSession("src"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSession("dst"); err != nil {
		t.Fatal(err)
	}
	src, dst := m.Session("src"), m.Session("dst")
	src.InsertFakeWindow("x")
	// The destination name is taken, so every move below fails and leaves the
	// state unchanged, which is what makes it safe to repeat. It is also a
	// different name from the source's, so the old code's setName(finalName) ran
	// before the collision check and the window briefly answered to a name its
	// own session did not know it by.
	//
	// The two counters below do not split that interval evenly, and the difference
	// is worth knowing before either is read as evidence. Measured with 799d57d
	// reverted, seven runs: missing fires on about 40% of samples — 224,512 of
	// 542,873 in the seventh run, and 198,953-226,140 of 468,185-542,873 taken as
	// minima and maxima across all seven — renamed on 1, 1, 1, 2, 2, 2 and 8 of
	// them. The ranges are per-field, so their endpoints are not a run and do not
	// divide; the paired numbers above them are there so the rate can be checked.
	//
	// renamed is that rare because it does not catch the state its name describes.
	// The old order deleted the window from src.windows before renaming it, and
	// Session.Window is a lookup by map key, so a window whose name is wrong is
	// already out of the map and samples as missing. What renamed catches is that
	// the sampler's two reads are not one observation: it looks the window up under
	// the source name, then calls Name() holding nothing, so the delete and the
	// rename can both land in between. That is a real sighting of the old code —
	// it happened in every run — but of a window that had already left the source
	// session, not one sitting in it under the destination name.
	//
	// Post-fix both are 0 by construction rather than by timing: the collision
	// check returns before the delete, so the failing path never detaches or
	// renames anything, and both session locks are held across the whole move.
	dst.InsertFakeWindow("y")

	const moves = 200_000
	var missing, renamed, sampled int
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			w := src.Window("x")
			switch {
			case w == nil:
				missing++
			case w.Name() != "x":
				renamed++
			default:
				sampled++
			}
		}
	}()

	for range moves {
		if err := m.MoveWindow("src", "x", "dst", "y"); err == nil {
			close(stop)
			<-done
			t.Fatal("the move onto a taken name succeeded")
		}
	}
	close(stop)
	<-done // the sampler's counters are published by this, so -race is clean

	if missing > 0 {
		t.Errorf("the window was in neither session on %d of %d samples: a "+
			"concurrent lookup answers 404 and a concurrent create is overwritten "+
			"by the rollback", missing, missing+renamed+sampled)
	}
	if renamed > 0 {
		t.Errorf("the window was renamed between the lookup that found it in the "+
			"source session and the read of its name, on %d of %d samples",
			renamed, missing+renamed+sampled)
	}
	// Liveness: with no samples at all both counts above are zero for the wrong
	// reason. This is not a timing assertion — the sampler runs until it is told
	// to stop, so a single sample is enough to know it ran.
	if sampled == 0 {
		t.Error("the sampler never observed the window, so it proved nothing")
	}
	// And the end state, which the old code also got right.
	if w := src.Window("x"); w == nil || w.Name() != "x" {
		t.Errorf("after %d failed moves the source window is %v", moves, w)
	}
	if dst.Window("x") != nil {
		t.Error("a failed move left the window in the destination session")
	}
}
