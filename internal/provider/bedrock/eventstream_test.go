package bedrock

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

// encodeFrame builds one event-stream frame with string headers (in the given
// order), mirroring what Bedrock sends. Used by the tests and the fake server.
func encodeFrame(headers [][2]string, payload []byte) []byte {
	var hb bytes.Buffer
	for _, h := range headers {
		hb.WriteByte(byte(len(h[0])))
		hb.WriteString(h[0])
		hb.WriteByte(headerTypeStr)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(h[1])))
		hb.Write(l[:])
		hb.WriteString(h[1])
	}
	total := preludeLen + hb.Len() + len(payload) + messageCRCLen
	var out bytes.Buffer
	var pre [8]byte
	binary.BigEndian.PutUint32(pre[0:4], uint32(total))
	binary.BigEndian.PutUint32(pre[4:8], uint32(hb.Len()))
	out.Write(pre[:])
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(pre[:]))
	out.Write(c[:])
	out.Write(hb.Bytes())
	out.Write(payload)
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(out.Bytes()))
	out.Write(c[:])
	return out.Bytes()
}

// eventFrame is a ConverseStream event frame as Bedrock frames it.
func eventFrame(eventType, json string) []byte {
	return encodeFrame([][2]string{
		{":event-type", eventType},
		{":content-type", "application/json"},
		{":message-type", "event"},
	}, []byte(json))
}

// exceptionFrame is a modeled exception frame.
func exceptionFrame(exceptionType, json string) []byte {
	return encodeFrame([][2]string{
		{":exception-type", exceptionType},
		{":content-type", "application/json"},
		{":message-type", "exception"},
	}, []byte(json))
}

func TestEventStreamDecodesFrameSequence(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(eventFrame("messageStart", `{"p":"abcd","role":"assistant"}`))
	stream.Write(eventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"},"p":"ab"}`))
	stream.Write(eventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo"}}`))
	stream.Write(eventFrame("messageStop", `{"stopReason":"end_turn"}`))

	rd := newEventStreamReader(&stream)
	var types, payloads []string
	for {
		m, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if m.Headers[":message-type"] != "event" || m.Headers[":content-type"] != "application/json" {
			t.Errorf("headers = %v", m.Headers)
		}
		types = append(types, m.Headers[":event-type"])
		payloads = append(payloads, string(m.Payload))
	}
	want := []string{"messageStart", "contentBlockDelta", "contentBlockDelta", "messageStop"}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("frame %d type = %s, want %s", i, types[i], want[i])
		}
	}
	if payloads[2] != `{"contentBlockIndex":0,"delta":{"text":"lo"}}` {
		t.Errorf("payload = %s", payloads[2])
	}
}

func TestEventStreamNonStringHeadersSkipped(t *testing.T) {
	// A frame with an int32 header, a bool header and a timestamp header.
	var hb bytes.Buffer
	hb.WriteByte(3)
	hb.WriteString("num")
	hb.WriteByte(4)
	hb.Write([]byte{0, 0, 0, 7})
	hb.WriteByte(4)
	hb.WriteString("flag")
	hb.WriteByte(0)
	hb.WriteByte(2)
	hb.WriteString("ts")
	hb.WriteByte(8)
	hb.Write(make([]byte, 8))
	h, err := parseEventHeaders(hb.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h["flag"] != "true" {
		t.Errorf("bool header = %q", h["flag"])
	}
	if _, ok := h["num"]; ok {
		t.Errorf("int header should be skipped")
	}
}

func TestEventStreamRejectsCorruption(t *testing.T) {
	f := eventFrame("messageStart", `{"role":"assistant"}`)

	bad := append([]byte(nil), f...)
	bad[len(bad)-6] ^= 0xFF // flip a payload byte -> message CRC mismatch
	if _, err := newEventStreamReader(bytes.NewReader(bad)).Next(); !errors.Is(err, errCRCMismatch) {
		t.Errorf("payload corruption: err = %v", err)
	}

	bad = append([]byte(nil), f...)
	bad[1] ^= 0x01 // prelude corruption
	if _, err := newEventStreamReader(bytes.NewReader(bad)).Next(); !errors.Is(err, errCRCMismatch) {
		t.Errorf("prelude corruption: err = %v", err)
	}

	if _, err := newEventStreamReader(bytes.NewReader(f[:len(f)-3])).Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated frame: err = %v", err)
	}
	if _, err := newEventStreamReader(bytes.NewReader(nil)).Next(); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: err = %v", err)
	}
}
