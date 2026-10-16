//go:build windows

package main

import "time"

// watchResize polls the console size every 250ms (Windows has no SIGWINCH).
func watchResize(fn func()) (stop func()) {
	quit := make(chan struct{})
	// The size to compare against is read before watchResize returns: read in
	// the goroutine, it could be taken after a resize that came first, which the
	// poll would then never report.
	lastC, lastR := termSize()
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if c, r := termSize(); c != lastC || r != lastR {
					lastC, lastR = c, r
					fn()
				}
			case <-quit:
				return
			}
		}
	}()
	return func() { close(quit) }
}
