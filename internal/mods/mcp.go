package mods

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

// MCPHost is the session's MCP host as $.mcp reaches it (mcphost.Host has
// these methods): the connected tools, a call, and the servers' states.
type MCPHost interface {
	Tools() []agent.ToolSchema
	Call(ctx context.Context, modelToolName string, input json.RawMessage) (text string, isError bool, err error)
	ServerStates() []MCPServerState
}

// MCPServerState is one server as /mcp lists it.
type MCPServerState struct {
	// Name is the server's key ("plugin:<plugin>:<server>" for a plugin's).
	Name string
	// State is pending, connected, failed, needs-auth, reconnecting,
	// disconnected or closed.
	State string
	Error string
}

// mcpNameUnsafe: what is not [a-zA-Z0-9_-] becomes _.
var mcpNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func mcpNormalize(s string) string { return mcpNameUnsafe.ReplaceAllString(s, "_") }

// mcpToolCandidates: the model-facing names a server's tool may
// go by (the server's name as /mcp lists it, or its tool-name spelling).
func mcpToolCandidates(server, tool string) map[string]bool {
	return map[string]bool{
		"mcp__" + mcpNormalize(server) + "__" + tool:               true,
		"mcp__" + server + "__" + tool:                             true,
		"mcp__" + mcpNormalize(server) + "__" + mcpNormalize(tool): true,
	}
}

// mcpCallCheck and mcpConnectCheck are the ops table's checks.
func mcpCallCheck(a map[string]any) string {
	_, sok := a["server"].(string)
	_, tok := a["tool"].(string)
	if !sok || !tok {
		return "takes { server, tool, args? }"
	}
	if v, has := a["args"]; has && v != nil {
		if _, ok := v.(map[string]any); !ok {
			return "takes { server, tool, args? }"
		}
	}
	return ""
}

func mcpConnectCheck(a map[string]any) string {
	if s, ok := a["server"].(string); !ok || s == "" {
		return "takes { server } (a non-empty string)"
	}
	return ""
}

func (en *Engine) mcpHost() MCPHost {
	if h := en.opts.Host; h != nil && h.MCP != nil {
		return h.MCP()
	}
	return nil
}

// mcpCall: the tool on one of the session's connected servers,
// with the session's own connection and credentials. The call is the
// plugin's tool.call, seen by the hooks above it: they may refuse
// or answer it; beneath them the tool runs. No permission prompt: the
// plugin's call, seen by those hooks, is the grant.
func (en *Engine) mcpCall(ctx context.Context, s *seat, reg *registration, e map[string]any) (any, error) {
	server, _ := e["server"].(string)
	tool, _ := e["tool"].(string)
	args, _ := e["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	h := en.mcpHost()
	var tools []agent.ToolSchema
	if h != nil {
		tools = h.Tools()
	}
	want := mcpToolCandidates(server, tool)
	name := ""
	for _, t := range tools {
		if want[t.Name] {
			name = t.Name
			break
		}
	}
	if name == "" {
		servers := map[string]bool{}
		for _, t := range tools {
			if parts := strings.Split(t.Name, "__"); len(parts) > 2 && parts[0] == "mcp" {
				servers[parts[1]] = true
			}
		}
		list := sortedKeys(servers)
		with := "none"
		if len(list) > 0 {
			with = strings.Join(list, ", ")
		}
		en.logf("$.mcp.call (%s): no tool %q on a server named %q; servers with tools: %s", s.name, tool, server, with)
		return nil, fmt.Errorf("%s: $.mcp.call: no connected MCP tool %q on a server named %q", s.name, tool, server)
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	shown := strings.Join(keys, ", ")
	if shown == "" {
		shown = "no args"
	}
	en.logf("$.mcp.call (%s): %s (%s)", s.name, name, shown)
	start := time.Now()
	raw, _ := json.Marshal(args)
	call := agent.ToolUse{ID: "mcp_" + newCallID(), Name: name, Input: raw}
	core := func(ctx context.Context, p map[string]any) (map[string]any, error) {
		input, err := json.Marshal(toolInput(p))
		if err != nil {
			return nil, err
		}
		text, isErr, err := h.Call(ctx, name, input)
		if err != nil {
			return map[string]any{"ref": float64(1), "result": "Error: " + err.Error(), "text": "Error: " + err.Error(), "isError": true}, nil
		}
		out := map[string]any{"ref": float64(1), "result": text, "text": text}
		if isErr {
			out["isError"] = true
		}
		return out, nil
	}
	res, err := en.dispatch(ctx, "tool.call", toolEnvelope(call), core, Origin{Plugin: s.name, Tier: s.tier}, reg)
	if err != nil {
		return nil, err
	}
	if d, ok := res["deny"].(string); ok {
		return nil, fmt.Errorf("%s: $.mcp.call(%s, %s) refused: %s", s.name, server, tool, d)
	}
	var content []any
	if arr, ok := res["result"].([]any); ok {
		content = arr
	} else {
		t, _ := res["text"].(string)
		if t == "" {
			t = resultText(res["result"])
		}
		content = []any{map[string]any{"type": "text", "text": t}}
	}
	isErr := res["isError"] == true
	en.logf("$.mcp.call (%s): %s answered in %dms: %d block(s)%s", s.name, name, time.Since(start).Milliseconds(), len(content), map[bool]string{true: ", isError"}[isErr])
	return map[string]any{"content": content, "isError": isErr}, nil
}

// mcpConnectWait is how long $.mcp.connect waits on a server still
// connecting.
const mcpConnectWait = 30 * time.Second

// mcpConnect: one of the servers the plugin's own manifest
// lists, connected when the session has it connected; a refusal resolves
// { isConnected: false, refused, message }. A plugin's listed servers are
// started with the session, so connecting is waiting for the one the
// session holds under "plugin:<plugin>:<key>" to settle.
func (en *Engine) mcpConnect(ctx context.Context, s *seat, e map[string]any) (any, error) {
	key, _ := e["server"].(string)
	refuse := func(kind, msg string) (any, error) {
		en.logf("$.mcp.connect (%s): %s: %s", s.name, kind, msg)
		return map[string]any{"isConnected": false, "refused": kind, "message": msg}, nil
	}
	h := en.mcpHost()
	name := "plugin:" + s.name + ":" + key
	find := func() (MCPServerState, bool) {
		if h == nil {
			return MCPServerState{}, false
		}
		for _, st := range h.ServerStates() {
			if st.Name == name {
				return st, true
			}
		}
		return MCPServerState{}, false
	}
	st, ok := find()
	if !ok {
		return refuse("unlisted", fmt.Sprintf("This plugin's manifest lists no MCP server named %q.", key))
	}
	deadline := time.Now().Add(mcpConnectWait)
	for st.State == "pending" || st.State == "reconnecting" {
		if time.Now().After(deadline) {
			return refuse("failed", fmt.Sprintf("The MCP server %q is still connecting after %dms; it stays in the session.", name, mcpConnectWait.Milliseconds()))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if st, ok = find(); !ok {
			return refuse("failed", fmt.Sprintf("The MCP server %q is not in this session.", name))
		}
	}
	switch st.State {
	case "connected":
		en.logf("$.mcp.connect (%s): %s: connected", s.name, name)
		return map[string]any{"isConnected": true, "server": name}, nil
	case "needs-auth":
		return refuse("auth", fmt.Sprintf("The MCP server %q needs you to sign in; run /mcp to authenticate.", name))
	case "failed":
		why := st.Error
		if why == "" {
			why = "no reason given"
		}
		return refuse("failed", fmt.Sprintf("The MCP server %q did not connect: %s.", name, why))
	}
	return refuse("failed", fmt.Sprintf("The MCP server %q is turned off in this session.", name))
}
