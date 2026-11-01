package fsfacade

import (
	"errors"
	"io/fs"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// MaxSymlinkHops is the symlink-chain depth limit. Once this many symlinks have
// been traversed while components still remain to resolve, the chain is treated
// as unresolvable (an ELOOP-like failure).
const MaxSymlinkHops = 64

// ErrSymlinkLoop is the sentinel wrapped by the error Realpath returns when a
// path cannot be canonicalized because its symlink chain either cycles or
// exceeds MaxSymlinkHops. Callers test it with errors.Is.
var ErrSymlinkLoop = errors.New("fsfacade: symbolic link chain too long to resolve (ELOOP)")

// Realpath resolves p to its canonical real path over fsys, following every
// symlink component and returning an absolute, cleaned, NFC-normalized path.
//
// Semantics:
//
//   - The input is first made absolute. A relative path is joined onto the
//     working directory when fsys implements Cwder, else onto "/".
//   - The path is walked component by component from the root. Each component is
//     Lstat-ed; a non-symlink extends the resolved prefix, a symlink is read and
//     its target spliced in front of the remaining components. An absolute target restarts the walk
//     from the root; a relative target is resolved against the directory
//     containing the link.
//   - A symlink cycle is detected by a visited set keyed on the link path plus
//     the remaining components, and treated as unresolvable. Exceeding
//     MaxSymlinkHops distinct hops is likewise unresolvable. Both surface as an
//     error wrapping ErrSymlinkLoop.
//   - A missing component (ENOENT) or a non-directory in the middle of the path
//     (ENOTDIR) stops resolution; Realpath returns an error wrapping
//     fs.ErrNotExist.
//
// Unicode canonicalization: the canonical form is the NFC-normalized
// realpath, so Realpath applies NFC (via golang.org/x/text/unicode/norm, already a module dependency)
// to the final path.
func Realpath(fsys FS, p string) (string, error) {
	abs := resolveAbs(fsys, p)

	resolved := rootOf(abs)
	segs := splitSegments(abs)

	hops := 0
	visited := make(map[string]struct{})

	for len(segs) > 0 {
		// Depth limit: components still pending after MaxSymlinkHops symlink
		// traversals => unresolvable chain.
		if hops >= MaxSymlinkHops {
			return "", loopError(abs)
		}

		seg := segs[0]
		f := path.Join(resolved, seg)

		fi, err := fsys.Lstat(f)
		if err != nil {
			// ENOENT / ENOTDIR (and any "does not exist" error) stop
			// resolution; anything else is surfaced verbatim.
			return "", &fs.PathError{Op: "realpath", Path: abs, Err: err}
		}

		if fi.Mode()&fs.ModeSymlink == 0 {
			// Ordinary component: extend the resolved prefix.
			resolved = f
			segs = segs[1:]
			continue
		}

		// Symbolic link: count the hop and guard against cycles.
		hops++
		key := f + "\x00" + strings.Join(segs, "\x00")
		if _, seen := visited[key]; seen {
			return "", loopError(abs)
		}
		visited[key] = struct{}{}

		target, err := fsys.Readlink(f)
		if err != nil {
			return "", &fs.PathError{Op: "readlink", Path: f, Err: err}
		}

		tsegs := splitSegments(target)
		if path.IsAbs(target) {
			// Absolute target restarts the walk from the root; the resolved
			// prefix is reset.
			resolved = rootOf(target)
		}
		// A relative target is resolved against the directory containing the
		// link, which is exactly the current `resolved` prefix (left unchanged
		// for a relative link target).
		segs = append(tsegs, segs[1:]...)
	}

	return canonicalNFC(resolved), nil
}

// resolveAbs makes p absolute, resolving a relative path against fsys's working
// directory (Cwder) or "/". The result is lexically cleaned.
func resolveAbs(fsys FS, p string) string {
	p = toSlash(p)
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	cwd := "/"
	if c, ok := fsys.(Cwder); ok {
		if wd, err := c.Getwd(); err == nil && wd != "" {
			cwd = toSlash(wd)
		}
	}
	return path.Clean(path.Join(cwd, p))
}

// rootOf returns the filesystem root of an absolute slash path. On POSIX this
// is always "/".
func rootOf(string) string { return "/" }

// splitSegments splits a path into its non-empty "/"-delimited components,
// dropping empties. "." and ".." segments are preserved and are collapsed by
// path.Join as the walk proceeds.
func splitSegments(p string) []string {
	p = toSlash(p)
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toSlash converts OS separators to forward slashes so OSFS results on a
// backslash host still feed the POSIX walker.
func toSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// canonicalNFC applies NFC normalization to realpath output.
func canonicalNFC(p string) string { return norm.NFC.String(p) }

// loopError builds the ELOOP-like error for an unresolvable symlink chain.
func loopError(p string) error {
	return &fs.PathError{Op: "realpath", Path: p, Err: ErrSymlinkLoop}
}
