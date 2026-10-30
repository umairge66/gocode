//go:build windows

package cli

import "time"

// watchResize polls the console size (Windows has no SIGWINCH) and calls fn
// when it changes, until the returned stop is called.
func watchResize(fn func()) func() {
	done := make(chan struct{})
	go func() {
		c0, r0, _ := terminalSize()
		tk := time.NewTicker(250 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				if c, r, ok := terminalSize(); ok && (c != c0 || r != r0) {
					c0, r0 = c, r
					fn()
				}
			}
		}
	}()
	return func() { close(done) }
}
