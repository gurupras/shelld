//go:build linux || darwin

package daemon

import (
	"runtime"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// ATTACK 9: an observer's declared size is no longer tied to the grid, so a
// watcher can ask for a 1024x1024 view of an 80x24 window. At HEAD a client
// that declared that size also dragged the grid up to it (visible in
// GET /windows and paid for by everyone); an observer gets the per-client cost
// with the grid — and the daemon's reported size — unchanged.
func TestAttackObserverOversizeAmplification(t *testing.T) {
	measure := func(cols, rows int) (bytes, frames int, heapKB uint64) {
		sock := boot(t)
		openWindow(t, sock, "s")
		agentConn, _ := attachAt(t, sock, "s", 80, 24)
		defer agentConn.Close()
		waitSize(t, sock, "s", [2]int{80, 24})

		obsConn, obsCh := attachObserverAt(t, sock, "s", cols, rows)
		defer obsConn.Close()
		time.Sleep(200 * time.Millisecond)
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)

		_ = agentConn.WriteMessage(ws.OpBinary, []byte("seq 1 400\n"))
		deadline := time.Now().Add(3 * time.Second)
		for {
			m, ok := next(obsCh, time.Until(deadline))
			if !ok {
				break
			}
			if m.op == ws.OpBinary {
				bytes += len(m.data)
				frames++
			}
			if time.Now().After(deadline) {
				break
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&m1)
		if m1.HeapAlloc > m0.HeapAlloc {
			heapKB = (m1.HeapAlloc - m0.HeapAlloc) / 1024
		}
		if got := effectiveSize(t, sock, "s"); got != [2]int{80, 24} {
			t.Fatalf("grid moved to %v", got)
		}
		return bytes, frames, heapKB
	}
	b1, f1, h1 := measure(80, 24)
	b2, f2, h2 := measure(1024, 1024)
	t.Logf("observer  80x24   : %7d bytes in %d frames, heap +%d KB", b1, f1, h1)
	t.Logf("observer 1024x1024: %7d bytes in %d frames, heap +%d KB (grid still 80x24)", b2, f2, h2)
	if b1 > 0 {
		t.Logf("byte amplification: %.1fx", float64(b2)/float64(b1))
	}
}
