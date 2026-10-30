package mcp

// WebSocket transport (server config type "ws").
//
// Implementation note: this is a minimal RFC 6455 client written against the
// standard library. go.mod carries golang.org/x/net only as an indirect
// dependency and its websocket package lacks proper close-handshake and
// message-size control, so rather than promote a dependency this speaks
// the small subset MCP needs: the HTTP/1.1 Upgrade handshake (with the "mcp"
// subprotocol and the config headers), masked client frames, text/binary
// messages with continuation frames, ping -> pong, and the close handshake.
// Extensions (permessage-deflate) are never negotiated. Proxies are not used
// for ws:// / wss://.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket opcodes (RFC 6455 §5.2).
const (
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

// Close status codes used here.
const (
	wsCloseNormal        = 1000
	wsCloseProtocolError = 1002
	wsCloseTooBig        = 1009
)

// wsMaxMessage caps one inbound message.
const wsMaxMessage = 16 << 20

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsFrame is one decoded frame.
type wsFrame struct {
	fin     bool
	op      byte
	masked  bool
	payload []byte
}

var errWSTooBig = fmt.Errorf("MCP server sent a WebSocket message over %d MiB. gocode did not read it and closed the connection.", wsMaxMessage>>20)

// readWSFrame reads one frame, unmasking it when the mask bit is set. limit
// bounds the payload length.
func readWSFrame(r io.Reader, limit int) (wsFrame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return wsFrame{}, err
	}
	f := wsFrame{fin: hdr[0]&0x80 != 0, op: hdr[0] & 0x0F, masked: hdr[1]&0x80 != 0}
	if hdr[0]&0x70 != 0 {
		return f, errors.New("mcp: websocket: reserved bits set (no extension was negotiated)")
	}
	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if f.op >= 0x8 && (n > 125 || !f.fin) {
		return f, errors.New("mcp: websocket: invalid control frame")
	}
	if n > uint64(limit) {
		return f, errWSTooBig
	}
	var key [4]byte
	if f.masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return f, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return f, err
	}
	if f.masked {
		for i := range f.payload {
			f.payload[i] ^= key[i%4]
		}
	}
	return f, nil
}

// writeWSFrame writes one final frame; clients must mask (RFC 6455 §5.3).
func writeWSFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	buf := make([]byte, 0, 14+len(payload))
	buf = append(buf, 0x80|op)
	var mbit byte
	if mask {
		mbit = 0x80
	}
	switch n := len(payload); {
	case n <= 125:
		buf = append(buf, mbit|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, mbit|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, mbit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		buf = append(buf, key[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := range payload {
			buf[start+i] ^= key[i%4]
		}
	} else {
		buf = append(buf, payload...)
	}
	_, err := w.Write(buf)
	return err
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// WebSocketTransport speaks JSON-RPC as WebSocket text messages.
type WebSocketTransport struct {
	stateNotifier
	cfg  ServerConfig
	seam httpSeam

	mu        sync.Mutex
	conn      net.Conn
	br        *bufio.Reader
	started   bool
	closing   bool
	sentClose bool
	msgs      chan Message
	done      chan struct{} // closed when the read loop has exited
	closeOnce sync.Once

	wmu sync.Mutex // serializes frame writes
}

// NewWebSocketTransport builds (but does not start) a WebSocket transport.
func NewWebSocketTransport(cfg ServerConfig, opts ...HTTPOption) *WebSocketTransport {
	t := &WebSocketTransport{cfg: cfg, seam: newHTTPSeam(opts), msgs: make(chan Message), done: make(chan struct{})}
	t.SetStateListener(t.seam.listener)
	return t
}

// Start dials, performs the Upgrade handshake and starts the read loop.
func (t *WebSocketTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("Start can only be called once per transport.")
	}
	t.started = true
	t.mu.Unlock()

	u, err := url.Parse(t.cfg.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mcp: invalid WebSocket url %q", t.cfg.URL)
	}
	secure := false
	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return fmt.Errorf("mcp: WebSocket url must be ws:// or wss:// (got %s://)", u.Scheme)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("mcp: WebSocket connect: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if secure {
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("mcp: WebSocket TLS handshake: %w", err)
		}
		conn = tc
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	hsURL := *u
	hsURL.Scheme = "http"
	if secure {
		hsURL.Scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hsURL.String(), nil)
	if err != nil {
		conn.Close()
		return err
	}
	if err := t.seam.applyHeaders(ctx, t.cfg, req); err != nil {
		conn.Close()
		return err
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Protocol", "mcp")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return fmt.Errorf("mcp: WebSocket handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mcp: WebSocket handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return fmt.Errorf("mcp: WebSocket handshake failed: %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(resp.Header, "Connection", "upgrade") ||
		resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		conn.Close()
		return errors.New("mcp: WebSocket handshake failed: invalid upgrade response")
	}
	if p := resp.Header.Get("Sec-WebSocket-Protocol"); p != "" && p != "mcp" {
		conn.Close()
		return fmt.Errorf("mcp: WebSocket server selected unexpected subprotocol %q", p)
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		conn.Close()
		return errors.New("mcp: WebSocket server negotiated an extension that was not offered")
	}
	_ = conn.SetDeadline(time.Time{})

	t.mu.Lock()
	t.conn, t.br = conn, br
	closing := t.closing
	t.mu.Unlock()
	if closing {
		conn.Close()
		close(t.msgs)
		close(t.done)
		return errors.New("WebSocket is not open. Cannot start transport.")
	}
	go t.readLoop()
	return nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (t *WebSocketTransport) writeFrame(op byte, payload []byte) error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return errors.New("WebSocket is not open. Cannot send message.")
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})
	return writeWSFrame(conn, op, payload, true)
}

func closePayload(code int, reason string) []byte {
	b := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(b, uint16(code))
	return append(b, reason...)
}

// sendClose sends our close frame once.
func (t *WebSocketTransport) sendClose(code int, reason string) {
	t.mu.Lock()
	if t.sentClose {
		t.mu.Unlock()
		return
	}
	t.sentClose = true
	t.mu.Unlock()
	_ = t.writeFrame(wsOpClose, closePayload(code, reason))
}

func (t *WebSocketTransport) readLoop() {
	defer close(t.done)
	defer close(t.msgs)
	defer func() {
		t.mu.Lock()
		conn, closing := t.conn, t.closing
		t.mu.Unlock()
		_ = conn.Close()
		if !closing {
			t.emit(ConnEvent{State: ConnDisconnected, Err: errors.New("WebSocket closed")})
		}
	}()
	var msg []byte
	inMsg := false
	for {
		f, err := readWSFrame(t.br, wsMaxMessage)
		if err != nil {
			if errors.Is(err, errWSTooBig) {
				t.sendClose(wsCloseTooBig, "message too big")
			}
			return
		}
		switch f.op {
		case wsOpPing:
			_ = t.writeFrame(wsOpPong, f.payload)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			// Echo the peer's status code (RFC 6455 §5.5.1) unless we
			// started the close.
			code := wsCloseNormal
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload))
			}
			t.sendClose(code, "")
			return
		case wsOpText, wsOpBinary:
			if inMsg {
				t.sendClose(wsCloseProtocolError, "unexpected data frame")
				return
			}
			msg = append(msg[:0], f.payload...)
			inMsg = true
		case wsOpContinuation:
			if !inMsg {
				t.sendClose(wsCloseProtocolError, "unexpected continuation")
				return
			}
			if len(msg)+len(f.payload) > wsMaxMessage {
				t.sendClose(wsCloseTooBig, "message too big")
				return
			}
			msg = append(msg, f.payload...)
		default:
			t.sendClose(wsCloseProtocolError, "unknown opcode")
			return
		}
		if !f.fin {
			continue
		}
		inMsg = false
		m, err := DecodeMessage(msg)
		if err != nil {
			continue // unparseable message: skip, like the stdio transport
		}
		t.msgs <- m
	}
}

// Send writes one JSON-RPC message as a text frame.
func (t *WebSocketTransport) Send(ctx context.Context, msg any) error {
	t.mu.Lock()
	open := t.conn != nil && !t.closing && !t.sentClose
	t.mu.Unlock()
	if !open {
		return errors.New("WebSocket is not open. Cannot send message.")
	}
	select {
	case <-t.done:
		return errors.New("WebSocket is not open. Cannot send message.")
	default:
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return t.writeFrame(wsOpText, data)
}

// Messages returns the inbound message channel.
func (t *WebSocketTransport) Messages() <-chan Message { return t.msgs }

// Close performs the close handshake (waiting briefly for the server's echo)
// and releases the connection.
func (t *WebSocketTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		conn := t.conn
		t.mu.Unlock()
		if conn == nil {
			// Never started (or Start still running: it sees closing).
			t.mu.Lock()
			started := t.started
			t.mu.Unlock()
			if !started {
				close(t.msgs)
				close(t.done)
			}
			return
		}
		t.sendClose(wsCloseNormal, "")
		select {
		case <-t.done:
		case <-time.After(time.Second):
			_ = conn.Close()
			// Unblock a read loop stuck delivering to an absent reader.
			go func() {
				for range t.msgs {
				}
			}()
			<-t.done
		}
	})
	return nil
}
