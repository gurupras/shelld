//go:build linux

package daemon

import (
	"fmt"
	"syscall"
	"testing"
	"time"
)

// Idle attached clients must not each wake the daemon on their own schedule.
// With a safety deadline per client at its own phase, every client was a
// separate runtime wake-up per second, about three voluntary context switches
// each. That measurement is where the limit below comes from rather than from a
// document: at 64 clients over three seconds the per-client cost is around 576
// switches, and the test fails past 96, which is the same rusage counter this
// test reads for itself.
func TestAttachIdleWakeupsDoNotScaleWithClients(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 7 s")
	}
	const clients = 64
	const window = 3 * time.Second
	sock := boot(t)
	for i := range clients {
		s := fmt.Sprintf("s%d", i)
		openWindow(t, sock, s)
		_, ch := attachAt(t, sock, s, 80, 24)
		go func() {
			for range ch {
			}
		}()
	}
	// Let prompts render and hidden cursors settle.
	time.Sleep(2 * idlePoll)
	var r0, r1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
	time.Sleep(window)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
	switches := r1.Nvcsw - r0.Nvcsw
	// Per-client deadlines cost clients × seconds × ~3; a shared grid a few
	// per second regardless of clients.
	if limit := int64(clients * window / time.Second / 2); switches > limit {
		t.Fatalf("%d idle clients: %d voluntary context switches in %v, want ≤ %d", clients, switches, window, limit)
	}
	t.Logf("%d idle clients: %d voluntary context switches in %v", clients, switches, window)
}
