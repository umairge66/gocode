//go:build unix

package proctree

import (
	"errors"
	"os/exec"
	"syscall"
)

// Prepare makes cmd start in a new session, and therefore in a new process
// group whose id equals the child's pid, so the whole tree it spawns can be
// signalled as one unit.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Setctty {
		cmd.SysProcAttr.Setpgid = true
		return
	}
	cmd.SysProcAttr.Setsid = true
}

type unixOps struct{}

var platformOps ops = unixOps{}

// signalGroup sends sig to the group led by pid, falling back to the pid alone
// when the group signal fails (the child was not started with Prepare).
func signalGroup(pid int, sig syscall.Signal) error {
	gerr := syscall.Kill(-pid, sig)
	if gerr == nil {
		return nil
	}
	if perr := syscall.Kill(pid, sig); perr == nil {
		return nil
	}
	return gerr
}

// graceful sends SIGTERM to the group.
func (unixOps) graceful(pid int) error { return signalGroup(pid, syscall.SIGTERM) }

// force sends SIGKILL to the group and to the leader.
func (unixOps) force(pid int) error {
	gerr := syscall.Kill(-pid, syscall.SIGKILL)
	perr := syscall.Kill(pid, syscall.SIGKILL)
	if gerr == nil || perr == nil {
		return nil
	}
	if errors.Is(gerr, syscall.ESRCH) {
		return nil // already gone
	}
	return gerr
}

// alive probes the group and the leader with signal 0; EPERM still means a
// process exists.
func (unixOps) alive(pid int) bool {
	for _, target := range []int{-pid, pid} {
		err := syscall.Kill(target, 0)
		if err == nil || !errors.Is(err, syscall.ESRCH) {
			return true
		}
	}
	return false
}
