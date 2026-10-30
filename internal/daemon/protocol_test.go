package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/errutil"
)

func TestFrameRoundTrip(t *testing.T) {
	req := Request{
		Op:        OpReply,
		Short:     "abc123",
		Nonce:     "n-1",
		Text:      "hello\tworld",
		NextTurn:  true,
		TimeoutMs: 1500,
		Auth:      "key",
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// The frame must be exactly one line terminated by a single newline.
	if n := bytes.Count(buf.Bytes(), []byte{'\n'}); n != 1 {
		t.Fatalf("want exactly 1 newline, got %d", n)
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		t.Fatal("frame not newline-terminated")
	}

	br := bufio.NewReader(&buf)
	frame, err := ReadFrame(br)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	var got Request
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Op != req.Op || got.Short != req.Short || got.Text != req.Text ||
		got.NextTurn != req.NextTurn || got.TimeoutMs != req.TimeoutMs || got.Auth != req.Auth {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, req)
	}
}

func TestFrameMultipleMessages(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := WriteFrame(&buf, Reply{OK: true, Op: OpPing, Proto: i}); err != nil {
			t.Fatal(err)
		}
	}
	br := bufio.NewReader(&buf)
	for i := 0; i < 3; i++ {
		frame, err := ReadFrame(br)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var rep Reply
		if err := json.Unmarshal(frame, &rep); err != nil {
			t.Fatalf("frame %d unmarshal: %v", i, err)
		}
		if rep.Proto != i {
			t.Fatalf("frame %d: proto = %d, want %d", i, rep.Proto, i)
		}
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	// A long unterminated run must trip ErrFrameTooLarge rather than block.
	big := strings.Repeat("x", maxReplyBytes+16)
	br := bufio.NewReader(strings.NewReader(big))
	_, err := ReadFrame(br)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	var we *errutil.WorkerError
	if !errors.As(err, &we) || we.Code != errutil.WorkerETooLarge {
		t.Fatalf("ErrFrameTooLarge should carry WorkerETooLarge, got %v", err)
	}
}

func TestProtoGate(t *testing.T) {
	if !ProtoAcceptable(ProtoVersion) {
		t.Fatal("current proto must be acceptable")
	}
	if ProtoAcceptable(ProtoVersion + 1) {
		t.Fatal("future proto must be rejected")
	}
	if ProtoAcceptable(MinProtoVersion - 1) {
		t.Fatal("too-old proto must be rejected")
	}
	rep := ProtoMismatchReply("2.1.306", 99)
	if rep.OK || rep.Code != errutil.WorkerEProto || rep.ServerProto != ProtoVersion {
		t.Fatalf("bad proto-mismatch reply: %+v", rep)
	}
}

func TestReplyAsError(t *testing.T) {
	if err := (Reply{OK: true}).AsError(); err != nil {
		t.Fatalf("ok reply should have no error, got %v", err)
	}
	rep := FailReply(errutil.WorkerERespawning, "retry")
	err := rep.AsError()
	var we *errutil.WorkerError
	if !errors.As(err, &we) || we.Code != errutil.WorkerERespawning {
		t.Fatalf("AsError lost the code: %v", err)
	}
	// A failure reply with no code defaults to EUNKNOWN.
	err = (Reply{OK: false, Error: "x"}).AsError()
	if !errors.As(err, &we) || we.Code != errutil.WorkerEUnknown {
		t.Fatalf("missing code should default to EUNKNOWN, got %v", err)
	}
}

func TestSocketPaths(t *testing.T) {
	rt := RuntimeDir("/tmp", 1000, "inst")
	if rt != filepath.FromSlash("/tmp/cc-daemon-1000/inst") {
		t.Fatalf("runtime dir = %q", rt)
	}
}
