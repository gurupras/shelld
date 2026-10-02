//go:build shelld_trace

package main

// The untagged tests feed this tool fixtures encoded by hand. That leaves one
// gap they cannot close: whether the REAL writer produces what the analysis
// reads. The writer only exists under this tag, and everything about the
// agreement is implicit -- which field a kind puts its byte count in, that a
// render's b packs the generation above the mode, that a span's Begin and End
// carry the same span id, that an interned name reaches the file before the
// records using it. Any of those could change on the writer's side and every
// untagged test here would still pass while echo silently reported nothing.
//
// So this test records one input's full round trip through the real Begin/End/
// Instant/Intern calls in a child process (tracing's init reads SHELLD_TRACE at
// startup, so a child is the only way to turn recording on), then feeds the file
// it leaves through tracing.Read and the analysis, and requires that the chain
// still matches end to end.
//
// Run it with: go test -tags shelld_trace ./cmd/shelld-trace/

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gurupras/shelld/internal/tracing"
)

func TestTheRealWriterProducesAChainEchoCanFollow(t *testing.T) {
	if os.Getenv("SHELLD_TRACE") != "" {
		recordOneEcho()
		return
	}
	path := filepath.Join(t.TempDir(), "real.trace")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheRealWriterProducesAChainEchoCanFollow$")
	cmd.Env = append(os.Environ(), "SHELLD_TRACE="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	a := load(t, path)

	// The pairing the analysis does must find the writer's spans, and the byte
	// counts must arrive where each kind puts them -- pty.read at its End,
	// vt.advance at its Begin.
	if got := a.byKind[tracing.KindPTYRead]; len(got) != 1 || got[0].a != 3 {
		t.Errorf("pty.read spans from the real writer: %+v, want one with a=3", got)
	}
	if got := a.byKind[tracing.KindVTAdvance]; len(got) != 1 || got[0].a != 3 {
		t.Errorf("vt.advance spans from the real writer: %+v, want one with a=3 (recorded at Begin)", got)
	}

	// The whole point: every one of echo's seven matching rules still holds
	// against what the writer actually wrote.
	out := capture(t, func() { a.echo(1) })
	if !strings.Contains(out, "1 inputs traced to their echo frame") {
		t.Fatalf("echo could not follow a chain the real writer recorded:\n%s\n%s", out, timelineOf(t, a))
	}
	// Real timestamps, so the hops are sub-millisecond rather than exact; what
	// must hold is that none of them is negative, which is what an endpoint taken
	// from the wrong record looks like.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "n=1") && strings.Contains(line, "-") {
			t.Errorf("a hop measured negative, so two endpoints are out of order: %q", line)
		}
	}

	// The interning path: Intern wrote a name record, the reader bound it, and
	// the analysis resolved a window name from it.
	named := false
	for _, n := range a.winName {
		named = named || n == "shell"
	}
	if !named {
		t.Errorf("no window named \"shell\": winName = %v, names = %v", a.winName, a.t.Names)
	}
	if len(a.windowOf) != 1 {
		t.Errorf("attach connections = %v, want exactly the one the child opened", a.windowOf)
	}

	// The export must survive a real trace, canary records and all.
	chromePath := filepath.Join(t.TempDir(), "real.json")
	if out := capture(t, func() { a.chrome(chromePath) }); !strings.Contains(out, "wrote ") {
		t.Errorf("chrome on a real trace: %q", out)
	}
	if st, err := os.Stat(chromePath); err != nil || st.Size() == 0 {
		t.Errorf("chrome wrote nothing usable: %v", err)
	}
}

// recordOneEcho is the child: one client keystroke through to the frame written
// back, in the order and with the fields the daemon uses.
func recordOneEcho() {
	const gen = int64(1)
	conn, win := tracing.NewID(), tracing.NewID()
	tracing.Instant(tracing.KindAttachOpen, conn, int64(win), 80<<16|24)
	tracing.Instant(tracing.KindWindowName, win, int64(tracing.Intern("shell")), 0)

	rd := tracing.Begin(tracing.KindWSRead, conn, 0)
	tracing.End(tracing.KindWSRead, conn, rd, 3, 2) // 3 bytes, binary opcode
	in := tracing.Begin(tracing.KindInput, conn, 3)
	tracing.End(tracing.KindInput, conn, in, 3, 0)
	pr := tracing.Begin(tracing.KindPTYRead, win, 0)
	tracing.End(tracing.KindPTYRead, win, pr, 3, 0)
	adv := tracing.Begin(tracing.KindVTAdvance, win, 3)
	tracing.End(tracing.KindVTAdvance, win, adv, 0, gen) // count came from Begin
	tracing.Instant(tracing.KindTickWake, conn, int64(tracing.WakeDirty), 1_000_000)
	rn := tracing.Begin(tracing.KindRender, conn, 0)
	tracing.End(tracing.KindRender, conn, rn, 20, gen<<4|int64(tracing.RenderDiff))
	wr := tracing.Begin(tracing.KindWSWrite, conn, 0)
	tracing.End(tracing.KindWSWrite, conn, wr, 20, 2)

	tracing.Close()
}

// timelineOf renders the whole trace for a failure message: when the chain does
// not match, the only useful evidence is what the writer actually wrote.
func timelineOf(t *testing.T, a *analysis) string {
	t.Helper()
	if len(a.t.Records) == 0 {
		return "(no records)"
	}
	last := a.t.Records[len(a.t.Records)-1]
	sort.SliceStable(a.t.Records, func(i, j int) bool { return a.t.Records[i].Time < a.t.Records[j].Time })
	width := a.ms(last.Time) + 2
	return capture(t, func() { a.timeline(width/2, width) })
}
