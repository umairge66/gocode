// Package fsfacade provides a small filesystem abstraction (FS, with a real
// OS implementation and an in-memory MemFS for tests) and a canonical
// realpath / symlink-chain resolver.
//
// Settings, skills, plugins, agent definitions, MCP configuration, project
// memory and the path guard read the filesystem through FS so that their
// filesystem-dependent logic can be exercised without touching the real disk.
//
// # Scope
//
// The resolver implements the POSIX core. Windows UNC / device-namespace /
// network-share classification is out of scope for this facade, matching the
// POSIX-only handling in internal/pathguard/inode.go.
package fsfacade

import (
	"io/fs"
	"os"
)

// FS is the minimal filesystem surface the realpath engine (and the
// pathguard/envtrust seams) need: lstat, readlink, (real) stat, directory
// listing and file reads.
//
// Stat follows symlinks; Lstat does not (it reports on the link itself). All
// methods accept and return slash-style (POSIX) paths.
type FS interface {
	// Stat returns info about the file named by name, following symlinks.
	Stat(name string) (fs.FileInfo, error)
	// Lstat returns info about the file named by name without following a
	// final symlink.
	Lstat(name string) (fs.FileInfo, error)
	// Readlink returns the target of the symlink named by name.
	Readlink(name string) (string, error)
	// ReadDir reads the named directory.
	ReadDir(name string) ([]fs.DirEntry, error)
	// Open opens the named file for reading, following symlinks.
	Open(name string) (fs.File, error)
	// ReadFile reads the named file, following symlinks.
	ReadFile(name string) ([]byte, error)
}

// Cwder is an optional interface an FS may implement to supply a working
// directory for resolving relative paths. OSFS implements it via os.Getwd;
// MemFS via its settable Cwd field. When an FS does not implement Cwder,
// relative paths are resolved against "/".
type Cwder interface {
	Getwd() (string, error)
}

// OSFS is the real filesystem implementation of FS over the os package.
type OSFS struct{}

var _ FS = OSFS{}
var _ Cwder = OSFS{}

// Stat implements FS.
func (OSFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// Lstat implements FS.
func (OSFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

// Readlink implements FS.
func (OSFS) Readlink(name string) (string, error) { return os.Readlink(name) }

// ReadDir implements FS.
func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// Open implements FS.
func (OSFS) Open(name string) (fs.File, error) { return os.Open(name) }

// ReadFile implements FS.
func (OSFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Getwd implements Cwder.
func (OSFS) Getwd() (string, error) { return os.Getwd() }
