package main

// shelld-trace had no test on any platform. It is the only tool that reads a
// trace file, so a defect in it is not visible anywhere else: the numbers it
// prints are the evidence a performance claim is built on, and a wrong hop
// latency or a dropped span reads exactly like a fast daemon.
//
// The fixtures here are written by hand rather than by internal/tracing's
// writer, which exists only under the shelld_trace tag and cannot be linked
// into an untagged test. What keeps them honest is that they are decoded by the
// REAL reader, tracing.Read, so a fixture that encodes a shape the format
// cannot hold fails at the first test rather than quietly feeding the analysis
// something no daemon could produce. The remaining gap -- whether the real
// writer produces what this tool reads -- is closed by
// realwriter_trace_test.go, which is tagged and uses the writer itself.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/internal/tracing"
)

// magic repeats internal/tracing's unexported file magic. If the format's magic
// changes this does not silently write a different file: tracing.Read refuses
// it and every test here fails with "not a shelld trace", which is the loud
// failure a duplicated constant should have.
const magic = "SHELLDTR1\n"

// writeTrace encodes a trace file: header, then the name records, then recs in
// the order given. Names are emitted first because the reader binds a name to
// its id when it reads the record, so a name used by an earlier record would
// resolve to "" -- which is also what the daemon's interning guarantees.
func writeTrace(t *testing.T, h tracing.Header, names map[uint64]string, recs ...tracing.Record) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.trace")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	hb, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, magic); err != nil {
		t.Fatal(err)
	}
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(hb)))
	if _, err := f.Write(n[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(hb); err != nil {
		t.Fatal(err)
	}

	ids := make([]uint64, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		text := names[id]
		rec := tracing.Record{Kind: tracing.KindName, Obj: id, Span: uint64(len(text))}
		if _, err := f.Write(encodeRecord(rec)); err != nil {
			t.Fatal(err)
		}
		pad := make([]byte, (len(text)+7)&^7)
		copy(pad, text)
		if _, err := f.Write(pad); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range recs {
		if _, err := f.Write(encodeRecord(r)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// encodeRecord mirrors tracing.encode, which is unexported. Field order and
// offsets come from the Record doc comment ("RecordSize bytes, little endian, in
// field order").
func encodeRecord(r tracing.Record) []byte {
	b := make([]byte, tracing.RecordSize)
	binary.LittleEndian.PutUint64(b[0:], uint64(r.Time))
	binary.LittleEndian.PutUint16(b[8:], uint16(r.Kind))
	b[10] = byte(r.Phase)
	binary.LittleEndian.PutUint16(b[12:], r.CPU)
	binary.LittleEndian.PutUint32(b[16:], r.TID)
	binary.LittleEndian.PutUint64(b[24:], r.Obj)
	binary.LittleEndian.PutUint64(b[32:], r.Span)
	binary.LittleEndian.PutUint64(b[40:], uint64(r.A))
	binary.LittleEndian.PutUint64(b[48:], uint64(r.B))
	return b
}

const traceStart = int64(1_700_000_000_000_000_000)

func header() tracing.Header {
	return tracing.Header{Version: 1, Pid: 4242, Start: traceStart, Kinds: tracing.Kinds[:]}
}

// ms converts milliseconds since the trace start into a record timestamp, so a
// fixture reads in the same unit the tool prints.
func ms(x float64) int64 { return traceStart + int64(x*1e6) }

// load is main's own pipeline: read, sort by time, build the analysis. The sort
// lives in main() rather than in newAnalysis, and several outputs depend on it
// (summary's duration reads the LAST record, echo's wake search assumes
// ascending time), so a test that skipped it would be testing a state main never
// hands the analysis.
func load(t *testing.T, path string) *analysis {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr, err := tracing.Read(f)
	if err != nil {
		t.Fatalf("reading the fixture back: %v", err)
	}
	sort.SliceStable(tr.Records, func(i, j int) bool { return tr.Records[i].Time < tr.Records[j].Time })
	return newAnalysis(tr)
}

// capture collects what f prints. The analysis methods write to os.Stdout with
// fmt.Printf, so the only way to read them is to be os.Stdout for the duration.
// The pipe is drained by a goroutine because these outputs can exceed a pipe
// buffer, and a full pipe would block f forever.
func capture(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = old
	out := <-done
	r.Close()
	return out
}

// TestAFixtureRoundTripsThroughTheRealReader is the gate on every other test in
// this file. The fixtures are encoded here and decoded by tracing.Read, so if
// the two disagree about a field's offset the analysis would be handed plausible
// records with the wrong values -- an 8-byte slip puts a span's duration in its
// byte count -- and every assertion below would be measuring the slip instead of
// the tool. It also pins the thing a naive encoder gets wrong: a name record's
// text is padded to 8 bytes and is NOT a record.
func TestAFixtureRoundTripsThroughTheRealReader(t *testing.T) {
	want := tracing.Record{
		Time: ms(1.5), Kind: tracing.KindRender, Phase: tracing.PhaseEnd,
		CPU: 7, TID: 99, Obj: 3, Span: 11, A: 4096, B: int64(2<<4) | int64(tracing.RenderDiff),
	}
	path := writeTrace(t, header(), map[uint64]string{5: "a name", 6: "seven+1"}, want)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr, err := tracing.Read(f)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tr.Header.Pid != 4242 || tr.Header.Start != traceStart {
		t.Errorf("header: pid %d start %d, want 4242 and %d", tr.Header.Pid, tr.Header.Start, traceStart)
	}
	if len(tr.Records) != 1 {
		t.Fatalf("got %d records, want 1 (a name record must not become a record)", len(tr.Records))
	}
	if got := tr.Records[0]; got != want {
		t.Errorf("record round-trip:\n got %+v\nwant %+v", got, want)
	}
	// Six bytes of text and seven bytes of text both pad to 8: the second name
	// is the case that catches a padding rule of "round up only when longer".
	if tr.Names[5] != "a name" || tr.Names[6] != "seven+1" {
		t.Errorf("names: %q and %q, want \"a name\" and \"seven+1\"", tr.Names[5], tr.Names[6])
	}
}

// TestSpanMatchingKeepsOnlyPairsAndInheritsBegins is about newAnalysis, which
// every subcommand reads through.
//
// Three decisions are load-bearing and none of them is visible in the output as
// anything but a number:
//
//   - An End with no Begin is dropped. A trace always starts mid-flight (the
//     buffer wrapped, records were dropped, a span began before recording
//     started), so treating an orphan End as a span would invent one beginning
//     at zero -- a 1.7-billion-second duration landing in the max column of
//     whatever kind it was.
//   - A span's fields come from its End, except that a zero a falls back to the
//     Begin's a. That is how "bytes" reaches the span for kinds that record the
//     count at Begin (pty.write, vt.advance), and dropping it turns every such
//     span into 0 bytes.
//   - A Begin still open when the trace ends is not a span either, which is the
//     normal state of a read blocked on the PTY when the daemon was killed.
func TestSpanMatchingKeepsOnlyPairsAndInheritsBegins(t *testing.T) {
	path := writeTrace(t, header(), nil,
		// A complete span: the End carries the byte count.
		tracing.Record{Time: ms(1), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: 1, Span: 10},
		tracing.Record{Time: ms(3), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: 1, Span: 10, A: 64},
		// A span whose count was recorded at Begin: the End's a is zero.
		tracing.Record{Time: ms(4), Kind: tracing.KindPTYWrite, Phase: tracing.PhaseBegin, Obj: 1, Span: 11, A: 7},
		tracing.Record{Time: ms(5), Kind: tracing.KindPTYWrite, Phase: tracing.PhaseEnd, Obj: 1, Span: 11},
		// An End whose Begin is not in the trace.
		tracing.Record{Time: ms(6), Kind: tracing.KindVTAdvance, Phase: tracing.PhaseEnd, Obj: 1, Span: 12, A: 1},
		// A Begin with no End: still open when the trace ended.
		tracing.Record{Time: ms(7), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: 2, Span: 13},
	)
	a := load(t, path)

	if len(a.spans) != 2 {
		t.Fatalf("got %d spans, want 2 (the orphan End and the open Begin must not become spans): %+v", len(a.spans), a.spans)
	}
	read := a.byKind[tracing.KindPTYRead]
	if len(read) != 1 || read[0].begin != ms(1) || read[0].end != ms(3) || read[0].a != 64 {
		t.Errorf("pty.read span: %+v, want begin %d end %d a 64", read, ms(1), ms(3))
	}
	write := a.byKind[tracing.KindPTYWrite]
	if len(write) != 1 || write[0].a != 7 {
		t.Errorf("pty.write span: %+v, want a 7 inherited from its Begin", write)
	}
	if got := len(a.byKind[tracing.KindVTAdvance]); got != 0 {
		t.Errorf("vt.advance spans: %d, want 0", got)
	}
}

// TestAttachOpenBuildsBothDirectionsAndEveryWindowIsCounted pins the maps that
// carry a connection back to its window. echo needs windowOf to follow input
// from a connection to the PTY it reached, so an inverted map (the window id
// used as the connection key) makes every hop after the first fail to match and
// echo reports "0 inputs traced" -- a silence that reads like a quiet daemon.
//
// It also pins what summary counts. The count used to come from NAME records
// alone, which undercounted every window that never emitted one -- including any
// window that already existed when tracing was switched on, since the name is
// recorded once at creation. The four windows below are reachable four different
// ways on purpose: named and attached, named only, attached only, and mentioned
// only by a span. A count that reads one source misses at least one of them.
func TestAttachOpenBuildsBothDirectionsAndEveryWindowIsCounted(t *testing.T) {
	path := writeTrace(t, header(), map[uint64]string{100: "shell", 101: "build"},
		tracing.Record{Time: ms(1), Kind: tracing.KindWindowName, Obj: 7, A: 100},
		tracing.Record{Time: ms(1), Kind: tracing.KindWindowName, Obj: 8, A: 101},
		tracing.Record{Time: ms(2), Kind: tracing.KindAttachOpen, Obj: 50, A: 7, B: 80<<16 | 24},
		tracing.Record{Time: ms(3), Kind: tracing.KindAttachOpen, Obj: 51, A: 7},
		// A window nobody named, attached to.
		tracing.Record{Time: ms(4), Kind: tracing.KindAttachOpen, Obj: 52, A: 9},
		// A window nobody named and nobody attached to, which only produced
		// output. This is the shape of a window running a build on its own.
		tracing.Record{Time: ms(5), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: 11, Span: 60},
		tracing.Record{Time: ms(6), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: 11, Span: 60, A: 32},
	)
	a := load(t, path)

	if a.windowOf[50] != 7 || a.windowOf[51] != 7 || a.windowOf[52] != 9 {
		t.Errorf("windowOf = %v, want conns 50,51 -> window 7 and conn 52 -> window 9", a.windowOf)
	}
	if got := a.connsOf[7]; len(got) != 2 || got[0] != 50 || got[1] != 51 {
		t.Errorf("connsOf[7] = %v, want [50 51] in the order they opened", got)
	}
	if a.winName[7] != "shell" || a.winName[8] != "build" {
		t.Errorf("winName = %v, want 7 -> shell and 8 -> build", a.winName)
	}
	for _, id := range []uint64{7, 8, 9, 11} {
		if !a.windows[id] {
			t.Errorf("window %d is not counted; windows = %v", id, a.windows)
		}
	}
	if len(a.windows) != 4 {
		t.Errorf("windows = %v, want exactly the four the trace mentions", a.windows)
	}
	out := capture(t, a.summary)
	if !strings.Contains(out, "4 windows, 3 attach connections") {
		t.Errorf("summary counts every window mentioned and connections from attach opens; got:\n%s", out)
	}
}

// TestWindowIdZeroIsNotAWindow pins the one id that must not be counted.
// tracing.NewID returns 0 in the build without the shelld_trace tag, so 0 is the
// "not tracing" sentinel rather than a window; a trace that carries it -- records
// written while the tag was off, or an attach.open whose window had no id yet --
// would otherwise add a phantom window to every summary.
func TestWindowIdZeroIsNotAWindow(t *testing.T) {
	path := writeTrace(t, header(), nil,
		tracing.Record{Time: ms(1), Kind: tracing.KindWindowName, Obj: 0, A: 0},
		tracing.Record{Time: ms(2), Kind: tracing.KindAttachOpen, Obj: 50, A: 0},
		tracing.Record{Time: ms(3), Kind: tracing.KindNotify, Obj: 4, A: 1},
	)
	a := load(t, path)
	if a.windows[0] {
		t.Error("window id 0 was counted as a window")
	}
	if len(a.windows) != 1 || !a.windows[4] {
		t.Errorf("windows = %v, want just window 4", a.windows)
	}
}

// TestPercentilesAreIndexedNotInterpolated states the convention dist.q uses,
// because the numbers it produces are quoted as p50/p90/p99 and read as if they
// were the usual ones. They are not: q indexes the sorted samples at int(p*n)
// and clamps to the last, so with fewer than 100 samples p99 IS the maximum, and
// p50 is the upper of the two middle samples on an even count.
//
// The test exists to make a change to that rule visible. Someone switching to an
// interpolating percentile would move every published p99 in a way no other test
// would notice, and someone "fixing" the clamp would index out of range on
// exactly the trace sizes the tool is used on.
func TestPercentilesAreIndexedNotInterpolated(t *testing.T) {
	var d dist
	for i := 1; i <= 10; i++ {
		d = append(d, float64(i))
	}
	d[0], d[9] = d[9], d[0] // deliberately unsorted

	for _, tc := range []struct{ p, want float64 }{
		{0.5, 6}, // int(0.5*10) = 5 -> the 6th smallest, not the mean of 5 and 6
		{0.9, 10},
		{0.99, 10}, // clamped to the last: p99 of 10 samples is the max
		{1, 10},
	} {
		if got := d.q(tc.p); got != tc.want {
			t.Errorf("q(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := (dist{}).q(0.5); got != 0 {
		t.Errorf("q on no samples = %v, want 0", got)
	}
	if got := (dist{}).line(); got != "n=0" {
		t.Errorf("line on no samples = %q, want \"n=0\"", got)
	}
	// q must not sort in place: echo builds one dist per hop and prints them in
	// order, and a q that reordered its receiver would misalign the rows.
	if d[0] != 10 || d[9] != 1 {
		t.Errorf("q sorted the caller's slice in place: %v", d)
	}
}

// TestSummaryReportsDropsRenderModesAndWakeReasons pins the four summary
// readings that a reader acts on.
//
// Dropped records are the reason to distrust everything else in the file, and
// they are reported as a SUM over drop records rather than the last one -- a
// mutant keeping only the latest under-reports a busy trace. Render modes are
// packed with the generation (b = generation<<4 | mode), so decoding b without
// the mask attributes every render to a mode by its generation number, which is
// almost always out of range and silently counts as nothing. Wake reasons are
// indexed by reason id, so an off-by-one labels every dirty wake as a first wake.
func TestSummaryReportsDropsRenderModesAndWakeReasons(t *testing.T) {
	gen := int64(9) // large enough that b>>4 is outside the mode range
	path := writeTrace(t, header(), map[uint64]string{200: "sched/latencies:seconds#p99"},
		tracing.Record{Time: ms(1), Kind: tracing.KindDropped, A: 12},
		tracing.Record{Time: ms(2), Kind: tracing.KindDropped, A: 30},
		// Two diff renders and one full one, all with a nonzero generation.
		tracing.Record{Time: ms(3), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: 1, Span: 1},
		tracing.Record{Time: ms(4), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: 1, Span: 1, A: 10, B: gen<<4 | int64(tracing.RenderDiff)},
		tracing.Record{Time: ms(5), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: 1, Span: 2},
		tracing.Record{Time: ms(6), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: 1, Span: 2, A: 10, B: gen<<4 | int64(tracing.RenderDiff)},
		tracing.Record{Time: ms(7), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: 1, Span: 3},
		tracing.Record{Time: ms(8), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: 1, Span: 3, A: 99, B: gen<<4 | int64(tracing.RenderFull)},
		// Wake reasons: one dirty, one timer, and one out-of-range id that must
		// not be counted as anything.
		tracing.Record{Time: ms(9), Kind: tracing.KindTickWake, Obj: 1, A: int64(tracing.WakeDirty), B: 2e6},
		tracing.Record{Time: ms(10), Kind: tracing.KindTickWake, Obj: 1, A: int64(tracing.WakeTimer), B: 8e6},
		tracing.Record{Time: ms(11), Kind: tracing.KindTickWake, Obj: 1, A: 99, B: 1e6},
		tracing.Record{Time: ms(12), Kind: tracing.KindGC, A: 1},
		tracing.Record{Time: ms(13), Kind: tracing.KindMetric, Obj: 200, A: 5, B: 3},
		tracing.Record{Time: ms(14), Kind: tracing.KindMetric, Obj: 200, A: 2, B: 4},
	)
	out := capture(t, load(t, path).summary)

	for _, want := range []string{
		"pid 4242",
		"42 dropped", // 12 + 30, not 30
		"renders: 0 skipped, 2 diff, 1 full, 0 paused",
		"dirty",
		"timer",
		"GC cycles: 1",
		"sched/latencies:seconds#p99",
		"2 / 5", // last / max, in that order: 2 arrived after 5
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q; got:\n%s", want, out)
		}
	}
	// The labels for reasons nobody woke for must not be printed at all, which is
	// what keeps an out-of-range reason id from looking like a first wake.
	for _, absent := range []string{"first ", "ctrl ", "stop "} {
		if strings.Contains(out, absent) {
			t.Errorf("summary printed a wake label %q that no record used; got:\n%s", absent, out)
		}
	}
}

// TestSummaryOnAnEmptyTraceSaysSo covers the guard that stops summary indexing
// the last record of nothing. A header with no records is what SHELLD_TRACE
// leaves when a daemon is killed before the writer's first flush, so this is a
// file people really do open.
func TestSummaryOnAnEmptyTraceSaysSo(t *testing.T) {
	out := capture(t, load(t, writeTrace(t, header(), nil)).summary)
	if strings.TrimSpace(out) != "empty trace" {
		t.Errorf("summary of an empty trace = %q, want \"empty trace\"", out)
	}
}

// echoFixture is one input's full round trip, in the order the daemon records
// it: the client's frame arrives, is written to the PTY, the shell echoes it,
// the VT parses it, the tick loop wakes, renders, and writes the frame back.
// Consecutive hops are 1 ms apart so a swapped pair of endpoints shows up as a
// negative or doubled number rather than as noise.
func echoFixture() []tracing.Record {
	const conn, win = uint64(50), uint64(7)
	const gen = int64(4)
	return []tracing.Record{
		{Time: ms(1), Kind: tracing.KindAttachOpen, Obj: conn, A: int64(win)},
		// ws.read: the Begin is when the daemon started waiting for the client,
		// so the blocked time is deliberately long and must not be part of a hop.
		{Time: ms(2), Kind: tracing.KindWSRead, Phase: tracing.PhaseBegin, Obj: conn, Span: 1},
		{Time: ms(100), Kind: tracing.KindWSRead, Phase: tracing.PhaseEnd, Obj: conn, Span: 1, A: 3, B: 2},
		{Time: ms(100), Kind: tracing.KindInput, Phase: tracing.PhaseBegin, Obj: conn, Span: 2},
		{Time: ms(101), Kind: tracing.KindInput, Phase: tracing.PhaseEnd, Obj: conn, Span: 2, A: 3},
		// A PTY read that returned nothing: the echo has not arrived yet, and it
		// must not be mistaken for it.
		{Time: ms(101), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: win, Span: 3},
		{Time: ms(101.5), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: win, Span: 3, A: 0},
		{Time: ms(101.5), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: win, Span: 4},
		{Time: ms(102), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: win, Span: 4, A: 3},
		{Time: ms(102), Kind: tracing.KindVTAdvance, Phase: tracing.PhaseBegin, Obj: win, Span: 5, A: 3},
		{Time: ms(103), Kind: tracing.KindVTAdvance, Phase: tracing.PhaseEnd, Obj: win, Span: 5, A: 3, B: gen},
		// A wake on another connection first: it must be skipped, not consumed.
		{Time: ms(103.5), Kind: tracing.KindTickWake, Obj: 51, A: int64(tracing.WakeDirty), B: 1e6},
		{Time: ms(104), Kind: tracing.KindTickWake, Obj: conn, A: int64(tracing.WakeDirty), B: 1e6},
		// A render of an older generation: it cannot be carrying this input, and
		// taking it would report an impossibly small hop.
		{Time: ms(104), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: conn, Span: 6},
		{Time: ms(104.5), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: conn, Span: 6, A: 5, B: (gen-1)<<4 | int64(tracing.RenderDiff)},
		{Time: ms(104.5), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: conn, Span: 7},
		{Time: ms(105), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: conn, Span: 7, A: 20, B: gen<<4 | int64(tracing.RenderDiff)},
		{Time: ms(105), Kind: tracing.KindWSWrite, Phase: tracing.PhaseBegin, Obj: conn, Span: 8},
		{Time: ms(106), Kind: tracing.KindWSWrite, Phase: tracing.PhaseEnd, Obj: conn, Span: 8, A: 20, B: 2},
	}
}

// TestEchoMeasuresEachHopFromTheRightEndpoint is the test this tool most needs.
// echo's seven columns are the per-hop latency budget quoted in every latency
// investigation, and each is a difference between two timestamps chosen out of
// six candidate spans. Every one of those choices is silently wrong-able:
//
//   - using a Begin where an End belongs (or the reverse) moves time between
//     adjacent hops while the total still looks right;
//   - the first hop must start at the read's END, because its Begin is when the
//     daemon began waiting for the client -- 98 ms of idle here, which a
//     Begin-based hop would report as the daemon's own latency;
//   - the echo must be matched to a PTY read that returned bytes, to a render of
//     at least the generation the parse produced, and to a wake on the same
//     connection, or the row is built from another input's spans.
//
// The fixture has a decoy at each of those three points, so the assertion is on
// exact millisecond values rather than on a row merely existing.
func TestEchoMeasuresEachHopFromTheRightEndpoint(t *testing.T) {
	out := capture(t, func() { load(t, writeTrace(t, header(), nil, echoFixture()...)).echo(5) })

	if !strings.Contains(out, "1 inputs traced to their echo frame") {
		t.Fatalf("echo did not trace the one input in the fixture:\n%s", out)
	}
	for _, tc := range []struct{ hop, want string }{
		{"ws.read→pty.write done", "1.000"},     // 100 -> 101
		{"pty.write→pty.read of echo", "1.000"}, // 101 -> 102, skipping the 0-byte read
		{"pty.read→parsed", "1.000"},            // 102 -> 103
		{"parsed→tick wake", "1.000"},           // 103 -> 104, skipping conn 51's wake
		{"tick wake→render done", "1.000"},      // 104 -> 105, skipping the older generation
		{"render→ws.write done", "1.000"},       // 105 -> 106
		{"total (ws.read→ws.write)", "6.000"},   // 100 -> 106
		{"ws.read blocked (idle wait)", "98.000"},
	} {
		line := lineWith(out, tc.hop)
		if line == "" {
			t.Errorf("no %q row in:\n%s", tc.hop, out)
			continue
		}
		// One sample, so p50, p90, p99 and max are all this number: an endpoint
		// taken from the wrong record cannot round into agreement.
		if !strings.Contains(line, "n=1") || strings.Count(line, tc.want) != 4 {
			t.Errorf("hop %q: %q, want n=1 and %s in all four columns", tc.hop, line, tc.want)
		}
	}
	if !strings.Contains(out, "slowest:") || !strings.Contains(out, "conn 50:") {
		t.Errorf("the slowest list should name the connection; got:\n%s", out)
	}
}

// TestEchoDropsARowWhoseChainIsIncomplete states the other half of echo's
// contract: a row is reported only when every hop was found. Reporting a partial
// chain would put a zero (or a difference against a missing endpoint) into the
// per-hop distributions, which is worse than a smaller n -- it moves p50 towards
// zero, so the daemon would look faster the more spans a trace is missing.
//
// Each case removes exactly one link from the complete fixture.
func TestEchoDropsARowWhoseChainIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(tracing.Record) bool
	}{
		{"no input span", func(r tracing.Record) bool { return r.Kind == tracing.KindInput }},
		{"no pty read carrying bytes", func(r tracing.Record) bool {
			return r.Kind == tracing.KindPTYRead && r.A == 3
		}},
		{"nothing parsed", func(r tracing.Record) bool { return r.Kind == tracing.KindVTAdvance }},
		{"no wake on this connection", func(r tracing.Record) bool {
			return r.Kind == tracing.KindTickWake && r.Obj == 50
		}},
		{"no render of that generation", func(r tracing.Record) bool {
			return r.Kind == tracing.KindRender && r.B>>4 >= 4
		}},
		{"no frame written back", func(r tracing.Record) bool { return r.Kind == tracing.KindWSWrite }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := echoFixture()
			var kept []tracing.Record
			for _, r := range full {
				if !tc.drop(r) {
					kept = append(kept, r)
				}
			}
			if len(kept) == len(full) {
				t.Fatalf("the case removed nothing, so it proves nothing")
			}
			out := capture(t, func() { load(t, writeTrace(t, header(), nil, kept...)).echo(5) })
			if !strings.Contains(out, "0 inputs traced") {
				t.Errorf("with %s, echo should trace no input; got:\n%s", tc.name, out)
			}
		})
	}
}

// TestEchoIgnoresNonBinaryAndEmptyReads pins the filter on what counts as client
// input. A text frame is a control message (resize, scroll) and an empty binary
// read is a close or a keepalive; counting either as an input measures a hop that
// never happened and pulls the whole distribution towards zero.
func TestEchoIgnoresNonBinaryAndEmptyReads(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bytes, opcode int64
	}{
		{"a text frame", 3, 1},
		{"an empty binary frame", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := echoFixture()
			for i, r := range recs {
				if r.Kind == tracing.KindWSRead && r.Phase == tracing.PhaseEnd {
					recs[i].A, recs[i].B = tc.bytes, tc.opcode
				}
			}
			out := capture(t, func() { load(t, writeTrace(t, header(), nil, recs...)).echo(5) })
			if !strings.Contains(out, "0 inputs traced") {
				t.Errorf("%s is not client input; got:\n%s", tc.name, out)
			}
		})
	}
}

// TestMetricsBucketsByIntervalAndKeepsTheLastSample pins the two decisions in
// the metrics table: one row per -every window holding the LAST sample in it
// (not the first, and not an average), and percentile metrics hidden unless a
// -name filter asks for them.
//
// The second is the one worth a test. The names containing "#" are the
// percentile and max summaries, and they are the numbers a latency investigation
// wants; dropping them when a filter IS given -- the mutation that makes the
// condition symmetric -- makes `-name latencies` print an empty table, which
// reads as "the daemon recorded nothing".
func TestMetricsBucketsByIntervalAndKeepsTheLastSample(t *testing.T) {
	names := map[uint64]string{1: "sched/goroutines:goroutines", 2: "sched/latencies:seconds#p99"}
	path := writeTrace(t, header(), names,
		tracing.Record{Time: ms(10), Kind: tracing.KindMetric, Obj: 1, A: 5},
		tracing.Record{Time: ms(90), Kind: tracing.KindMetric, Obj: 1, A: 7}, // same 100 ms bucket, later
		tracing.Record{Time: ms(110), Kind: tracing.KindMetric, Obj: 1, A: 9},
		tracing.Record{Time: ms(120), Kind: tracing.KindMetric, Obj: 2, A: 44},
	)

	out := capture(t, func() { load(t, path).metrics("", 100) })
	if strings.Contains(out, "#p99") {
		t.Errorf("with no filter the percentile metrics are hidden; got:\n%s", out)
	}
	rows := dataRows(out)
	if len(rows) != 2 {
		t.Fatalf("want one row per 100 ms bucket, got %d:\n%s", len(rows), out)
	}
	if !strings.Contains(rows[0], "7") || strings.Contains(rows[0], "5") {
		t.Errorf("the first bucket should hold the last sample in it (7, not 5): %q", rows[0])
	}
	if !strings.HasPrefix(strings.TrimSpace(rows[1]), "100") {
		t.Errorf("the second row should be labelled with its bucket start (100 ms): %q", rows[1])
	}

	out = capture(t, func() { load(t, path).metrics("latencies", 100) })
	if !strings.Contains(out, "sched/latencies:seconds#p99") || !strings.Contains(out, "44") {
		t.Errorf("a filter naming a percentile metric must show it; got:\n%s", out)
	}
	if strings.Contains(out, "goroutines") {
		t.Errorf("the filter must exclude metrics that do not match; got:\n%s", out)
	}
}

// TestTimelineIncludesBothEdgesOfItsWindow pins the window arithmetic: -at is
// the centre and -window the total width, so a record exactly at either edge is
// in. Halving or doubling the width is the kind of change that makes a stall
// investigation read the wrong 10 ms and conclude nothing was happening.
func TestTimelineIncludesBothEdgesOfItsWindow(t *testing.T) {
	path := writeTrace(t, header(), map[uint64]string{9: "a/metric:things"},
		tracing.Record{Time: ms(89.9), Kind: tracing.KindNotify, Obj: 1, A: 1},
		tracing.Record{Time: ms(90), Kind: tracing.KindNotify, Obj: 2, A: 2},
		tracing.Record{Time: ms(100), Kind: tracing.KindMetric, Obj: 9, A: 3},
		tracing.Record{Time: ms(110), Kind: tracing.KindNotify, Obj: 4, A: 4},
		tracing.Record{Time: ms(110.1), Kind: tracing.KindNotify, Obj: 5, A: 5},
	)
	out := capture(t, func() { load(t, path).timeline(100, 20) })

	for _, obj := range []string{"obj 2", "obj 9", "obj 4"} {
		if !strings.Contains(out, obj) {
			t.Errorf("record %s is inside [90,110] and must be shown; got:\n%s", obj, out)
		}
	}
	for _, obj := range []string{"obj 1", "obj 5"} {
		if strings.Contains(out, obj) {
			t.Errorf("record %s is outside [90,110] and must not be shown; got:\n%s", obj, out)
		}
	}
	// A metric record is unreadable without its name, since its obj is an
	// interned id rather than a window or a connection.
	if !strings.Contains(out, "a/metric:things") {
		t.Errorf("a metric line must carry the interned name; got:\n%s", out)
	}
}

// TestChromeWritesEveryRecordWithADurationFloor pins the export another program
// consumes. Perfetto drops a zero-duration complete event, so the 0.001 µs floor
// is what keeps the sub-microsecond spans -- exactly the ones an optimisation is
// trying to remove -- on the timeline at all.
//
// The window-name argument is the other decision here: obj is a window id for
// pty.* and vt.* spans and a CONNECTION id for the rest, so labelling by obj
// without checking the prefix puts a window's name on a connection's span
// whenever the two ids collide, which they routinely do (both start small).
func TestChromeWritesEveryRecordWithADurationFloor(t *testing.T) {
	path := writeTrace(t, header(), map[uint64]string{300: "shell", 301: "sched/goroutines:goroutines"},
		tracing.Record{Time: ms(1), Kind: tracing.KindWindowName, Obj: 7, A: 300},
		// A zero-length span: Begin and End at the same instant.
		tracing.Record{Time: ms(2), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: 7, Span: 1},
		tracing.Record{Time: ms(2), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: 7, Span: 1, A: 8},
		// A connection span whose obj collides with the window id.
		tracing.Record{Time: ms(3), Kind: tracing.KindWSWrite, Phase: tracing.PhaseBegin, Obj: 7, Span: 2},
		tracing.Record{Time: ms(4), Kind: tracing.KindWSWrite, Phase: tracing.PhaseEnd, Obj: 7, Span: 2, A: 9, B: 2},
		tracing.Record{Time: ms(5), Kind: tracing.KindNetpollPing, A: 3e6},
		tracing.Record{Time: ms(6), Kind: tracing.KindMetric, Obj: 301, A: 12},
		tracing.Record{Time: ms(7), Kind: tracing.KindNotify, Obj: 7, A: 2},
	)
	a := load(t, path)
	outPath := filepath.Join(t.TempDir(), "chrome.json")
	// Two spans and four instants: every record reaches the export, which is what
	// makes it usable as the primary view of a trace.
	if out := capture(t, func() { a.chrome(outPath) }); !strings.Contains(out, "wrote 6 events") {
		t.Errorf("chrome should report the 6 events it wrote; got %q", out)
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		TraceEvents []struct {
			Name string         `json:"name"`
			Cat  string         `json:"cat"`
			Ph   string         `json:"ph"`
			Ts   float64        `json:"ts"`
			Dur  float64        `json:"dur"`
			Pid  int            `json:"pid"`
			Args map[string]any `json:"args"`
		} `json:"traceEvents"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("the export must be valid JSON for Perfetto: %v", err)
	}
	byName := map[string]int{}
	for i, e := range file.TraceEvents {
		byName[e.Name] = i
		if e.Pid != 4242 {
			t.Errorf("event %q has pid %d, want the traced process's 4242", e.Name, e.Pid)
		}
	}
	for _, want := range []string{"pty.read", "ws.write", "canary.netpoll", "sched/goroutines:goroutines", "window.notify"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("no %q event in the export: %s", want, b)
		}
	}
	read := file.TraceEvents[byName["pty.read"]]
	if read.Ph != "X" || read.Dur != 0.001 {
		t.Errorf("a zero-length span: ph %q dur %v, want \"X\" and the 0.001 floor", read.Ph, read.Dur)
	}
	if read.Cat != "pty" {
		t.Errorf("category %q, want the part before the first dot", read.Cat)
	}
	if read.Args["window"] != "shell" {
		t.Errorf("a pty.read span should carry its window name, got %v", read.Args["window"])
	}
	if w, ok := file.TraceEvents[byName["ws.write"]].Args["window"]; ok {
		t.Errorf("a ws.write span's obj is a connection, not a window; it must not be named %v", w)
	}
	if e := file.TraceEvents[byName["canary.netpoll"]]; e.Ph != "C" || e.Args["ms"] != 3.0 {
		t.Errorf("a canary is a counter in ms: ph %q args %v", e.Ph, e.Args)
	}
	if e := file.TraceEvents[byName["sched/goroutines:goroutines"]]; e.Ph != "C" || e.Args["value"] != 12.0 {
		t.Errorf("a metric is a counter named by its interned name: ph %q args %v", e.Ph, e.Args)
	}
	if e := file.TraceEvents[byName["window.notify"]]; e.Ph != "i" {
		t.Errorf("an instant that is not a counter is ph %q, want \"i\"", e.Ph)
	}
}

// TestStallsReportsWhatEveryThreadWasDoing pins the one output whose purpose is
// attribution rather than measurement. A stall is only actionable with the spans
// that overlapped it, and the overlap is a clamped intersection, so a span that
// merely reaches into the window contributes the part inside it -- not its whole
// duration, which would attribute a 46 ms PTY read to a 6 ms stall.
//
// It also pins the -min filter, because a threshold compared in the wrong unit
// lists every canary record and buries the one that matters.
func TestStallsReportsWhatEveryThreadWasDoing(t *testing.T) {
	path := writeTrace(t, header(), nil,
		// A 6 ms netpoll stall ending at 100 ms, so it covers [94,100].
		tracing.Record{Time: ms(100), Kind: tracing.KindNetpollPing, A: 6e6},
		// A 1 ms one, below the filter.
		tracing.Record{Time: ms(200), Kind: tracing.KindNetpollPing, A: 1e6},
		// A span on thread 11 that started long before the stall and ended inside
		// it: only the overlap counts.
		tracing.Record{Time: ms(50), Kind: tracing.KindPTYRead, Phase: tracing.PhaseBegin, Obj: 1, Span: 1, TID: 11},
		tracing.Record{Time: ms(96), Kind: tracing.KindPTYRead, Phase: tracing.PhaseEnd, Obj: 1, Span: 1, TID: 11, A: 1},
		// A span entirely inside the stall, on another thread.
		tracing.Record{Time: ms(97), Kind: tracing.KindVTAdvance, Phase: tracing.PhaseBegin, Obj: 1, Span: 2, TID: 12},
		tracing.Record{Time: ms(98), Kind: tracing.KindVTAdvance, Phase: tracing.PhaseEnd, Obj: 1, Span: 2, TID: 12, A: 1, B: 1},
		// A span after the stall: it must not appear.
		tracing.Record{Time: ms(150), Kind: tracing.KindRender, Phase: tracing.PhaseBegin, Obj: 1, Span: 3, TID: 13},
		tracing.Record{Time: ms(151), Kind: tracing.KindRender, Phase: tracing.PhaseEnd, Obj: 1, Span: 3, TID: 13, A: 1},
	)
	out := capture(t, func() { load(t, path).stalls(5 * time.Millisecond) })

	if !strings.Contains(out, "1 canary delays") {
		t.Errorf("only the 6 ms delay is at or above the 5 ms filter; got:\n%s", out)
	}
	if !strings.Contains(out, "canary.netpoll 6.000 ms, from 94.000 to 100.000 ms") {
		t.Errorf("the stall's window runs back from the record by its delay; got:\n%s", out)
	}
	if l := lineWith(out, "thread 11"); !strings.Contains(l, "pty.read 2.00ms") {
		t.Errorf("thread 11's overlap with [94,100] is 2 ms of a 46 ms read: %q", l)
	}
	if l := lineWith(out, "thread 12"); !strings.Contains(l, "vt.advance 1.00ms") {
		t.Errorf("thread 12 spent 1 ms inside the stall: %q", l)
	}
	if strings.Contains(out, "thread 13") {
		t.Errorf("thread 13's span is after the stall and must not be attributed to it; got:\n%s", out)
	}
}

// TestTheCommandLineRefusesWhatItCannotDo pins main's own wiring, which no test
// of the analysis reaches: the argument count, the exit codes, the unknown
// command, and chrome's required -o. These are the failures a person meets
// first, and an exit status of 0 on a command that printed nothing is the shape
// that makes a script believe it has a trace summary.
//
// Each case runs THIS test binary as a child with an argv it takes from the
// environment (see TestMain), so it executes the real main() rather than a copy
// of its logic.
func TestTheCommandLineRefusesWhatItCannotDo(t *testing.T) {
	good := writeTrace(t, header(), nil,
		tracing.Record{Time: ms(1), Kind: tracing.KindGC, A: 1},
	)
	// One row's wording belongs to the OS rather than to this tool, and it is the
	// only one here that does. Linux renders a missing file as "no such file or
	// directory" and Windows as "The system cannot find the file specified.", so
	// the literal that used to sit in that row asserted the host it was written
	// on and nothing else. It did not read as a gap either: nine sibling rows
	// passed beside it, and the row itself passed on every Linux and macOS run
	// the tree had ever had. It failed the first time the package was run on
	// Windows, against a tool that had behaved correctly -- exit 1, the path
	// named, the OS's own reason printed.
	absent := filepath.Join(t.TempDir(), "absent")
	wantAbsent := notFoundText(t, absent)
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no arguments at all", nil, 2, "usage:"},
		{"a command with no file", []string{"summary"}, 2, "usage:"},
		{"a file that is not there", []string{"summary", absent}, 1, wantAbsent},
		{"a file that is not a trace", []string{"summary", writeNotATrace(t)}, 1, "not a shelld trace"},
		{"a command nobody has", []string{"wat", good}, 1, `unknown command "wat"`},
		{"chrome with nowhere to write", []string{"chrome", good}, 1, "chrome needs -o"},
		// -every is a divisor. At 0 the bucket index is int64(±Inf), which Go
		// leaves undefined, so the table printed a row labelled with an arbitrary
		// number -- output that looks like data. Negative buckets time backwards.
		{"metrics with a zero bucket", []string{"metrics", good, "-every", "0"}, 1, "-every must be greater than 0"},
		{"metrics with a negative bucket", []string{"metrics", good, "-every", "-5"}, 1, "-every must be greater than 0"},
		// A non-positive -window makes from >= to, so timeline prints nothing and
		// exits 0 -- which reads as "nothing was happening at that moment".
		{"timeline with no width", []string{"timeline", good, "-at", "1", "-window", "0"}, 1, "-window must be greater than 0"},
		{"timeline with a negative width", []string{"timeline", good, "-at", "1", "-window", "-2"}, 1, "-window must be greater than 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr %q, want it to mention %q", stderr, tc.wantErr)
			}
		})
	}
	// The positive control: the same path with everything present must succeed,
	// or the cases above would pass for a binary that refuses everything.
	code, stdout, stderr := runCLI(t, "summary", good)
	if code != 0 || !strings.Contains(stdout, "pid 4242") {
		t.Errorf("a well-formed summary: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// notFoundText returns THIS host's wording for opening a path that is not there,
// by making the failure rather than by writing the sentence down.
//
// It returns only the OS's half. A *fs.PathError renders as "open <path>: <os
// text>", and the path is the test's own temp directory, so asserting the whole
// string would pin the tool's wrapping and its choice of verb as well -- which is
// not what the row is about. pe.Err is the part the two hosts disagree about and
// the only part worth asserting.
//
// The two premise checks are what keep this from degrading into a tautology. If
// the path existed, or the failure were something other than not-found, this
// would hand back some other OS sentence and the row would go green against an
// error that is not the one it names -- a permission error would satisfy it just
// as well. Failing here instead says which of the two assumptions broke.
func notFoundText(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		t.Fatalf("premise: %s exists, so opening it cannot produce a not-found error", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("premise: opening %s gave %v, which is not a not-found error", path, err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("premise: %v is not a *fs.PathError, so the OS's own text cannot be separated from it", err)
	}
	return pe.Err.Error()
}

func writeNotATrace(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(path, []byte("this is not a trace file at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// childEnv marks a child process. A child that sees it runs main() on its own
// argv instead of running the tests; the arguments are passed as real process
// arguments rather than through the environment, which cannot carry the NUL a
// packed argv would need.
const childEnv = "SHELLD_TRACE_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		// os.Args is already [binary, args...], the shape main() reads.
		main()
		// main returned without exiting, which for these commands means success.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI runs main() in a child with these arguments (argv[1] onwards).
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	switch e := cmd.Run().(type) {
	case nil:
		code = 0
	case *exec.ExitError:
		code = e.ExitCode()
	default:
		t.Fatalf("running the child: %v", e)
	}
	return code, out.String(), errOut.String()
}

// lineWith returns the first line containing sub, or "".
func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// dataRows returns the metric table's rows, dropping its "# col" header lines.
func dataRows(out string) []string {
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		rows = append(rows, l)
	}
	return rows
}
