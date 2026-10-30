//go:build windows

package updater

import (
	"os"
	"os/exec"
)

// defaultExec implements the Windows restart handoff. Windows cannot replace the
// running process image the way syscall.Exec does on unix, so instead of
// re-execing in place it spawns the freshly installed binary as a NEW process
// (exec.Cmd.Start, not Run — the parent never waits) carrying the original argv
// and environment, detaches from it, and returns errHandoffExit to tell Restart
// to exit the current process so the new one takes over the console.
func defaultExec(argv0 string, argv []string, env []string) error {
	cmd := &exec.Cmd{
		Path: argv0,
		// argv already has the installed binary path as argv[0]; carry it through
		// verbatim so the new process sees the same command line.
		Args:   argv,
		Env:    env,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detach: release the OS handle so the parent can exit without waiting on or
	// reaping the child.
	_ = cmd.Process.Release()
	// Signal Restart to exit the current process; the spawned binary continues.
	return errHandoffExit
}
