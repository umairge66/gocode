//go:build windows

package daemon

import (
	"os"
	"syscall"
)

// hostForwardSignals: Windows delivers only console interrupts.
func hostForwardSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// lowerPriority is a no-op on Windows (renicing happens only off Windows).
func lowerPriority() {}
