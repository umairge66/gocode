//go:build windows

package browser

import "os"

// privateDir reports whether dir is still a directory. Windows has no uid/mode
// bits to compare.
func privateDir(dir string) bool {
	fi, err := os.Lstat(dir)
	return err == nil && fi.IsDir()
}
