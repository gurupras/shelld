//go:build linux || darwin

// The end-to-end repair guard lives here, apart from idle_test.go's Linux-only
// wake-up count: it needs only the attach harness in daemon_test.go, which is
// linux || darwin (Unix socket clients and /bin/sh windows). It cannot be
// untagged until that harness has a Windows form; TestIdleWaitBound covers the
// bound itself on every OS.

package daemon

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// Output that reaches the grid without a dirty notification (a lost wake-up)
// must still reach an idle client within the safety tick. The writes repeat,
// each after the previous repair and a drain, so they land at a known phase
// after the loop re-armed its idle wait: known, not the worst. A write just
// after a grid instant waits nearly the whole idlePoll (about 950 ms); these
// land about 300 ms past one, so they are repaired in about 700 ms. The limit
// below is the bound for any phase, and idleWait's own bound is checked for
// every phase in TestIdleWaitBound, which runs on every OS.
func TestAttachSafetyTickRepairsLostWakeup(t *testing.T) {
	var st *State
	sock := bootWith(t, func(s *State) { st = s })
	openWindow(t, sock, "s")
	_, ch := attachAt(t, sock, "s", 80, 24)
	win := st.Mgr.Session("s").Window("w")
	limit := idlePoll + 250*time.Millisecond
	var worst time.Duration
	for k := range 3 {
		for {
			if _, ok := next(ch, 300*time.Millisecond); !ok {
				break
			}
		}
		marker := fmt.Sprintf("lost-wakeup-%d", k)
		g := win.LockVT()
		g.Advance(fmt.Appendf(nil, "\x1b[%d;1H%s", 5+k, marker))
		win.UnlockVT()
		start := time.Now()
		for {
			m, ok := next(ch, limit-time.Since(start))
			if !ok {
				t.Fatalf("write %d: output without a notification not sent within %v (worst so far %v)", k, limit, worst)
			}
			if m.op == ws.OpBinary && bytes.Contains(m.data, []byte(marker)) {
				worst = max(worst, time.Since(start))
				break
			}
		}
	}
	t.Logf("repaired, worst %v", worst)
}
