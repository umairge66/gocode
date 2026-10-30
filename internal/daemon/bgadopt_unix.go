//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// verifyOwnedFile rejects a file not owned by this user or writable by
// group/others.
func verifyOwnedFile(path string, fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		return fmt.Errorf("%s is owned by uid %d, not this user", path, st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users", path)
	}
	return nil
}
