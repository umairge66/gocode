package chromehost

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestCodec_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Command{ID: "7", Method: MethodNavigate, Params: json.RawMessage(`{"url":"https://example.com"}`)}
	if err := WriteMessage(&buf, in); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	// The first 4 bytes are the little-endian length of the JSON payload.
	if buf.Len() < 4 {
		t.Fatalf("frame too short: %d bytes", buf.Len())
	}
	got := binary.LittleEndian.Uint32(buf.Bytes()[:4])
	if int(got) != buf.Len()-4 {
		t.Fatalf("length prefix = %d, want %d", got, buf.Len()-4)
	}
	raw, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var out Command
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.Method != in.Method || string(out.Params) != string(in.Params) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestReadMessage_CleanEOF(t *testing.T) {
	_, err := ReadMessage(bytes.NewReader(nil))
	if err != io.EOF {
		t.Fatalf("ReadMessage on empty stream = %v, want io.EOF", err)
	}
}

func TestReadMessage_TruncatedPrefix(t *testing.T) {
	_, err := ReadMessage(bytes.NewReader([]byte{0x01, 0x02}))
	if err == nil || err == io.EOF {
		t.Fatalf("truncated prefix err = %v, want a non-EOF error", err)
	}
}

func TestReadMessage_TruncatedPayload(t *testing.T) {
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], 10)
	frame := append(prefix[:], []byte("abc")...) // claims 10, provides 3
	_, err := ReadMessage(bytes.NewReader(frame))
	if err == nil || err == io.EOF {
		t.Fatalf("truncated payload err = %v, want a non-EOF error", err)
	}
}

func TestReadMessage_OversizeRejected(t *testing.T) {
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], MaxReadSize+1)
	// No payload needed: the length check happens before any payload read.
	_, err := ReadMessage(bytes.NewReader(prefix[:]))
	if err == nil {
		t.Fatalf("oversize frame accepted, want rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v, want an 'exceeds limit' message", err)
	}
}

func TestWriteMessage_OversizeRejected(t *testing.T) {
	big := strings.Repeat("x", MaxWriteSize)
	var buf bytes.Buffer
	err := WriteMessage(&buf, big) // JSON-encodes to > 1 MiB with quotes
	if err == nil {
		t.Fatalf("oversize write accepted, want rejection")
	}
	if buf.Len() != 0 {
		t.Fatalf("partial frame written on rejection: %d bytes", buf.Len())
	}
}
