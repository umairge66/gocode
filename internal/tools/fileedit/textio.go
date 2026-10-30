package fileedit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// notText classifies bytes the tools refuse to write back.
const (
	notTextNone    = ""
	notTextUTF8    = "not_utf8"
	notTextNUL     = "nul"
	notTextUTF16BE = "utf16be"
)

var notTextDescriptions = map[string]string{
	notTextUTF8:    "is not valid UTF-8: it uses a legacy encoding such as Windows-1252, Shift-JIS or GBK, or it is binary",
	notTextNUL:     "has NUL bytes in its first 8 KB: binary data, UTF-16/UTF-32 without a byte-order mark, or text with embedded NULs",
	notTextUTF16BE: "is UTF-16 big-endian (it starts FE FF), which these tools cannot write",
}

// notTextEditMessage is the Edit refusal for a non-text file.
func notTextEditMessage(kind string) string {
	return "File " + notTextDescriptions[kind] + ". These tools write UTF-8 (or UTF-16LE behind its byte-order mark) and never convert a file's encoding on their own, so nothing was written. If the file is text, make the change with a shell command that reads and writes it in its own encoding (iconv, or PowerShell Get-Content/Set-Content -Encoding), or ask the user whether to convert it to UTF-8 first. To replace the whole file with new UTF-8 text anyway, delete it first, then Write it."
}

// notTextReadNotice is appended to a Read of a non-text file.
func notTextReadNotice(kind string) string {
	extra := ""
	if kind == notTextUTF8 {
		extra = " Its undecodable bytes show above as U+FFFD."
	}
	return "[This file " + notTextDescriptions[kind] + "." + extra + " Edit and Write will refuse it: change it with a shell command that keeps its encoding, or ask the user whether to convert it to UTF-8.]"
}

const sniffBytes = 8192

func classifyText(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return notTextUTF16BE
	}
	head := b
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return notTextNUL
	}
	if !utf8.Valid(b) {
		return notTextUTF8
	}
	return notTextNone
}

const utf8BOM = "\uFEFF"

// textFile is a decoded text file: LF-normalized content plus what is needed
// to write it back the same way.
type textFile struct {
	content string // LF-normalized, BOM kept as the first rune if present
	crlf    bool   // original line endings were predominantly CRLF
	notText string
	mode    os.FileMode
	raw     []byte
	info    os.FileInfo
}

// detectCRLF reports whether CRLF endings outnumber bare LF endings.
func detectCRLF(s string) bool {
	crlf, lf := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > 0 && s[i-1] == '\r' {
				crlf++
			} else {
				lf++
			}
		}
	}
	return crlf > lf
}

func loadText(path string) (*textFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, eisdir(path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	return &textFile{
		content: strings.ReplaceAll(s, "\r\n", "\n"),
		crlf:    detectCRLF(s),
		notText: classifyText(raw),
		mode:    info.Mode().Perm(),
		raw:     raw,
		info:    info,
	}, nil
}

// eisdir is the directory error, followed by a hint on how to list a
// directory (without it the model retries Read on parent directories before
// reaching for ls).
func eisdir(path string) error {
	return fmt.Errorf("EISDIR: illegal operation on a directory, read '%s'. "+
		"This path is a directory, not a file: list it with `ls` via the Bash tool, "+
		"or find files in it with Glob (e.g. pattern \"**/*\" and this path), then Read a file inside it.", path)
}

// encodeForDisk restores CRLF endings when the original used them.
func encodeForDisk(content string, crlf bool) []byte {
	if crlf {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	return []byte(content)
}

// writeAtomic writes data to path via a temp file in the same directory and a
// rename, preserving mode (perm when the file is new).
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// cwdNotePrefix starts the working-directory note that follows the
// "File does not exist." message.
const cwdNotePrefix = "Note: your current working directory is"

// fileNotExistMessage builds the missing-file message, including
// the "Did you mean" hint for a same-stem sibling.
func fileNotExistMessage(path string) string {
	cwd, _ := os.Getwd()
	msg := fmt.Sprintf("File does not exist. %s %s.", cwdNotePrefix, cwd)
	if sim := findSimilarFile(path); sim != "" {
		msg += " Did you mean " + sim + "?"
	}
	return msg
}

func findSimilarFile(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		n := e.Name()
		if n == base || e.IsDir() {
			continue
		}
		if strings.TrimSuffix(n, filepath.Ext(n)) == stem {
			return n
		}
	}
	return ""
}

// formatBytes renders a byte count for size messages.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/(1<<30))) + "GB"
	case n >= 1<<20:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/(1<<20))) + "MB"
	case n >= 1<<10:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/(1<<10))) + "KB"
	}
	return fmt.Sprintf("%d bytes", n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }
