//go:build windows

package cli

import (
	"os"
	"syscall"
)

func shutdownSignalSet() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// shutdownCode maps a signal to its exit code (Windows has no SIGHUP).
func shutdownCode(sig os.Signal, _ bool, _ func() bool) (int, bool) {
	switch sig {
	case os.Interrupt:
		return exitCodeSIGINT, true
	case syscall.SIGTERM:
		return exitCodeSIGTERM, true
	}
	return 0, false
}

func ownsControllingTerminal() bool { return false }
