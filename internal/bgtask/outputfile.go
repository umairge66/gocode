package bgtask

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// MaxOutputFileBytes bounds the output file of one background task. Output
// past it is dropped and a single truncation marker is appended, so a command
// that prints without end cannot fill the disk.
const MaxOutputFileBytes int64 = 64 << 20

// outputFileName is the name of a task's output file inside the output dir.
func outputFileName(id string) string { return id + ".output" }

// outputFile is the io.Writer that records a task's full output on disk, up to
// a byte limit. Write never fails: an I/O error stops the recording (the
// in-memory tail keeps working) rather than the command.
type outputFile struct {
	mu      sync.Mutex
	f       *os.File
	written int64
	limit   int64
	capped  bool
}

// createOutputFile creates dir/<id>.output exclusively, readable only by the
// current user.
func createOutputFile(dir, id string, limit int64) (*outputFile, string, error) {
	path := filepath.Join(dir, outputFileName(id))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = MaxOutputFileBytes
	}
	return &outputFile{f: f, limit: limit}, path, nil
}

func (o *outputFile) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil || o.capped {
		return len(p), nil
	}
	room := o.limit - o.written
	chunk := p
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
	}
	if len(chunk) > 0 {
		n, err := o.f.Write(chunk)
		o.written += int64(n)
		if err != nil {
			_ = o.f.Close()
			o.f = nil
			return len(p), nil
		}
	}
	if len(chunk) < len(p) {
		o.capped = true
		_, _ = fmt.Fprintf(o.f, "\n[output truncated: the output file limit of %d bytes was reached]\n", o.limit)
	}
	return len(p), nil
}

// Close closes the file; later writes are dropped.
func (o *outputFile) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f = nil
	return err
}

// teeWriter writes to the in-memory tail and, when present, the output file.
type teeWriter struct {
	tail *ring
	file *outputFile
}

func (t teeWriter) Write(p []byte) (int, error) {
	n, _ := t.tail.Write(p)
	if t.file != nil {
		_, _ = t.file.Write(p)
	}
	return n, nil
}
