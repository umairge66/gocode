//go:build !unix && !windows

package proctree

import (
	"errors"
	"os"
	"os/exec"
)

// Prepare is a no-op on platforms without process groups.
func Prepare(cmd *exec.Cmd) {}

type otherOps struct{}

var platformOps ops = otherOps{}

func (otherOps) graceful(int) error { return errors.ErrUnsupported }

func (otherOps) force(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func (otherOps) alive(int) bool { return false }
