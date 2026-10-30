package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeRequest reads a JSON-RPC request off an HTTP request body.
func decodeRequest(t *testing.T, r *http.Request) *Request {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	msg, err := DecodeMessage(body)
	if err != nil {
		t.Fatalf("decode request %q: %v", body, err)
	}
	if msg.Request == nil {
		return nil
	}
	return msg.Request
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestStreamableHTTPInitializeAndListTools drives a full
// initialize -> notifications/initialized -> tools/list exchange against an
// httptest server that answers every POST with a single JSON body (the
// "application/json" reply branch), and verifies Mcp-Session-Id handling: the
// server assigns a session id on the initialize reply and the client echoes it
// on every later request.
func TestStreamableHTTPInitializeAndListTools(t *testing.T) {
	const sessionID = "sess-abc-123"
	var sawInitialized bool
	var listToolsSessionID string
	var initHadSession bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Accept must offer both json and event-stream.
		if ac := r.Header.Get("Accept"); !strings.Contains(ac, "application/json") || !strings.Contains(ac, "text/event-stream") {
			t.Errorf("Accept header = %q, want both application/json and text/event-stream", ac)
		}
		req := decodeRequest(t, r)
		if req == nil {
			// A notification (e.g. notifications/initialized): 202 Accepted, no body.
			if r.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("notification session id = %q, want %q", r.Header.Get("Mcp-Session-Id"), sessionID)
			}
			sawInitialized = true
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		switch req.Method {
		case MethodInitialize:
			initHadSession = r.Header.Get("Mcp-Session-Id") != ""
			w.Header().Set("Mcp-Session-Id", sessionID)
			result = InitializeResult{
				ProtocolVersion: LatestProtocolVersion,
				Capabilities:    ServerCapabilities{Tools: &ListChangedCapability{ListChanged: true}},
				ServerInfo:      Implementation{Name: "httpfake", Version: "9.9.9"},
				Instructions:    "stream well",
			}
		case MethodToolsList:
			listToolsSessionID = r.Header.Get("Mcp-Session-Id")
			ro := true
			result = ListToolsResult{Tools: []Tool{{
				Name:        "ping",
				Description: "pong",
				InputSchema: ToolSchema{Type: "object"},
				Annotations: &ToolAnnotations{ReadOnlyHint: &ro},
			}}}
		default:
			w.Header().Set("Content-Type", "application/json")
			resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID, Error: &RPCError{Code: ErrMethodNotFound, Message: "no"}}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tr := NewStreamableHTTPTransport(ServerConfig{Type: TransportHTTP, URL: srv.URL})
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if init.ServerInfo.Name != "httpfake" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}
	if initHadSession {
		t.Error("initialize request must not carry a session id")
	}
	if got := tr.SessionID(); got != sessionID {
		t.Errorf("session id after initialize = %q, want %q", got, sessionID)
	}

	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	if listToolsSessionID != sessionID {
		t.Errorf("tools/list carried session id %q, want %q", listToolsSessionID, sessionID)
	}
	if !sawInitialized {
		t.Error("server never received notifications/initialized (202 branch)")
	}
}

// TestStreamableHTTPEventStreamReply verifies the "text/event-stream" reply
// branch: a POSTed request whose answer is delivered as an SSE frame rather than
// a JSON body.
func TestStreamableHTTPEventStreamReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := ListToolsResult{Tools: []Tool{{Name: "streamed", InputSchema: ToolSchema{Type: "object"}}}}
		resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", mustJSON(t, resp))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	tr := NewStreamableHTTPTransport(ServerConfig{Type: TransportHTTP, URL: srv.URL})
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools over event-stream: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "streamed" {
		t.Fatalf("tools = %+v", list.Tools)
	}
}

// TestStreamableHTTPHeaderHook verifies the auth seam: a HeaderHook injects an
// Authorization header onto every request.
func TestStreamableHTTPHeaderHook(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		req := decodeRequest(t, r)
		if req == nil {
			// notifications/initialized: 202, no body.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: mustJSON(t, InitializeResult{
			ProtocolVersion: LatestProtocolVersion,
			ServerInfo:      Implementation{Name: "x", Version: "1"},
		})}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tr := NewStreamableHTTPTransport(ServerConfig{Type: TransportHTTP, URL: srv.URL},
		WithHeaderHook(func(ctx context.Context, h http.Header) error {
			h.Set("Authorization", "Bearer tok-42")
			return nil
		}))
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if gotAuth != "Bearer tok-42" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-42")
	}
}

// TestSSETransportHandshake drives the SSE transport against an httptest server
// that emits an "endpoint" event on the GET stream, accepts POSTed messages at
// the advertised endpoint, and pushes JSON-RPC replies back as "message" events
// on the GET stream. It exercises the endpoint handshake plus an
// initialize -> tools/list round-trip.
func TestSSETransportHandshake(t *testing.T) {
	type sseConn struct {
		w http.ResponseWriter
		f http.Flusher
	}
	replies := make(chan *Response, 8)

	mux := http.NewServeMux()
	// GET event-stream: advertise the POST endpoint, then relay replies.
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("SSE GET Accept = %q", r.Header.Get("Accept"))
		}
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not a Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Advertise a relative endpoint, resolved against the stream URL.
		fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
		f.Flush()
		_ = sseConn{w, f}
		for {
			select {
			case <-r.Context().Done():
				return
			case resp := <-replies:
				b, _ := json.Marshal(resp)
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				f.Flush()
			}
		}
	})
	// POST endpoint: accept a message, craft a reply, relay via the GET stream.
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case MethodInitialize:
			result = InitializeResult{
				ProtocolVersion: LatestProtocolVersion,
				ServerInfo:      Implementation{Name: "ssefake", Version: "2.0.0"},
			}
		case MethodToolsList:
			result = ListToolsResult{Tools: []Tool{{Name: "sse-tool", InputSchema: ToolSchema{Type: "object"}}}}
		}
		replies <- &Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: mustJSON(t, result)}
		w.WriteHeader(http.StatusAccepted)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := NewSSETransport(ServerConfig{Type: TransportSSE, URL: srv.URL + "/sse"})
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect (SSE handshake): %v", err)
	}
	defer c.Close()

	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize over SSE: %v", err)
	}
	if init.ServerInfo.Name != "ssefake" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}

	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "sse-tool" {
		t.Fatalf("tools = %+v", list.Tools)
	}
}

// TestSSEEndpointOriginMismatch verifies the SSE handshake rejects a
// cross-origin endpoint advertisement.
func TestSSEEndpointOriginMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: endpoint\ndata: http://evil.example.com/messages\n\n")
		f.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	tr := NewSSETransport(ServerConfig{Type: TransportSSE, URL: srv.URL + "/sse"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := tr.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to reject a cross-origin endpoint")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("error = %v, want an origin-mismatch error", err)
	}
	_ = tr.Close()
}
