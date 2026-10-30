//go:build !windows

package daemon

import (
	"os/exec"

	"github.com/umairge66/gocode/internal/proctree"
)

// Detach starts cmd in its own session so it outlives the caller's terminal
// and can be signalled as one process group (Node's detached:true).
func Detach(cmd *exec.Cmd) { proctree.Prepare(cmd) }
