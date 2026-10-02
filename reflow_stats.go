package shelld

import (
	"sync/atomic"
	"unsafe"

	"github.com/gurupras/shelld/vt"
)

// Why these counters exist, stated here because the investigation that wanted
// them is not in this repo and a reader has nothing else to go on.
//
// A memory-retention investigation attributed an 839 MiB high-water figure to
// repeated reflows, and that argument rests entirely on "the workload is
// identical across reps" — a quantity no run observes. The effective size is
// max(pin, min over attachers), so how often it actually changes depends on the
// phase relationship between several drifting tickers, which depends on
// scheduling. Requests are an upper bound on reflows, not a measure of them.
// Until the size changes themselves are counted, every mechanism story for the
// ratchet — the reflow one included — is unfalsifiable, so the counter comes
// before any measurement.
//
// Placement: every path to setEffectiveSize goes through
// RecomputeEffectiveSize's equality short-circuit, the pin path included
// (Window.Resize sets the pin and then calls the recompute, it does not call
// setEffectiveSize directly). So a count taken inside setEffectiveSize counts
// real size changes and never a no-op. TestReflowCounterSkipsNoOps holds that
// property, because a counter one line earlier would count requests instead and
// would answer the question above with the very number it exists to distrust.
var (
	reflowCount atomic.Uint64
	reflowCells atomic.Uint64
)

// ReflowStats returns the number of effective-size changes since this process
// started and the total number of cells in the grids those changes produced.
//
// Cells, not bytes: sizeof(vt.Cell) is a property of the vt package's struct
// layout, and folding it in here would bake that layout into the metric and
// silently restate it if a field is ever added. ReflowCellBytes reports it
// alongside so a consumer can multiply.
//
// The two loads are not atomic with respect to each other, so a reader
// concurrent with a reflow can see the count from after it and the cells from
// before. E1 differences the counters across a whole storm, where a one-reflow
// skew is not a quantity it can act on.
func ReflowStats() (reflows, cells uint64) {
	return reflowCount.Load(), reflowCells.Load()
}

// ReflowCellBytes is sizeof(vt.Cell), so a consumer can turn ReflowStats' cell
// count into bytes without hardcoding the layout. It is the size of one cell in
// a grid, which is not the same as the memory a reflow retains — a reflow that
// reuses capacity allocates nothing, and that difference is the point of the
// item-4 arms. Read this as the size of the grid produced, not as an allocation.
const ReflowCellBytes = uint64(unsafe.Sizeof(vt.Cell{}))

// countReflow records one effective-size change to a cols x rows grid.
func countReflow(cols, rows uint16) {
	reflowCount.Add(1)
	reflowCells.Add(uint64(cols) * uint64(rows))
}
