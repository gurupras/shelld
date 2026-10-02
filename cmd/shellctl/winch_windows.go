//go:build windows

package main

import "time"

// watchResize polls the console size every 250ms (Windows has no SIGWINCH).
func watchResize(fn func()) (stop func()) {
	quit := make(chan struct{})
	go func() {
		lastC, lastR := termSize()
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
