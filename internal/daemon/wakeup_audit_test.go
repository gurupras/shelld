//go:build linux || darwin

// The audit guard: every state ticker.render reads must fire the window's
// notifier when it changes, because the idle safety tick is the only other way
// a client learns of it and that repair is up to idlePoll away. Each case here
// makes one change and requires the client's tick loop to wake on the notifier,
// with no intervening idle tick — so it fails whether the notification is
// missing outright or merely late.
//
// This needs the Unix attach harness in daemon_test.go. window_peer_notify_test.go
// (root package) covers the effective-size notifications on every OS.
//
// What it adds is the audit, not a hole it is the only cover for: every mutant it
// catches -- a deleted notify in the read loop, a deleted notify at
// setEffectiveSize, a notify delayed past idlePoll -- is also caught by at least
// one of TestAttachChangeJustAfterFrame, TestAttachLiveMessagesAndWakes,
// TestAttachLivePacesContinuousOutput and TestAttachSilentWhenIdleAndRepaintOnResize.
// Those reach the property through what a client received and when; this reaches it
// through the wake itself, at the site, for six inputs in one place, so a new input
// that forgets to notify is a line added here rather than a latency bar somewhere
// else that happens to fail.
//
// A ctrl wake counts as a notification (see scan), so the three peer arms have a
// second route to their wake: the peer's own control queue. Deleting the notify at
// setEffectiveSize is caught here by the POST /resize arm, which has no such route
// -- and narrowing that notify to the pinned case leaves this test green, which is
// what window_peer_notify_test.go is for.

package daemon

import (
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/internal/tracing"
)

// wakeLog records one window's tick-loop wakes. Both clients of a window wait
// on the same notifier, so a dirty wake from either proves the notification
// fired; the log does not distinguish them.
type wakeLog struct {
	t     *testing.T
	win   *shelld.Window
	mu    chan struct{} // a 1-buffered channel as the mutex
	wakes []stampedWake
}

func newWakeLog(t *testing.T, win *shelld.Window) *wakeLog {
	t.Helper()
	l := &wakeLog{t: t, win: win, mu: make(chan struct{}, 1)}
	observe := func(w tickWake) {
		if w.win != l.win {
			return
		}
		l.mu <- struct{}{}
		l.wakes = append(l.wakes, stampedWake{w, time.Now()})
		<-l.mu
	}
	tickObserver.Store(&observe)
	t.Cleanup(func() { tickObserver.Store(nil) })
	return l
}

// scan reports, for the waits that ended after t0, whether one ended on a
// notification and whether an idle safety tick came first. Wakes of a loop's
// first tick and of a burst's settle check are neither: they are noise here.
func (l *wakeLog) scan(t0 time.Time) (notified, idleFirst bool) {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	for _, w := range l.wakes {
		if w.site != siteWait || !w.at.After(t0) {
			continue
		}
		switch {
		case w.reason == tracing.WakeDirty || w.reason == tracing.WakeCtrl:
			return true, idleFirst
		case w.armed == armIdle && w.reason == tracing.WakeTimer:
			idleFirst = true
		}
	}
	return false, idleFirst
}

// requireNotified fails unless, within d of t0, some loop's wait ended on the
// window's notifier (or on this client's control queue) with no idle safety
// tick before it. A missing notification shows up either as the idle tick
// getting there first — the repair this asserts nothing relies on — or as no
// notification at all within d.
func (l *wakeLog) requireNotified(t0 time.Time, d time.Duration, what string) {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for {
		notified, idleFirst := l.scan(t0)
		switch {
		case idleFirst:
			l.t.Fatalf("%s: the idle safety tick woke the loop before any notification did", what)
		case notified:
			return
		case time.Now().After(deadline):
			l.t.Fatalf("%s: no notification reached a tick loop within %v", what, d)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAttachRenderInputsNotifyWithoutTheIdleTick(t *testing.T) {
	var st *State
	sock := bootWith(t, func(s *State) { st = s })
	openWindow(t, sock, "s")
	_, ch := attachAt(t, sock, "s", 80, 24)
	win := st.Mgr.Session("s").Window("w")
	log := newWakeLog(t, win)

	// quiet drains the client until nothing has arrived for 400 ms, longer than
	// a burst's settle, so the loop is back on its idle wait and no unrelated
	// notification is in flight.
	quiet := func() {
		t.Helper()
		for {
			if _, ok := next(ch, 400*time.Millisecond); !ok {
				return
			}
		}
	}

	// A peer client whose smaller declaration shrinks the shared grid under
	// this one. Registration happens before the peer's own tick loop exists,
	// so only the notifier can carry it.
	quiet()
	t0 := time.Now()
	peer, peerCh := attachAt(t, sock, "s", 40, 10)
	log.requireNotified(t0, 500*time.Millisecond, "a peer attaching at a smaller size")
	waitSize(t, sock, "s", [2]int{40, 10})

	// The peer growing its declaration grows the shared grid again.
	quiet()
	for {
		if _, ok := next(peerCh, 400*time.Millisecond); !ok {
			break
		}
	}
	t0 = time.Now()
	if err := peer.WriteMessage(1, resizeCtl(60, 20)); err != nil {
		t.Fatal(err)
	}
	log.requireNotified(t0, 500*time.Millisecond, "a peer growing its declared size")
	waitSize(t, sock, "s", [2]int{60, 20})

	// The peer leaving restores the size this client declared.
	quiet()
	t0 = time.Now()
	peer.Close()
	log.requireNotified(t0, 500*time.Millisecond, "a peer disconnecting")
	waitSize(t, sock, "s", [2]int{80, 24})

	// POST /resize pins a floor above this client's declaration.
	quiet()
	t0 = time.Now()
	if code, _, raw := request(t, sock, "POST", "/sessions/s/windows/w/resize", `{"cols":120,"rows":40}`); code != 200 {
		t.Fatal(raw)
	}
	log.requireNotified(t0, 500*time.Millisecond, "POST /resize pinning a larger size")
	waitSize(t, sock, "s", [2]int{120, 40})

	// Output from the program.
	quiet()
	t0 = time.Now()
	if err := win.WriteInput([]byte("printf notify-audit\n")); err != nil {
		t.Fatal(err)
	}
	log.requireNotified(t0, 500*time.Millisecond, "program output")

	// Kill clears IsAlive, which the render reads and the loop returns on. On
	// Unix this case is redundant: closing the master ends readLoop, which
	// notifies too. Kill's own notification is what a platform where the read
	// does not end needs, and TestKillNotifiesWaiters (root package) is its
	// direct guard.
	quiet()
	t0 = time.Now()
	go win.Kill()
	log.requireNotified(t0, 500*time.Millisecond, "Window.Kill clearing IsAlive")
}
