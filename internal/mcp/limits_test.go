package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// endless yields an unending stream of the given chunk.
type endless struct{ chunk []byte }

func (e endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		n += copy(p[n:], e.chunk)
	}
	return n, nil
}

// TestSSEEventIsBounded: a server that sends one SSE event made of endless
// "data:" lines must not grow the client's buffer without bound.
func TestSSEEventIsBounded(t *testing.T) {
	r := io.LimitReader(endless{[]byte("data: " + strings.Repeat("x", 1000) + "\n")}, 2*maxInboundMessage)
	err := parseSSE(r, func(sseEvent) bool { return true })
	if !errors.Is(err, errInboundTooBig) {
		t.Fatalf("parseSSE over the cap = %v, want errInboundTooBig", err)
	}
}

// TestStreamableHTTPJSONReplyIsBounded: an application/json reply body that
// never ends is cut at the cap instead of being read into memory whole.
func TestStreamableHTTPJSONReplyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req Request
		if json.Unmarshal(body, &req) != nil || req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == MethodInitialize {
			res, _ := json.Marshal(InitializeResult{ProtocolVersion: LatestProtocolVersion})
			_ = json.NewEncoder(w).Encode(Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: res})
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"tools":[],"pad":"`))
		_, _ = io.Copy(w, io.LimitReader(endless{bytes.Repeat([]byte("A"), 4096)}, 3*maxInboundMessage))
	}))
	defer srv.Close()

	tr := NewStreamableHTTPTransport(ServerConfig{Type: TransportHTTP, URL: srv.URL})
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ListTools(ctx, nil); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("ListTools with an oversized reply = %v, want the size-cap error", err)
	}
}
