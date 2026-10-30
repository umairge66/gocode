package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

// newRequest builds a framed-ready JSON-RPC request with an integer id.
func newRequest(t *testing.T, id int, method string, params any) mcp.Request {
	t.Helper()
	idJSON, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal id: %v", err)
	}
	req := mcp.Request{JSONRPC: mcp.JSONRPCVersion, ID: idJSON, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		req.Params = p
	}
	return req
}

// echoServer registers a single "echo" tool that returns its "msg" argument.
func echoServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(DefaultServerInfo)
	schema := mcp.ToolSchema{
		Type: "object",
		Properties: map[string]json.RawMessage{
			"msg": json.RawMessage(`{"type":"string"}`),
		},
		Required: []string{"msg"},
	}
	s.RegisterTool("echo", schema, func(_ context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
		var in struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return mcp.CallToolResult{}, err
		}
		return mcp.CallToolResult{
			Content: []mcp.ContentBlock{{Type: "text", Text: "echo: " + in.Msg}},
		}, nil
	})
	return s
}

func TestHandleInitialize(t *testing.T) {
	s := echoServer(t)
	req := newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "0.0.1"},
	})
	resp := s.Handle(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %v", resp.Error)
	}
	var res mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", res.ProtocolVersion, mcp.LatestProtocolVersion)
	}
	if res.ServerInfo.Name != "gocode" {
		t.Errorf("serverInfo.name = %q, want gocode", res.ServerInfo.Name)
	}
	if res.Capabilities.Tools == nil || !res.Capabilities.Tools.ListChanged {
		t.Errorf("expected tools.listChanged capability, got %+v", res.Capabilities.Tools)
	}
}

func TestHandleInitializeVersionFallback(t *testing.T) {
	s := echoServer(t)
	req := newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: "1999-01-01", // unsupported
	})
	resp := s.Handle(context.Background(), req)
	var res mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Errorf("fallback protocolVersion = %q, want %q", res.ProtocolVersion, mcp.LatestProtocolVersion)
	}
}

func TestHandleToolsList(t *testing.T) {
	s := echoServer(t)
	resp := s.Handle(context.Background(), newRequest(t, 2, mcp.MethodToolsList, nil))
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %v", resp.Error)
	}
	var res mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want single echo tool", res.Tools)
	}
	if res.Tools[0].InputSchema.Type != "object" {
		t.Errorf("inputSchema.type = %q, want object", res.Tools[0].InputSchema.Type)
	}
}

func TestHandleToolsCallSuccess(t *testing.T) {
	s := echoServer(t)
	req := newRequest(t, 3, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "echo",
		Arguments: json.RawMessage(`{"msg":"hi"}`),
	})
	resp := s.Handle(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError result: %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "echo: hi" {
		t.Errorf("content = %+v, want [echo: hi]", res.Content)
	}
}

func TestHandleToolsCallHandlerErrorIsInBand(t *testing.T) {
	s := echoServer(t)
	// A wrong-typed "msg" (number, not string) forces the handler's unmarshal
	// to fail, which must surface as an isError content result, not a
	// JSON-RPC error.
	req := newRequest(t, 4, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "echo",
		Arguments: json.RawMessage(`{"msg":123}`),
	})
	resp := s.Handle(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("expected in-band error, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected isError=true, got %+v", res)
	}
}

func TestHandleToolsCallUnknownTool(t *testing.T) {
	s := echoServer(t)
	req := newRequest(t, 5, mcp.MethodToolsCall, mcp.CallToolParams{Name: "nope"})
	resp := s.Handle(context.Background(), req)
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error for unknown tool")
	}
	if resp.Error.Code != mcp.ErrInternalError {
		t.Errorf("error code = %d, want %d", resp.Error.Code, mcp.ErrInternalError)
	}
	if !strings.Contains(resp.Error.Message, "not found") {
		t.Errorf("error message = %q, want ...not found", resp.Error.Message)
	}
}

func TestHandleMethodNotFound(t *testing.T) {
	s := echoServer(t)
	resp := s.Handle(context.Background(), newRequest(t, 6, "resources/read", nil))
	if resp.Error == nil || resp.Error.Code != mcp.ErrMethodNotFound {
		t.Fatalf("expected MethodNotFound, got %+v", resp.Error)
	}
}

func TestHandlePing(t *testing.T) {
	s := echoServer(t)
	resp := s.Handle(context.Background(), newRequest(t, 7, mcp.MethodPing, nil))
	if resp.Error != nil {
		t.Fatalf("ping errored: %v", resp.Error)
	}
	if strings.TrimSpace(string(resp.Result)) != "{}" {
		t.Errorf("ping result = %s, want {}", resp.Result)
	}
}

func TestSetRequestHandlerOverride(t *testing.T) {
	s := echoServer(t)
	s.SetRequestHandler("custom/method", func(_ context.Context, _ mcp.Request) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})
	resp := s.Handle(context.Background(), newRequest(t, 8, "custom/method", nil))
	if resp.Error != nil {
		t.Fatalf("custom method errored: %v", resp.Error)
	}
	if !strings.Contains(string(resp.Result), `"ok":"yes"`) {
		t.Errorf("custom result = %s", resp.Result)
	}
}

// --- ServeStdio round-trip over pipes ---

func TestServeStdioRoundTrip(t *testing.T) {
	s := echoServer(t)

	// Track the inbound notifications/initialized.
	var mu sync.Mutex
	var gotInitialized bool
	s.OnNotification(func(_ context.Context, n mcp.Notification) {
		if n.Method == mcp.MethodInitializedNotif {
			mu.Lock()
			gotInitialized = true
			mu.Unlock()
		}
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.ServeStdio(context.Background(), inR, outW)
		outW.Close()
	}()

	// Feed a framed initialize request, an initialized notification, and a
	// tools/call, then close the input to end the loop.
	go func() {
		enc := func(v any) {
			b, _ := json.Marshal(v)
			inW.Write(append(b, '\n'))
		}
		enc(newRequest(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
			ProtocolVersion: mcp.LatestProtocolVersion,
		}))
		enc(mcp.Notification{JSONRPC: mcp.JSONRPCVersion, Method: mcp.MethodInitializedNotif})
		enc(newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{
			Name:      "echo",
			Arguments: json.RawMessage(`{"msg":"world"}`),
		}))
		inW.Close()
	}()

	// Read framed replies: one for initialize (id 1), one for tools/call
	// (id 2). The notification draws no reply.
	responses := make([][]byte, 0, 2)
	sc := bufio.NewScanner(outR)
	for len(responses) < 2 && sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		responses = append(responses, []byte(line))
	}
	if len(responses) != 2 {
		t.Fatalf("read %d framed replies, want 2", len(responses))
	}

	if err := <-serveDone; err != nil {
		t.Fatalf("ServeStdio returned error: %v", err)
	}

	// First reply: initialize result.
	var initResp mcp.Response
	if err := json.Unmarshal(responses[0], &initResp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize response errored: %v", initResp.Error)
	}
	var initRes mcp.InitializeResult
	if err := json.Unmarshal(initResp.Result, &initRes); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if initRes.ServerInfo.Name != "gocode" {
		t.Errorf("serverInfo.name = %q", initRes.ServerInfo.Name)
	}

	// Second reply: tools/call result.
	var callResp mcp.Response
	if err := json.Unmarshal(responses[1], &callResp); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	if callResp.Error != nil {
		t.Fatalf("tools/call response errored: %v", callResp.Error)
	}
	var callRes mcp.CallToolResult
	if err := json.Unmarshal(callResp.Result, &callRes); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if len(callRes.Content) != 1 || callRes.Content[0].Text != "echo: world" {
		t.Errorf("content = %+v, want [echo: world]", callRes.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotInitialized {
		t.Errorf("notifications/initialized was not delivered to the handler")
	}
}
