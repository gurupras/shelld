//go:build !shelld_trace

package tracing

// Enabled is false in builds without the shelld_trace tag: call sites guarded
// by it compile to nothing.
const Enabled = false

func Begin(Kind, uint64, int64) uint64       { return 0 }
func End(Kind, uint64, uint64, int64, int64) {}
func Instant(Kind, uint64, int64, int64)     {}
func Intern(string) uint64                   { return 0 }
func NewID() uint64                          { return 0 }
func Close()                                 {}
