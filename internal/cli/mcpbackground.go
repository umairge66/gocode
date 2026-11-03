package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// mcpBackgroundConnect selects the MCP servers the session connects in the
// background, so the first turn is not held up by a slow server: those whose
// tools are deferred behind tool search, which the first request does not
// carry in full anyway. ToolSearch waits for them before reporting a miss.
//
// The rule is static, because the host starts before any request is built:
//
//   - tool search on (ENABLE_TOOL_SEARCH unset/true/auto:0/force): every
//     server without "alwaysLoad" connects in the background;
//   - auto mode: every server connects in the foreground. Whether auto mode
//     defers depends on the size of all deferrable definitions, MCP ones
//     included, which is only known once the servers have listed their tools;
//   - off, or ToolSearch disallowed with --disallowedTools: foreground (the
//     tools are sent in full from the first request).
//
// A nil result connects every server in the foreground.
func mcpBackgroundConnect(env *Env, s *printSession) func(string, mcp.ServerConfig) bool {
	if toolsearch.ResolveMode(toolSearchGetenv(env)) != toolsearch.ModeOn {
		return nil
	}
	if s != nil {
		for _, raw := range s.DisallowedTools {
			for _, t := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
				if t == string(protocol.ToolSearch) {
					return nil
				}
			}
		}
	}
	return func(_ string, cfg mcp.ServerConfig) bool { return !cfg.AlwaysLoad }
}

// mcpStatusNotice is the user-facing line for a server that did not connect
// (failed, or needs sign-in), or "" for any other state.
func mcpStatusNotice(st mcphost.ServerStatus, interactive bool) string {
	switch st.State {
	case mcphost.StateFailed:
		return fmt.Sprintf("MCP server %q failed to connect: %s", st.Name, st.Error)
	case mcphost.StateNeedsAuth:
		if interactive {
			return fmt.Sprintf("MCP server %q needs authentication — run /mcp to authenticate", st.Name)
		}
		return fmt.Sprintf("MCP server %q needs authentication — %s", st.Name, mcpNeedsAuthHint)
	}
	return ""
}

// mcpToolRefresher is the conversation's RefreshTools: when the MCP host's
// tool table changed (a background server finished connecting, a server
// dropped, tools/list_changed), it applies that change to the session's tool
// list — MCP tools that appeared are added with the dispatcher's schema (its
// deferral marks), those that went away are dropped, the rest is kept as is.
// Only the change is applied, so a list a caller narrowed (a tool withheld by
// name) stays narrowed. schemas is the dispatcher's current list.
func mcpToolRefresher(h *mcphost.Host, schemas func() []agent.ToolSchema) func([]agent.ToolSchema) []agent.ToolSchema {
	if h == nil || schemas == nil {
		return nil
	}
	var mu sync.Mutex
	ver := h.ToolsVersion()
	known := mcpToolNames(h)
	return func(cur []agent.ToolSchema) []agent.ToolSchema {
		mu.Lock()
		defer mu.Unlock()
		v := h.ToolsVersion()
		if v == ver {
			return cur
		}
		now := mcpToolNames(h)
		fresh := map[string]agent.ToolSchema{}
		var added []agent.ToolSchema
		for _, s := range schemas() {
			if !now[s.Name] {
				continue
			}
			fresh[s.Name] = s
			if !known[s.Name] {
				added = append(added, s)
			}
		}
		out := make([]agent.ToolSchema, 0, len(cur)+len(added))
		have := map[string]bool{}
		for _, s := range cur {
			switch {
			case known[s.Name] && !now[s.Name]:
				continue // the server dropped it
			case now[s.Name]:
				if f, ok := fresh[s.Name]; ok {
					s = f // its definition may have changed
				}
			}
			have[s.Name] = true
			out = append(out, s)
		}
		for _, s := range added {
			if !have[s.Name] {
				out = append(out, s)
			}
		}
		ver, known = v, now
		return out
	}
}

// mcpToolNames is the set of the host's model-facing tool names.
func mcpToolNames(h *mcphost.Host) map[string]bool {
	out := map[string]bool{}
	for _, s := range h.Tools() {
		out[s.Name] = true
	}
	return out
}

// reportBackgroundMCP delivers the outcome of each background MCP connect: a
// failure as a notice (notify), and every outcome to refresh (the interactive
// sidebar; nil in -p). Connects that settled before the call are reported at
// once. A server still connecting at startup gets no startup notice
// (mcpNotices skips it; /mcp shows it as "connecting…"), so this is where its
// failure surfaces.
func (r *sessionRuntime) reportBackgroundMCP(interactive bool, notify func(string), refresh func()) {
	if r == nil || r.mcp == nil {
		return
	}
	r.mcp.OnBackgroundSettled(func(st mcphost.ServerStatus) {
		if n := mcpStatusNotice(st, interactive); n != "" && notify != nil {
			notify(n)
		}
		if refresh != nil {
			refresh()
		}
	})
}
