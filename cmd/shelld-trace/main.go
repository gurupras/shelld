// Command shelld-trace analyzes trace files written by a shelld built with the
// shelld_trace tag (see internal/tracing).
//
//	shelld-trace summary  FILE             span latencies, canaries, runtime metrics
//	shelld-trace echo     FILE [-slow N]   client input → echo frame, hop by hop
//	shelld-trace stalls   FILE [-min 5ms]  canary stalls with what every thread was doing
//	shelld-trace timeline FILE -at MS [-window MS]  every record around a moment
//	shelld-trace chrome   FILE -o OUT.json Chrome/Perfetto trace (ui.perfetto.dev)
//	shelld-trace metrics  FILE [-name SUBSTR] [-every MS]  runtime metric time series
//
// Times are milliseconds since the trace started unless stated otherwise.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gurupras/shelld/internal/tracing"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: shelld-trace summary|echo|stalls|timeline|chrome FILE [flags]")
		os.Exit(2)
	}
	cmd, path := os.Args[1], os.Args[2]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	slow := fs.Int("slow", 5, "echo: slowest inputs to show in detail")
	minStall := fs.Duration("min", 5*time.Millisecond, "stalls: minimum canary delay")
	at := fs.Float64("at", 0, "timeline: centre, ms since start")
	win := fs.Float64("window", 20, "timeline: total width, ms")
	out := fs.String("o", "", "chrome: output file")
	metricName := fs.String("name", "", "metrics: only metrics whose name contains this")
	every := fs.Float64("every", 1000, "metrics: one row per this many ms")
	_ = fs.Parse(os.Args[3:])

	f, err := os.Open(path)
	if err != nil {
		fail(err)
	}
	t, err := tracing.Read(f)
	f.Close()
	if err != nil {
		fail(err)
	}
	sort.SliceStable(t.Records, func(i, j int) bool { return t.Records[i].Time < t.Records[j].Time })
	a := newAnalysis(t)
	switch cmd {
	case "summary":
		a.summary()
	case "echo":
		a.echo(*slow)
	case "stalls":
		a.stalls(*minStall)
	case "timeline":
		if *win <= 0 {
			fail(fmt.Errorf("-window must be greater than 0, not %g: it is the total width of the slice to print", *win))
		}
		a.timeline(*at, *win)
	case "metrics":
		// A zero -every divides by zero: every bucket index becomes ±Inf, and
		// converting that to int64 is undefined in Go, so the table comes out
		// labelled with an arbitrary number rather than empty or wrong-but-obvious.
		// A negative one buckets time backwards. Neither is a view of anything, so
		// neither is worth producing.
		if *every <= 0 {
			fail(fmt.Errorf("-every must be greater than 0, not %g: it is the width of each row's time bucket", *every))
		}
		a.metrics(*metricName, *every)
	case "chrome":
		if *out == "" {
			fail(fmt.Errorf("chrome needs -o"))
		}
		a.chrome(*out)
	default:
		fail(fmt.Errorf("unknown command %q", cmd))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "shelld-trace:", err)
	os.Exit(1)
}

// span is a matched Begin/End pair.
type span struct {
	kind       tracing.Kind
	obj        uint64
	begin, end int64
	tid        uint32
	a, b       int64 // from End (Begin's a when End's is zero)
}

type analysis struct {
	t      *tracing.Trace
	start  int64
	spans  []span
	byKind map[tracing.Kind][]span
	// window id → attach connection ids; connection id → window id
	connsOf  map[uint64][]uint64
	windowOf map[uint64]uint64
	winName  map[uint64]string
	// every window the trace mentions, named or not
	windows map[uint64]bool
}

func newAnalysis(t *tracing.Trace) *analysis {
	a := &analysis{t: t, start: t.Header.Start, byKind: map[tracing.Kind][]span{},
		connsOf: map[uint64][]uint64{}, windowOf: map[uint64]uint64{}, winName: map[uint64]string{},
		windows: map[uint64]bool{}}
	open := map[uint64]tracing.Record{}
	for _, r := range t.Records {
		// Which kinds carry a window id in Obj is already written down, in the
		// Kinds table that goes into every trace header, so this reads it rather
		// than keeping a second list that a new window-scoped kind would not
		// reach. Counting names instead missed any window that was created,
		// attached to, read from and killed without a name record — every window
		// that already existed when tracing was switched on, for one.
		//
		// Id 0 is the "not tracing" sentinel tracing.NewID returns in the
		// no-op build, never a real window, so it is not a window to count.
		if int(r.Kind) < len(tracing.Kinds) && tracing.Kinds[r.Kind].Obj == "window" && r.Obj != 0 {
			a.windows[r.Obj] = true
		}
		switch r.Phase {
		case tracing.PhaseBegin:
			open[r.Span] = r
		case tracing.PhaseEnd:
			b, ok := open[r.Span]
			if !ok {
				continue
			}
			delete(open, r.Span)
			s := span{kind: r.Kind, obj: r.Obj, begin: b.Time, end: r.Time, tid: r.TID, a: r.A, b: r.B}
			if s.a == 0 {
				s.a = b.A
			}
			a.spans = append(a.spans, s)
			a.byKind[r.Kind] = append(a.byKind[r.Kind], s)
		default:
			switch r.Kind {
			case tracing.KindAttachOpen:
				a.windowOf[r.Obj] = uint64(r.A)
				a.connsOf[uint64(r.A)] = append(a.connsOf[uint64(r.A)], r.Obj)
				// The one kind whose window id is in A rather than Obj, where Obj
				// is the connection.
				if r.A > 0 {
					a.windows[uint64(r.A)] = true
				}
			case tracing.KindWindowName:
				a.winName[r.Obj] = t.Names[uint64(r.A)]
			}
		}
	}
	return a
}

func (a *analysis) ms(ts int64) float64 { return float64(ts-a.start) / 1e6 }

func (a *analysis) instants(k tracing.Kind) []tracing.Record {
	var out []tracing.Record
	for _, r := range a.t.Records {
		if r.Kind == k && r.Phase == tracing.PhaseInstant {
			out = append(out, r)
		}
	}
	return out
}

type dist []float64

func (d dist) q(p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	s := append(dist{}, d...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(p*float64(len(s))))]
}

func (d dist) line() string {
	if len(d) == 0 {
		return "n=0"
	}
	return fmt.Sprintf("n=%-7d p50 %8.3f  p90 %8.3f  p99 %8.3f  max %8.3f", len(d), d.q(.5), d.q(.9), d.q(.99), d.q(1))
}

func (a *analysis) summary() {
	recs := a.t.Records
	if len(recs) == 0 {
		fmt.Println("empty trace")
		return
	}
	dur := float64(recs[len(recs)-1].Time-a.start) / 1e9
	var dropped int64
	for _, r := range a.instants(tracing.KindDropped) {
		dropped += r.A
	}
	fmt.Printf("pid %d, %.1f s, %d records, %d dropped, %d windows, %d attach connections\n\n",
		a.t.Header.Pid, dur, len(recs), dropped, len(a.windows), len(a.windowOf))

	fmt.Println("spans (duration, ms):")
	kinds := make([]tracing.Kind, 0, len(a.byKind))
	for k := range a.byKind {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		var d dist
		for _, s := range a.byKind[k] {
			d = append(d, float64(s.end-s.begin)/1e6)
		}
		fmt.Printf("  %-18s %s\n", k, d.line())
	}
	if rs := a.byKind[tracing.KindRender]; len(rs) > 0 {
		modes := map[int64]int{}
		for _, s := range rs {
			modes[s.b&0xf]++
		}
		fmt.Printf("  renders: %d skipped, %d diff, %d full, %d paused\n",
			modes[int64(tracing.RenderSkipped)], modes[int64(tracing.RenderDiff)], modes[int64(tracing.RenderFull)], modes[int64(tracing.RenderPaused)])
	}

	fmt.Println("\ncanaries (ms):")
	for _, k := range []tracing.Kind{tracing.KindTimerLate, tracing.KindNetpollPing} {
		var d dist
		for _, r := range a.instants(k) {
			d = append(d, float64(r.A)/1e6)
		}
		fmt.Printf("  %-18s %s\n", k, d.line())
	}
	var wakes [5]dist
	for _, r := range a.instants(tracing.KindTickWake) {
		if r.A >= 0 && r.A < 5 {
			wakes[r.A] = append(wakes[r.A], float64(r.B)/1e6)
		}
	}
	fmt.Println("\ntick wake-ups (time waited, ms):")
	for i, n := range []string{"first", "dirty", "ctrl", "timer", "stop"} {
		if len(wakes[i]) > 0 {
			fmt.Printf("  %-18s %s\n", n, wakes[i].line())
		}
	}
	fmt.Printf("\nGC cycles: %d\n", len(a.instants(tracing.KindGC)))

	type ms struct {
		last, max int64
		n         int
	}
	metrics := map[string]*ms{}
	for _, r := range a.instants(tracing.KindMetric) {
		name := a.t.Names[r.Obj]
		m := metrics[name]
		if m == nil {
			m = &ms{}
			metrics[name] = m
		}
		m.last, m.max, m.n = r.A, max(m.max, r.A), m.n+1
	}
	names := make([]string, 0, len(metrics))
	for n := range metrics {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("\nruntime metrics (last / max over samples):")
	for _, n := range names {
		m := metrics[n]
		fmt.Printf("  %-52s %14d / %d\n", n, m.last, m.max)
	}
}

// echo follows each binary input message through the daemon: read returned,
// written to the PTY, echoed output read from the PTY, parsed, noticed by the
// connection's tick loop, rendered, and written back.
func (a *analysis) echo(slow int) {
	names := []string{"ws.read→pty.write done", "pty.write→pty.read of echo", "pty.read→parsed", "parsed→tick wake", "tick wake→render done", "render→ws.write done", "total (ws.read→ws.write)"}
	type row struct {
		conn uint64
		t    [7]float64
		at   int64
	}
	find := func(list []span, from int64, obj uint64, pred func(span) bool) (span, bool) {
		i := sort.Search(len(list), func(i int) bool { return list[i].end >= from })
		for ; i < len(list); i++ {
			if list[i].obj == obj && (pred == nil || pred(list[i])) {
				return list[i], true
			}
		}
		return span{}, false
	}
	sortByEnd := func(k tracing.Kind) []span {
		s := append([]span{}, a.byKind[k]...)
		sort.Slice(s, func(i, j int) bool { return s[i].end < s[j].end })
		return s
	}
	reads, inputs, ptyReads, advances, renders, writes := sortByEnd(tracing.KindWSRead), sortByEnd(tracing.KindInput),
		sortByEnd(tracing.KindPTYRead), sortByEnd(tracing.KindVTAdvance), sortByEnd(tracing.KindRender), sortByEnd(tracing.KindWSWrite)
	wakes := a.instants(tracing.KindTickWake)

	var rows []row
	for _, rd := range reads {
		if rd.b != 2 || rd.a == 0 { // binary messages only
			continue
		}
		win := a.windowOf[rd.obj]
		in, ok := find(inputs, rd.end, rd.obj, nil)
		if !ok {
			continue
		}
		pr, ok := find(ptyReads, in.end, win, func(s span) bool { return s.a > 0 })
		if !ok {
			continue
		}
		adv, ok := find(advances, pr.end, win, nil)
		if !ok {
			continue
		}
		gen := adv.b
		wi := sort.Search(len(wakes), func(i int) bool { return wakes[i].Time >= adv.end })
		for wi < len(wakes) && wakes[wi].Obj != rd.obj {
			wi++
		}
		if wi == len(wakes) {
			continue
		}
		rn, ok := find(renders, wakes[wi].Time, rd.obj, func(s span) bool { return s.b>>4 >= gen && s.a > 0 })
		if !ok {
			continue
		}
		wr, ok := find(writes, rn.end, rd.obj, func(s span) bool { return s.b == 2 })
		if !ok {
			continue
		}
		d := func(x, y int64) float64 { return float64(y-x) / 1e6 }
		rows = append(rows, row{conn: rd.obj, at: rd.end, t: [7]float64{
			d(rd.end, in.end), d(in.end, pr.end), d(pr.end, adv.end), d(adv.end, wakes[wi].Time),
			d(wakes[wi].Time, rn.end), d(rn.end, wr.end), d(rd.end, wr.end)}})
	}
	fmt.Printf("%d inputs traced to their echo frame\n\n", len(rows))
	for i, n := range names {
		var dd dist
		for _, r := range rows {
			dd = append(dd, r.t[i])
		}
		fmt.Printf("  %-28s %s\n", n, dd.line())
	}
	// Time a message waited before ReadMessage returned is not visible from
	// inside: the Begin of a read span is when the daemon started waiting.
	var idle dist
	for _, rd := range reads {
		if rd.b == 2 {
			idle = append(idle, float64(rd.end-rd.begin)/1e6)
		}
	}
	fmt.Printf("\n  %-28s %s\n", "ws.read blocked (idle wait)", idle.line())
	sort.Slice(rows, func(i, j int) bool { return rows[i].t[6] > rows[j].t[6] })
	if len(rows) > 0 && slow > 0 {
		fmt.Println("\nslowest:")
		for _, r := range rows[:min(slow, len(rows))] {
			fmt.Printf("  at %9.3f ms conn %d:", a.ms(r.at), r.conn)
			for i := range 6 {
				fmt.Printf(" %.3f", r.t[i])
			}
			fmt.Printf("  = %.3f\n", r.t[6])
		}
	}
}

// stalls lists canary delays of at least min and, for each, the spans open
// on every OS thread during the stall and the nearest runtime metrics.
func (a *analysis) stalls(minDelay time.Duration) {
	type stall struct {
		kind       tracing.Kind
		start, end int64
	}
	var ss []stall
	for _, r := range a.instants(tracing.KindNetpollPing) {
		if time.Duration(r.A) >= minDelay {
			ss = append(ss, stall{r.Kind, r.Time - r.A, r.Time})
		}
	}
	for _, r := range a.instants(tracing.KindTimerLate) {
		if time.Duration(r.A) >= minDelay {
			ss = append(ss, stall{r.Kind, r.Time - r.A - int64(time.Millisecond), r.Time})
		}
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].start < ss[j].start })
	fmt.Printf("%d canary delays ≥ %v\n", len(ss), minDelay)
	for i, s := range ss {
		if i == 20 {
			fmt.Printf("... %d more\n", len(ss)-20)
			break
		}
		fmt.Printf("\n%s %.3f ms, from %.3f to %.3f ms\n", s.kind, float64(s.end-s.start)/1e6, a.ms(s.start), a.ms(s.end))
		busy := map[uint32]map[tracing.Kind]float64{}
		for _, sp := range a.spans {
			if sp.end < s.start || sp.begin > s.end {
				continue
			}
			ov := float64(min(sp.end, s.end)-max(sp.begin, s.start)) / 1e6
			if busy[sp.tid] == nil {
				busy[sp.tid] = map[tracing.Kind]float64{}
			}
			busy[sp.tid][sp.kind] += ov
		}
		counts := map[uint32]int{}
		for _, r := range a.t.Records {
			if r.Time >= s.start && r.Time <= s.end {
				counts[r.TID]++
			}
		}
		tids := make([]uint32, 0, len(counts))
		for tid := range counts {
			tids = append(tids, tid)
		}
		sort.Slice(tids, func(i, j int) bool { return tids[i] < tids[j] })
		for _, tid := range tids {
			var parts []string
			for k, v := range busy[tid] {
				parts = append(parts, fmt.Sprintf("%s %.2fms", k, v))
			}
			sort.Strings(parts)
			fmt.Printf("  thread %-7d %5d records  %s\n", tid, counts[tid], strings.Join(parts, ", "))
		}
		for _, r := range a.t.Records {
			if r.Kind == tracing.KindMetric && r.Time >= s.start-int64(100*time.Millisecond) && r.Time <= s.end+int64(100*time.Millisecond) {
				if n := a.t.Names[r.Obj]; strings.Contains(n, "#p99") || strings.Contains(n, "#max") {
					fmt.Printf("  metric at %.1f ms: %s = %d (n=%d)\n", a.ms(r.Time), n, r.A, r.B)
				}
			}
		}
		for _, r := range a.instants(tracing.KindGC) {
			if r.Time >= s.start-int64(50*time.Millisecond) && r.Time <= s.end+int64(50*time.Millisecond) {
				fmt.Printf("  GC cycle %d finished at %.3f ms\n", r.A, a.ms(r.Time))
			}
		}
	}
}

// metrics prints metric values as columns, one row per interval (the last
// sample in each).
func (a *analysis) metrics(filter string, every float64) {
	var names []string
	seen := map[string]bool{}
	rows := map[int64]map[string]int64{}
	for _, r := range a.instants(tracing.KindMetric) {
		n := a.t.Names[r.Obj]
		// The percentile and max summaries ("#p99", "#max") are hidden unless a
		// -name filter asks for something, because unfiltered they are most of the
		// table. The dropped third term was !strings.Contains(filter, "#"), which
		// an empty filter satisfies by definition — it could only ever be true
		// alongside filter == "", so it decided nothing.
		if !strings.Contains(n, filter) || filter == "" && strings.Contains(n, "#") {
			continue
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
		bucket := int64(a.ms(r.Time) / every)
		if rows[bucket] == nil {
			rows[bucket] = map[string]int64{}
		}
		rows[bucket][n] = r.A
	}
	sort.Strings(names)
	for i, n := range names {
		fmt.Printf("# col %d: %s\n", i+2, n)
	}
	buckets := make([]int64, 0, len(rows))
	for b := range rows {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	for _, b := range buckets {
		fmt.Printf("%9.0f", float64(b)*every)
		for _, n := range names {
			fmt.Printf(" %14d", rows[b][n])
		}
		fmt.Println()
	}
}

func (a *analysis) timeline(at, width float64) {
	from := a.start + int64((at-width/2)*1e6)
	to := a.start + int64((at+width/2)*1e6)
	phase := map[tracing.Phase]string{tracing.PhaseInstant: " ", tracing.PhaseBegin: "B", tracing.PhaseEnd: "E"}
	for _, r := range a.t.Records {
		if r.Time < from || r.Time > to {
			continue
		}
		extra := ""
		if r.Kind == tracing.KindMetric {
			extra = " " + a.t.Names[r.Obj]
		}
		fmt.Printf("%10.3f cpu%-2d tid %-7d %s %-18s obj %-5d span %-8d a %-10d b %d%s\n",
			a.ms(r.Time), r.CPU, r.TID, phase[r.Phase], r.Kind, r.Obj, r.Span, r.A, r.B, extra)
	}
}

// chrome writes the Chrome trace event format: spans per OS thread, instants,
// and canaries as counters.
func (a *analysis) chrome(path string) {
	type ev struct {
		Name string         `json:"name"`
		Cat  string         `json:"cat,omitempty"`
		Ph   string         `json:"ph"`
		Ts   float64        `json:"ts"` // µs
		Dur  float64        `json:"dur,omitempty"`
		Pid  int            `json:"pid"`
		Tid  uint32         `json:"tid"`
		Args map[string]any `json:"args,omitempty"`
	}
	pid := a.t.Header.Pid
	us := func(t int64) float64 { return float64(t-a.start) / 1e3 }
	var evs []ev
	for _, s := range a.spans {
		name := s.kind.String()
		args := map[string]any{"obj": s.obj, "a": s.a, "b": s.b}
		if n, ok := a.winName[s.obj]; ok && (strings.HasPrefix(name, "pty.") || strings.HasPrefix(name, "vt.")) {
			args["window"] = n
		}
		evs = append(evs, ev{Name: name, Cat: strings.SplitN(name, ".", 2)[0], Ph: "X", Ts: us(s.begin), Dur: max(us(s.end)-us(s.begin), 0.001), Pid: pid, Tid: s.tid, Args: args})
	}
	for _, r := range a.t.Records {
		if r.Phase != tracing.PhaseInstant {
			continue
		}
		switch r.Kind {
		case tracing.KindTimerLate, tracing.KindNetpollPing:
			evs = append(evs, ev{Name: r.Kind.String(), Ph: "C", Ts: us(r.Time), Pid: pid, Args: map[string]any{"ms": float64(r.A) / 1e6}})
		case tracing.KindMetric:
			evs = append(evs, ev{Name: a.t.Names[r.Obj], Ph: "C", Ts: us(r.Time), Pid: pid, Args: map[string]any{"value": r.A}})
		default:
			evs = append(evs, ev{Name: r.Kind.String(), Ph: "i", Ts: us(r.Time), Pid: pid, Tid: r.TID, Args: map[string]any{"obj": r.Obj, "a": r.A, "b": r.B}})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(map[string]any{"traceEvents": evs}); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %d events to %s\n", len(evs), path)
}
