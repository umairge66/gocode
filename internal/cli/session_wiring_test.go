package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// ---------------------------------------------------------------------------
// In-memory MCP server (injected through printSession.mcpTransport)
// ---------------------------------------------------------------------------

type memMCPServer struct {
	tools    []mcp.Tool
	startErr error
	closed   atomic.Int32
	// initGate, when set, holds the initialize reply until it is closed.
	initGate chan struct{}
}

type memMCPTransport struct {
	srv    *memMCPServer
	mu     sync.Mutex
	msgs   chan mcp.Message
	closed bool
}

func (s *memMCPServer) factory() mcphost.TransportFactory {
	return func(string, mcp.ServerConfig) (mcp.Transport, error) {
		return &memMCPTransport{srv: s, msgs: make(chan mcp.Message, 16)}, nil
	}
}

func (f *memMCPTransport) Start(context.Context) error  { return f.srv.startErr }
func (f *memMCPTransport) Messages() <-chan mcp.Message { return f.msgs }
func (f *memMCPTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.msgs)
		f.srv.closed.Add(1)
	}
	return nil
}

func (f *memMCPTransport) reply(id mcp.RequestID, result any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	raw, _ := json.Marshal(result)
	f.msgs <- mcp.Message{Response: &mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: id, Result: raw}}
}

func (f *memMCPTransport) Send(_ context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	m, err := mcp.DecodeMessage(data)
	if err != nil || m.Request == nil {
		return err
	}
	req := m.Request
	switch req.Method {
	case mcp.MethodInitialize:
		go func() {
			if g := f.srv.initGate; g != nil {
				<-g
			}
			f.reply(req.ID, mcp.InitializeResult{
				ProtocolVersion: mcp.LatestProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ListChangedCapability{}},
				ServerInfo:      mcp.Implementation{Name: "mem", Version: "1"},
			})
		}()
	case mcp.MethodToolsList:
		go f.reply(req.ID, mcp.ListToolsResult{Tools: f.srv.tools})
	case mcp.MethodToolsCall:
		var p mcp.CallToolParams
		_ = json.Unmarshal(req.Params, &p)
		var args struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(p.Arguments, &args)
		go f.reply(req.ID, mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "echo: " + args.Msg}}})
	}
	return nil
}

func echoServer() *memMCPServer {
	var schema mcp.ToolSchema
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"msg":{"$ref":"#/$defs/m"}},"required":["msg"],"$defs":{"m":{"type":"string"}},"additionalProperties":false}`), &schema)
	return &memMCPServer{tools: []mcp.Tool{{Name: "echo", Description: "echoes", InputSchema: schema}}}
}

// ---------------------------------------------------------------------------
// Scripted OpenAI-compatible provider
// ---------------------------------------------------------------------------

type scriptedProvider struct {
	mu     sync.Mutex
	bodies []string
	// respond picks the reply for request n (0-based) with raw body.
	respond func(n int, body string) string
}

func (p *scriptedProvider) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		n := len(p.bodies)
		p.bodies = append(p.bodies, string(b))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, p.respond(n, string(b)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *scriptedProvider) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

func textReply(text string) string {
	b, _ := json.Marshal(text)
	return `{"model":"m","choices":[{"message":{"role":"assistant","content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
}

func toolReply(id, name string, args any) string {
	a, _ := json.Marshal(args)
	as, _ := json.Marshal(string(a))
	return `{"model":"m","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(as) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
}

// sessionEnv builds an Env whose settings live in a temp config dir and whose
// project is a temp cwd. settings is the user settings.json content ("" for
// none).
func sessionEnv(t *testing.T, settings string) (*Env, *strings.Builder, *strings.Builder, string) {
	t.Helper()
	cfgDir, cwd := t.TempDir(), t.TempDir()
	if settings != "" {
		if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb strings.Builder
	// The session scratchpad goes under a test temp dir, not the real one.
	vars := map[string]string{"GOCODE_CONFIG_DIR": cfgDir, "GOCODE_CODE_TMPDIR": t.TempDir()}
	env := &Env{
		Getenv: func(k string) string { return vars[k] },
		Setenv: func(string, string) {},
		Stdout: &out,
		Stderr: &errb,
		Cwd:    func() (string, error) { return cwd, nil },
	}
	return env, &out, &errb, cwd
}

func openAISession(srv *httptest.Server, mode toolreg.Mode) *printSession {
	return &printSession{
		Model:     "parent-model",
		Mode:      mode,
		Provider:  providerOpenAI,
		APIKey:    "test-key",
		BaseURL:   srv.URL,
		newCaller: defaultNewCaller,
	}
}

// fakeHookRunner classifies scripted commands the way real hooks would exit.
func fakeHookRunner(run func(cmd string, payload hooks.Payload) (exit int, stdout, stderr string)) hookhost.CommandRunner {
	return hookhost.CommandRunnerFunc(func(_ context.Context, req hookhost.RunRequest) (hooks.Decision, hooks.RunResult, error) {
		code, so, se := run(req.Hook.Command, req.Payload)
		return hooks.Classify(code, so, se, req.Options), hooks.RunResult{ExitCode: code, Stdout: so, Stderr: se}, nil
	})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestPrint_MCPToolEndToEnd: --mcp-config adds a server (served in memory),
// its tool reaches the model with the full schema, the model calls it, the
// call is allowed by a server-level "mcp__mem" rule, the result rides in the
// second request, and the host is closed when the session ends.
func TestPrint_MCPToolEndToEnd(t *testing.T) {
	env, out, errb, _ := sessionEnv(t, `{"permissions":{"allow":["mcp__mem"]}}`)
	mem := echoServer()
	// The server is still connecting (in the background: its tools are
	// deferred) when the first request goes out; ToolSearch waits for it.
	mem.initGate = make(chan struct{})
	sp := &scriptedProvider{respond: func(n int, _ string) string {
		switch n {
		case 0:
			close(mem.initGate)
			return toolReply("s1", "ToolSearch", map[string]string{"query": "select:mcp__mem__echo"})
		case 1:
			return toolReply("c1", "mcp__mem__echo", map[string]string{"msg": "hi"})
		}
		return textReply("done")
	}}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.MCPConfig = []string{`{"mcpServers":{"mem":{"command":"unused"}}}`}
	s.mcpTransport = mem.factory()

	if code := runPrintSession(env, s, "use the tool"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "done" {
		t.Fatalf("stdout = %q", out.String())
	}
	reqs := sp.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	// The first request did not wait for the server.
	if strings.Contains(reqs[0], `mcp__mem__echo`) {
		t.Fatalf("first request waited for the background MCP server: %s", reqs[0])
	}
	if !strings.Contains(reqs[1], `"mcp__mem__echo"`) || !strings.Contains(reqs[1], `$defs`) || !strings.Contains(reqs[1], `additionalProperties`) {
		t.Fatalf("request after ToolSearch lacks the MCP tool with its full schema: %s", reqs[1])
	}
	if !strings.Contains(reqs[2], "echo: hi") {
		t.Fatalf("third request lacks the MCP result: %s", reqs[2])
	}
	if mem.closed.Load() != 1 {
		t.Fatalf("MCP transport closed %d times, want 1 (host closed at session end)", mem.closed.Load())
	}
}

// TestPrint_MCPProjectPendingGuidance: in -p a project .mcp.json server is NOT
// auto-approved; /mcp lists it as pending and the startup notices print the
// set-enableAllProjectMcpServers guidance (no interactive prompt).
func TestPrint_MCPProjectPendingGuidance(t *testing.T) {
	env, out, errb, cwd := sessionEnv(t, "")
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("x") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.mcpTransport = echoServer().factory()

	if code := runPrintSession(env, s, "/mcp"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "proj") || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("/mcp stdout should list the pending project server: %q", out.String())
	}
	if !strings.Contains(errb.String(), "pending approval") || !strings.Contains(errb.String(), "enableAllProjectMcpServers") {
		t.Fatalf("-p guidance missing from stderr: %q", errb.String())
	}
}

// TestPrint_StrictMCPConfigIgnoresProject: strict --mcp-config mode imports no
// project/.mcp.json/Claude servers (unchanged behavior).
func TestPrint_StrictMCPConfigIgnoresProject(t *testing.T) {
	env, out, _, cwd := sessionEnv(t, "")
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("x") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.StrictMCPConfig = true
	s.mcpTransport = echoServer().factory()

	if code := runPrintSession(env, s, "/mcp"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out.String(), "proj") {
		t.Fatalf("strict mode must ignore the project .mcp.json: %q", out.String())
	}
}

// TestPrint_MCPDeniedWithoutRule: in default mode with no allow rule the MCP
// call goes through the Decider and is refused (the server is never called).
func TestPrint_MCPDeniedWithoutRule(t *testing.T) {
	env, _, errb, _ := sessionEnv(t, "")
	mem := echoServer()
	sp := &scriptedProvider{respond: func(n int, _ string) string {
		switch n {
		case 0:
			return toolReply("s1", "ToolSearch", map[string]string{"query": "select:mcp__mem__echo"})
		case 1:
			return toolReply("c1", "mcp__mem__echo", map[string]string{"msg": "hi"})
		}
		return textReply("done")
	}}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.MCPConfig = []string{`{"mcpServers":{"mem":{"command":"unused"}}}`}
	s.mcpTransport = mem.factory()
	if code := runPrintSession(env, s, "x"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if r := sp.requests()[2]; strings.Contains(r, "echo: hi") || !strings.Contains(r, "haven't granted it yet") {
		t.Fatalf("MCP call should be refused by the Decider: %s", r)
	}
}

// TestPrint_PreToolUseHookBlocksBash: a settings PreToolUse hook exiting 2
// blocks a Bash call even in bypass mode; its stderr is the model feedback.
func TestPrint_PreToolUseHookBlocksBash(t *testing.T) {
	env, out, errb, _ := sessionEnv(t, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard"}]}]}}`)
	var ran atomic.Int32
	sp := &scriptedProvider{respond: func(n int, _ string) string {
		if n == 0 {
			return toolReply("b1", "Bash", map[string]string{"command": "echo should-not-run > /dev/null"})
		}
		return textReply("ok")
	}}
	s := openAISession(sp.server(t), toolreg.ModeBypassPermissions)
	s.hookRunner = fakeHookRunner(func(cmd string, p hooks.Payload) (int, string, string) {
		if cmd == "guard" {
			ran.Add(1)
			return 2, "", "bash is forbidden here"
		}
		return 0, "", ""
	})
	if code := runPrintSession(env, s, "run bash"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if ran.Load() != 1 {
		t.Fatalf("PreToolUse hook ran %d times, want exactly 1", ran.Load())
	}
	if r := sp.requests()[1]; !strings.Contains(r, "bash is forbidden here") {
		t.Fatalf("second request lacks the hook's block reason: %s", r)
	}
	if strings.TrimSpace(out.String()) != "ok" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestPrint_UserPromptSubmitBlock: a blocking UserPromptSubmit hook keeps the
// prompt from the model and reports the reason.
func TestPrint_UserPromptSubmitBlock(t *testing.T) {
	env, out, errb, _ := sessionEnv(t, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"ups"}]}]}}`)
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("never") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.hookRunner = fakeHookRunner(func(string, hooks.Payload) (int, string, string) {
		return 2, "", "secrets in prompt"
	})
	if code := runPrintSession(env, s, "my password is hunter2"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if n := len(sp.requests()); n != 0 {
		t.Fatalf("model called %d times, want 0", n)
	}
	if out.String() != "" || !strings.Contains(errb.String(), "secrets in prompt") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestPrint_UserPromptSubmitContextAndSessionHooks: UserPromptSubmit and
// SessionStart context reach the model; SessionEnd runs at exit.
func TestPrint_UserPromptSubmitContextAndSessionHooks(t *testing.T) {
	env, _, errb, _ := sessionEnv(t, `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"ups"}]}],
		"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"start"}]}],
		"SessionEnd":[{"hooks":[{"type":"command","command":"end"}]}]}}`)
	var ended atomic.Int32
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.hookRunner = fakeHookRunner(func(cmd string, _ hooks.Payload) (int, string, string) {
		switch cmd {
		case "ups":
			return 0, "branch is main", ""
		case "start":
			return 0, "project uses tabs", ""
		case "end":
			ended.Add(1)
		}
		return 0, "", ""
	})
	if code := runPrintSession(env, s, "hello"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	r := sp.requests()[0]
	if !strings.Contains(r, "branch is main") || !strings.Contains(r, "project uses tabs") {
		t.Fatalf("hook context missing from the request: %s", r)
	}
	if ended.Load() != 1 {
		t.Fatalf("SessionEnd ran %d times, want 1", ended.Load())
	}
}

// TestPrint_StopHookForcesContinuation: a Stop hook that blocks once makes
// the turn continue with its reason; the second stop is allowed.
func TestPrint_StopHookForcesContinuation(t *testing.T) {
	env, out, errb, _ := sessionEnv(t, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"stop"}]}]}}`)
	var stops atomic.Int32
	sp := &scriptedProvider{respond: func(n int, _ string) string {
		if n == 0 {
			return textReply("first")
		}
		return textReply("second")
	}}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.hookRunner = fakeHookRunner(func(cmd string, p hooks.Payload) (int, string, string) {
		if cmd == "stop" && stops.Add(1) == 1 {
			return 2, "", "run the tests first"
		}
		return 0, "", ""
	})
	if code := runPrintSession(env, s, "do it"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	reqs := sp.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "run the tests first") {
		t.Fatalf("expected one Stop-hook continuation, got %d requests: %v", len(reqs), reqs)
	}
	if strings.TrimSpace(out.String()) != "second" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestPrint_AgentToolRunsSubagent: the model calls Agent; the subagent runs on
// a provider caller for the same backend (the scripted server), hands back a
// report, and the report returns to the parent.
func TestPrint_AgentToolRunsSubagent(t *testing.T) {
	env, out, errb, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(n int, body string) string {
		switch {
		case strings.Contains(body, "SubagentHandback") && strings.Contains(body, `"tool_call_id":"hb1"`):
			return textReply("child done")
		case strings.Contains(body, "SubagentHandback") && strings.Contains(body, "find the config loader"):
			return toolReply("hb1", "SubagentHandback", map[string]string{"message": "loader lives in settingsload"})
		case strings.Contains(body, "loader lives in settingsload"):
			return textReply("parent got report")
		default:
			return toolReply("a1", "Agent", map[string]string{"description": "find loader", "prompt": "find the config loader", "subagent_type": "general-purpose"})
		}
	}}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	if code := runPrintSession(env, s, "where is the loader?"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "parent got report" {
		t.Fatalf("stdout = %q; requests: %v", out.String(), sp.requests())
	}
	reqs := sp.requests()
	if !strings.Contains(reqs[0], `"name":"Agent"`) || !strings.Contains(reqs[0], `"name":"Skill"`) {
		t.Fatalf("Agent/Skill not advertised: %s", reqs[0])
	}
	// The child request uses the same provider with the parent model (openai
	// maps every alias to the parent).
	var child string
	for _, r := range reqs {
		if strings.Contains(r, "find the config loader") && strings.Contains(r, "SubagentHandback") {
			child = r
			break
		}
	}
	if child == "" || !strings.Contains(child, `"model":"parent-model"`) {
		t.Fatalf("subagent request missing or wrong model: %q", child)
	}
}

// TestPrint_SkillToolLoadsSkill: a project SKILL.md is listed and loaded by
// the Skill tool with its arguments substituted.
func TestPrint_SkillToolLoadsSkill(t *testing.T) {
	env, out, errb, cwd := sessionEnv(t, "")
	dir := filepath.Join(cwd, ".gocode", "skills", "greet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: greet\ndescription: Greets someone warmly\n---\nSay a warm hello to $ARGUMENTS.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedProvider{respond: func(n int, _ string) string {
		if n == 0 {
			return toolReply("s1", "Skill", map[string]string{"skill": "greet", "args": "Bob"})
		}
		return textReply("hello Bob")
	}}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	if code := runPrintSession(env, s, "greet bob"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	reqs := sp.requests()
	if !strings.Contains(reqs[0], "greet: Greets someone warmly") {
		t.Fatalf("skill not listed: %s", reqs[0])
	}
	if !strings.Contains(reqs[1], "Say a warm hello to Bob.") {
		t.Fatalf("skill content not loaded: %s", reqs[1])
	}
	if strings.TrimSpace(out.String()) != "hello Bob" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestSession_ModelCommandRetargetsProvider: /model switches the live
// provider model used by the next request.
func TestSession_WorkflowToolAdvertisedMainOnly(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("done")

	// Advertised to the main agent (reaches internal/workflows).
	found := false
	for _, sc := range rt.conv.ToolSchemas {
		if sc.Name == "Workflow" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Workflow tool not advertised in conv.ToolSchemas")
	}
	if rt.workflowTool == nil {
		t.Fatalf("workflowTool not wired on the session runtime")
	}

	// NOT part of the subagent tool roster (main-only, like AmendAgent), so a
	// workflow's own subagents never launch workflows.
	for _, n := range rt.agentTool.ToolNames {
		if n == "Workflow" {
			t.Fatalf("Workflow leaked into the subagent tool roster")
		}
	}
}

// TestSession_BrowserToolsAdvertised: the browser-control tools are wired
// into a built session's advertised tool schemas even when Chrome is not
// running (the client dials lazily), so the model is offered them and a call
// returns the not-connected error rather than failing session startup. An
// interactive session offers them unless --no-chrome is given; a
// non-interactive one only with --chrome (or GOCODE_CODE_ENABLE_CFC).
func TestSession_BrowserToolsAdvertised(t *testing.T) {
	cases := []struct {
		name        string
		interactive bool
		chrome      string
		want        bool
	}{
		{"interactive", true, "", true},
		{"interactive --no-chrome", true, "off", false},
		{"print", false, "", false},
		{"print --chrome", false, "on", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, _, _, _ := sessionEnv(t, "")
			sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
			s := openAISession(sp.server(t), toolreg.ModeDefault)
			s.Chrome = c.chrome
			caller, err := s.buildCaller()
			if err != nil {
				t.Fatal(err)
			}
			rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: c.interactive})
			defer rt.Close("done")

			have := map[string]bool{}
			for _, sc := range rt.conv.ToolSchemas {
				have[sc.Name] = true
			}
			for _, name := range []string{"navigate", "read_page", "get_page_text", "tabs_context", "tabs_create", "tabs_close", "screenshot", "computer", "find", "form_input", "browser_batch"} {
				if have[name] != c.want {
					t.Fatalf("browser tool %q advertised=%v, want %v", name, have[name], c.want)
				}
			}
			if have["AskUserQuestion"] != c.interactive {
				t.Fatalf("AskUserQuestion advertised=%v in interactive=%v session", have["AskUserQuestion"], c.interactive)
			}
		})
	}
}

func TestSession_ModelCommandRetargetsProvider(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	if _, err := rt.conv.Submit(context.Background(), "/model other-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.conv.Submit(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if r := sp.requests(); len(r) != 1 || !strings.Contains(r[0], `"model":"other-model"`) {
		t.Fatalf("request did not use the /model selection: %v", r)
	}
}

// TestInteractive_MCPNoticesAndCloseOnQuit: a failing MCP server is reported
// as a system message, and the host is closed when the program exits.
func TestInteractive_MCPNoticesAndCloseOnQuit(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	good := echoServer()
	bad := &memMCPServer{startErr: errors.New("boom")}
	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{
			Mode:      toolreg.ModeDefault,
			MCPConfig: []string{`{"mcpServers":{"good":{"command":"g"},"bad":{"command":"b"}}}`},
			mcpTransport: func(name string, cfg mcp.ServerConfig) (mcp.Transport, error) {
				if name == "bad" {
					return bad.factory()(name, cfg)
				}
				return good.factory()(name, cfg)
			},
			newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
	}
	done := make(chan int, 1)
	go func() { done <- runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, `"bad" failed to connect`)
	}, 2*time.Second) {
		t.Fatal("failed MCP server was not surfaced")
	}
	prog.Quit()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit")
	}
	if good.closed.Load() != 1 {
		t.Fatalf("good server closed %d times, want 1", good.closed.Load())
	}
}

// TestResolveSubagentModel covers the alias table.
func TestResolveSubagentModel(t *testing.T) {
	cases := []struct{ prov, req, want string }{
		{providerAnthropic, "sonnet", "claude-sonnet-5-5"},
		{providerAnthropic, "fable", "claude-fable-5-1"},
		{providerOpenRouter, "opus", "anthropic/claude-opus-5-5"},
		{providerOpenRouter, "haiku", "anthropic/claude-haiku-5-5"},
		{providerOpenAI, "opus", "parent"},
		{providerAnthropic, "", "parent"},
		{providerAnthropic, "inherit", "parent"},
		{providerAnthropic, "mystery", "parent"},
		{providerAnthropic, "claude-x-1", "claude-x-1"},
		{providerOpenRouter, "meta/llama-9", "meta/llama-9"},
	}
	for _, c := range cases {
		if got := resolveSubagentModel(c.prov, c.req, "parent"); got != c.want {
			t.Errorf("%s/%q = %q, want %q", c.prov, c.req, got, c.want)
		}
	}
}

// TestRunMCPList prints the server status.
func TestRunMCPList(t *testing.T) {
	env, out, _, _ := sessionEnv(t, "")
	mem := echoServer()
	s := &printSession{
		MCPConfig:    []string{`{"mcpServers":{"mem":{"command":"x"}}}`},
		mcpTransport: mem.factory(),
	}
	if code := runMCPListSession(env, s); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "mem: x  - \u2713 Connected") {
		t.Fatalf("stdout = %q", out.String())
	}
	if mem.closed.Load() != 1 {
		t.Fatal("mcp list did not close the host")
	}
}

// jsonString is s as a JSON string literal (Windows paths carry backslashes).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
