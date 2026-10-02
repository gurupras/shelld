//go:build linux || darwin

package shelld

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The reachable form of the stall, and the deadline that ends it.
//
// bash's `clear` emits CSI H, ED and CSI 3J and then draws the prompt on that
// same line, so the newest non-blank line carries an escape the scan cannot place
// on a column buffer and the verdict is a refusal. Nothing the window writes next
// clears it: the only thing that would write is the injection the verdict is
// gating, so the drain loop waits on output that only a delivery would produce.
// An attached human breaks it with one Enter; a window driven only by inject —
// which is what inject is for — has nobody to press it.
//
// The scan's refusal is deliberate and stays. What the deadline adds is that the
// payload leaves the queue, is recorded, and is recorded as dropped rather than
// typed: the scan cannot tell a prompt it failed to model from a full-screen
// program, so typing on a deadline would type into whatever is running.
func TestInjectionExpiresAfterClearWedgesTheScan(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	home := t.TempDir()
	stall := uint64(600)
	w, err := NewWindow(WindowOpts{
		Name:           "clear-stall",
		Command:        []string{bash, "--norc", "--noprofile", "-i"},
		Cwd:            home,
		StallTimeoutMS: &stall,
		Env: [][2]string{{"PS1", "$ "}, {"TERM", "xterm-256color"},
			{"HOME", home}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()

	lines := func() string { return w.Scrollback().LastN(w.IdleDetector().ScanLines()) }
	waitFor(t, 8*time.Second, w.isIdleForInjection, "prompt never idle before clear")

	// A first injection delivers, so the stall below is caused by clear and not
	// by anything about this window: the premise is a healthy delivery path.
	if _, err := w.InjectQueue().Enqueue("clear", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		out, _ := w.Capture(50)
		return strings.Contains(lines(), "\x1b[2J") && strings.Contains(out, "$")
	}, "clear never ran")
	if n := w.InjectQueue().Len(); n != 0 {
		t.Fatalf("premise: the first injection did not drain; queue %d", n)
	}

	// The prompt now sits on a line holding ED, so the scan refuses it. Checked
	// over five polls rather than once: a single sample could catch a window
	// mid-write, and this case needs a refusal that persists.
	for i := 0; i < 5; i++ {
		if w.isIdleForInjection() {
			t.Fatalf("premise: the scan reads %q as idle; this case needs a refusal", lines())
		}
		time.Sleep(100 * time.Millisecond)
	}

	msg, err := w.InjectQueue().Enqueue("echo clear-marker-$((6*7))", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return w.InjectQueue().Len() == 0 },
		"a payload stayed queued past its stall timeout")

	// Dropped, not typed. Both halves matter: leaving the queue is what bounds
	// the wait, and not appearing on screen is what keeps the refusal's guarantee
	// — the scan promised never to type into a program it cannot read.
	out, _ := w.Capture(50)
	if strings.Contains(out, "clear-marker-42") {
		t.Errorf("expiry typed the payload into a window the scan refused to read")
	}

	// And visible afterwards: the enqueue returned a message, so a payload that
	// is neither pending nor on screen is otherwise indistinguishable from one
	// still waiting.
	expiries := w.InjectExpiries()
	if len(expiries) != 1 {
		t.Fatalf("expected one recorded expiry, got %+v", expiries)
	}
	if got := expiries[0]; got.Message.ID != msg.ID || got.Reason != "scan-refused" {
		t.Errorf("recorded %+v, want id %s reason scan-refused", got, msg.ID)
	}
	if expiries[0].ExpiredAt < msg.CreatedAt {
		t.Errorf("expiredAt %d precedes createdAt %d",
			expiries[0].ExpiredAt, msg.CreatedAt)
	}

	// The escape hatch the injection path cannot take: one bare Enter, from any
	// client with SendRawKey, redraws the prompt on a clean line and the scan
	// reads it again. Asserted so that the deadline is not mistaken for the only
	// way out, and so a change that left the window permanently unreadable would
	// be caught here.
	if err := w.SendRawKey("\r"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, w.isIdleForInjection,
		"the scan never recovered after an out-of-band Enter")
}

// StallTimeoutMS of zero is the opt-out, and it has to be reachable through
// WindowOpts: that is why the field is a pointer. A window configured this way
// holds a payload the scan refuses for as long as the caller leaves it there,
// which is the behaviour before the deadline existed.
func TestStallTimeoutZeroKeepsThePayloadQueued(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	home := t.TempDir()
	stall := uint64(0)
	w, err := NewWindow(WindowOpts{
		Name:           "no-stall",
		Command:        []string{bash, "--norc", "--noprofile", "-i"},
		Cwd:            home,
		StallTimeoutMS: &stall,
		Env: [][2]string{{"PS1", "$ "}, {"TERM", "xterm-256color"},
			{"HOME", home}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if got := w.injectCfg.StallTimeoutMS; got != 0 {
		t.Fatalf("WindowOpts did not reach the config: StallTimeoutMS %d", got)
	}

	waitFor(t, 8*time.Second, w.isIdleForInjection, "prompt never idle before clear")
	if _, err := w.InjectQueue().Enqueue("clear", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool { return w.InjectQueue().Len() == 0 },
		"clear never drained")

	if _, err := w.InjectQueue().Enqueue("echo never-42", 0); err != nil {
		t.Fatal(err)
	}
	enqueued := time.Now()
	// Waited for rather than slept out. A window built from WindowOpts carries the
	// default two-second delivery cooldown, so a fixed two-second sleep starting
	// when `clear` drained ends while the drain loop is still inside that cooldown:
	// no poll runs, the expiry check is never reached, and the assertion below
	// would hold just as well for a build with no expiry check in it at all.
	//
	// scanStalled is set by a poll that scanned a screen it could not read with
	// something queued — `clear` is what leaves the screen that way — and a scan
	// that refuses is never idle, so that poll took the branch the deadline is
	// checked in. The elapsed time is kept in the same condition so the payload has
	// also outlived any deadline these tests use, which is what would fail if zero
	// were read as a default rather than as no deadline.
	waitFor(t, 10*time.Second, func() bool {
		return w.scanStalled.Load() && time.Since(enqueued) > 2*time.Second
	}, "no poll ever evaluated the queued payload")

	if w.InjectQueue().Len() != 1 {
		t.Errorf("with no deadline the payload left the queue; queue %d, expiries %+v",
			w.InjectQueue().Len(), w.InjectExpiries())
	}
	if len(w.InjectExpiries()) != 0 {
		t.Errorf("with no deadline something expired: %+v", w.InjectExpiries())
	}
}

// The default is five minutes, not zero: a caller that sets nothing gets the
// bound. Asserted on the config rather than by waiting it out.
func TestStallTimeoutDefaultsToFiveMinutes(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "default-stall", Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	if got := w.injectCfg.StallTimeoutMS; got != 300000 {
		t.Errorf("default StallTimeoutMS is %d, want 300000", got)
	}
}
