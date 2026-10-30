package lsp

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// slowReader hands out at most n bytes per Read, so a framed message is
// delivered across several Read calls.
type slowReader struct {
	data []byte
	n    int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	k := s.n
	if k > len(p) {
		k = len(p)
	}
	if k > len(s.data) {
		k = len(s.data)
	}
	copy(p, s.data[:k])
	s.data = s.data[k:]
	return k, nil
}

func frame(payload string) []byte {
	var b bytes.Buffer
	if err := WriteMessage(&b, []byte(payload)); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func TestWriteMessageShape(t *testing.T) {
	got := string(frame(`{"a":1}`))
	want := "Content-Length: 7\r\n\r\n" + `{"a":1}`
	if got != want {
		t.Fatalf("framing mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestRoundTripSingle(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	r := NewFrameReader(bytes.NewReader(frame(payload)))
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("round-trip mismatch: got %q want %q", got, payload)
	}
	// Next read hits a clean EOF at the boundary.
	if _, err := r.ReadMessage(); err != io.EOF {
		t.Fatalf("expected io.EOF at boundary, got %v", err)
	}
}

func TestMultipleMessagesInOneBuffer(t *testing.T) {
	msgs := []string{`{"id":1}`, `{"id":2}`, `{"method":"x"}`}
	var buf bytes.Buffer
	for _, m := range msgs {
		buf.Write(frame(m))
	}
	r := NewFrameReader(bytes.NewReader(buf.Bytes()))
	for i, want := range msgs {
		got, err := r.ReadMessage()
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("msg %d: got %q want %q", i, got, want)
		}
	}
	if _, err := r.ReadMessage(); err != io.EOF {
		t.Fatalf("expected io.EOF after last message, got %v", err)
	}
}

func TestPartialReads(t *testing.T) {
	msgs := []string{`{"id":1,"big":"` + string(bytes.Repeat([]byte("x"), 500)) + `"}`, `{"id":2}`}
	var buf bytes.Buffer
	for _, m := range msgs {
		buf.Write(frame(m))
	}
	// Deliver 3 bytes at a time: headers and bodies span many Read calls.
	r := NewFrameReader(&slowReader{data: buf.Bytes(), n: 3})
	for i, want := range msgs {
		got, err := r.ReadMessage()
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("msg %d mismatch (len got=%d want=%d)", i, len(got), len(want))
		}
	}
}

func TestNonProtocolOutputRejected(t *testing.T) {
	// A banner line on stdout before any header -> desync.
	raw := "Starting language server...\r\nContent-Length: 2\r\n\r\n{}"
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	_, err := r.ReadMessage()
	if !errors.Is(err, ErrNonProtocol) {
		t.Fatalf("expected ErrNonProtocol, got %v", err)
	}
}

func TestMissingContentLength(t *testing.T) {
	raw := "Content-Type: application/vscode-jsonrpc\r\n\r\n{}"
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	_, err := r.ReadMessage()
	if !errors.Is(err, ErrNoContentLen) {
		t.Fatalf("expected ErrNoContentLen, got %v", err)
	}
}

func TestNonNumericContentLength(t *testing.T) {
	raw := "Content-Length: abc\r\n\r\n{}"
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	_, err := r.ReadMessage()
	if !errors.Is(err, ErrBadContentLen) {
		t.Fatalf("expected ErrBadContentLen, got %v", err)
	}
}

func TestOversizeMessageRejected(t *testing.T) {
	raw := "Content-Length: 99999999999\r\n\r\n"
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	_, err := r.ReadMessage()
	if !errors.Is(err, ErrMessageTooLarge) && !errors.Is(err, ErrBadContentLen) {
		t.Fatalf("expected ErrMessageTooLarge/ErrBadContentLen, got %v", err)
	}
}

func TestTruncatedBody(t *testing.T) {
	raw := "Content-Length: 10\r\n\r\n{}" // declares 10 bytes, provides 2
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	_, err := r.ReadMessage()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUnexpectedEOF, got %v", err)
	}
}

func TestHeaderTooLarge(t *testing.T) {
	raw := bytes.Repeat([]byte("X-Long: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n"), MaxHeaderSize/40)
	r := NewFrameReader(bytes.NewReader(raw))
	if _, err := r.ReadMessage(); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("expected ErrHeaderTooLarge, got %v", err)
	}
}

func TestContentTypeHeaderAccepted(t *testing.T) {
	raw := "Content-Length: 2\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n{}"
	r := NewFrameReader(bytes.NewReader([]byte(raw)))
	got, err := r.ReadMessage()
	if err != nil || string(got) != "{}" {
		t.Fatalf("got %q, %v", got, err)
	}
}
