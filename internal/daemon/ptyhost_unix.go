//go:build !windows

package daemon

import (
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// hostForwardSignals are the signals the pty host forwards to its worker.
func hostForwardSignals() []os.Signal {
	return []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT}
}

// lowerPriority renices the pty host by +5 (capped at 19).
func lowerPriority() {
	cur, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		return
	}
	if runtime.GOOS == "linux" {
		// The raw Linux syscall returns 20-nice.
		cur = 20 - cur
	}
	next := cur + 5
	if next > 19 {
		next = 19
	}
	_ = unix.Setpriority(unix.PRIO_PROCESS, 0, next)
}
