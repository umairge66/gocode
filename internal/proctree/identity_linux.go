//go:build linux

package proctree

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// startTime is field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat.
func startTime(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); starttime is field 22.
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// ownedBySelf: signal 0 must succeed (EPERM means another user's process) and
// /proc/<pid> must belong to our uid (a root daemon could otherwise signal
// anything).
func ownedBySelf(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
