// Package chromehost implements the Go side of a Chrome Native Messaging host
// for gocode's LOCAL browser-control feature (milestone 1).
//
// The host speaks Chrome's native-messaging wire protocol to a self-authored
// MV3 extension over stdio: Chrome launches this process, connects the
// extension to it with chrome.runtime.connectNative("com.gocode.gocode"), and
// the two exchange length-prefixed JSON frames on stdin/stdout. There is NO
// network, NO account/OAuth, NO cloud bridge and NO telemetry anywhere in this
// package — every byte flows between the local Chrome process and this host.
//
// Security model: Chrome enforces that only the extension whose id matches the
// host manifest's allowed_origins may connect. No port is opened and no shared
// token is needed — the operating-system pipe plus Chrome's origin check are
// the trust boundary. See internal/cli's `chrome install` for how the id is
// pinned into the native-messaging host manifest.
package chromehost

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Message-size limits for the native-messaging transport.
//
// Chrome caps a single message sent from the host TO the extension at 1 MiB,
// and a message from the extension TO the host at roughly 64 MiB. WriteMessage
// (host -> extension) enforces the former; ReadMessage (extension -> host)
// enforces the latter and rejects anything larger with a clear error rather
// than attempting a multi-gigabyte allocation from a corrupt length prefix.
const (
	// MaxWriteSize bounds a frame written host -> extension (Chrome's limit).
	MaxWriteSize = 1 << 20 // 1 MiB
	// MaxReadSize bounds a frame read extension -> host.
	MaxReadSize = 64 << 20 // 64 MiB
)

// WriteMessage encodes v as JSON and writes it as one native-messaging frame:
// a 4-byte little-endian uint32 length prefix followed by that many bytes of
// UTF-8 JSON. It refuses to write a frame larger than MaxWriteSize.
//
// The write is issued as a single Write call (prefix+payload in one buffer) so
// callers that serialize writes with a mutex produce atomic frames.
func WriteMessage(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("chromehost: marshal message: %w", err)
	}
	if len(payload) > MaxWriteSize {
		return fmt.Errorf("chromehost: message of %d bytes exceeds the %d-byte host->extension limit", len(payload), MaxWriteSize)
	}
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("chromehost: write frame: %w", err)
	}
	return nil
}

// FrameTooLargeError is ReadMessage's error for a frame whose length prefix
// exceeds MaxReadSize. The payload has not been read: a reader that wants to
// carry on discards Size bytes and reads the next frame.
type FrameTooLargeError struct {
	Size uint32
}

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("chromehost: frame length %d exceeds the %d-byte extension->host limit", e.Size, MaxReadSize)
}

// ReadMessage reads one native-messaging frame from r and returns its raw JSON
// payload. It reads the 4-byte little-endian length prefix, rejects a length
// greater than MaxReadSize (with a *FrameTooLargeError), then reads exactly
// that many payload bytes.
//
// EOF handling: a clean EOF before any byte of the length prefix returns
// io.EOF unwrapped, so a read loop can treat it as a normal end-of-stream. An
// EOF partway through the prefix or payload is a truncated frame and is
// reported as io.ErrUnexpectedEOF wrapped with context.
func ReadMessage(r io.Reader) (json.RawMessage, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("chromehost: truncated length prefix: %w", err)
		}
		return nil, fmt.Errorf("chromehost: read length prefix: %w", err)
	}
	n := binary.LittleEndian.Uint32(prefix[:])
	if n > MaxReadSize {
		return nil, &FrameTooLargeError{Size: n}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("chromehost: truncated payload (want %d bytes): %w", n, io.ErrUnexpectedEOF)
		}
		return nil, fmt.Errorf("chromehost: read payload: %w", err)
	}
	return json.RawMessage(payload), nil
}
