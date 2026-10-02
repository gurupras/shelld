//go:build linux || darwin

package shelld

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// IsAlive is a fact about the terminal and ExitStatus is a fact about the
// window's own process, and this is the case where they disagree for a long
// time: the shell exits while something it started still holds the PTY slave
// open. The state is documented on IsAlive and Done, but nothing pinned it, and
// a listing that "reconciles" the two by suppressing alive once a status exists
// reports this window as dead while it is still accepting input and still
// writing to the scrollback. That mistake was made and reverted here.
//
// Measured while writing this (holder sleeping 5s, sampling every 20ms):
// IsAlive stayed true for 4.988s after the status was recorded, 246 writes were
// accepted and none refused, and the holder's output reached the grid 1.018s
// after the exit. The assertions below are looser than those numbers on purpose
// — the point is the state's existence and its ordering, not its duration.
//
// The shell ignores SIGHUP BEFORE forking the holder, and that order is load
// bearing:
//
//	(trap '' HUP; sleep 3) & exit 0        # RACES — how this was first written
//	trap '' HUP; (sleep 3) & exit 0        # what it says now
//
// (Keep those two lines indented. gofmt's doc-comment printer applies the old
// TeX-style quoting convention to comment PROSE, turning a pair of apostrophes
// into a Unicode right double quote, so a shell snippet survives only inside an
// indented block. This comment was corrupted that way twice, and each repair was
// undone by the next gofmt -w, before the cause was found.)
//
// In the first shape the holder races the SIGHUP the kernel sends the foreground
// process group when the session leader exits, and dies with the shell if it has
// not reached its `trap` builtin yet. Set in the parent, SIG_IGN is inherited
// across the fork and preserved across the exec of sleep, so there is no window at
// all rather than a smaller one.
//
// THE OLD SHAPE'S FAILURE RATE DEPENDS ON CPU AFFINITY, and that has a
// consequence for how this suite is run. Alternating arms in one window at load1
// 0.50:
//
//	unpinned, all CPUs                     8 FAIL of 8
//	nice -n 19 taskset -c 0,12             1 FAIL of 8
//
// So the project's own correctness configuration — pinned to two CPUs — is the
// one that HIDES this defect, by roughly 8x. Load is not the variable: the rate
// measured ~80% at load1 0.9 and at load1 4.3, 4.6x apart. The likely mechanism,
// stated as the hypothesis it is, is that with a whole machine available the
// forked subshell lands on another CPU and is still queued when the parent's exit
// triggers the SIGHUP, while two CPUs make parent and child share a runqueue and
// the child usually reaches its trap first.
//
// Verified against that harder configuration, unpinned: this test and its
// daemon-level sibling are 8 of 8 green each, where the old shape is 8 of 8 red.
// Injecting the gap directly agrees — `sleep 0.05` before the trap, alternating,
// old shape 5 of 5 red and this one 5 of 5 green.
//
// Two practical notes. A hunt for a fork or scheduling race in this repo should
// run unpinned; the gate's pinning is a suppressor. And a real pass here takes
// ~3s while a red takes 0.02s, so a fast green is itself a symptom.
//
// The failure that race produced is the expensive kind: the holder is genuinely
// gone, so alive=false is correct, and the red is identical in message and in
// duration to a deliberate suppression of alive — the defect these tests exist
// to catch would have read as a known flake.
func TestHolderKeepsTerminalOpenAfterExit(t *testing.T) {
	// The state does not exist on darwin, so this is skipped there rather than
	// softened into something that passes. BSD revokes a controlling terminal when
	// its controlling process exits, invalidating every descriptor for the tty --
	// the holder's included -- so the master sees EOF at once and IsAlive goes false
	// immediately however many processes still hold the slave. Measured with this
	// script's shape, reading the master until it ended: linux 3.001s, macOS 26.4
	// arm64 14ms. See livenessJSON in internal/daemon/routes.go, where the pair a
	// window publishes is documented, and the daemon-level sibling named above,
	// which skips for the same reason.
	if runtime.GOOS != "linux" {
		t.Skipf("a process cannot hold a window's terminal open past the shell's exit on %s; see the comment here", runtime.GOOS)
	}
	// holderLife has to equal the script's total sleep, and the echo has to land
	// inside holderLife/2, because the loop below samples for that long and requires
	// the output. The two are coupled by hand: change one without the other and this
	// fails for the fixture's reason while the message blames the window.
	const holderLife = 3 * time.Second
	script := `trap '' HUP; (sleep 1; echo HOLDER-OUTPUT; sleep 2) & exit 0`
	w, err := NewWindow(WindowOpts{Name: "holder", Command: []string{"/bin/sh", "-c", script}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()

	select {
	case <-w.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the window's process never exited")
	}
	exitedAt := time.Now()

	// Done only promises the process is gone; the status is recorded by a
	// different goroutine just after. Wait for the status itself, since the
	// claim under test is about alive with a status *present*.
	waitFor(t, 5*time.Second, func() bool { _, ok := w.ExitStatus(); return ok }, "exit status never recorded")

	alive, st, ok := w.Liveness()
	if !ok {
		t.Fatal("Liveness reports no status after ExitStatus returned one")
	}
	if st.Code != 0 {
		t.Errorf("exit status %+v, want code 0 — the shell ran `exit 0`", st)
	}
	if !alive {
		t.Fatalf("terminal already closed %v after the process exited, with a holder still running: "+
			"either the holder died with the shell or alive no longer tracks the terminal",
			time.Since(exitedAt).Round(time.Millisecond))
	}

	// The state persists, and input is accepted throughout it. A single sample
	// could not tell this from a scheduling skew.
	var accepted, refused int
	sawOutput := false
	for deadline := exitedAt.Add(holderLife / 2); time.Now().Before(deadline); {
		alive, _, ok := w.Liveness()
		if !alive || !ok {
			t.Fatalf("at +%v: alive=%v status recorded=%v, want both while the holder lives",
				time.Since(exitedAt).Round(time.Millisecond), alive, ok)
		}
		if err := w.WriteInput([]byte("x")); err != nil {
			refused++
		} else {
			accepted++
		}
		if grid, _ := w.Capture(40); strings.Contains(grid, "HOLDER-OUTPUT") {
			sawOutput = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	if refused != 0 || accepted == 0 {
		t.Errorf("writes accepted=%d refused=%d after the process exited; the terminal is open, "+
			"so every input path (which gates on IsAlive) must still take them", accepted, refused)
	}
	if !sawOutput {
		t.Error("the holder's output never reached the grid after the process exited; " +
			"output written after the status is recorded still belongs in the scrollback")
	}

	// And it does end: once the holder goes, so does the terminal.
	waitFor(t, holderLife+5*time.Second, func() bool { return !w.IsAlive() }, "terminal never closed after the holder exited")
	if _, st, ok := w.Liveness(); !ok || st.Code != 0 {
		t.Errorf("after the terminal closed, status recorded=%v %+v; the status must not be disturbed by the EOF", ok, st)
	}
}
