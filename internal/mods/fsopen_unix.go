//go:build !windows

package mods

import (
	"os"
	"syscall"
)

// openNonBlocking opens a file for reading without blocking on a FIFO
// .
func openNonBlocking(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
