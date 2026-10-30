//go:build !windows

package plugineval

import (
	"os"
	"syscall"
)

// hardLinks is the file's link count.
func hardLinks(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Nlink)
	}
	return 1
}
