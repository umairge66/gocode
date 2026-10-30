package exec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// --- Validation messages ---------------------------------------------------
//
// These are shared by the Write and Edit tools.

const (
	// MsgFileNotRead is returned when a caller tries to write over an existing
	// file that has not been read first (the "must read before overwrite"
	// rule).
	MsgFileNotRead = "File has not been read yet. Read it first before writing to it."
)

// Encoding refusals: the tools write UTF-8 (or UTF-16LE behind its
// byte-order mark) and refuse to write a target whose existing bytes are in a
// legacy/undecodable encoding, contain NUL bytes, or are UTF-16 big-endian.
var (
	ErrFileNotRead  = errors.New(MsgFileNotRead)
	ErrEmptyPath    = errors.New("write: file_path is required")
	ErrNonAbsPath   = errors.New("write: file_path must be an absolute path")
	ErrContentNUL   = errors.New("write: content has NUL bytes (binary data or UTF-16/UTF-32 without a BOM); these tools write UTF-8")
	ErrContentUTF16 = errors.New("write: content is UTF-16 big-endian (starts FE FF), which these tools cannot write")
	ErrContentUTF8  = errors.New("write: content is not valid UTF-8; these tools write UTF-8 (or UTF-16LE behind its byte-order mark)")
)

// WriteInput is the Write tool input schema: an absolute file_path and the
// full content to write.
type WriteInput struct {
	// FilePath is the absolute path of the file to write.
	FilePath string `json:"file_path"`
	// Content is the full file content to write.
	Content string `json:"content"`
}

// WriteResult reports what Write did.
type WriteResult struct {
	Path    string
	Created bool // true when a new file was created (did not exist before)
	Bytes   int
}

// ReadTracker records which files the agent has read, so the Write tool can
// enforce the "must read before overwrite" rule.
//
// The session's file-state tracker (which also detects the "changed since
// last read" case) is fileedit.FileStateTracker
// (internal/tools/fileedit/filestate.go), which satisfies this interface and is
// shared by Read/Edit/MultiEdit/Write in toolrun's registerDefaults.
type ReadTracker interface {
	// WasRead reports whether the file at path has been read in this session.
	WasRead(path string) bool
}

// AllowAllReads is a ReadTracker that treats every file as already read. It is
// convenient for callers that do not enforce the read-before-write rule (for
// example, non-interactive writes).
type AllowAllReads struct{}

// WasRead always returns true.
func (AllowAllReads) WasRead(string) bool { return true }

// WriteTool writes files to the local filesystem with the overwrite
// and encoding semantics.
type WriteTool struct {
	// Tracker enforces the read-before-overwrite rule. When nil, overwrites of
	// existing files are refused with ErrFileNotRead (fail closed), matching
	// the default that a file must be read before it is written.
	Tracker ReadTracker
	// Perm is the file mode used when creating a new file (default 0o644).
	Perm os.FileMode
	// RequireAbs, when true, rejects a non-absolute file_path (the
	// schema documents file_path as an absolute path).
	RequireAbs bool
}

// validateContent applies the encoding refusals.
func validateContent(content string) error {
	b := []byte(content)
	if bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		return ErrContentUTF16
	}
	if bytes.IndexByte(b, 0x00) >= 0 {
		return ErrContentNUL
	}
	if !utf8.ValidString(content) {
		return ErrContentUTF8
	}
	return nil
}

// Execute writes Content to FilePath.
//
// Semantics:
//   - A new file (one that does not yet exist) may always be created; parent
//     directories are created as needed.
//   - Overwriting an existing file requires that the file has been read first
//     (ReadTracker.WasRead); otherwise it is refused with ErrFileNotRead.
//   - Content must be writable as UTF-8 (no NUL bytes, no UTF-16BE BOM).
func (w WriteTool) Execute(in WriteInput) (WriteResult, error) {
	if in.FilePath == "" {
		return WriteResult{}, ErrEmptyPath
	}
	if w.RequireAbs && !filepath.IsAbs(in.FilePath) {
		return WriteResult{}, ErrNonAbsPath
	}
	if err := validateContent(in.Content); err != nil {
		return WriteResult{}, err
	}

	_, statErr := os.Stat(in.FilePath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return WriteResult{}, statErr
	}

	if exists {
		// Read-before-overwrite rule. Fail closed when no tracker.
		tracker := w.Tracker
		if tracker == nil || !tracker.WasRead(in.FilePath) {
			return WriteResult{}, ErrFileNotRead
		}
	} else {
		// Create parent directories for a new file.
		if dir := filepath.Dir(in.FilePath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return WriteResult{}, err
			}
		}
	}

	perm := w.Perm
	if perm == 0 {
		perm = 0o644
	}
	if err := os.WriteFile(in.FilePath, []byte(in.Content), perm); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: in.FilePath, Created: !exists, Bytes: len(in.Content)}, nil
}
