package pty

import "os"

// openExit returns nil; Wait blocks a thread in wait4.
func openExit(int) *os.File { return nil }

func awaitExit(*os.File) {}
