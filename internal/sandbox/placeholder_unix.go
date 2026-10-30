//go:build unix

package sandbox

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Placeholder is an empty file or directory the Linux backend created on the
// host as a mount point (to block creating a write-protected path that did
// not exist yet), with the identity it had when created. Cleanup removes it
// only while it is still that very inode, still empty, and still reachable
// without crossing a symbolic link.
type Placeholder struct {
	Path string
	Dir  bool
	dev  uint64
	ino  uint64
}

// openDirNoFollow opens an absolute directory path component by component
// with O_NOFOLLOW, so no symbolic link anywhere on the path is crossed.
func openDirNoFollow(path string) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, errors.New("sandbox: placeholder path is not absolute")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, comp := range strings.Split(filepath.Clean(path), "/") {
		if comp == "" {
			continue
		}
		next, err := unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// createPlaceholder creates path (a 0700 directory or an empty 0600 file)
// exclusively, without following a symlink at any component, and records its
// identity.
func createPlaceholder(path string, dir bool) (Placeholder, error) {
	parent, base := filepath.Dir(path), filepath.Base(path)
	pfd, err := openDirNoFollow(parent)
	if err != nil {
		return Placeholder{}, err
	}
	defer unix.Close(pfd)
	if dir {
		if err := unix.Mkdirat(pfd, base, 0o700); err != nil {
			return Placeholder{}, err
		}
	} else {
		fd, err := unix.Openat(pfd, base, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return Placeholder{}, err
		}
		_ = unix.Close(fd)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Placeholder{}, err
	}
	return Placeholder{Path: path, Dir: dir, dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// RemovePlaceholders removes the placeholders that are still exactly what
// was created — same inode, same type, a 0-byte file or an empty directory —
// deepest first. Anything else (content written since, a different file or
// symlink swapped in, a parent replaced by a symlink) is left alone: cleanup
// never follows a symlink and never deletes or truncates anything it did not
// create.
func RemovePlaceholders(ps []Placeholder) {
	sorted := append([]Placeholder(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Path) > len(sorted[j].Path) })
	for _, p := range sorted {
		removePlaceholder(p)
	}
}

func removePlaceholder(p Placeholder) {
	if p.Path == "" || p.ino == 0 {
		return
	}
	pfd, err := openDirNoFollow(filepath.Dir(p.Path))
	if err != nil {
		return
	}
	defer unix.Close(pfd)
	base := filepath.Base(p.Path)
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	if uint64(st.Dev) != p.dev || uint64(st.Ino) != p.ino {
		return
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if p.Dir {
			// rmdir removes only an empty directory.
			_ = unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR)
		}
	case unix.S_IFREG:
		if !p.Dir && st.Size == 0 {
			_ = unix.Unlinkat(pfd, base, 0)
		}
	}
}

// placeholderSkippable reports a creation failure that means the user cannot
// create the path either (so neither can the sandboxed command, which runs as
// the same user): a read-only or unwritable parent.
func placeholderSkippable(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS)
}
