//go:build windows

package mods

import "os"

// openNonBlocking opens a file for reading.
func openNonBlocking(p string) (*os.File, error) {
	return os.Open(p)
}
