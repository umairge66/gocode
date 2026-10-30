package browser

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Saving screenshots to disk.
//
// The computer tool takes save_to_disk:true on a screenshot / zoom
// (also on a computer item inside browser_batch): the image is still returned
// inline, and is ALSO written to a private per-process directory
// ("gocode-chrome-screenshots-XXXX" under the temp dir, mode 0700) as
// "screenshot-<ms>-<n>.<ext>" (mode 0600, never overwriting), and the result
// gains "Screenshot saved to: <path>" plus a line asking the model to include
// the path in its reply. A failure never fails the call: the image stays inline
// and a note says why nothing was saved.
//
// save_to is an explicit file path the model chooses. That path is
// not a private directory, so the session runs it through the normal Write
// permission check and path rules before the capture (see toolrun) and the
// tool writes it with WriteImageFile.

// SavedPathPrefix is the text before each saved path in a tool result.
const SavedPathPrefix = "Screenshot saved to: "

const (
	noteIncludePaths  = "Include the saved path(s) in your response so they can be attached for the user."
	noteDirFailed     = "Note: save_to_disk failed — the screenshot directory could not be created. The image is included inline above."
	noteWriteFailed   = "Note: save_to_disk failed for at least one screenshot — it could not be written to disk. The image is included inline above."
	noteNotPersisting = "Note: save_to_disk had no effect — screenshots are not persisted to disk in this session. The image is included inline above; refer to it directly. Do not retry with save_to_disk."
)

// imageExt maps an image media type to a file extension (png by default).
func imageExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "png"
}

// ScreenshotSaver writes save_to_disk screenshots. The zero value saves into a
// lazily created private temp directory; Dir overrides it (returning "" means
// screenshots are not persisted in this session).
type ScreenshotSaver struct {
	// Dir, when set, returns the directory to save into (created 0700 if
	// missing); "" disables saving.
	Dir func() string
	// TempDir is the parent of the default directory ("" -> os.TempDir()).
	TempDir string

	mu  sync.Mutex
	dir string
	seq int
}

// DefaultScreenshotSaver is shared by the tools of one process, so all
// save_to_disk screenshots of a session land in one directory.
var DefaultScreenshotSaver = &ScreenshotSaver{}

func (s *ScreenshotSaver) directory() (string, error) {
	if s.Dir != nil {
		d := s.Dir()
		if d == "" {
			return "", nil
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		return d, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" && privateDir(s.dir) {
		return s.dir, nil
	}
	d, err := os.MkdirTemp(s.TempDir, "gocode-chrome-screenshots-")
	if err != nil {
		return "", err
	}
	s.dir = d
	return d, nil
}

// SaveImages writes each image and returns the lines to append to the tool
// result (saved paths and notes). It never returns an error: failures become
// notes.
func (s *ScreenshotSaver) SaveImages(images []Image) []string {
	if len(images) == 0 {
		return nil
	}
	dir, err := s.directory()
	if err != nil {
		return []string{noteDirFailed}
	}
	if dir == "" {
		return []string{noteNotPersisting}
	}
	var lines []string
	saved, failed := false, false
	for _, img := range images {
		s.mu.Lock()
		n := s.seq
		s.seq++
		s.mu.Unlock()
		name := fmt.Sprintf("screenshot-%d-%d.%s", time.Now().UnixMilli(), n, imageExt(img.MediaType))
		path := filepath.Join(dir, name)
		if err := writeImage(path, img, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			failed = true
			continue
		}
		saved = true
		lines = append(lines, SavedPathPrefix+path)
	}
	if saved {
		lines = append(lines, noteIncludePaths)
	}
	if failed {
		lines = append(lines, noteWriteFailed)
	}
	return lines
}

// WriteImageFile writes one image to an explicit path (the save_to parameter),
// creating the parent directory and replacing an existing file. The caller has
// already run the path through the Write permission check.
func WriteImageFile(path string, img Image) error {
	if !filepath.IsAbs(path) {
		return errors.New("save_to must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeImage(path, img, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

func writeImage(path string, img Image, flag int, perm os.FileMode) error {
	data, err := base64.StdEncoding.DecodeString(img.Base64)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// appendLines adds result lines after a blank line.
func appendLines(text string, lines []string) string {
	if len(lines) == 0 {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + strings.Join(lines, "\n")
}
