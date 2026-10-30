package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// -----------------------------------------------------------------------------
// PTY-host wire framing.
//
// The pty host (`gocode --bg-pty-host`, ptyhost.go) and its clients (the
// daemon's job handle, ptyclient.go) speak a tiny length-prefixed protocol over
// the per-job pty socket: every frame is a 4-byte big-endian payload length, a
// 1-byte kind, then the payload. Kind 0 carries raw terminal bytes; kind 1
// carries one JSON control object ({"t":"hello"|"live"|"ping"|"pong"|"exit"|
// "resize"|"kill"|"auth"|"auth-required", ...}).

const (
	// FrameKindData tags a raw terminal-bytes frame.
	FrameKindData byte = 0
	// FrameKindCtrl tags a JSON control frame.
	FrameKindCtrl byte = 1
	// FrameHeaderLen is the 4-byte length + 1-byte kind header.
	FrameHeaderLen = 5
	// MaxFrameBytes caps one frame's payload.
	MaxFrameBytes = 1 << 20
	// PTYRingBytes is the replay ring kept by the pty host and by the daemon's
	// job handle.
	PTYRingBytes = 262144
	// MaxPTYDimension bounds a resize request.
	MaxPTYDimension = 10000
)

// Control-frame types (the "t" field).
const (
	CtrlHello        = "hello"
	CtrlLive         = "live"
	CtrlPing         = "ping"
	CtrlPong         = "pong"
	CtrlExit         = "exit"
	CtrlResize       = "resize"
	CtrlKill         = "kill"
	CtrlAuth         = "auth"
	CtrlAuthRequired = "auth-required"
)

// Ctrl is one control frame. Only the fields relevant to T are set.
type Ctrl struct {
	T string `json:"t"`
	// hello
	ReplPID int    `json:"replPid,omitempty"`
	Version string `json:"version,omitempty"`
	// exit (code is always sent; signal only when the child was signalled)
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
	// resize
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// kill
	Sig string `json:"sig,omitempty"`
	// auth
	Token string `json:"token,omitempty"`
}

// EncodeDataFrame wraps raw terminal bytes in a data frame.
func EncodeDataFrame(p []byte) []byte {
	out := make([]byte, FrameHeaderLen+len(p))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(p)))
	out[4] = FrameKindData
	copy(out[FrameHeaderLen:], p)
	return out
}

// EncodeCtrlFrame wraps a control object in a ctrl frame.
func EncodeCtrlFrame(c Ctrl) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		// Ctrl has only plain fields; Marshal cannot fail.
		b = []byte(`{}`)
	}
	out := make([]byte, FrameHeaderLen+len(b))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(b)))
	out[4] = FrameKindCtrl
	copy(out[FrameHeaderLen:], b)
	return out
}

// FrameDecoder is the streaming decoder for the pty socket. Feed may be called
// with arbitrary chunk boundaries. After the first protocol error the decoder
// is poisoned and ignores further input (the caller destroys the connection).
type FrameDecoder struct {
	OnData  func([]byte)
	OnCtrl  func(Ctrl)
	OnError func(string)

	buf    []byte
	broken bool
}

// Feed consumes a chunk of socket bytes.
func (d *FrameDecoder) Feed(chunk []byte) {
	if d.broken {
		return
	}
	d.buf = append(d.buf, chunk...)
	for len(d.buf) >= FrameHeaderLen {
		n := binary.BigEndian.Uint32(d.buf[0:4])
		if n > MaxFrameBytes {
			d.fail(fmt.Sprintf("frame too large (%d > %d)", n, MaxFrameBytes))
			return
		}
		total := FrameHeaderLen + int(n)
		if len(d.buf) < total {
			return
		}
		kind := d.buf[4]
		payload := make([]byte, n)
		copy(payload, d.buf[FrameHeaderLen:total])
		d.buf = d.buf[total:]
		switch kind {
		case FrameKindData:
			if d.OnData != nil {
				d.OnData(payload)
			}
		case FrameKindCtrl:
			var c Ctrl
			if err := json.Unmarshal(payload, &c); err != nil {
				d.fail("bad ctrl json")
				return
			}
			if d.OnCtrl != nil {
				d.OnCtrl(c)
			}
		default:
			d.fail(fmt.Sprintf("unknown frame kind %d", kind))
			return
		}
	}
	if len(d.buf) == 0 {
		d.buf = nil
	}
}

func (d *FrameDecoder) fail(msg string) {
	d.broken = true
	d.buf = nil
	if d.OnError != nil {
		d.OnError(msg)
	}
}

// ByteRing is the bounded replay ring of terminal chunks. Pushing past the
// byte budget drops whole chunks from the front (always keeping the newest
// one), and after a drop trims up to three leading UTF-8 continuation bytes so
// a replay never starts mid-character.
type ByteRing struct {
	max    int
	chunks [][]byte
	size   int
}

// NewByteRing returns a ring capped at max bytes.
func NewByteRing(max int) *ByteRing { return &ByteRing{max: max} }

// Push appends one chunk.
func (r *ByteRing) Push(p []byte) {
	c := make([]byte, len(p))
	copy(c, p)
	r.chunks = append(r.chunks, c)
	r.size += len(c)
	dropped := false
	for r.size > r.max && len(r.chunks) > 1 {
		r.size -= len(r.chunks[0])
		r.chunks = r.chunks[1:]
		dropped = true
	}
	if dropped {
		// Trim at most 3 continuation bytes across the new head chunk(s).
		trimmed := 0
		for trimmed < 3 && len(r.chunks) > 0 {
			h := r.chunks[0]
			k := 0
			for trimmed+k < 3 && k < len(h) && h[k]&0xC0 == 0x80 {
				k++
			}
			if k == 0 {
				break
			}
			r.chunks[0] = h[k:]
			r.size -= k
			trimmed += k
			if len(r.chunks[0]) > 0 || len(r.chunks) == 1 {
				break
			}
			r.chunks = r.chunks[1:]
		}
	}
}

// Chunks returns the buffered chunks oldest-first (shared slices; do not
// mutate).
func (r *ByteRing) Chunks() [][]byte { return r.chunks }

// Bytes returns the concatenated buffer.
func (r *ByteRing) Bytes() []byte {
	out := make([]byte, 0, r.size)
	for _, c := range r.chunks {
		out = append(out, c...)
	}
	return out
}
