//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func shutdownSignalSet() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}

// shutdownCode maps a signal to its exit code. A background worker ignores
// SIGHUP unless it owns its controlling terminal.
func shutdownCode(sig os.Signal, bgWorker bool, ownsCtty func() bool) (int, bool) {
	switch sig {
	case syscall.SIGINT:
		return exitCodeSIGINT, true
	case syscall.SIGTERM:
		return exitCodeSIGTERM, true
	case syscall.SIGHUP:
		if bgWorker && !ownsCtty() {
			return 0, false
		}
		return exitCodeSIGHUP, true
	}
	return 0, false
}

// ownsControllingTerminal reports whether the process has a controlling
// terminal (the pty a background worker's host made it the session leader
// of).
func ownsControllingTerminal() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
