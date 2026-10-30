//go:build windows

package plugineval

import (
	"os/exec"
	"strconv"
)

// setProcessGroup makes a timeout kill the child's whole tree (like
// taskkill /T /F /PID).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
