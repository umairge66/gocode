//go:build unix

package updater

import "syscall"

// defaultExec re-execs in place via syscall.Exec, replacing the current process
// image. On success it never returns.
func defaultExec(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}
