//go:build linux

// linux and not unix, even though Descendants answers on darwin too: the figure is
// only readable beside the number of processes on the box, /proc is the only source
// for that this file can use, and a benchmark whose premise check fails on darwin is
// a FAILING BENCHMARK there rather than a missing measurement. Not a broken build --
// tagged unix this file still compiles and vets clean for darwin, because nothing in
// it is linux-only at the type level and /proc's absence is a runtime fact. So what
// the tag buys is a skip instead of a red, which is worth saying accurately: someone
// who reads "build" here will go looking for a compile error that was never there.
//
// The darwin enumeration is a separate implementation -- one sysctl plus a getsid per
// process, see platform_darwin.go, whose own figures put the getsid loop at half the
// total -- so it is O(processes) exactly as this one is. It wants its own benchmark
// for its own numbers, not a share of this one, and the reason is the /proc denominator
// above rather than any difference in how the cost scales.

package proctree

import (
	"os"
	"strconv"
	"testing"
)

// BenchmarkDescendantsSelf prices the production enumeration against this box's real
// process table. It exists because the cost is O(processes on the machine) rather than
// O(descendants), so the figure is a property of the box as much as of the code and
// there is no way to read it off a unit test — and because a per-entry change to the
// /proc read is invisible to every correctness test in this package, all of which pass
// either way.
//
// Report it with the process count, which the benchmark prints as procs: a number
// without /proc's population attached cannot be compared against another box or
// another day. At ~900 processes this is the single largest latency in a window close.
func BenchmarkDescendantsSelf(b *testing.B) {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		b.Fatalf("premise: reading /proc: %v", err)
	}
	// Count the NUMERIC entries, not len(entries). /proc holds a fixed set of kernel
	// pseudo-files alongside the pids — meminfo, self, net, sys and ~58 more — so
	// len(entries) overcounts processes by 62 on this box, and it does so silently:
	// the number still moves with the process table, it just sits above it. An
	// earlier version of this benchmark reported len(entries) and called it the
	// process count, which put every figure derived from it ~7% high.
	procs := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			procs++
		}
	}
	// The population is the premise. "Descendants(self) came back non-empty" is not,
	// and it was the check here first: unionDescendants seeds its answer from the
	// ARGUMENT rather than from the walk — out := []int{root} in proctree.go — so for
	// any positive pid, and os.Getpid() is always positive, it returns at least [self]
	// whatever the walk did. Driven against an empty map it answers [1], [999999],
	// [12345]. A /proc that could not be read, or a stat reader that failed on every
	// single entry, would have passed that guard and left the timing loop measuring an
	// empty map lookup. It could not fail, so it was not evidence.
	//
	// This one can fail, and it fails on exactly the condition that makes the number
	// below meaningless: no process table to walk.
	if procs < 2 {
		b.Fatalf("premise: /proc named %d processes, so there is no walk here to price", procs)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Descendants(self)
	}
	// AFTER the loop, and that is not a style choice. ResetTimer calls clear() on the
	// map user metrics live in — "deletes user-reported metrics", in its own doc — so a
	// ReportMetric sitting up beside the computation that produces the number, where it
	// reads as though it belongs, is silently thrown away. It was up there, and this
	// benchmark printed no procs column at all while the doc comment above promised one.
	// Nothing failed: the line just looked like an ordinary benchmark result, so the
	// only way to catch it was to notice a column that was never there.
	b.ReportMetric(float64(procs), "procs")
}
