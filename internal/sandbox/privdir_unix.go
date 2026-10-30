//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"syscall"
)

// ensurePrivateDir creates dir (0700) if needed and verifies it is a real
// directory (not a symlink) owned by this user, tightening its mode to 0700.
// The per-user temp root lives under the shared /tmp, where another user
// could have pre-created the name; using such a directory would put the
// session's sockets and temp files under someone else's control.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (a symlink or other file is in the way)", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not by this user", dir, st.Uid)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}
