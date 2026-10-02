//go:build !linux

package shelld

// InitDescriptorTable does nothing off Linux. The cost it removes is Linux's
// alone: expand_fdtable's RCU grace period on a shared files_struct. Reporting
// true would claim an expansion that did not happen, so it declines.
func InitDescriptorTable() bool { return false }
