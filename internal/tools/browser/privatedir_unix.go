//go:build !windows

package browser

import (
	"os"
	"syscall"
)

// privateDir reports whether dir is still a directory owned by this user with
// mode 0700, so a reused screenshot directory was not swapped or loosened.
func privateDir(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid) == os.Getuid()
	}
	return true
}
