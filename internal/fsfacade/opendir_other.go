//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package fsfacade

import (
	"errors"
	"io/fs"
)

// openDirNoFollow has no O_DIRECTORY|O_NOFOLLOW equivalent here (Windows and
// other platforms): it reports failure, so a component whose lstat failed is
// never crossed on the strength of a directory open — the cautious outcome.
func openDirNoFollow(name string) error {
	return &fs.PathError{Op: "opendir", Path: name, Err: errors.ErrUnsupported}
}
