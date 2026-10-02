//go:build shelld_trace && !linux

package tracing

func threadID() uint32 { return 0 }
func cpuID() uint16    { return 0 }
