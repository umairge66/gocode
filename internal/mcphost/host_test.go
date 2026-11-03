package mcphost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// ---------------------------------------------------------------------------
// In-memory fake MCP server transport
// ---------------------------------------------------------------------------

type fakeServer struct {
	tools     []mcp.Tool
	call      func(name string, args json.RawMessage) (*mcp.CallToolResult, error)
	callDelay time.Duration
	startErr  error
	hangInit  bool
	// initGate, when set, holds the initialize response until it is closed.
	initGate chan struct{}
	// onList, when set, runs before each tools/list response is sent (with
	// the catalog that response carries).
	onList   func(f *fakeTransport)
	noTools  bool
	pageSize int // >0 paginates tools/list
	// prompts, when non-nil, advertises the prompts capability.
	prompts    []mcp.Prompt
	getPrompt  func(p mcp.GetPromptParams) (*mcp.GetPromptResult, error)
	closeCount atomic.Int32

	// mu guards tools (replaced by setTools) and live.
	mu   sync.Mutex
	live *fakeTransport // the most recent transport
}

// setTools replaces the catalog later tools/list calls return.
func (s *fakeServer) setTools(tools []mcp.Tool) {
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
}

// notify sends a server notification on the live transport.
func (s *fakeServer) notify(method string) {
	s.mu.Lock()
	f := s.live
	s.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.msgs <- mcp.Message{Notification: &mcp.Notification{JSONRPC: mcp.JSONRPCVersion, Method: method}}
	}
}

type fakeTransport struct {
	srv    *fakeServer
	mu     sync.Mutex
	msgs   chan mcp.Message
	closed bool
	done   chan struct{}
}

func (s *fakeServer) factory() TransportFactory {
	return func(string, mcp.ServerConfig) (mcp.Transport, error) {
		f := &fakeTransport{srv: s, msgs: make(chan mcp.Message, 16), done: make(chan struct{})}
		s.mu.Lock()
		s.live = f
		s.mu.Unlock()
		return f, nil
	}
}

func (f *fakeTransport) Start(context.Context) error  { return f.srv.startErr }
func (f *fakeTransport) Messages() <-chan mcp.Message { return f.msgs }

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
		close(f.msgs)
		f.srv.closeCount.Add(1)
	}
	return nil
}

func (f *fakeTransport) deliver(id mcp.RequestID, result any, rerr *mcp.RPCError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	resp := &mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: id, Error: rerr}
	if rerr == nil {
		resp.Result, _ = json.Marshal(result)
	}
	f.msgs <- mcp.Message{Response: resp}
}

func (f *fakeTransport) Send(_ context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	m, err := mcp.DecodeMessage(data)
	if err != nil {
		return err
	}
	if m.Request == nil {
		return nil
	}
	req := m.Request
	switch req.Method {
	case mcp.MethodInitialize:
		if f.srv.hangInit {
			return nil
		}
		caps := mcp.ServerCapabilities{}
		if !f.srv.noTools {
			caps.Tools = &mcp.ListChangedCapability{}
		}
		if f.srv.prompts != nil {
			caps.Prompts = &mcp.ListChangedCapability{}
		}
		init := mcp.InitializeResult{
			ProtocolVersion: mcp.LatestProtocolVersion,
			Capabilities:    caps,
			ServerInfo:      mcp.Implementation{Name: "fake", Version: "1"},
			Instructions:    "use me",
		}
		go func() {
			if g := f.srv.initGate; g != nil {
				select {
				case <-g:
				case <-f.done:
					return
				}
			}
			f.deliver(req.ID, init, nil)
		}()
	case mcp.MethodToolsList:
		var p mcp.ListToolsParams
		_ = json.Unmarshal(req.Params, &p)
		f.srv.mu.Lock()
		tools := f.srv.tools
		f.srv.mu.Unlock()
		res := mcp.ListToolsResult{Tools: tools}
		if n := f.srv.pageSize; n > 0 {
			start := 0
			fmt.Sscan(p.Cursor, &start)
			end := min(start+n, len(tools))
			res.Tools = tools[start:end]
			if end < len(tools) {
				res.NextCursor = fmt.Sprint(end)
			}
		}
		go func() {
			if f.srv.onList != nil {
				f.srv.onList(f)
			}
			f.deliver(req.ID, res, nil)
		}()
	case mcp.MethodPromptsList:
		go f.deliver(req.ID, mcp.ListPromptsResult{Prompts: f.srv.prompts}, nil)
	case mcp.MethodPromptsGet:
		var p mcp.GetPromptParams
		_ = json.Unmarshal(req.Params, &p)
		go func() {
			res, err := f.srv.getPrompt(p)
			if err != nil {
				f.deliver(req.ID, nil, &mcp.RPCError{Code: mcp.ErrInternalError, Message: err.Error()})
				return
			}
			f.deliver(req.ID, res, nil)
		}()
	case mcp.MethodToolsCall:
		var p mcp.CallToolParams
		_ = json.Unmarshal(req.Params, &p)
		go func() {
			if d := f.srv.callDelay; d > 0 {
				select {
				case <-time.After(d):
				case <-f.done:
					return
				}
			}
			res, err := f.srv.call(p.Name, p.Arguments)
			if err != nil {
				f.deliver(req.ID, nil, &mcp.RPCError{Code: mcp.ErrInternalError, Message: err.Error()})
				return
			}
			f.deliver(req.ID, res, nil)
		}()
	}
	return nil
}

func echoServer() *fakeServer {
	return &fakeServer{
		tools: []mcp.Tool{
			{Name: "echo", Description: "echoes", InputSchema: mcp.ToolSchema{Type: "object", Properties: map[string]json.RawMessage{"msg": json.RawMessage(`{"type":"string"}`)}, Required: []string{"msg"}}},
			{Name: "fail.hard", Description: "always errors"},
		},
		call: func(name string, args json.RawMessage) (*mcp.CallToolResult, error) {
			switch name {
			case "echo":
				var in struct{ Msg string }
				_ = json.Unmarshal(args, &in)
				return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "echo: " + in.Msg}, {Type: "image", Data: "AAAA", MimeType: "image/png"}}}, nil
			case "fail.hard":
				return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "boom"}}, IsError: true}, nil
			}
			return nil, errors.New("no such tool")
		},
	}
}

// multiFactory routes server names to fake servers.
func multiFactory(m map[string]*fakeServer) TransportFactory {
	return func(name string, cfg mcp.ServerConfig) (mcp.Transport, error) {
		s, ok := m[name]
		if !ok {
			return nil, fmt.Errorf("no fake for %s", name)
		}
		return s.factory()(name, cfg)
	}
}

func noEnv(string) string { return "" }

func TestHostToolsAndCall(t *testing.T) {
	srv := echoServer()
	h := Start(context.Background(), map[string]mcp.ServerConfig{"my srv": {Command: "x"}}, Options{Transport: multiFactory(map[string]*fakeServer{"my srv": srv}), Getenv: noEnv})
	defer h.Close()

	tools := h.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].Name != "mcp__my_srv__echo" || tools[1].Name != "mcp__my_srv__fail_hard" {
		t.Fatalf("names = %q, %q", tools[0].Name, tools[1].Name)
	}
	if tools[0].Description != "echoes" {
		t.Fatalf("desc = %q", tools[0].Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || schema["type"] != "object" || schema["properties"] == nil {
		t.Fatalf("schema = %s (%v)", tools[0].InputSchema, err)
	}
	var schema2 map[string]any
	_ = json.Unmarshal(tools[1].InputSchema, &schema2)
	if schema2["type"] != "object" {
		t.Fatalf("empty schema should default to object: %s", tools[1].InputSchema)
	}

	text, isErr, err := h.Call(context.Background(), "mcp__my_srv__echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || isErr {
		t.Fatalf("call: %v %v", err, isErr)
	}
	if text != "echo: hi\n[Image: image/png]" {
		t.Fatalf("text = %q", text)
	}

	// Routes to the original (unsanitized) tool name and propagates isError.
	text, isErr, err = h.Call(context.Background(), "mcp__my_srv__fail_hard", nil)
	if err != nil || !isErr || text != "boom" {
		t.Fatalf("isError call: %q %v %v", text, isErr, err)
	}

	if _, _, err := h.Call(context.Background(), "mcp__my_srv__nope", nil); err == nil {
		t.Fatal("unknown tool should error")
	}

	st := h.Status()
	if len(st) != 1 || st[0].State != StateConnected || st[0].Tools != 2 || st[0].Instructions != "use me" {
		t.Fatalf("status = %+v", st)
	}
}

func TestHostAddServers(t *testing.T) {
	a, b := echoServer(), echoServer()
	fac := multiFactory(map[string]*fakeServer{"a": a, "b": b})
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {Command: "x"}}, Options{Transport: fac, Getenv: noEnv})
	defer h.Close()
	if n := len(h.Tools()); n != 2 {
		t.Fatalf("initial tools = %d, want 2", n)
	}

	// Add a newly approved server and an already-known one (a no-op).
	h.AddServers(context.Background(), map[string]mcp.ServerConfig{"b": {Command: "y"}, "a": {Command: "x"}})
	if n := len(h.Tools()); n != 4 {
		t.Fatalf("tools after AddServers = %d, want 4", n)
	}
	if !h.Has("mcp__b__echo") {
		t.Error("newly added server's tool not advertised")
	}
	connected := 0
	for _, st := range h.Status() {
		if st.State == StateConnected {
			connected++
		}
	}
	if connected != 2 {
		t.Fatalf("connected servers = %d, want 2", connected)
	}
}

func TestHostToolTimeout(t *testing.T) {
	srv := echoServer()
	srv.callDelay = 5 * time.Second
	cfg := mcp.ServerConfig{Command: "x", Timeout: 1000} // 1s, the minimum
	h := Start(context.Background(), map[string]mcp.ServerConfig{"slow": cfg}, Options{Transport: srv.factory(), Getenv: noEnv})
	defer h.Close()
	start := time.Now()
	_, _, err := h.Call(context.Background(), "mcp__slow__echo", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), `MCP server "slow" tool "echo" timed out after 1s`) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("timeout not applied")
	}
}

func TestHostFailingServerDoesNotBlockOthers(t *testing.T) {
	good := echoServer()
	bad := &fakeServer{startErr: errors.New("spawn failed")}
	hang := &fakeServer{hangInit: true}
	h := New(map[string]mcp.ServerConfig{"good": {}, "bad": {}, "hang": {}, "missing": {}},
		Options{Transport: multiFactory(map[string]*fakeServer{"good": good, "bad": bad, "hang": hang}), ConnectTimeout: 200 * time.Millisecond, Getenv: noEnv})
	defer h.Close()
	start := time.Now()
	h.Connect(context.Background())
	if time.Since(start) > 3*time.Second {
		t.Fatal("connect not bounded by timeout")
	}
	got := map[string]ServerStatus{}
	for _, s := range h.Status() {
		got[s.Name] = s
	}
	if got["good"].State != StateConnected {
		t.Fatalf("good = %+v", got["good"])
	}
	if got["bad"].State != StateFailed || !strings.Contains(got["bad"].Error, "spawn failed") {
		t.Fatalf("bad = %+v", got["bad"])
	}
	if got["hang"].State != StateFailed || !strings.Contains(got["hang"].Error, `MCP server "hang" connection timed out after 200ms`) {
		t.Fatalf("hang = %+v", got["hang"])
	}
	if got["missing"].State != StateFailed {
		t.Fatalf("missing = %+v", got["missing"])
	}
	if hang.closeCount.Load() != 1 {
		t.Fatalf("hung transport should be closed, got %d", hang.closeCount.Load())
	}
	if len(h.Tools()) != 2 {
		t.Fatalf("tools = %+v", h.Tools())
	}
	if _, _, err := h.Call(context.Background(), "mcp__good__echo", json.RawMessage(`{"msg":"x"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestHostPaginationAndNoToolsCapability(t *testing.T) {
	paged := echoServer()
	paged.tools = append(paged.tools, mcp.Tool{Name: "third"})
	paged.pageSize = 1
	none := echoServer()
	none.noTools = true
	h := Start(context.Background(), map[string]mcp.ServerConfig{"p": {}, "n": {}},
		Options{Transport: multiFactory(map[string]*fakeServer{"p": paged, "n": none}), Getenv: noEnv})
	defer h.Close()
	if n := len(h.Tools()); n != 3 {
		t.Fatalf("tools = %d", n)
	}
	for _, s := range h.Status() {
		if s.State != StateConnected {
			t.Fatalf("status %+v", s)
		}
	}
}

func TestHostCloseAndConcurrency(t *testing.T) {
	srv := echoServer()
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: srv.factory(), Getenv: noEnv})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := fmt.Sprint(i)
			text, _, err := h.Call(context.Background(), "mcp__a__echo", json.RawMessage(`{"msg":"`+msg+`"}`))
			if err != nil || !strings.HasPrefix(text, "echo: "+msg+"\n") {
				t.Errorf("call %d: %q %v", i, text, err)
			}
			_ = h.Tools()
			_ = h.Status()
		}()
	}
	wg.Wait()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal("second close:", err)
	}
	if srv.closeCount.Load() != 1 {
		t.Fatalf("transport closed %d times", srv.closeCount.Load())
	}
	if len(h.Tools()) != 0 {
		t.Fatal("tools after close")
	}
	if _, _, err := h.Call(context.Background(), "mcp__a__echo", nil); err == nil {
		t.Fatal("call after close should fail")
	}
	if st := h.Status(); st[0].State != StateClosed {
		t.Fatalf("status after close = %+v", st)
	}
}

func TestHostOutputTruncation(t *testing.T) {
	srv := echoServer()
	big := strings.Repeat("x", 1000)
	srv.call = func(string, json.RawMessage) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: big}}}, nil
	}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: srv.factory(), MaxOutputTokens: 100, Getenv: noEnv})
	defer h.Close()
	text, _, err := h.Call(context.Background(), "mcp__a__echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, strings.Repeat("x", 400)+"\n\n[OUTPUT TRUNCATED - exceeded 100 token limit]") || strings.HasPrefix(text, strings.Repeat("x", 401)) {
		t.Fatalf("text = %q", text)
	}
}

// ---------------------------------------------------------------------------
// Hermetic stdio test: the test binary re-executes itself as a tiny MCP server.
// ---------------------------------------------------------------------------

const helperEnv = "GOCODE_MCPHOST_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runHelperServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHelperServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.LatestProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "helper", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "pid", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("pid=%d", os.Getpid())}}}
		default:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "nope"}})
			continue
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func TestHostStdioHelperProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path:", err)
	}
	cfg := mcp.ServerConfig{Type: mcp.TransportStdio, Command: exe, Args: []string{"-test.run=^$"}, Env: map[string]string{helperEnv: "1"}}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"helper": cfg}, Options{Getenv: noEnv, ConnectTimeout: 10 * time.Second})
	defer h.Close()
	if st := h.Status(); st[0].State != StateConnected {
		t.Fatalf("status = %+v", st)
	}
	text, isErr, err := h.Call(context.Background(), "mcp__helper__pid", nil)
	if err != nil || isErr || !strings.HasPrefix(text, "pid=") {
		t.Fatalf("call: %q %v %v", text, isErr, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}
