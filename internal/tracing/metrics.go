//go:build shelld_trace

package tracing

import (
	"math"
	"runtime/metrics"
	"time"
)

// Sampled runtime metrics. Scalars are recorded as they are (bytes and counts
// as integers, cpu-seconds as microseconds); histograms as the p50, p99 and
// max, in nanoseconds, of the samples added since the previous sample.
var sampled = []string{
	"/sched/latencies:seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/goroutines:goroutines",
	"/sched/gomaxprocs:threads",
	"/gc/cycles/total:gc-cycles",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/total:bytes",
	"/cpu/classes/user:cpu-seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/scavenge/total:cpu-seconds",
	"/cpu/classes/idle:cpu-seconds",
}

func sampleMetrics(every time.Duration) {
	known := map[string]bool{}
	for _, d := range metrics.All() {
		known[d.Name] = true
	}
	var samples []metrics.Sample
	for _, n := range sampled {
		if known[n] {
			samples = append(samples, metrics.Sample{Name: n})
		}
	}
	prev := map[string][]uint64{}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for range tick.C {
		if !on.Load() {
			return
		}
		metrics.Read(samples)
		for _, s := range samples {
			switch s.Value.Kind() {
			case metrics.KindUint64:
				Instant(KindMetric, Intern(s.Name), int64(s.Value.Uint64()), 0)
			case metrics.KindFloat64:
				Instant(KindMetric, Intern(s.Name), int64(s.Value.Float64()*1e6), 0)
			case metrics.KindFloat64Histogram:
				h := s.Value.Float64Histogram()
				p50, p99, mx, n := histDelta(h, prev[s.Name])
				prev[s.Name] = append(prev[s.Name][:0], h.Counts...)
				if n == 0 {
					continue
				}
				Instant(KindMetric, Intern(s.Name+"#p50_ns"), p50, n)
				Instant(KindMetric, Intern(s.Name+"#p99_ns"), p99, n)
				Instant(KindMetric, Intern(s.Name+"#max_ns"), mx, n)
			}
		}
	}
}

// histDelta returns quantiles of the samples added since prev, taking each
// bucket's upper bound (capped at its lower bound for the open last bucket).
func histDelta(h *metrics.Float64Histogram, prev []uint64) (p50, p99, mx, n int64) {
	delta := make([]uint64, len(h.Counts))
	var total uint64
	for i, c := range h.Counts {
		if i < len(prev) {
			c -= prev[i]
		}
		delta[i] = c
		total += c
	}
	if total == 0 {
		return 0, 0, 0, 0
	}
	bound := func(i int) int64 {
		v := h.Buckets[i+1]
		if math.IsInf(v, 1) {
			v = h.Buckets[i]
		}
		return int64(v * 1e9)
	}
	at := func(q float64) int64 {
		want := uint64(math.Ceil(q * float64(total)))
		var seen uint64
		for i, c := range delta {
			seen += c
			if seen >= want {
				return bound(i)
			}
		}
		return bound(len(delta) - 1)
	}
	for i := len(delta) - 1; i >= 0; i-- {
		if delta[i] > 0 {
			mx = bound(i)
			break
		}
	}
	return at(0.5), at(0.99), mx, int64(total)
}
