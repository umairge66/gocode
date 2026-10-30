//go:build unix && !linux

package proctree

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// psFields runs `ps -o <fields> -p <pid>` with a fixed locale and timezone.
func psFields(pid int, fields string) string {
	cmd := exec.Command("ps", "-o", fields, "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func startTime(pid int) string { return psFields(pid, "lstart=") }

func ownedBySelf(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	uid, err := strconv.Atoi(psFields(pid, "uid="))
	return err == nil && uid == os.Getuid()
}
