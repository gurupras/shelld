package shelld

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// countHandler counts records by message, so a test can assert on this window's
// log lines while other tests' logging passes through it.
type countHandler struct {
	mu    sync.Mutex
	count map[string]int
	attrs map[string]string
}

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count[r.Message]++
	r.Attrs(func(a slog.Attr) bool {
		h.attrs[r.Message+"/"+a.Key] = a.Value.String()
		return true
	})
	return nil
}

func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

func (h *countHandler) get(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count[msg]
}

func (h *countHandler) attr(msg, key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.attrs[msg+"/"+key]
}

const (
	msgScanRefused = "idle scan cannot read the newest output; injections wait"
	msgScanCleared = "idle scan can read the output again"
)

// A refusal is the safe direction and it is also indefinite: drainLoop has no
// deadline, InjectConfig.StallTimeoutMS is not implemented, and a shell sitting
// at a prompt the scan cannot read writes nothing that would restore trust. So
// the one thing the operator must get is a line saying it happened — and exactly
// one, since the verdict is recomputed every 200 ms for as long as a payload is
// queued.
func TestIdleScanRefusalIsLoggedOnceOnEachTransition(t *testing.T) {
	h := &countHandler{count: map[string]int{}, attrs: map[string]string{}}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := NewFakeWindow("scanlog")
	if _, err := w.InjectQueue().Enqueue("payload", 0); err != nil {
		t.Fatal(err)
	}

	// CSI J is not modelled, so the newest line cannot be placed on the screen.
	w.scrollback.Write([]byte("\x1b[J❯ "))
	for i := 0; i < 3; i++ {
		if w.isIdleForInjection() {
			t.Fatal("a line the render refuses must not read idle")
		}
	}
	if got := h.get(msgScanRefused); got != 1 {
		t.Errorf("refusal logged %d times over three verdicts, want 1", got)
	}
	if got := h.attr(msgScanRefused, "queued"); got != "1" {
		t.Errorf("queued=%q on the refusal line, want the payload to be counted", got)
	}
	if got := h.attr(msgScanRefused, "window"); got != "scanlog" {
		t.Errorf("window=%q, want scanlog", got)
	}
	if got := h.get(msgScanCleared); got != 0 {
		t.Errorf("cleared logged %d times while still refusing", got)
	}
	// The line's text is program output, so it goes to Debug and not to the Info
	// line above; this handler is enabled for every level, so it sees both.
	if got := h.attr("idle scan refused a line", "line"); !strings.Contains(got, "❯") {
		t.Errorf("the refused line was not logged at Debug: %q", got)
	}

	// A line the render can place ends the refusal, once.
	w.scrollback.Write([]byte("\n❯ "))
	for i := 0; i < 3; i++ {
		if !w.isIdleForInjection() {
			t.Fatalf("a bare prompt must read idle; history %q", w.scrollback.LastN(5))
		}
	}
	if got := h.get(msgScanCleared); got != 1 {
		t.Errorf("recovery logged %d times over three verdicts, want 1", got)
	}
	if got := h.get(msgScanRefused); got != 1 {
		t.Errorf("refusal logged %d times in total, want 1", got)
	}
}

// A window whose prompt the scan cannot read refuses from the moment it draws
// that prompt, which for a live bash with CSI J in PS1 is before any client has
// enqueued anything: the first verdict came back refused with an empty queue and
// logged "injections wait" naming zero of them, and then the payload that did
// stall arrived to silence. The line has to name the payload it is about.
func TestIdleScanRefusalIsNotLoggedUntilSomethingWaits(t *testing.T) {
	h := &countHandler{count: map[string]int{}, attrs: map[string]string{}}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := NewFakeWindow("scanlog-empty")
	w.scrollback.Write([]byte("\x1b[J❯ "))
	for i := 0; i < 3; i++ {
		if w.isIdleForInjection() {
			t.Fatal("a line the render refuses must not read idle")
		}
	}
	if got := h.get(msgScanRefused); got != 0 {
		t.Errorf("refusal logged %d times with nothing queued, want 0", got)
	}

	if _, err := w.InjectQueue().Enqueue("payload", 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w.isIdleForInjection()
	}
	if got := h.get(msgScanRefused); got != 1 {
		t.Errorf("refusal logged %d times once a payload was waiting, want 1", got)
	}
	if got := h.attr(msgScanRefused, "queued"); got != "1" {
		t.Errorf("queued=%q on the refusal line, want the payload to be counted", got)
	}
}
