//go:build windows

package proctree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Prepare starts cmd as the root of a new console process group so
// CTRL_BREAK_EVENT can be delivered to it (and its console children) without
// reaching this process.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = windowsCreationFlags(cmd.SysProcAttr.CreationFlags)
}

type windowsOps struct{}

var platformOps ops = windowsOps{}

// graceful sends CTRL_BREAK_EVENT to the child's console process group. It
// fails when the child shares no console with us (e.g. a GUI host); the
// escalation then forces immediately.
func (windowsOps) graceful(pid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

// force runs the selected forceful strategies until one succeeds.
func (windowsOps) force(pid int) error {
	path, lookErr := exec.LookPath("taskkill")
	var lastErr error
	for _, step := range windowsForcePlan(lookErr == nil) {
		switch step {
		case forceTaskkillTree:
			c := exec.Command(path, taskkillArgs(pid)...)
			c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if lastErr = c.Run(); lastErr == nil {
				return nil
			}
		case forceTerminateLeader:
			p, err := os.FindProcess(pid)
			if err != nil {
				lastErr = err
				continue
			}
			if lastErr = p.Kill(); lastErr == nil || errors.Is(lastErr, os.ErrProcessDone) {
				return nil
			}
		}
	}
	return lastErr
}

// alive reports whether the leader is still running. Windows has no cheap
// group probe; the taskkill /T step covers descendants.
func (windowsOps) alive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER: no such pid. Anything else (e.g. access
		// denied) means a process with that pid exists.
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
