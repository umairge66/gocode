package mcp

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsTestServer is an in-process MCP-over-WebSocket server built on the same
// frame codec (server side: unmasked writes, requires masked client frames).
type wsTestServer struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	gotHeaders  http.Header
	pongPayload string
	unmasked    bool
	closeCode   int
	closed      chan struct{}
}

func newWSTestServer(t *testing.T) *wsTestServer {
	s := &wsTestServer{t: t, closed: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *wsTestServer) url() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/mcp" }

func (s *wsTestServer) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "not a websocket upgrade", 400)
		return
	}
	s.mu.Lock()
	s.gotHeaders = r.Header.Clone()
	s.mu.Unlock()
	hj, _ := w.(http.Hijacker)
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	key := r.Header.Get("Sec-WebSocket-Key")
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\nSec-WebSocket-Protocol: mcp\r\n\r\n")
	_ = brw.Flush()
	s.serve(conn, brw.Reader)
}

func (s *wsTestServer) serve(conn net.Conn, br *bufio.Reader) {
	defer close(s.closed)
	// Ping first: the client must answer with a pong carrying the payload.
	_ = writeWSFrame(conn, wsOpPing, []byte("hb-1"), false)
	for {
		f, err := readWSFrame(br, wsMaxMessage)
		if err != nil {
			return
		}
		if !f.masked {
			s.mu.Lock()
			s.unmasked = true
			s.mu.Unlock()
		}
		switch f.op {
		case wsOpPong:
			s.mu.Lock()
			s.pongPayload = string(f.payload)
			s.mu.Unlock()
		case wsOpClose:
			code := 0
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload))
			}
			s.mu.Lock()
			s.closeCode = code
			s.mu.Unlock()
			_ = writeWSFrame(conn, wsOpClose, f.payload[:2], false)
			return
		case wsOpText:
			msg, err := DecodeMessage(f.payload)
			if err != nil || msg.Request == nil {
				continue
			}
			var result any
			switch msg.Request.Method {
			case MethodInitialize:
				result = InitializeResult{ProtocolVersion: LatestProtocolVersion,
					Capabilities: ServerCapabilities{Tools: &ListChangedCapability{}},
					ServerInfo:   Implementation{Name: "ws-fixture", Version: "1"}}
			case MethodToolsList:
				result = ListToolsResult{Tools: []Tool{{Name: "a", InputSchema: ToolSchema{Type: "object"}}, {Name: "b", InputSchema: ToolSchema{Type: "object"}}}}
			}
			raw, _ := json.Marshal(result)
			out, _ := json.Marshal(Response{JSONRPC: JSONRPCVersion, ID: msg.Request.ID, Result: raw})
			// Send the reply fragmented to exercise continuation frames.
			half := len(out) / 2
			first := append([]byte{wsOpText}, out[:half]...) // op + payload
			_ = writeFragment(conn, first[0], first[1:], false)
			_ = writeFragment(conn, wsOpContinuation, out[half:], true)
		}
	}
}

// writeFragment writes an unmasked frame with an explicit FIN bit.
func writeFragment(conn net.Conn, op byte, payload []byte, fin bool) error {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	switch n := len(payload); {
	case n <= 125:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_, err := conn.Write(append(hdr, payload...))
	return err
}

func TestWebSocketRoundTrip(t *testing.T) {
	s := newWSTestServer(t)
	defer s.srv.Close()

	tr, err := NewTransport(ServerConfig{Type: TransportWS, URL: s.url(), Headers: map[string]string{"X-Api-Key": "k1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.(*WebSocketTransport); !ok {
		t.Fatalf("ws config built %T", tr)
	}
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if init.ServerInfo.Name != "ws-fixture" {
		t.Errorf("server = %+v", init.ServerInfo)
	}
	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 2 {
		t.Errorf("tools = %v", tools.Tools)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server never saw the close handshake")
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client read loop did not end after close")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gotHeaders.Get("Sec-WebSocket-Protocol") != "mcp" || s.gotHeaders.Get("X-Api-Key") != "k1" || s.gotHeaders.Get("User-Agent") == "" {
		t.Errorf("handshake headers = %v", s.gotHeaders)
	}
	if s.pongPayload != "hb-1" {
		t.Errorf("pong payload = %q, want hb-1", s.pongPayload)
	}
	if s.unmasked {
		t.Error("client sent an unmasked frame")
	}
	if s.closeCode != wsCloseNormal {
		t.Errorf("close code = %d, want 1000", s.closeCode)
	}
	if err := tr.Send(context.Background(), map[string]any{"jsonrpc": "2.0", "method": "x"}); err == nil {
		t.Error("Send after close succeeded")
	}
}

func TestWebSocketServerDropFailsInFlight(t *testing.T) {
	// A server that accepts the upgrade, reads one request, and vanishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, _ := w.(http.Hijacker).Hijack()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			wsAccept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
		_ = brw.Flush()
		_, _ = readWSFrame(brw.Reader, wsMaxMessage)
		conn.Close()
	}))
	defer srv.Close()
	tr := NewWebSocketTransport(ServerConfig{Type: TransportWS, URL: "ws" + strings.TrimPrefix(srv.URL, "http")})
	var events []ConnState
	var mu sync.Mutex
	tr.SetStateListener(func(ev ConnEvent) { mu.Lock(); events = append(events, ev.State); mu.Unlock() })
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.Initialize(ctx)
	if err == nil {
		t.Fatal("expected a connection-closed error")
	}
	if ctx.Err() != nil {
		t.Fatal("request hung until the deadline instead of failing")
	}
	<-c.Done()
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || events[len(events)-1] != ConnDisconnected {
		t.Errorf("state events = %v, want ... disconnected", events)
	}
}

func TestWebSocketRejectsBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
		_ = brw.Flush()
	}))
	defer srv.Close()
	tr := NewWebSocketTransport(ServerConfig{Type: TransportWS, URL: "ws" + strings.TrimPrefix(srv.URL, "http")})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tr.Start(ctx); err == nil || !strings.Contains(err.Error(), "invalid upgrade response") {
		t.Fatalf("Start err = %v", err)
	}
}

func TestWSFrameCodec(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 1000, 70000} {
		var buf strings.Builder
		payload := []byte(strings.Repeat("x", n))
		if err := writeWSFrame(&buf, wsOpText, payload, true); err != nil {
			t.Fatal(err)
		}
		f, err := readWSFrame(strings.NewReader(buf.String()), wsMaxMessage)
		if err != nil || !f.fin || !f.masked || f.op != wsOpText || string(f.payload) != string(payload) {
			t.Fatalf("n=%d: frame %+v err %v", n, f.op, err)
		}
	}
	var buf strings.Builder
	_ = writeWSFrame(&buf, wsOpText, make([]byte, 200), false)
	if _, err := readWSFrame(strings.NewReader(buf.String()), 100); err != errWSTooBig {
		t.Errorf("over-limit frame err = %v", err)
	}
}
