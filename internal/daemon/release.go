package daemon

import (
	"runtime/debug"
	"sync"
	"time"
)

// releaser returns memory freed by deleted windows to the OS.
//
// A window's grid and scrollback run to tens of MiB. When windows are
// deleted they become garbage, but Go collects only when allocation grows,
// so a daemon that goes quiet afterwards would keep that memory until the
// runtime's two-minute forced collection: 20 deleted windows left 870 MiB
// resident for at least 30 s. Deleting windows schedules one
// debug.FreeOSMemory (a collection plus returning free pages), after delay
// so a burst of deletions is handled once, and at most every minInterval.
type releaser struct {
	delay, minInterval time.Duration
	free               func()

	mu      sync.Mutex
	pending bool
	last    time.Time
}

func newReleaser() *releaser {
	return &releaser{delay: time.Second, minInterval: 10 * time.Second, free: debug.FreeOSMemory}
}

func (r *releaser) schedule() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending {
		return
	}
	r.pending = true
	wait := max(r.delay, r.minInterval-time.Since(r.last))
	time.AfterFunc(wait, func() {
		r.free()
		r.mu.Lock()
		r.pending, r.last = false, time.Now()
		r.mu.Unlock()
	})
}
