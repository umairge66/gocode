//go:build unix

package filehistory

import (
	"errors"
	"io/fs"
	"syscall"
)

// noFollow makes an open fail on a final-component symlink.
const noFollow = syscall.O_NOFOLLOW

// linkCount is the file's hard-link count.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// isLinkErr reports an open refused because of a symlink (ELOOP / EMLINK) or a
// FIFO without a reader (ENXIO).
func isLinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) || errors.Is(err, syscall.ENXIO)
}
