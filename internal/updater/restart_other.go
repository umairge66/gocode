//go:build !unix && !windows

package updater

// defaultExec has no restart primitive on platforms that are neither unix (no
// syscall.Exec in-place re-exec) nor Windows (no detached spawn-and-exit
// handoff), so it reports ErrRestartUnsupported.
func defaultExec(argv0 string, argv []string, env []string) error {
	return ErrRestartUnsupported
}
