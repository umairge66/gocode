package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// fakeTransport is an in-memory Transport that plays the role of an MCP server.
// For each request the client sends it crafts a matching JSON-RPC response and
// delivers it on the Messages channel; notifications are recorded.
type fakeTransport struct {
	msgs          chan Message
	gotInit       bool
	gotInitedNtfy bool
	initVersion   string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{msgs: make(chan Message, 8)}
}

func (f *fakeTransport) Start(ctx context.Context) error { return nil }

func (f *fakeTransport) Messages() <-chan Message { return f.msgs }

func (f *fakeTransport) Close() error {
	close(f.msgs)
	return nil
}

func (f *fakeTransport) Send(ctx context.Context, msg any) error {
	// Re-encode through the wire so we exercise real (un)marshaling.
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	decoded, err := DecodeMessage(data)
	if err != nil {
		return err
	}
	switch {
	case decoded.Notification != nil:
		if decoded.Notification.Method == MethodInitializedNotif {
			f.gotInitedNtfy = true
		}
		return nil
	case decoded.Request != nil:
		return f.respond(decoded.Request)
	}
	return nil
}

func (f *fakeTransport) respond(req *Request) error {
	var result any
	switch req.Method {
	case MethodInitialize:
		f.gotInit = true
		var p InitializeParams
		_ = json.Unmarshal(req.Params, &p)
		f.initVersion = p.ProtocolVersion
		result = InitializeResult{
			ProtocolVersion: LatestProtocolVersion,
			Capabilities:    ServerCapabilities{Tools: &ListChangedCapability{ListChanged: true}},
			ServerInfo:      Implementation{Name: "fake", Version: "1.2.3"},
			Instructions:    "be good",
		}
	case MethodToolsList:
		ro := true
		result = ListToolsResult{
			Tools: []Tool{{
				Name:        "echo",
				Description: "echoes input",
				InputSchema: ToolSchema{
					Type:       "object",
					Properties: map[string]json.RawMessage{"msg": json.RawMessage(`{"type":"string"}`)},
					Required:   []string{"msg"},
				},
				Annotations: &ToolAnnotations{ReadOnlyHint: &ro},
			}},
		}
	case MethodToolsCall:
		var p CallToolParams
		_ = json.Unmarshal(req.Params, &p)
		result = CallToolResult{
			Content: []ContentBlock{{Type: "text", Text: "called " + p.Name}},
		}
	case MethodPromptsList:
		result = ListPromptsResult{Prompts: []Prompt{{
			Name: "review", Description: "review code",
			Arguments: []PromptArgument{{Name: "file", Required: true}, {Name: "focus"}},
		}}}
	case MethodPromptsGet:
		var p GetPromptParams
		_ = json.Unmarshal(req.Params, &p)
		result = GetPromptResult{Messages: []PromptMessage{{
			Role: "user", Content: ContentBlock{Type: "text", Text: "review " + p.Arguments["file"] + " for " + p.Arguments["focus"]},
		}}}
	default:
		// Unknown method -> JSON-RPC error.
		f.deliverError(req.ID, ErrMethodNotFound, "method not found")
		return nil
	}
	f.deliverResult(req.ID, result)
	return nil
}

func (f *fakeTransport) deliverResult(id RequestID, result any) {
	rb, _ := json.Marshal(result)
	f.msgs <- Message{Response: &Response{JSONRPC: JSONRPCVersion, ID: id, Result: rb}}
}

func (f *fakeTransport) deliverError(id RequestID, code int, msg string) {
	f.msgs <- Message{Response: &Response{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}}
}

// TestClientFullFlow drives Initialize -> ListTools -> CallTool against the
// fake transport: the connect/discover/call sequence.
func TestClientFullFlow(t *testing.T) {
	ft := newFakeTransport()
	c := NewClient(ft, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	// Initialize handshake.
	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !ft.gotInit {
		t.Error("server never received initialize request")
	}
	if ft.initVersion != LatestProtocolVersion {
		t.Errorf("client advertised version %q, want %q", ft.initVersion, LatestProtocolVersion)
	}
	if init.ServerInfo.Name != "fake" || init.ServerInfo.Version != "1.2.3" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}
	if init.Instructions != "be good" {
		t.Errorf("instructions = %q", init.Instructions)
	}
	if !ft.gotInitedNtfy {
		t.Error("client never sent notifications/initialized")
	}
	if !c.initialized {
		t.Error("client not marked initialized")
	}
	if c.ServerInfo().Name != "fake" {
		t.Error("ServerInfo() not stored")
	}

	// tools/list.
	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	if list.Tools[0].Annotations == nil || list.Tools[0].Annotations.ReadOnlyHint == nil {
		t.Error("tool annotations lost over the wire")
	}

	// tools/call.
	call, err := c.CallTool(ctx, CallToolParams{
		Name:      "echo",
		Arguments: json.RawMessage(`{"msg":"hi"}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(call.Content) != 1 || call.Content[0].Text != "called echo" {
		t.Fatalf("call result = %+v", call.Content)
	}
	if call.IsError {
		t.Error("unexpected isError")
	}
}

// TestClientRPCErrorPropagation verifies a JSON-RPC error response surfaces as
// an *RPCError from the calling method.
func TestClientRPCErrorPropagation(t *testing.T) {
	ft := newFakeTransport()
	c := NewClient(ft, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// "resources/list" is unknown to the fake server -> MethodNotFound error.
	err := c.request(ctx, MethodResourcesList, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("error type = %T, want *RPCError", err)
	}
	if rpcErr.Code != ErrMethodNotFound {
		t.Errorf("code = %d, want %d", rpcErr.Code, ErrMethodNotFound)
	}
}

// TestClientRejectsUnsupportedProtocol verifies the handshake fails when the
// server negotiates a protocol version the client does not support.
func TestClientRejectsUnsupportedProtocol(t *testing.T) {
	ft := &badVersionTransport{msgs: make(chan Message, 4)}
	c := NewClient(ft, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Initialize(ctx); err == nil {
		t.Fatal("expected Initialize to reject unsupported protocol version")
	}
}

type badVersionTransport struct {
	msgs chan Message
}

func (b *badVersionTransport) Start(ctx context.Context) error { return nil }
func (b *badVersionTransport) Messages() <-chan Message        { return b.msgs }
func (b *badVersionTransport) Close() error                    { close(b.msgs); return nil }
func (b *badVersionTransport) Send(ctx context.Context, msg any) error {
	data, _ := json.Marshal(msg)
	decoded, err := DecodeMessage(data)
	if err != nil {
		return err
	}
	if decoded.Request != nil && decoded.Request.Method == MethodInitialize {
		res := InitializeResult{
			ProtocolVersion: "1999-01-01", // unsupported
			ServerInfo:      Implementation{Name: "old", Version: "0.0.1"},
		}
		rb, _ := json.Marshal(res)
		b.msgs <- Message{Response: &Response{JSONRPC: JSONRPCVersion, ID: decoded.Request.ID, Result: rb}}
	}
	return nil
}

// TestNewTransportSelection confirms the transport-selection switch.
func TestNewTransportSelection(t *testing.T) {
	cases := []struct {
		cfg     ServerConfig
		wantErr bool
		stdio   bool
	}{
		{ServerConfig{Type: TransportStdio, Command: "x"}, false, true},
		{ServerConfig{Command: "x"}, false, true}, // empty type defaults to stdio
		{ServerConfig{Type: TransportHTTP, URL: "http://x"}, false, false},
		{ServerConfig{Type: TransportSSE, URL: "http://x"}, false, false},
		{ServerConfig{Type: "bogus"}, true, false},
	}
	for _, tc := range cases {
		tr, err := NewTransport(tc.cfg)
		if tc.wantErr {
			if err == nil {
				t.Errorf("cfg %+v: expected error", tc.cfg)
			}
			continue
		}
		if err != nil {
			t.Errorf("cfg %+v: %v", tc.cfg, err)
			continue
		}
		_, isStdio := tr.(*StdioTransport)
		if isStdio != tc.stdio {
			t.Errorf("cfg %+v: stdio=%v, want %v", tc.cfg, isStdio, tc.stdio)
		}
	}
}

// TestStdioTransportFraming starts a stdio transport against a tiny shell
// server that echoes a canned JSON-RPC response, exercising real process spawn
// and newline framing.
func TestStdioTransportFraming(t *testing.T) {
	// A one-shot "server": emit a JSON-RPC response line, then exit.
	resp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	tr := NewStdioTransport(ServerConfig{
		Type:    TransportStdio,
		Command: "printf",
		Args:    []string{resp + "\n"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Close()

	select {
	case msg, ok := <-tr.Messages():
		if !ok {
			t.Fatal("messages channel closed before a message arrived")
		}
		if msg.Response == nil {
			t.Fatalf("expected a response, got %+v", msg)
		}
		var lr ListToolsResult
		if err := json.Unmarshal(msg.Response.Result, &lr); err != nil {
			t.Fatalf("decode result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for stdio message")
	}
}
