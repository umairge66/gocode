//go:build !unix

package filehistory

import "io/fs"

// noFollow is unavailable here; the Lstat checks before every open still
// refuse a symlinked destination.
const noFollow = 0

// linkCount is not reported on this platform.
func linkCount(fs.FileInfo) uint64 { return 1 }

// isLinkErr has no symlink-specific open error on this platform.
func isLinkErr(error) bool { return false }
