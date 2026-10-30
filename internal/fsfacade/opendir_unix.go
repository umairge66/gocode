//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package fsfacade

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// openDirNoFollow opens name with O_DIRECTORY|O_NOFOLLOW and closes it.
func openDirNoFollow(name string) error {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "opendir", Path: name, Err: err}
	}
	return unix.Close(fd)
}
