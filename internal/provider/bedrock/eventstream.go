package bedrock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// This file decodes the AWS event-stream binary framing
// (application/vnd.amazon.eventstream) that ConverseStream responses use:
//
//	[total length uint32][headers length uint32][prelude CRC32 uint32]
//	[headers ...][payload ...][message CRC32 uint32]
//
// Both CRCs are IEEE CRC-32; the prelude CRC covers the first 8 bytes and
// the message CRC covers everything before it. Each header is
// [name length uint8][name][value type uint8][value], with the value encoded
// per its type (see readHeaderValue). Reference:
// https://docs.aws.amazon.com/AmazonS3/latest/API/RESTSelectObjectAppendix.html
// (the same framing is shared by every AWS event-stream API).

const (
	preludeLen     = 12
	messageCRCLen  = 4
	minMessageLen  = preludeLen + messageCRCLen
	maxMessageLen  = 24 << 20 // generous upper bound; real frames are tiny
	maxHeadersLen  = 128 << 10
	headerTypeStr  = 7
	headerTypeBlob = 6
)

// eventMessage is one decoded event-stream frame.
type eventMessage struct {
	Headers map[string]string // string-valued headers (others are skipped)
	Payload []byte
}

// eventStreamReader reads frames from an event stream.
type eventStreamReader struct {
	r io.Reader
}

func newEventStreamReader(r io.Reader) *eventStreamReader { return &eventStreamReader{r: r} }

// errCRCMismatch reports a corrupted frame.
var errCRCMismatch = errors.New("bedrock: event stream CRC mismatch")

// Next returns the next frame, or io.EOF at a clean end of stream.
func (e *eventStreamReader) Next() (eventMessage, error) {
	var prelude [preludeLen]byte
	if _, err := io.ReadFull(e.r, prelude[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return eventMessage{}, io.EOF
		}
		return eventMessage{}, fmt.Errorf("bedrock: read event stream prelude: %w", err)
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	hlen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[0:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return eventMessage{}, errCRCMismatch
	}
	if total < minMessageLen || total > maxMessageLen || hlen > maxHeadersLen || hlen > total-minMessageLen {
		return eventMessage{}, fmt.Errorf("bedrock: invalid event stream frame (length %d, headers %d)", total, hlen)
	}
	rest := make([]byte, total-preludeLen)
	if _, err := io.ReadFull(e.r, rest); err != nil {
		return eventMessage{}, fmt.Errorf("bedrock: read event stream frame: %w", io.ErrUnexpectedEOF)
	}
	body := rest[:len(rest)-messageCRCLen]
	want := binary.BigEndian.Uint32(rest[len(rest)-messageCRCLen:])
	crc := crc32.NewIEEE()
	crc.Write(prelude[:])
	crc.Write(body)
	if crc.Sum32() != want {
		return eventMessage{}, errCRCMismatch
	}
	headers, err := parseEventHeaders(body[:hlen])
	if err != nil {
		return eventMessage{}, err
	}
	return eventMessage{Headers: headers, Payload: body[hlen:]}, nil
}

// parseEventHeaders decodes the header block, keeping string values.
func parseEventHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(b) > 0 {
		nlen := int(b[0])
		b = b[1:]
		if len(b) < nlen+1 {
			return nil, errors.New("bedrock: truncated event stream header")
		}
		name := string(b[:nlen])
		typ := b[nlen]
		b = b[nlen+1:]
		var (
			val string
			n   int
		)
		switch typ {
		case 0, 1: // bool true / false
			n = 0
			if typ == 0 {
				val = "true"
			} else {
				val = "false"
			}
		case 2: // byte
			n = 1
		case 3: // int16
			n = 2
		case 4: // int32
			n = 4
		case 5, 8: // int64, timestamp (ms)
			n = 8
		case 9: // uuid
			n = 16
		case headerTypeBlob, headerTypeStr:
			if len(b) < 2 {
				return nil, errors.New("bedrock: truncated event stream header value")
			}
			l := int(binary.BigEndian.Uint16(b[:2]))
			b = b[2:]
			if len(b) < l {
				return nil, errors.New("bedrock: truncated event stream header value")
			}
			if typ == headerTypeStr {
				val = string(b[:l])
			}
			n = l
		default:
			return nil, fmt.Errorf("bedrock: unknown event stream header type %d", typ)
		}
		if len(b) < n {
			return nil, errors.New("bedrock: truncated event stream header value")
		}
		b = b[n:]
		if typ == headerTypeStr || typ == 0 || typ == 1 {
			out[name] = val
		}
	}
	return out, nil
}
