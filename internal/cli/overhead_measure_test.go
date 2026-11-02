package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
)

// captureCaller records every model request and answers with a fixed text.
type captureCaller struct {
	mu   sync.Mutex
	reqs []agent.ModelRequest
}

func (c *captureCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

func (c *captureCaller) first() agent.ModelRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[0]
}

// overhead is the fixed per-request prompt: the system prompt plus the tool
// definitions, in characters. reminderChars is the one-time deferred-tools
// reminder in the first user turn (part of the conversation, not resent as a
// prefix of its own).
type overhead struct {
	systemChars, toolChars, tools, reminderChars int
}

func (o overhead) total() int { return o.systemChars + o.toolChars }

func (o overhead) String() string {
	s := fmt.Sprintf("system %6d + tools %6d (%2d defs) = %6d chars ≈ %5d tokens",
		o.systemChars, o.toolChars, o.tools, o.total(), o.total()/4)
	if o.reminderChars > 0 {
		s += fmt.Sprintf("; one-time name reminder %d chars", o.reminderChars)
	}
	return s
}

func measureRequest(req agent.ModelRequest) overhead {
	b, _ := json.Marshal(req.Tools)
	o := overhead{systemChars: len(req.System), toolChars: len(b), tools: len(req.Tools)}
	for _, m := range req.Messages {
		for _, blk := range m.Content {
			if blk.AnnouncedTools != nil {
				o.reminderChars += len(blk.Text)
			}
		}
	}
	return o
}

// mcpServerWithTools serves n tools with realistic descriptions and schemas.
func mcpServerWithTools(server string, n int) *memMCPServer {
	var tools []mcp.Tool
	for i := 0; i < n; i++ {
		var schema mcp.ToolSchema
		_ = json.Unmarshal([]byte(`{"type":"object","properties":{"query":{"type":"string","description":"What to look up in the `+server+` service."},"limit":{"type":"number","description":"Maximum number of results to return."},"cursor":{"type":"string","description":"Pagination cursor from a previous call."}},"required":["query"]}`), &schema)
		tools = append(tools, mcp.Tool{
			Name:        fmt.Sprintf("action_%02d", i),
			Description: fmt.Sprintf("Performs operation %d against the %s service and returns the matching records as JSON. Use it when the user asks about %s data.", i, server, server),
			InputSchema: schema,
		})
	}
	return &memMCPServer{tools: tools}
}

// measureConfig selects the session a measurement builds.
type measureConfig struct {
	interactive bool
	// mcp is the number of MCP servers (ten tools each).
	mcp int
	// env overrides environment variables.
	env map[string]string
	// settings is the user settings.json content ("" for none).
	settings string
	// mode is the permission mode (default when empty).
	mode toolreg.Mode
}

// buildMeasuredSession builds a session for c with a capturing model caller.
// The caller closes the runtime.
func buildMeasuredSession(t *testing.T, c measureConfig) (*sessionRuntime, *captureCaller) {
	t.Helper()
	env, _, _, _ := sessionEnv(t, c.settings)
	if len(c.env) > 0 {
		base := env.Getenv
		env.Getenv = func(k string) string {
			if v, ok := c.env[k]; ok {
				return v
			}
			return base(k)
		}
	}
	mode := c.mode
	if mode == "" {
		mode = toolreg.ModeDefault
	}
	s := &printSession{Model: "gocode-sonnet-4-5", Provider: providerOpenRouter, Mode: mode}
	if c.mcp > 0 {
		cfg := `{"mcpServers":{`
		servers := map[string]*memMCPServer{}
		for i := 0; i < c.mcp; i++ {
			name := fmt.Sprintf("srv%d", i)
			servers[name] = mcpServerWithTools(name, 10)
			if i > 0 {
				cfg += ","
			}
			cfg += fmt.Sprintf(`%q:{"command":"unused"}`, name)
		}
		cfg += "}}"
		s.MCPConfig = []string{cfg}
		s.mcpTransport = multiMCPFactory(servers)
	}
	caller := &captureCaller{}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: c.interactive})
	return rt, caller
}

// firstRequest runs one prompt in the session c describes and returns the
// first model request.
func firstRequest(t *testing.T, c measureConfig) agent.ModelRequest {
	t.Helper()
	rt, caller := buildMeasuredSession(t, c)
	defer rt.Close("other")
	if _, err := rt.conv.Submit(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	return caller.first()
}

// measureSession builds a session (interactive or -p), optionally with MCP
// servers, runs one prompt and measures the first model request.
func measureSession(t *testing.T, interactive bool, mcpServers int, extraEnv map[string]string) overhead {
	t.Helper()
	return measureRequest(firstRequest(t, measureConfig{interactive: interactive, mcp: mcpServers, env: extraEnv}))
}

// promptSection is one "# Heading" section of a system prompt and its size.
type promptSection struct {
	name  string
	chars int
}

// systemSections splits a system prompt at its top-level "# " headings; the
// text before the first heading is reported as "(intro)".
func systemSections(system string) []promptSection {
	var out []promptSection
	name, start := "(intro)", 0
	for i := 0; i <= len(system); i++ {
		atHeading := i < len(system) && strings.HasPrefix(system[i:], "\n\n# ")
		if !atHeading && i < len(system) {
			continue
		}
		out = append(out, promptSection{name, i - start})
		if i == len(system) {
			break
		}
		heading := system[i+2:]
		if nl := strings.IndexByte(heading, '\n'); nl >= 0 {
			heading = heading[:nl]
		}
		name, start = heading, i
	}
	return out
}

// toolSizes is the serialized size of each tool definition, in request order.
func toolSizes(tools []agent.ToolSchema) []promptSection {
	out := make([]promptSection, 0, len(tools))
	for _, tool := range tools {
		b, _ := json.Marshal(tool)
		out = append(out, promptSection{tool.Name, len(b)})
	}
	return out
}

// printBreakdown prints the per-section and per-tool sizes of req.
func printBreakdown(name string, req agent.ModelRequest) {
	fmt.Printf("  %s system sections:\n", name)
	for _, s := range systemSections(req.System) {
		fmt.Printf("    %-44s %6d chars ≈ %5d tokens\n", s.name, s.chars, s.chars/4)
	}
	fmt.Printf("  %s tools:\n", name)
	for _, s := range toolSizes(req.Tools) {
		fmt.Printf("    %-44s %6d chars ≈ %5d tokens\n", s.name, s.chars, s.chars/4)
	}
}

// headlessOverheadCeiling bounds the fixed per-request prompt (system + tool
// definitions, tool search on) of a default -p session, in characters. It
// includes the scratchpad section (about 250 chars plus the directory path).
const headlessOverheadCeiling = 30400

// TestMeasureFixedOverhead reports the fixed per-request prompt overhead for
// the default -p tool set, the default interactive tool set, and interactive
// with two MCP servers of ten tools each, with tool search on (the default)
// and off, and checks the -p total stays under headlessOverheadCeiling. Run
// with GOCODE_PRINT_OVERHEAD=1 to print the table and the per-section and
// per-tool breakdown:
//
//	GOCODE_PRINT_OVERHEAD=1 go test ./internal/cli -run TestMeasureFixedOverhead -v
func TestMeasureFixedOverhead(t *testing.T) {
	off := map[string]string{"ENABLE_TOOL_SEARCH": "false"}
	print := os.Getenv("GOCODE_PRINT_OVERHEAD") != ""
	cases := []struct {
		name        string
		interactive bool
		mcp         int
	}{
		{"-p default", false, 0},
		{"interactive default", true, 0},
		{"interactive + 2 MCP x 10 tools", true, 2},
	}
	for _, c := range cases {
		before := measureSession(t, c.interactive, c.mcp, off)
		req := firstRequest(t, measureConfig{interactive: c.interactive, mcp: c.mcp})
		after := measureRequest(req)
		if print {
			fmt.Printf("%-32s off: %s\n%-32s on:  %s  (saved %d chars ≈ %d tokens)\n",
				c.name, before, "", after, before.total()-after.total(), (before.total()-after.total())/4)
			printBreakdown(c.name, req)
		}
		if after.total() >= before.total() {
			t.Errorf("%s: tool search did not reduce the overhead: off %v, on %v", c.name, before, after)
		}
		if !c.interactive && after.total() > headlessOverheadCeiling {
			t.Errorf("%s: fixed overhead %d chars exceeds the ceiling %d: %v", c.name, after.total(), headlessOverheadCeiling, after)
		}
	}
}

// multiMCPFactory serves each configured server name from its own in-memory
// server.
func multiMCPFactory(servers map[string]*memMCPServer) mcphost.TransportFactory {
	return func(name string, cfg mcp.ServerConfig) (mcp.Transport, error) {
		srv, ok := servers[name]
		if !ok {
			return nil, fmt.Errorf("unknown server %q", name)
		}
		return srv.factory()(name, cfg)
	}
}

// headlessGrepViaBashOverheadCeiling bounds the -p fixed prompt when `grep`
// in Bash runs ugrep (no Grep tool definition), in characters.
const headlessGrepViaBashOverheadCeiling = 28900

// TestGrepViaBashShrinksOverhead: when `grep` in Bash runs ugrep the Grep
// tool is not sent, so the fixed per-request prompt is smaller than the
// default one and stays under its own -p ceiling.
func TestGrepViaBashShrinksOverhead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shadow needs Git Bash on Windows")
	}
	bin := filepath.Join(t.TempDir(), "ugrep")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, interactive := range []bool{false, true} {
		plain := measureRequest(firstRequest(t, measureConfig{interactive: interactive}))
		viaBash := measureRequest(firstRequest(t, measureConfig{interactive: interactive, env: map[string]string{"GOCODE_UGREP_PATH": bin}}))
		if viaBash.total() >= plain.total() || viaBash.tools != plain.tools-1 {
			t.Errorf("interactive=%v: grep via Bash %v, default %v", interactive, viaBash, plain)
		}
		if !interactive && viaBash.total() > headlessGrepViaBashOverheadCeiling {
			t.Errorf("-p with grep via Bash: %d chars exceeds the ceiling %d", viaBash.total(), headlessGrepViaBashOverheadCeiling)
		}
		if os.Getenv("GOCODE_PRINT_OVERHEAD") != "" {
			fmt.Printf("interactive=%v default %s\ninteractive=%v grep via Bash %s\n", interactive, plain, interactive, viaBash)
		}
	}
}
