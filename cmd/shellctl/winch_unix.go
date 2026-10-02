//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls fn on SIGWINCH until the returned stop is called.
func watchResize(fn func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				fn()
			case <-quit:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(quit)
	}
}
