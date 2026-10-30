// Package lsp is gocode's Language Server Protocol client: the
// Content-Length-framed JSON-RPC base protocol spoken to stdio language
// servers, the initialize handshake, document synchronization (didOpen /
// didChange / didSave / didClose), passive diagnostics collection, the server
// lifecycle (lazy start, crash recovery, shutdown), the server configuration
// plugins declare (`.lsp.json` and the `lspServers` manifest field), the
// model-facing LSP tool, and the new-diagnostics reminder sent after edits.
//
// LSP frames each message with an HTTP-style header block (a Content-Length
// header terminated by a blank line) followed by exactly that many body
// bytes. This is unrelated to the newline-delimited JSON the MCP stdio
// transport uses.
package lsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Framing limits.
const (
	// MaxHeaderSize caps the header block. A header block that grows past
	// this without a terminator is a protocol violation: the server is most
	// likely printing logs to stdout instead of stderr.
	MaxHeaderSize = 65536
	// MaxMessageSize caps a single message body. A declared Content-Length
	// larger than this is refused rather than buffered.
	MaxMessageSize = 32 << 20
)

// headerSeparator terminates the header block.
const headerSeparator = "\r\n\r\n"

// headerLine matches one well-formed header field: a token name, optional
// whitespace, a colon, optional whitespace, then a value without CR/LF.
var headerLine = regexp.MustCompile("^([!#$%&'*+.^_`|~0-9A-Za-z-]+)[ \t]*:[ \t]*([^\r\n]*)$")

// allowedHeaders is the set of header names the base protocol permits. Any
// other header name signals a desynchronized stream.
var allowedHeaders = map[string]struct{}{
	"content-length": {},
	"content-type":   {},
}

// Protocol-violation errors, distinct so a desynchronized server can be told
// apart from a closed transport.
var (
	ErrHeaderTooLarge  = errors.New("lsp: server exceeded the header size limit without a message header (likely printing non-protocol output to stdout instead of stderr)")
	ErrNonProtocol     = errors.New("lsp: server sent non-protocol output in the header block (stdout desynchronized from the base protocol)")
	ErrNoContentLen    = errors.New("lsp: server sent a header block without a Content-Length (stdout desynchronized)")
	ErrBadContentLen   = errors.New("lsp: server sent a Content-Length that is not a number (stdout desynchronized)")
	ErrMessageTooLarge = errors.New("lsp: server declared a message larger than the size limit; refusing to buffer it")
)

// WriteMessage frames and writes one JSON-RPC payload:
// "Content-Length: <n>\r\n\r\n<body>". Header and body go out in one write so
// a partial frame is never interleaved with another writer's.
func WriteMessage(w io.Writer, payload []byte) error {
	head := "Content-Length: " + strconv.Itoa(len(payload)) + headerSeparator
	buf := make([]byte, 0, len(head)+len(payload))
	buf = append(buf, head...)
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// FrameReader decodes a stream of Content-Length-framed messages. It
// tolerates a message split across several reads and several messages
// buffered together.
type FrameReader struct {
	r *bufio.Reader
}

// NewFrameReader wraps r in a FrameReader.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReader(r)}
}

// ReadMessage returns the body of the next framed message. It returns io.EOF
// when the stream ends cleanly at a message boundary and a wrapped
// io.ErrUnexpectedEOF when it ends mid-message. A protocol violation returns
// one of the Err* sentinels.
func (fr *FrameReader) ReadMessage() ([]byte, error) {
	length, err := fr.readHeaders()
	if err != nil {
		return nil, err
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("lsp: stream ended mid-body: %w", io.ErrUnexpectedEOF)
		}
		return nil, err
	}
	return body, nil
}

// readHeaders consumes the header block up to and including the blank line
// and returns the declared Content-Length. Only content-length and
// content-type are accepted, Content-Length is mandatory and numeric, and
// both the header block and the declared body are size-bounded.
func (fr *FrameReader) readHeaders() (int, error) {
	var lines []string
	headerBytes := 0
	first := true
	for {
		line, err := fr.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 && first {
				return 0, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("lsp: stream ended mid-header: %w", io.ErrUnexpectedEOF)
			}
			return 0, err
		}
		first = false
		headerBytes += len(line)
		if headerBytes > MaxHeaderSize {
			return 0, ErrHeaderTooLarge
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		lines = append(lines, trimmed)
	}
	return parseContentLength(lines)
}

// parseContentLength validates the header lines and extracts Content-Length.
func parseContentLength(lines []string) (int, error) {
	var raw string
	found := false
	for _, line := range lines {
		m := headerLine.FindStringSubmatch(line)
		if m == nil {
			return 0, ErrNonProtocol
		}
		name := strings.ToLower(m[1])
		if _, ok := allowedHeaders[name]; !ok {
			return 0, ErrNonProtocol
		}
		if name == "content-length" {
			raw = strings.TrimSpace(m[2])
			found = true
		}
	}
	if !found {
		return 0, ErrNoContentLen
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, ErrBadContentLen
	}
	if n > MaxMessageSize {
		return 0, ErrMessageTooLarge
	}
	return n, nil
}
