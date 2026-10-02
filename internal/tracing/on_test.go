//go:build shelld_trace

package tracing

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordsToFile runs a child test process with SHELLD_TRACE set (init
// reads it at startup) and checks the file it leaves.
func TestRecordsToFile(t *testing.T) {
	if os.Getenv("SHELLD_TRACE") != "" {
		id := NewID()
		sp := Begin(KindPTYRead, id, 0)
		End(KindPTYRead, id, sp, 17, 0)
		Instant(KindNotify, id, 3, 0)
		Instant(KindMetric, Intern("a metric"), 9, 0)
		time.Sleep(50 * time.Millisecond) // let canaries record
		Close()
		return
	}
	path := filepath.Join(t.TempDir(), "t.trace")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecordsToFile$")
	cmd.Env = append(os.Environ(), "SHELLD_TRACE="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	var begin, end, notify, timer, metric bool
	for _, r := range tr.Records {
		switch {
		case r.Kind == KindPTYRead && r.Phase == PhaseBegin:
			begin = true
		case r.Kind == KindPTYRead && r.Phase == PhaseEnd && r.A == 17:
			end = true
		case r.Kind == KindNotify && r.A == 3:
			notify = true
		case r.Kind == KindTimerLate:
			timer = true
		case r.Kind == KindMetric && tr.Names[r.Obj] == "a metric":
			metric = true
		}
		if r.TID == 0 {
			t.Fatalf("record without a thread id: %+v", r)
		}
	}
	if !begin || !end || !notify || !timer || !metric {
		t.Fatalf("missing records: begin=%v end=%v notify=%v timer=%v metric=%v (%d records)", begin, end, notify, timer, metric, len(tr.Records))
	}
}
