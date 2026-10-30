//go:build windows

package inbox

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE, the exit code of a running process.
const stillActive = 259

// pidAlive reports whether pid names a running process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied still means the process exists.
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// recordOwnedByUs is true on Windows: the registry lives in this user's
// profile, and namedpipe.Dial refuses a pipe served by another user's SID.
func recordOwnedByUs(os.FileInfo) bool { return true }

// probeEndpoint reports that the pipe accepts (or is momentarily busy) within 250ms.
func probeEndpoint(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	c, err := dialLocal(ctx, path, 250*time.Millisecond)
	if err != nil {
		return isBusyError(err)
	}
	_ = c.Close()
	return true
}
