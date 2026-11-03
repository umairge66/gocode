package sessionstore

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// Reading a transcript from its end.
//
// Resuming needs a few facts that live near the end of the transcript (the
// last chain uuid, the latest cost totals). Reading the whole file for each
// of them made resuming a long session read it several times over; these
// helpers read backwards in blocks and stop at the first match.

// tailBlock is how much is read per step when scanning backwards.
const tailBlock = 256 << 10

// readerAtFS is the optional FS capability the backward scan uses. osFS has
// it; an FS without it falls back to reading the whole file.
type readerAtFS interface {
	OpenReaderAt(name string) (io.ReaderAt, int64, io.Closer, error)
}

func (osFS) OpenReaderAt(name string) (io.ReaderAt, int64, io.Closer, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, 0, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	return f, st.Size(), f, nil
}

// scanLinesBackward calls fn with each non-blank line of the file at path,
// last line first, until fn returns true. It reports whether fn did.
func (s *Store) scanLinesBackward(path string, fn func(line []byte) bool) bool {
	ra, ok := s.fs.(readerAtFS)
	if !ok {
		data, err := s.fs.ReadFile(path)
		if err != nil {
			return false
		}
		return eachLineBackward(data, fn)
	}
	r, size, c, err := ra.OpenReaderAt(path)
	if err != nil {
		return false
	}
	defer c.Close()
	// carry holds the start of a line whose beginning is in an earlier block.
	var carry []byte
	for end := size; end > 0; {
		start := max(0, end-tailBlock)
		block := make([]byte, end-start, int(end-start)+len(carry))
		if _, err := r.ReadAt(block, start); err != nil && err != io.EOF {
			return false
		}
		block = append(block, carry...)
		// Everything after the first newline is made of whole lines; the
		// part before it may continue into the previous block.
		first := 0
		if start > 0 {
			i := bytes.IndexByte(block, '\n')
			if i < 0 {
				carry = block
				end = start
				continue
			}
			first = i + 1
		}
		if eachLineBackward(block[first:], fn) {
			return true
		}
		carry = append([]byte(nil), block[:first]...)
		end = start
	}
	return len(carry) > 0 && eachLineBackward(carry, fn)
}

// eachLineBackward calls fn with each non-blank line of data, last first,
// until fn returns true.
func eachLineBackward(data []byte, fn func([]byte) bool) bool {
	for len(data) > 0 {
		i := bytes.LastIndexByte(bytes.TrimRight(data, "\r\n"), '\n')
		line := bytes.TrimSpace(data[i+1:])
		data = data[:max(i, 0)]
		if len(line) > 0 && fn(line) {
			return true
		}
		if i < 0 {
			break
		}
	}
	return false
}

// lastChainUUIDAt is the uuid of the last well-formed line that carries one.
func (s *Store) lastChainUUIDAt(path string) string {
	var last string
	s.scanLinesBackward(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"uuid"`)) {
			return false
		}
		var e struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(line, &e) != nil || e.UUID == "" {
			return false
		}
		last = e.UUID
		return true
	})
	return last
}
