//go:build !windows

package inbox

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports that signal 0 reaches the process (EPERM still means
// it exists).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// recordOwnedByUs reports whether a session-record file belongs to this OS
// user. A record another user planted is ignored: its socket path is never
// probed or offered as an address.
func recordOwnedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// probeEndpoint is true when something accepts on the inbox socket within
// 250ms.
func probeEndpoint(path string) bool { return probeLive(path) }
