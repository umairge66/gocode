package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/mcp"
)

// --- Routing ----------------------------------------------------------------

func TestIsMcpServeInvocation_Routing(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"mcp", "serve"}, true},
		{[]string{"mcp", "serve", "--debug"}, true},
		{[]string{"mcp", "serve", "--transport", "stdio"}, true},
		{[]string{"mcp", "list"}, false},
		{[]string{"mcp"}, false},
		{[]string{"serve"}, false},
		{[]string{}, false},
	}
	for _, c := range cases {
		if got := isMcpServeInvocation(c.args); got != c.want {
			t.Errorf("isMcpServeInvocation(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// runMain must route `mcp serve` to runMcpServe (which serves stdio and returns
// on EOF) rather than falling through to the interactive session. With an empty
// stdin the stdio server reaches EOF immediately and returns 0, and stdout must
// stay clean (no banner) because it is the JSON-RPC channel.
func TestRunMain_McpServeRoutesToServer(t *testing.T) {
	env, out, errb := newTestEnv()
	env.Stdin = strings.NewReader("") // immediate EOF

	code := runMain(env, []string{"mcp", "serve"})
	if code != 0 {
		t.Fatalf("runMain(mcp serve) = %d, want 0; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout not clean on stdio transport: %q", out.String())
	}
}

// --- Stdout cleanliness -----------------------------------------------------

func TestRunMcpServe_StdoutCleanDiagnosticsToStderr(t *testing.T) {
	env, out, errb := newTestEnv()
	env.Stdin = strings.NewReader("")

	// --debug makes the handler emit a startup line; it must go to stderr only.
	code := runMcpServe(env, &DispatchDefaults{}, []string{"mcp", "serve", "--debug"})
	if code != 0 {
		t.Fatalf("runMcpServe = %d, want 0; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout must be empty on stdio transport, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "serving") {
		t.Errorf("expected --debug startup diagnostic on stderr, got %q", errb.String())
	}
}

func TestRunMcpServe_NoStdin(t *testing.T) {
	env, out, errb := newTestEnv()
	env.Stdin = nil
	if code := runMcpServe(env, &DispatchDefaults{}, []string{"mcp", "serve"}); code != 1 {
		t.Fatalf("runMcpServe(no stdin) = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay clean, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "stdin") {
		t.Errorf("expected stdin error on stderr, got %q", errb.String())
	}
}

// --- Option parsing / validation --------------------------------------------

func TestParseMcpServeOptions(t *testing.T) {
	opts, verr := parseMcpServeOptions([]string{"mcp", "serve", "--debug", "--verbose"})
	if verr != "" {
		t.Fatalf("unexpected error: %s", verr)
	}
	if !opts.debug || !opts.verbose {
		t.Errorf("debug/verbose not parsed: %+v", opts)
	}
	if opts.transport != "stdio" || opts.resultFormat != "raw" {
		t.Errorf("defaults wrong: %+v", opts)
	}

	opts, verr = parseMcpServeOptions([]string{"serve", "--transport=stdio", "--result-format", "rendered"})
	if verr != "" {
		t.Fatalf("unexpected error: %s", verr)
	}
	if opts.transport != "stdio" || opts.resultFormat != "rendered" {
		t.Errorf("options not parsed: %+v", opts)
	}
}

func TestParseMcpServeOptions_Validation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		frag string
	}{
		{"bad transport", []string{"serve", "--transport", "http"}, "--transport must be"},
		{"bad result-format", []string{"serve", "--result-format", "xml"}, "--result-format must be"},
	}
	for _, c := range cases {
		_, verr := parseMcpServeOptions(c.args)
		if !strings.Contains(verr, c.frag) {
			t.Errorf("%s: verr=%q, want fragment %q", c.name, verr, c.frag)
		}
	}
}

// --- Served catalog ---------------------------------------------------------

// The served catalog is assembled from the SAME toolrun dispatcher the session
// uses, so tools/list must be non-empty and every tool must carry an object
// input schema.
func TestBuildMcpServeServer_CatalogFromRegistry(t *testing.T) {
	env, _, _ := newTestEnv()
	srv, n := buildMcpServeServer(env, &DispatchDefaults{})
	if n == 0 {
		t.Fatal("served catalog is empty")
	}

	idJSON, _ := json.Marshal(1)
	resp := srv.Handle(context.Background(), mcp.Request{
		JSONRPC: mcp.JSONRPCVersion, ID: idJSON, Method: mcp.MethodToolsList,
	})
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	var result mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(result.Tools) != n {
		t.Errorf("tools/list returned %d tools, registered %d", len(result.Tools), n)
	}
	// Spot-check a couple of well-known built-ins and that schemas are present.
	names := map[string]bool{}
	for _, tool := range result.Tools {
		names[tool.Name] = true
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q has non-object input schema %q", tool.Name, tool.InputSchema.Type)
		}
	}
	for _, want := range []string{"Bash", "Grep", "Glob"} {
		if !names[want] {
			t.Errorf("served catalog missing expected tool %q (have %v)", want, names)
		}
	}

	// A registered tool with a nil handler falls back to the invoker, so a
	// tools/call reaches the real dispatcher: Grep with a missing required
	// "pattern" argument is an in-band schema-validation error, never a
	// transport error.
	callID, _ := json.Marshal(2)
	callParams, _ := json.Marshal(mcp.CallToolParams{Name: "Grep", Arguments: json.RawMessage(`{}`)})
	callResp := srv.Handle(context.Background(), mcp.Request{
		JSONRPC: mcp.JSONRPCVersion, ID: callID, Method: mcp.MethodToolsCall, Params: callParams,
	})
	if callResp.Error != nil {
		t.Fatalf("tools/call should not be a transport error: %+v", callResp.Error)
	}
	var cr mcp.CallToolResult
	if err := json.Unmarshal(callResp.Result, &cr); err != nil {
		t.Fatalf("decode call result: %v", err)
	}
	if !cr.IsError {
		t.Errorf("missing required argument should yield IsError result, got %+v", cr)
	}
}

// --- ToolInvoker adapter mapping --------------------------------------------

// fakeDispatcher stands in for toolrun's dispatcher at the agent.ToolDispatcher
// seam so the adapter's result/image/error mapping is tested without any tool
// execution. (dispatcherInvoker wraps *toolrun.Dispatcher directly; the mapping
// itself is exercised through toolResultToMCP.)

func TestToolResultToMCP_Text(t *testing.T) {
	got := toolResultToMCP(agent.ToolResult{Content: "hello world"})
	if got.IsError {
		t.Error("text result should not be IsError")
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "hello world" {
		t.Fatalf("want single text block, got %+v", got.Content)
	}
}

func TestToolResultToMCP_Image(t *testing.T) {
	got := toolResultToMCP(agent.ToolResult{
		Content: "see image",
		Images: []apiclient.ToolResultImage{
			{Base64: "QUJD", MediaType: "image/png"},
		},
	})
	if got.IsError {
		t.Error("image result should not be IsError")
	}
	if len(got.Content) != 2 {
		t.Fatalf("want text+image blocks, got %+v", got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "see image" {
		t.Errorf("first block should be text, got %+v", got.Content[0])
	}
	img := got.Content[1]
	if img.Type != "image" || img.Data != "QUJD" || img.MimeType != "image/png" {
		t.Errorf("image block mapped wrong: %+v", img)
	}
}

func TestToolResultToMCP_ImageOnly(t *testing.T) {
	got := toolResultToMCP(agent.ToolResult{
		Images: []apiclient.ToolResultImage{{Base64: "REVG", MediaType: "image/jpeg"}},
	})
	if len(got.Content) != 1 || got.Content[0].Type != "image" {
		t.Fatalf("image-only result should carry a single image block, got %+v", got.Content)
	}
}

func TestToolResultToMCP_Error(t *testing.T) {
	got := toolResultToMCP(agent.ToolResult{Content: "boom", IsError: true})
	if !got.IsError {
		t.Error("IsError must carry through")
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "boom" {
		t.Fatalf("error result should be a single text block, got %+v", got.Content)
	}
}

// The invoker reports a loop-fatal Go error back to the server (reserved for the
// last-resort case); a tool-level failure instead rides in-band as IsError with
// a nil error. This asserts the latter via a real dispatcher call that is denied
// by the headless permission gate (no prompter) — an in-band IsError, nil error.
func TestDispatcherInvoker_HeadlessPermissionDenied(t *testing.T) {
	env, _, _ := newTestEnv()
	// Build with no caller/prompter: an "ask" tool (Bash) resolves to a denial.
	disp := newSessionDispatcher(modeFromDefaults(nil), env, nil)
	inv := dispatcherInvoker{disp: disp}

	res, err := inv.InvokeTool(context.Background(), "Bash", json.RawMessage(`{"command":"/bin/echo hi"}`))
	if err != nil {
		t.Fatalf("tool-level denial must be in-band, got Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("headless Bash should be denied (IsError), got %+v", res)
	}
	if len(res.Content) == 0 || res.Content[0].Type != "text" || res.Content[0].Text == "" {
		t.Errorf("denial should carry an explanatory text block, got %+v", res.Content)
	}
}
