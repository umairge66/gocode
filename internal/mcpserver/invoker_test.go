package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

// fakeInvoker is an in-process ToolInvoker: it records the last call and returns
// a canned CallToolResult or error, standing in for the cli's adapter over the
// real tool dispatcher.
type fakeInvoker struct {
	result  mcp.CallToolResult
	err     error
	gotName string
	gotArgs json.RawMessage
}

func (f *fakeInvoker) InvokeTool(_ context.Context, name string, args json.RawMessage) (mcp.CallToolResult, error) {
	f.gotName = name
	f.gotArgs = args
	return f.result, f.err
}

// invokerServer registers a schema-only "greet" tool (no per-tool handler) and
// routes execution through the supplied invoker.
func invokerServer(t *testing.T, inv ToolInvoker) *Server {
	t.Helper()
	s := NewServer(DefaultServerInfo, WithToolInvoker(inv))
	schema := mcp.ToolSchema{
		Type: "object",
		Properties: map[string]json.RawMessage{
			"name": json.RawMessage(`{"type":"string"}`),
		},
		Required: []string{"name"},
	}
	s.RegisterTool("greet", schema, nil)
	return s
}

func TestToolsCallRoutesToInvoker(t *testing.T) {
	inv := &fakeInvoker{result: mcp.CallToolResult{
		Content: []mcp.ContentBlock{{Type: "text", Text: "hello ada"}},
	}}
	s := invokerServer(t, inv)
	resp := s.Handle(context.Background(), newRequest(t, 1, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "greet",
		Arguments: json.RawMessage(`{"name":"ada"}`),
	}))
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %v", resp.Error)
	}
	if inv.gotName != "greet" {
		t.Errorf("invoker got name %q, want greet", inv.gotName)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "hello ada" {
		t.Errorf("content = %+v, want [hello ada]", res.Content)
	}
}

func TestToolsCallInvokerErrorIsInBand(t *testing.T) {
	inv := &fakeInvoker{err: errString("dispatcher blew up")}
	s := invokerServer(t, inv)
	resp := s.Handle(context.Background(), newRequest(t, 2, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "greet",
		Arguments: json.RawMessage(`{"name":"x"}`),
	}))
	if resp.Error != nil {
		t.Fatalf("expected in-band error, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError || len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, "dispatcher blew up") {
		t.Errorf("want isError text result, got %+v", res)
	}
}

func TestToolsCallIsErrorResultFromInvoker(t *testing.T) {
	// A tool that fails reports isError=true via the result (not the err return).
	inv := &fakeInvoker{result: mcp.CallToolResult{
		IsError: true,
		Content: []mcp.ContentBlock{{Type: "text", Text: "tool failed"}},
	}}
	s := invokerServer(t, inv)
	resp := s.Handle(context.Background(), newRequest(t, 3, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "greet",
		Arguments: json.RawMessage(`{"name":"x"}`),
	}))
	if resp.Error != nil {
		t.Fatalf("expected result, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected isError=true, got %+v", res)
	}
}

func TestToolsCallMissingRequiredArgument(t *testing.T) {
	inv := &fakeInvoker{result: mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "unreached"}}}}
	s := invokerServer(t, inv)
	resp := s.Handle(context.Background(), newRequest(t, 4, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "greet",
		Arguments: json.RawMessage(`{}`), // missing required "name"
	}))
	if resp.Error != nil {
		t.Fatalf("expected in-band validation error, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "failed schema validation") {
		t.Errorf("want schema validation isError, got %+v", res)
	}
	if inv.gotName != "" {
		t.Errorf("invoker should not run on validation failure, got call %q", inv.gotName)
	}
}

func TestToolsCallNonObjectArguments(t *testing.T) {
	inv := &fakeInvoker{}
	s := invokerServer(t, inv)
	resp := s.Handle(context.Background(), newRequest(t, 5, mcp.MethodToolsCall, mcp.CallToolParams{
		Name:      "greet",
		Arguments: json.RawMessage(`"not-an-object"`),
	}))
	if resp.Error != nil {
		t.Fatalf("expected in-band validation error, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError {
		t.Errorf("want isError for non-object arguments, got %+v", res)
	}
}

func TestToolsCallNoExecutorNotEnabled(t *testing.T) {
	// A registered tool with neither handler nor invoker is reported not enabled.
	s := NewServer(DefaultServerInfo)
	s.RegisterTool("orphan", mcp.ToolSchema{Type: "object"}, nil)
	resp := s.Handle(context.Background(), newRequest(t, 6, mcp.MethodToolsCall, mcp.CallToolParams{Name: "orphan"}))
	if resp.Error != nil {
		t.Fatalf("expected in-band result, got JSON-RPC error: %v", resp.Error)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not enabled") {
		t.Errorf("want 'not enabled' isError, got %+v", res)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
