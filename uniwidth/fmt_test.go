package uniwidth

import "fmt"

func fmtSscan(s string, a ...any) (int, error) { return fmt.Sscan(s, a...) }
