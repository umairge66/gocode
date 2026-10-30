package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var fastPolicy = ReconnectPolicy{InitialDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond, GrowFactor: 1.5, MaxRetries: 2}

func TestReconnectPolicyDelays(t *testing.T) {
	p := DefaultReconnectPolicy
	if p.delay(0, 0) != time.Second || p.delay(1, 0) != 1500*time.Millisecond || p.delay(20, 0) != 30*time.Second {
		t.Errorf("delays = %v %v %v", p.delay(0, 0), p.delay(1, 0), p.delay(20, 0))
	}
	if p.delay(3, 250*time.Millisecond) != 250*time.Millisecond {
		t.Error("server retry hint not honoured")
	}
	if p.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d", p.MaxRetries)
	}
}

func jsonResult(t *testing.T, id json.RawMessage, v any) string {
	raw, _ := json.Marshal(v)
	out, _ := json.Marshal(Response{JSONRPC: JSONRPCVersion, ID: id, Result: raw})
	return string(out)
}

type stateLog struct {
	mu sync.Mutex
	ev []ConnEvent
}

func (l *stateLog) add(e ConnEvent) { l.mu.Lock(); l.ev = append(l.ev, e); l.mu.Unlock() }
func (l *stateLog) states() []ConnState {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ConnState
	for _, e := range l.ev {
		out = append(out, e.State)
	}
	return out
}

// startHTTP connects a client over a StreamableHTTP transport and runs
// initialize.
func startHTTP(t *testing.T, url string, log *stateLog) (*Client, *StreamableHTTPTransport) {
	t.Helper()
	tr := NewStreamableHTTPTransport(ServerConfig{Type: TransportHTTP, URL: url}, WithReconnectPolicy(fastPolicy), WithStateListener(log.add))
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return c, tr
}

// TestStreamableHTTPResumeWithLastEventID: the tools/list reply stream sends
// one id-tagged event and drops; the client resumes with GET + Last-Event-ID
// and receives the response on the resumed stream.
func TestStreamableHTTPResumeWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	var lastEventIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			lastEventIDs = append(lastEventIDs, r.Header.Get("Last-Event-ID"))
			n := len(lastEventIDs)
			mu.Unlock()
			if n == 1 {
				// First resume attempt fails: exercises backoff.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			id := json.RawMessage(`2`)
			fmt.Fprintf(w, "id: e2\ndata: %s\n\n", jsonResult(t, id, ListToolsResult{Tools: []Tool{{Name: "resumed", InputSchema: ToolSchema{Type: "object"}}}}))
			return
		}
		req := decodeRequest(t, r)
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case MethodInitialize:
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonResult(t, req.ID, InitializeResult{ProtocolVersion: LatestProtocolVersion, ServerInfo: Implementation{Name: "x", Version: "1"}}))
		case MethodToolsList:
			w.Header().Set("Content-Type", "text/event-stream")
			// A progress notification tagged e1, then the connection drops.
			fmt.Fprintf(w, "retry: 5\nid: e1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	log := &stateLog{}
	c, _ := startHTTP(t, srv.URL, log)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "resumed" {
		t.Errorf("tools = %+v", res.Tools)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastEventIDs) != 2 || lastEventIDs[0] != "e1" || lastEventIDs[1] != "e1" {
		t.Errorf("Last-Event-ID on resume = %v, want [e1 e1]", lastEventIDs)
	}
	st := log.states()
	if len(st) < 3 || st[0] != ConnReconnecting || st[len(st)-1] != ConnConnected {
		t.Errorf("state events = %v", st)
	}
}

// TestStreamableHTTPInFlightFailsAfterMaxReconnects: resumption keeps
// failing; the request fails cleanly with the give-up message and the
// transport closes (so a host would reconnect).
func TestStreamableHTTPInFlightFailsAfterMaxReconnects(t *testing.T) {
	var gets int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req := decodeRequest(t, r)
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == MethodInitialize {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonResult(t, req.ID, InitializeResult{ProtocolVersion: LatestProtocolVersion}))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: only\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
	}))
	defer srv.Close()
	log := &stateLog{}
	c, _ := startHTTP(t, srv.URL, log)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.CallTool(ctx, CallToolParams{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "Maximum reconnection attempts (2) exceeded.") {
		t.Fatalf("err = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("request hung")
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not close after giving up")
	}
	mu.Lock()
	if gets != 2 {
		t.Errorf("resume attempts = %d, want 2", gets)
	}
	mu.Unlock()
	if st := log.states(); st[len(st)-1] != ConnDisconnected {
		t.Errorf("states = %v", st)
	}
}

// TestStreamableHTTPUnresumableStreamFailsFast: no event ids -> no resume.
func TestStreamableHTTPUnresumableStreamFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == MethodInitialize {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonResult(t, req.ID, InitializeResult{ProtocolVersion: LatestProtocolVersion}))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keepalive\n\n")
	}))
	defer srv.Close()
	c, _ := startHTTP(t, srv.URL, &stateLog{})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.CallTool(ctx, CallToolParams{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "cannot be resumed") {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamableHTTPSessionLostReinitializes: the server forgets the
// session (404); the transport replays initialize, adopts the new session id
// and retries the request once.
func TestStreamableHTTPSessionLostReinitializes(t *testing.T) {
	var mu sync.Mutex
	live := map[string]bool{}
	seq, inits := 0, 0
	var listSIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		sid := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		defer mu.Unlock()
		if req != nil && req.Method == MethodInitialize {
			inits++
			seq++
			ns := fmt.Sprintf("s%d", seq)
			live[ns] = true
			w.Header().Set("Mcp-Session-Id", ns)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonResult(t, req.ID, InitializeResult{ProtocolVersion: LatestProtocolVersion}))
			return
		}
		if !live[sid] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if req == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		listSIDs = append(listSIDs, sid)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonResult(t, req.ID, ListToolsResult{Tools: []Tool{{Name: "t", InputSchema: ToolSchema{Type: "object"}}}}))
	}))
	defer srv.Close()
	c, tr := startHTTP(t, srv.URL, &stateLog{})
	defer c.Close()
	if tr.SessionID() != "s1" {
		t.Fatalf("session = %q", tr.SessionID())
	}
	// Server restart: every session forgotten.
	mu.Lock()
	live = map[string]bool{}
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools after session loss: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Errorf("tools = %v", res.Tools)
	}
	mu.Lock()
	defer mu.Unlock()
	if inits != 2 || tr.SessionID() != "s2" || len(listSIDs) != 1 || listSIDs[0] != "s2" {
		t.Errorf("inits=%d session=%q listSIDs=%v", inits, tr.SessionID(), listSIDs)
	}
}

// TestSSEReconnectWithLastEventID: the legacy SSE stream drops; the client
// re-opens it with Last-Event-ID and keeps working on the same session.
func TestSSEReconnectWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	var streams []string // Last-Event-ID per GET
	push := make(chan string, 16)
	drop := make(chan struct{}, 1)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sse":
			mu.Lock()
			streams = append(streams, r.Header.Get("Last-Event-ID"))
			n := len(streams)
			mu.Unlock()
			if n == 2 {
				w.WriteHeader(http.StatusServiceUnavailable) // first retry fails
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=abc\n\n")
			fl.Flush()
			evID := n * 100
			for {
				select {
				case m := <-push:
					evID++
					fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", evID, m)
					fl.Flush()
				case <-drop:
					return
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/messages":
			req := decodeRequest(t, r)
			w.WriteHeader(http.StatusAccepted)
			if req == nil {
				return
			}
			var result any = ListToolsResult{Tools: []Tool{{Name: "t", InputSchema: ToolSchema{Type: "object"}}}}
			if req.Method == MethodInitialize {
				result = InitializeResult{ProtocolVersion: LatestProtocolVersion}
			}
			push <- jsonResult(t, req.ID, result)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	log := &stateLog{}
	tr := NewSSETransport(ServerConfig{Type: TransportSSE, URL: srv.URL + "/sse"}, WithReconnectPolicy(fastPolicy), WithStateListener(log.add))
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	drop <- struct{}{} // server drops the stream

	// Wait until the stream is back (reconnected event) then use it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := log.states()
		if len(st) > 0 && st[len(st)-1] == ConnConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reconnected; states = %v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools after reconnect: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(streams) != 3 || streams[0] != "" || streams[1] != "101" || streams[2] != "101" {
		t.Errorf("Last-Event-ID per stream = %v, want [\"\" 101 101]", streams)
	}
}

// TestSSEGivesUpAndFailsInFlight: the stream drops for good; the pending
// request fails and Messages() closes.
func TestSSEGivesUpAndFailsInFlight(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			n := gets
			mu.Unlock()
			if n > 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: endpoint\ndata: /messages\n\n")
			w.(http.Flusher).Flush()
			<-release // drop when told
			return
		}
		w.WriteHeader(http.StatusAccepted) // never answers on the stream
	}))
	defer srv.Close()
	tr := NewSSETransport(ServerConfig{Type: TransportSSE, URL: srv.URL + "/sse"}, WithReconnectPolicy(fastPolicy))
	c := NewClient(tr, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	errCh := make(chan error, 1)
	go func() { _, err := c.ListTools(ctx, nil); errCh <- err }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "Maximum reconnection attempts (2) exceeded.") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request hung")
	}
	<-c.Done()
}
