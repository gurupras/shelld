// The helpers these tests use (boot, request, attachAt, waitSize) live in
// daemon_test.go, which is linux-or-darwin only; without the same constraint
// `GOOS=windows go vet ./internal/daemon/` fails on undefined: boot.
//go:build linux || darwin

package daemon

import (
	"testing"

	"github.com/gurupras/shelld/ws"
)

// E1 reads the reflow counters over HTTP — the harness has no other way in — so
// /health reporting them is part of the metric, not a convenience. This test is
// about the wiring only: that the keys exist, that they are numbers, and that
// they move with real resizes. The exact "counts changes, never requests"
// arithmetic is pinned in the root package by TestReflowCounterSkipsNoOps and
// TestReflowCounterCountsUnpinnedAttacherChanges, which can drive Window
// directly.
//
// Note which path this test drives: POST /resize is routes.go:633, the only
// caller of Window.Resize and so the only way to set a pin, which means every
// reflow counted below happens on a PINNED window. That is not the shape
// `resizestorm` produces, and a counter that only fires while pinned would keep
// this test green — TestHealthReportsReflowsFromAnUnpinnedAttacher below is the
// arm that covers the other path.
//
// The counters are process-global, so the deltas here are only attributable
// because this package has no t.Parallel test and nothing in a daemon resizes a
// window without a request. If either stops being true, the equality assertion
// below is the one that will go flaky, and the strict arithmetic still lives in
// the root package.
func TestHealthReportsReflows(t *testing.T) {
	sock := boot(t)
	must := func(want int, method, path, body string) map[string]any {
		t.Helper()
		st, v, raw := request(t, sock, method, path, body)
		if st != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, st, want, raw)
		}
		return v
	}
	stats := func() (reflows, cells float64) {
		t.Helper()
		return reflowStats(t, sock)
	}

	must(200, "POST", "/sessions", `{"name":"s"}`)
	must(200, "POST", "/sessions/s/windows", `{"name":"w","cols":80,"rows":24,"shell":"/bin/sh"}`)

	r0, c0 := stats()
	must(200, "POST", "/sessions/s/windows/w/resize", `{"cols":120,"rows":40}`)
	r1, c1 := stats()
	if r1 != r0+1 {
		t.Fatalf("resize to a new geometry: reflows %v -> %v, want one more", r0, r1)
	}
	if c1 != c0+120*40 {
		t.Fatalf("resize to 120x40: cells %v -> %v, want %v more", c0, c1, 120*40)
	}

	// The same geometry twice more. This is the request/change distinction as the
	// harness sees it: three requests, one reflow.
	must(200, "POST", "/sessions/s/windows/w/resize", `{"cols":120,"rows":40}`)
	must(200, "POST", "/sessions/s/windows/w/resize", `{"cols":120,"rows":40}`)
	if r2, c2 := stats(); r2 != r1 || c2 != c1 {
		t.Fatalf("two no-op resizes: reflows %v -> %v, cells %v -> %v, want both unchanged", r1, r2, c1, c2)
	}
}

// reflowStats reads the metric as a consumer must: all three keys present and
// numeric, then the two that move. Shared by both tests here so a missing key is
// a failure on whichever path is being driven.
func reflowStats(t *testing.T, sock string) (reflows, cells float64) {
	t.Helper()
	st, v, raw := request(t, sock, "GET", "/health", "")
	if st != 200 {
		t.Fatalf("GET /health: status %d want 200: %s", st, raw)
	}
	for _, k := range []string{"reflows", "reflowCells", "reflowCellBytes"} {
		if _, ok := v[k].(float64); !ok {
			t.Fatalf("/health has no numeric %q: %v", k, v)
		}
	}
	if v["reflowCellBytes"].(float64) <= 0 {
		t.Fatalf("reflowCellBytes is %v; a consumer multiplying by it would report zero bytes", v["reflowCellBytes"])
	}
	return v["reflows"].(float64), v["reflowCells"].(float64)
}

// The path `resizestorm` actually drives, end to end: a window created with POST
// /windows and sized entirely by a client's ws `resize` frames, which reach
// Attacher.SetSize (attach.go:422). No POST /resize appears anywhere below, and
// routes.go:633 is that route's handler and the only caller of Window.Resize, so
// nothing in this test can set a pin. A counter that fired only while pinned
// would publish zero for this whole test — and for the storm.
//
// The no-op arm is sandwiched rather than polled. Frames on one connection are
// handled in order by the attach loop, so two identical 120x40 frames followed by
// a 100x30 frame need no sleep: once 100x30 is observable the repeats have been
// processed, and "exactly one reflow across three frames" cannot race.
//
// Attributability is the same argument the test above makes, with one addition:
// t.Cleanup runs synchronously, and an attacher's Release recomputes inline, so an
// earlier test's teardown reflows before this one reads its first baseline.
func TestHealthReportsReflowsFromAnUnpinnedAttacher(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	// Attaching at the window's own size is the resizestorm echo-probe window's
	// shape (created 80x24, attached 80x24): the recompute short-circuits and the
	// metric stays put. Asserted, because it is what makes the later deltas
	// attributable to the resize frames alone.
	r0, c0 := reflowStats(t, sock)
	conn, _ := attachAt(t, sock, "s", 80, 24)
	r1, c1 := reflowStats(t, sock)
	if r1 != r0 || c1 != c0 {
		t.Fatalf("attaching at the window's own 80x24: reflows %v -> %v, cells %v -> %v, want both unchanged",
			r0, r1, c0, c1)
	}

	resize := func(cols, rows int) {
		t.Helper()
		if err := conn.WriteMessage(ws.OpText, resizeCtl(cols, rows)); err != nil {
			t.Fatalf("sending a resize frame for %dx%d: %v", cols, rows, err)
		}
	}

	resize(120, 40)
	waitSize(t, sock, "s", [2]int{120, 40})
	r2, c2 := reflowStats(t, sock)
	if r2 != r1+1 {
		t.Fatalf("one resize frame to a new geometry: reflows %v -> %v, want one more", r1, r2)
	}
	if c2 != c1+120*40 {
		t.Fatalf("resize frame to 120x40: cells %v -> %v, want %v more", c1, c2, 120*40)
	}

	resize(120, 40)
	resize(120, 40)
	resize(100, 30)
	waitSize(t, sock, "s", [2]int{100, 30})
	r3, c3 := reflowStats(t, sock)
	if r3 != r2+1 {
		t.Fatalf("three frames of which one changed the size: reflows %v -> %v, want exactly one more", r2, r3)
	}
	if c3 != c2+100*30 {
		t.Fatalf("the two repeats contributed cells: %v -> %v, want %v more", c2, c3, 100*30)
	}
}
