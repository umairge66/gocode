// Package fileedit implements the core coding-agent file tools: Read, Edit
// and MultiEdit (the sequential multi-edit applier), plus the session
// file-state tracker that gates edits on a prior, still-current Read (the
// state consulted by the Edit/Write/NotebookEdit validators).
//
// There is no telemetry in this package.
package fileedit

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/tools/exec"
)

// File-state messages.
const (
	// MsgFileNotRead is returned when Edit/MultiEdit (or Write) targets an
	// existing file that was not read in this session.
	MsgFileNotRead = exec.MsgFileNotRead

	// MsgFileModifiedSinceRead is the Edit validator's refusal when the file
	// changed on disk after it was read (errorCode 7).
	MsgFileModifiedSinceRead = "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."
)

var (
	// ErrFileNotRead: the file was never read in this session.
	ErrFileNotRead = errors.New(MsgFileNotRead)
	// ErrFileModified: the file changed on disk since it was last read.
	ErrFileModified = errors.New(MsgFileModifiedSinceRead)
)

// fileState is what the tracker remembers about one file.
type fileState struct {
	modTime time.Time
	size    int64
	hash    [sha256.Size]byte
}

// FileStateTracker records, per absolute path, the (mtime, size, sha256) of a
// file as of the last time the agent read or wrote it. Edit and MultiEdit
// refuse to touch a file unless it was read and is unchanged since. It is safe
// for concurrent use.
//
// It satisfies exec.ReadTracker, so the same tracker can gate exec.WriteTool:
// WasRead reports true only when the file was recorded AND is still unchanged
// on disk, so a stale read fails closed for Write too.
type FileStateTracker struct {
	mu    sync.Mutex
	files map[string]fileState
}

// Compile-time check: the tracker can gate the Write tool.
var _ exec.ReadTracker = (*FileStateTracker)(nil)

// NewFileStateTracker returns an empty tracker.
func NewFileStateTracker() *FileStateTracker {
	return &FileStateTracker{files: make(map[string]fileState)}
}

func normPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// Record stores the state of path given its full on-disk bytes and stat info.
func (t *FileStateTracker) Record(path string, data []byte, info os.FileInfo) {
	st := fileState{hash: sha256.Sum256(data)}
	if info != nil {
		st.modTime = info.ModTime()
		st.size = info.Size()
	} else {
		st.size = int64(len(data))
	}
	t.mu.Lock()
	if t.files == nil {
		t.files = make(map[string]fileState)
	}
	t.files[normPath(path)] = st
	t.mu.Unlock()
}

// RecordFromDisk stats and reads path and records it. Use it after a write
// done outside this package (for example by exec.WriteTool).
func (t *FileStateTracker) RecordFromDisk(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	t.Record(path, data, info)
	return nil
}

// Forget drops any recorded state for path.
func (t *FileStateTracker) Forget(path string) {
	t.mu.Lock()
	delete(t.files, normPath(path))
	t.mu.Unlock()
}

// Check returns nil when path was recorded and is unchanged on disk,
// ErrFileNotRead when it was never recorded, and ErrFileModified when it
// changed since. It compares mtime first, then content, so
// a touched-but-identical file still counts as unchanged.
func (t *FileStateTracker) Check(path string) error {
	key := normPath(path)
	t.mu.Lock()
	st, ok := t.files[key]
	t.mu.Unlock()
	if !ok {
		return ErrFileNotRead
	}
	info, err := os.Stat(key)
	if err != nil {
		return ErrFileModified
	}
	if info.ModTime().Equal(st.modTime) && info.Size() == st.size {
		return nil
	}
	data, err := os.ReadFile(key)
	if err != nil || sha256.Sum256(data) != st.hash {
		return ErrFileModified
	}
	// Same content, new mtime: refresh so the next check is cheap.
	t.Record(key, data, info)
	return nil
}

// WasRead implements exec.ReadTracker: true only when the file was read and
// is unchanged since.
func (t *FileStateTracker) WasRead(path string) bool {
	return t.Check(path) == nil
}
