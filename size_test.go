package shelld

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests use only MaxCols, MaxRows and Window methods, so they also run
// against a tree without the bound (with the two constants added) and fail
// there without allocating more than about 150 MiB.

func TestNewWindowRefusesSizeAboveMaximum(t *testing.T) {
	for _, o := range []WindowOpts{
		{Name: "w", Command: []string{"shelld-test-no-such-program"}, Cols: MaxCols + 1, Rows: 24},
		{Name: "w", Command: []string{"shelld-test-no-such-program"}, Cols: 80, Rows: MaxRows + 1},
	} {
		w, err := NewWindow(o)
		if err == nil {
			w.Kill()
			t.Fatalf("%dx%d: no error", o.Cols, o.Rows)
		}
		if !strings.Contains(err.Error(), "must be at most") {
			t.Fatalf("%dx%d: got %q, want the size error before any spawn", o.Cols, o.Rows, err)
		}
	}
}

func TestResizeRefusesSizeAboveMaximum(t *testing.T) {
	w := NewFakeWindow("w")
	if err := w.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	for _, s := range [][2]uint16{{MaxCols + 1, 30}, {100, MaxRows + 1}, {MaxCols * 2, MaxRows * 2}} {
		if err := w.Resize(s[0], s[1]); err == nil {
			t.Errorf("Resize(%d, %d): no error", s[0], s[1])
		}
		if got := size(w); got != [2]uint16{100, 30} {
			t.Fatalf("Resize(%d, %d) changed the size to %v", s[0], s[1], got)
		}
		if c, r, _ := w.PinnedSize(); c != 100 || r != 30 {
			t.Fatalf("Resize(%d, %d) changed the pin to %dx%d", s[0], s[1], c, r)
		}
	}
	if err := w.Resize(MaxCols, MaxRows); err != nil || size(w) != [2]uint16{MaxCols, MaxRows} {
		t.Fatalf("Resize to the maximum: %v, size %v", err, size(w))
	}
}

func TestAttacherSizeClampedToMaximum(t *testing.T) {
	w := NewFakeWindow("w")
	a := w.RegisterAttacher(MaxCols*2, MaxRows*2)
	if a.Cols() != MaxCols || a.Rows() != MaxRows || size(w) != [2]uint16{MaxCols, MaxRows} {
		t.Fatalf("register: attacher %dx%d, window %v", a.Cols(), a.Rows(), size(w))
	}
	a.SetSize(80, 24)
	a.SetSize(MaxCols*2, 30)
	if a.Cols() != MaxCols || a.Rows() != 30 || size(w) != [2]uint16{MaxCols, 30} {
		t.Fatalf("SetSize: attacher %dx%d, window %v", a.Cols(), a.Rows(), size(w))
	}
	a.Release()
}

// One row is allowed where one column is not -- the two are not symmetric on
// purpose, see MinCols -- and asking for one row is not the same as getting it:
// a pin of one row settles at minEffectiveRows.
//
// Both halves are asserted. The first is the row that would quietly disappear if
// someone added a rows minimum "for consistency". The second stops that absence
// from reading as a promise that a pin of one row produces a one-row PTY, and
// pins where the floor applies: to the effective size, leaving the pin itself
// verbatim, so the size comes back if the client that forced the floor goes.
func TestOneRowAllowed(t *testing.T) {
	if err := CheckSize(80, 1); err != nil {
		t.Fatalf("CheckSize(80, 1): %v, want nil -- one row is a usable terminal", err)
	}
	w := NewFakeWindow("w")
	if err := w.Resize(80, 1); err != nil {
		t.Fatalf("Resize(80, 1): %v, want nil -- rows has no minimum, unlike cols", err)
	}
	if got := size(w); got != [2]uint16{80, minEffectiveRows} {
		t.Fatalf("size after Resize(80, 1) = %v, want 80x%d -- the effective size is floored at 20x5, as in the reference", got, minEffectiveRows)
	}
	if c, r, ok := w.PinnedSize(); !ok || c != 80 || r != 1 {
		t.Fatalf("pin = %dx%d (set %v), want 80x1 -- the floor raises the effective size and must not rewrite the pin", c, r, ok)
	}
}

// One column is refused: a wide character cannot fit, and the grid would
// have to widen it silently.
func TestOneColumnRefused(t *testing.T) {
	if err := CheckSize(1, 24); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("CheckSize(1, 24): %v", err)
	}
	if err := CheckSize(0, 0); err != nil {
		t.Fatalf("CheckSize(0, 0) (defaults): %v", err)
	}
	_, err := NewWindow(WindowOpts{Name: "w", Command: []string{"shelld-test-no-such-program"}, Cols: 1, Rows: 24})
	if err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("NewWindow 1 column: %v, want the size error before any spawn", err)
	}
	w := NewFakeWindow("w")
	if err := w.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if err := w.Resize(1, 30); err == nil {
		t.Fatal("Resize(1, 30): no error")
	}
	if got := size(w); got != [2]uint16{100, 30} {
		t.Fatalf("refused Resize changed the size to %v", got)
	}
}

// TestEffectiveSizeIsNeverAMixOfTwoDeclarations: a resize changes both axes at
// once, so a reader that takes them one at a time can assemble a pair that no
// client declared and the grid never had -- new width beside old height. Both
// sides of the wire are covered here, because the defect existed twice:
//
//	Attacher.Size   feeds RecomputeEffectiveSize, which reads every voting
//	                client's pair. A mix here is not merely reported, it is
//	                *written*: the window is resized to the fabricated pair and
//	                the PTY with it.
//	Window.Size     is what the status frame, the frame endpoint and the window
//	                list report. A mix here is a client told a geometry that was
//	                never real.
//
// Neither is a data race and `go test -race` is silent on both: every field is
// atomic or lock-guarded, and it is the *pair* that is not. So this test is the
// only instrument, which is why its detection rate was measured rather than
// assumed. One voting attacher alternates between two sizes, both above the 20x5
// floor and below the maximum, so the effective size is its declaration verbatim
// and exactly two pairs are legitimate:
//
//	mutant                                   reds
//	Attacher.Size as two loads of the word   100/100
//	Window.Size as Cols() then Rows()        100/100
//	fixed                                    0/200
//
// What the rate turns on is the *oracle's* cost, not the number of goroutines.
// The window a torn read has to land in is the gap between two adjacent loads, so
// anything the checking loop does besides those loads shrinks the fraction of
// time it spends there. Testing membership in a map never got past 76% red, over
// a dozen arrangements of 1-8 writers against 0-3 peer readers; the same test
// with the comparison written out reads 100/100 with one writer and one reader,
// which is why there is one of each below. Keep these loops arithmetic.
func TestEffectiveSizeIsNeverAMixOfTwoDeclarations(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("a torn pair needs a reader running while a writer stores; GOMAXPROCS=1 makes this a no-op rather than a weak test")
	}
	const wide, tall, narrow, short = 80, 24, 40, 10
	if narrow < minEffectiveCols || short < minEffectiveRows || wide > MaxCols || tall > MaxRows {
		t.Fatalf("the declarations must pass through unclamped for the oracle below to be exact: %dx%d and %dx%d against floor %dx%d, max %dx%d",
			wide, tall, narrow, short, minEffectiveCols, minEffectiveRows, MaxCols, MaxRows)
	}
	declared := func(c, r uint16) bool { return (c == wide && r == tall) || (c == narrow && r == short) }

	w := NewFakeWindow("w")
	a := w.RegisterAttacher(wide, tall)
	defer a.Release()

	var stop atomic.Bool
	var wg sync.WaitGroup
	fail := make(chan string, 2)
	// Reported once per goroutine and non-blocking, so a torn read cannot be lost
	// to a full channel and cannot deadlock the run either.
	report := func(what string, c, r uint16) {
		select {
		case fail <- fmt.Sprintf("%s read %dx%d, which is neither %dx%d nor %dx%d: half of one declaration and half of the other", what, c, r, wide, tall, narrow, short):
		default:
		}
		stop.Store(true)
	}

	// Bounded by time, not by a resize count: each SetSize that changes the
	// effective size reflows the grid, so a count large enough to be safe on a
	// loaded box is tens of seconds on an idle one.
	deadline := time.Now().Add(300 * time.Millisecond)
	wg.Add(4)
	go func() { // the client declaring sizes
		defer wg.Done()
		defer stop.Store(true)
		for i := 0; !stop.Load() && time.Now().Before(deadline); i++ {
			if i%2 == 0 {
				a.SetSize(narrow, short)
			} else {
				a.SetSize(wide, tall)
			}
		}
	}()
	// A peer's resize, which is the production reader of the attacher's pair and
	// the path that would *write* a mix. SetSize recomputes too, but a single
	// writer never races itself.
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = w.RecomputeEffectiveSize()
		}
	}()
	go func() { // oracle: the declaration
		defer wg.Done()
		for !stop.Load() {
			if c, r := a.Size(); !declared(c, r) {
				report("Attacher.Size", c, r)
				return
			}
		}
	}()
	go func() { // oracle: the effective size the clients are told
		defer wg.Done()
		for !stop.Load() {
			if c, r := w.Size(); !declared(c, r) {
				report("Window.Size", c, r)
				return
			}
		}
	}()
	wg.Wait()

	select {
	case msg := <-fail:
		t.Fatal(msg)
	default:
	}
	if c, r := w.Size(); !declared(c, r) {
		t.Fatalf("effective size settled at %dx%d, which no client declared", c, r)
	}
}
