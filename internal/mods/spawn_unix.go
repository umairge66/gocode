//go:build !windows

package mods

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// exitSignal names the signal that ended a child ("SIGTERM"), "" when it
// exited.
func exitSignal(st *os.ProcessState) string {
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	if name := unix.SignalName(ws.Signal()); name != "" {
		return name
	}
	return ws.Signal().String()
}
