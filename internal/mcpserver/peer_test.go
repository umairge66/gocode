package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// fakeClient drives a server Conn over a pair of pipes: the test writes framed
// JSON-RPC into the server's input and reads framed JSON-RPC from its output.
type fakeClient struct {
	inW  *io.PipeWriter
	outR *io.PipeReader
	sc   *bufio.Scanner
}

func startServer(t *testing.T, s *Server) (*fakeClient, chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- s.ServeStdio(context.Background(), inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &fakeClient{inW: inW, outR: outR, sc: sc}, done
}

func (fc *fakeClient) write(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := fc.inW.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readMsg reads the next framed message from the server.
func (fc *fakeClient) readMsg(t *testing.T) mcp.Message {
	t.Helper()
	for fc.sc.Scan() {
		line := strings.TrimRight(fc.sc.Text(), "\r")
		if line == "" {
			continue
		}
		msg, err := mcp.DecodeMessage([]byte(line))
		if err != nil {
			t.Fatalf("decode inbound: %v", err)
		}
		return msg
	}
	t.Fatalf("no message from server")
	return mcp.Message{}
}

// TestServerInitiatedRequestRoundTrip: a tool handler issues a server→client
// sampling/createMessage; the fake client answers by id; the handler sees the
// response and the tool result flows back.
func TestServerInitiatedRequestRoundTrip(t *testing.T) {
	s := NewServer(DefaultServerInfo)
	type samplingResult struct {
		Model string `json:"model"`
	}
	s.RegisterTool("ask", mcp.ToolSchema{Type: "object"}, func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
		peer := PeerFromContext(ctx)
		if peer == nil {
			return mcp.CallToolResult{}, errString("no peer in context")
		}
		var out samplingResult
		if err := peer.CreateMessage(ctx, map[string]any{"maxTokens": 16}, &out); err != nil {
			return mcp.CallToolResult{}, err
		}
		return mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "model=" + out.Model}}}, nil
	})

	fc, done := startServer(t, s)

	// initialize advertising sampling capability so CreateMessage is permitted.
	fc.write(t, newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		Capabilities:    mcp.ClientCapabilities{Sampling: json.RawMessage(`{}`)},
	}))
	initMsg := fc.readMsg(t)
	if initMsg.Response == nil || initMsg.Response.Error != nil {
		t.Fatalf("bad initialize response: %+v", initMsg)
	}

	// Fire tools/call; it will block server-side on the sampling request.
	fc.write(t, newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{Name: "ask"}))

	// The server issues sampling/createMessage back to us.
	srvReq := fc.readMsg(t)
	if srvReq.Request == nil || srvReq.Request.Method != MethodSamplingCreateMessage {
		t.Fatalf("expected sampling/createMessage request, got %+v", srvReq)
	}
	// Answer it by id.
	fc.write(t, mcp.Response{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      srvReq.Request.ID,
		Result:  json.RawMessage(`{"model":"test-model"}`),
	})

	// Now the tools/call response arrives.
	callMsg := fc.readMsg(t)
	if callMsg.Response == nil || callMsg.Response.Error != nil {
		t.Fatalf("bad tools/call response: %+v", callMsg)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(callMsg.Response.Result, &res); err != nil {
		t.Fatalf("decode call result: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "model=test-model" {
		t.Errorf("content = %+v, want [model=test-model]", res.Content)
	}

	fc.inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve error: %v", err)
	}
}

// TestServerInitiatedRequestTimeout: no response ever comes; the handler's
// request must return a timeout error without hanging.
func TestServerInitiatedRequestTimeout(t *testing.T) {
	s := NewServer(DefaultServerInfo)
	errCh := make(chan error, 1)
	s.RegisterTool("roots", mcp.ToolSchema{Type: "object"}, func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
		peer := PeerFromContext(ctx)
		cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		err := peer.ListRoots(cctx, nil, nil)
		errCh <- err
		if err != nil {
			return mcp.CallToolResult{}, err
		}
		return mcp.CallToolResult{}, nil
	})

	fc, done := startServer(t, s)
	fc.write(t, newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		Capabilities:    mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}},
	}))
	_ = fc.readMsg(t) // initialize response

	fc.write(t, newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{Name: "roots"}))

	// Server issues roots/list, which we deliberately never answer.
	srvReq := fc.readMsg(t)
	if srvReq.Request == nil || srvReq.Request.Method != MethodRootsList {
		t.Fatalf("expected roots/list request, got %+v", srvReq)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected a timeout/cancel error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("ListRoots hung past the context deadline")
	}

	// Drain the remaining frames (a notifications/cancelled and the tools/call
	// isError result) so the server can finish cleanly.
	for i := 0; i < 2; i++ {
		_ = fc.readMsg(t)
	}
	fc.inW.Close()
	<-done
}

// TestCapabilityGateRefusesWithoutClientSupport: a server→client request whose
// capability the client never advertised is refused before any frame is sent.
func TestCapabilityGateRefusesWithoutClientSupport(t *testing.T) {
	s := NewServer(DefaultServerInfo)
	gateErr := make(chan error, 1)
	s.RegisterTool("sample", mcp.ToolSchema{Type: "object"}, func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
		err := PeerFromContext(ctx).CreateMessage(ctx, map[string]any{}, nil)
		gateErr <- err
		return mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "done"}}}, nil
	})

	fc, done := startServer(t, s)
	// initialize WITHOUT sampling capability.
	fc.write(t, newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{ProtocolVersion: mcp.LatestProtocolVersion}))
	_ = fc.readMsg(t)
	fc.write(t, newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{Name: "sample"}))
	_ = fc.readMsg(t) // tools/call response

	select {
	case err := <-gateErr:
		if err == nil || !strings.Contains(err.Error(), "sampling") {
			t.Fatalf("want capability refusal mentioning sampling, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("CreateMessage did not return")
	}
	fc.inW.Close()
	<-done
}

// TestInboundClassification: a request draws a reply, a notification draws none,
// and the classification holds over one connection.
func TestInboundClassification(t *testing.T) {
	s := echoServer(t)
	var mu sync.Mutex
	notifs := 0
	s.OnNotification(func(_ context.Context, _ mcp.Notification) {
		mu.Lock()
		notifs++
		mu.Unlock()
	})

	fc, done := startServer(t, s)
	fc.write(t, newRequest(t, 1, mcp.MethodPing, nil))                                              // request -> reply
	fc.write(t, mcp.Notification{JSONRPC: mcp.JSONRPCVersion, Method: "notifications/initialized"}) // notification -> no reply
	fc.write(t, newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{Name: "echo", Arguments: json.RawMessage(`{"msg":"hi"}`)}))

	first := fc.readMsg(t)
	if first.Response == nil || strings.TrimSpace(string(first.Response.Result)) != "{}" {
		t.Fatalf("expected ping {} reply first, got %+v", first)
	}
	second := fc.readMsg(t)
	if second.Response == nil {
		t.Fatalf("expected tools/call reply second, got %+v", second)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(second.Response.Result, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Content[0].Text != "echo: hi" {
		t.Errorf("content = %+v", res.Content)
	}

	fc.inW.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if notifs != 1 {
		t.Errorf("notification handler called %d times, want 1", notifs)
	}
}
