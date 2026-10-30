//go:build windows

package daemon

import (
	"os/exec"

	"golang.org/x/sys/windows"

	"github.com/umairge66/gocode/internal/proctree"
)

// Detach starts cmd as a detached, windowless process in its own process group
// (Node's detached:true + windowsHide:true).
func Detach(cmd *exec.Cmd) {
	proctree.Prepare(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS
	cmd.SysProcAttr.HideWindow = true
}
