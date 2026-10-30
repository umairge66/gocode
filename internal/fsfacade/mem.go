package fsfacade

import (
	"bytes"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// MemFS is an in-memory FS implementation for tests. It supports regular files,
// directories and symbolic links, keyed by cleaned absolute slash paths. It is
// not safe for concurrent mutation; build it up before handing it to resolvers.
//
// Nodes are stored at their literal creation paths. The realpath walker only
// ever Lstat's a path whose ancestors are already resolved to real paths, so
// literal keying is sufficient for correct symlink resolution.
type MemFS struct {
	nodes map[string]*memNode
	// Cwd is the working directory used to resolve relative paths (satisfies
	// Cwder). Defaults to "/".
	Cwd string
}

type memNode struct {
	mode   fs.FileMode // includes fs.ModeDir / fs.ModeSymlink bits
	data   []byte      // regular-file contents
	target string      // symlink target
}

var _ FS = (*MemFS)(nil)
var _ Cwder = (*MemFS)(nil)

// NewMemFS returns an empty MemFS containing only the root directory.
func NewMemFS() *MemFS {
	m := &MemFS{nodes: map[string]*memNode{}, Cwd: "/"}
	m.nodes["/"] = &memNode{mode: fs.ModeDir | 0o755}
	return m
}

// Getwd implements Cwder.
func (m *MemFS) Getwd() (string, error) {
	if m.Cwd == "" {
		return "/", nil
	}
	return m.Cwd, nil
}

func (m *MemFS) clean(name string) string {
	name = toSlash(name)
	if !path.IsAbs(name) {
		base := m.Cwd
		if base == "" {
			base = "/"
		}
		name = path.Join(base, name)
	}
	return path.Clean(name)
}

// MkdirAll creates dir and any missing ancestors.
func (m *MemFS) MkdirAll(dir string) *MemFS {
	dir = m.clean(dir)
	parts := splitSegments(dir)
	cur := "/"
	for _, p := range parts {
		cur = path.Join(cur, p)
		if n, ok := m.nodes[cur]; !ok {
			m.nodes[cur] = &memNode{mode: fs.ModeDir | 0o755}
		} else if !n.mode.IsDir() {
			// A non-dir already occupies the path; leave it (callers in tests
			// control their own layout).
			break
		}
	}
	return m
}

// WriteFile creates (or replaces) a regular file, making parent dirs as needed.
func (m *MemFS) WriteFile(name string, data []byte) *MemFS {
	name = m.clean(name)
	m.MkdirAll(path.Dir(name))
	cp := make([]byte, len(data))
	copy(cp, data)
	m.nodes[name] = &memNode{mode: 0o644, data: cp}
	return m
}

// Symlink creates a symbolic link at linkpath pointing at target, making parent
// dirs as needed. target is stored verbatim (it may be relative or absolute).
func (m *MemFS) Symlink(target, linkpath string) *MemFS {
	linkpath = m.clean(linkpath)
	m.MkdirAll(path.Dir(linkpath))
	m.nodes[linkpath] = &memNode{mode: fs.ModeSymlink | 0o777, target: target}
	return m
}

// Lstat implements FS (does not follow a final symlink).
func (m *MemFS) Lstat(name string) (fs.FileInfo, error) {
	name = m.clean(name)
	n, ok := m.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
	}
	return memFileInfo{name: path.Base(name), node: n}, nil
}

// Stat implements FS (follows symlinks by canonicalizing via Realpath).
func (m *MemFS) Stat(name string) (fs.FileInfo, error) {
	real, err := Realpath(m, name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: m.clean(name), Err: unwrapCause(err)}
	}
	n, ok := m.nodes[real]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: real, Err: fs.ErrNotExist}
	}
	return memFileInfo{name: path.Base(real), node: n}, nil
}

// Readlink implements FS.
func (m *MemFS) Readlink(name string) (string, error) {
	name = m.clean(name)
	n, ok := m.nodes[name]
	if !ok {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
	}
	if n.mode&fs.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: errInvalid}
	}
	return n.target, nil
}

// ReadDir implements FS. The directory path is canonicalized first.
func (m *MemFS) ReadDir(name string) ([]fs.DirEntry, error) {
	real, err := Realpath(m, name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: m.clean(name), Err: unwrapCause(err)}
	}
	dn, ok := m.nodes[real]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: real, Err: fs.ErrNotExist}
	}
	if !dn.mode.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: real, Err: errNotDir}
	}
	prefix := real
	if prefix != "/" {
		prefix += "/"
	}
	var out []fs.DirEntry
	for p, n := range m.nodes {
		if p == real || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if strings.Contains(rest, "/") {
			continue // not a direct child
		}
		out = append(out, fs.FileInfoToDirEntry(memFileInfo{name: rest, node: n}))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Open implements FS (follows symlinks).
func (m *MemFS) Open(name string) (fs.File, error) {
	real, err := Realpath(m, name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: m.clean(name), Err: unwrapCause(err)}
	}
	n, ok := m.nodes[real]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: real, Err: fs.ErrNotExist}
	}
	return &memFile{info: memFileInfo{name: path.Base(real), node: n}, r: bytes.NewReader(n.data)}, nil
}

// ReadFile implements FS (follows symlinks).
func (m *MemFS) ReadFile(name string) ([]byte, error) {
	real, err := Realpath(m, name)
	if err != nil {
		return nil, &fs.PathError{Op: "readfile", Path: m.clean(name), Err: unwrapCause(err)}
	}
	n, ok := m.nodes[real]
	if !ok {
		return nil, &fs.PathError{Op: "readfile", Path: real, Err: fs.ErrNotExist}
	}
	cp := make([]byte, len(n.data))
	copy(cp, n.data)
	return cp, nil
}

// unwrapCause returns the underlying cause of a *fs.PathError so wrapped
// sentinels (fs.ErrNotExist, ErrSymlinkLoop) survive re-wrapping.
func unwrapCause(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

// errInvalid / errNotDir represent the POSIX EINVAL / ENOTDIR conditions without
// pulling in the syscall package (which MemFS must not depend on).
var (
	errInvalid = fs.ErrInvalid
	errNotDir  = errNotDirectory("not a directory")
)

type errNotDirectory string

func (e errNotDirectory) Error() string { return string(e) }

// memFileInfo implements fs.FileInfo over a memNode.
type memFileInfo struct {
	name string
	node *memNode
}

func (fi memFileInfo) Name() string       { return fi.name }
func (fi memFileInfo) Size() int64        { return int64(len(fi.node.data)) }
func (fi memFileInfo) Mode() fs.FileMode  { return fi.node.mode }
func (fi memFileInfo) ModTime() time.Time { return time.Time{} }
func (fi memFileInfo) IsDir() bool        { return fi.node.mode.IsDir() }
func (fi memFileInfo) Sys() any           { return nil }

// memFile implements fs.File over a regular file's bytes.
type memFile struct {
	info memFileInfo
	r    *bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *memFile) Close() error               { return nil }
