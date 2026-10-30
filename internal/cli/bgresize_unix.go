//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls fn on SIGWINCH until the returned stop is called.
func watchResize(fn func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ch:
				fn()
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
