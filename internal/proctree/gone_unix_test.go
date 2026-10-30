//go:build unix

package proctree

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// waitGone polls until pid no longer exists (or is a zombie awaiting its new
// parent's reap), up to d.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}
	// Linux: an orphaned zombie not yet reaped by init still answers kill(0).
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// No procfs (macOS, BSD): ask ps for the process state instead.
		st, perr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if perr != nil {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(string(st)), "Z")
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z'
	}
	return false
}
