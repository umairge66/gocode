package sessionstore

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS is the small write-capable filesystem seam the session store runs over.
// It is intentionally minimal (create directories, open a file for append or
// truncation, read files and directories, stat, remove) so a test can inject an
// in-memory implementation and no real disk is touched.
//
// internal/fsfacade.FS is read-only (Stat/ReadDir/ReadFile) — the persistence
// layer needs writes — so a dedicated interface is defined here rather than
// extending that shared read seam. The default osFS delegates to the os package
// and is what the live CLI uses; tests use osFS over a t.TempDir() (a real,
// offline, throwaway tree) or a fake.
type FS interface {
	// MkdirAll creates dir and any missing parents (os.MkdirAll semantics).
	MkdirAll(dir string, perm os.FileMode) error
	// OpenAppend opens name for appending, creating it (and nothing else) when
	// absent. The returned writer is closed by the caller. It is the crash-tolerant
	// write path: each turn's entries are appended and flushed as they happen.
	OpenAppend(name string, perm os.FileMode) (io.WriteCloser, error)
	// Create truncates (or creates) name for writing. It is used for the rewrite
	// path (e.g. after a compaction shrinks the history).
	Create(name string, perm os.FileMode) (io.WriteCloser, error)
	// ReadFile reads the whole file.
	ReadFile(name string) ([]byte, error)
	// ReadDir lists a directory.
	ReadDir(name string) ([]fs.DirEntry, error)
	// Stat reports file metadata (used for mtime fallbacks).
	Stat(name string) (fs.FileInfo, error)
	// Remove deletes a single file.
	Remove(name string) error
}

// osFS is the default FS backed by the real filesystem.
type osFS struct{}

func (osFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }

func (osFS) OpenAppend(name string, perm os.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
}

func (osFS) Create(name string, perm os.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
}

func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (osFS) Remove(name string) error                   { return os.Remove(name) }

// join is filepath.Join, isolated so a future FS could override path semantics.
func join(parts ...string) string { return filepath.Join(parts...) }

// pathSep is the platform path separator as a string.
const pathSep = string(filepath.Separator)
