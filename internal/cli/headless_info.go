package cli

import (
	"context"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/provider"
)

// The informational parts of the control protocol: initialize's models and
// account, and get_context_usage's per-category breakdown. Like /status and
// /context (sysinfo.go) they come from local session state; the only network
// call is the model catalog of a provider gocode has no alias table for.

// modelListTimeout bounds initialize's model catalog fetch.
const modelListTimeout = 5 * time.Second

// initModels is initialize's models (the SDK's ModelInfo: value,
// displayName, description, resolvedModel): "default" (the model the session
// started with), then the provider's Claude family aliases (as full ids,
// which set_model takes), or — for a provider without them — its model
// catalog. The availableModels policy filters the list as it does the
// /model picker.
func (h *headlessRun) initModels() []map[string]any {
	prov := h.s.Provider
	if prov == "" {
		prov = providerOpenRouter
	}
	policy := loadModelPolicy(h.env, prov)
	start := h.s.Model
	out := []map[string]any{{
		"value": "default", "displayName": "Default",
		"description": "The model the session started with", "resolvedModel": start,
	}}
	if start == "" {
		out[0]["description"] = "The provider's default model"
		delete(out[0], "resolvedModel")
	}
	var models []provider.ModelInfo
	if aliases := subagentModelAliases[prov]; aliases != nil {
		for _, fam := range modelFamilies {
			if id := aliases[fam]; id != "" {
				name := strings.ToUpper(fam[:1]) + fam[1:]
				models = append(models, provider.ModelInfo{ID: id, Name: name, Description: "Claude " + name + " (" + id + ")"})
			}
		}
	} else if b, err := h.s.backend(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), modelListTimeout)
		models, _ = provider.ListModels(ctx, b)
		cancel()
	}
	for _, m := range policy.pickerModels(models) {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		desc := m.Description
		if desc == "" {
			desc = m.ID
		}
		out = append(out, map[string]any{"value": m.ID, "displayName": name, "description": desc, "resolvedModel": m.ID})
	}
	return out
}

// initAccount is initialize's account: gocode has no account, only the
// provider and where its API key came from (an environment variable's name,
// the saved key file, settings, or apiKeyHelper) — never the key.
func (h *headlessRun) initAccount() map[string]any {
	prov := h.s.Provider
	if prov == "" {
		prov = providerOpenRouter
	}
	source := h.s.KeySource
	if source == "" {
		source = string(auth.SourceNone)
	}
	return map[string]any{"provider": prov, "apiKeySource": source}
}

// contextBreakdown is get_context_usage's categories, memoryFiles and
// mcpTools for the session between turns. The per-part numbers are the
// /context estimates (≈4 chars per token); Messages is what the measured
// total leaves after them, and Free space what the window leaves after that.
func (h *headlessRun) contextBreakdown(total, window int) (categories, memoryFiles, mcpTools []map[string]any) {
	conv := h.rt.conv
	mem := approxTokens(h.rt.memory.block)
	sys := max(approxTokens(conv.System)-mem, 0)
	builtin, mcp := 0, 0
	sent, _ := agent.SelectTools(conv.ToolSchemas, conv.History)
	for _, ts := range sent {
		n := approxTokens(ts.Name) + approxTokens(ts.Description) + approxTokens(string(ts.InputSchema))
		if server := mcpServerOfToolName(ts.Name); server != "" {
			mcp += n
			mcpTools = append(mcpTools, map[string]any{"name": ts.Name, "serverName": server, "tokens": n, "isLoaded": true})
			continue
		}
		builtin += n
	}
	msgs := max(total-sys-builtin-mcp-mem, 0)
	categories = []map[string]any{
		category("system_prompt", "System prompt", sys, "used"),
		category("system_tools", "System tools", builtin, "used"),
	}
	if mcp > 0 {
		categories = append(categories, category("mcp_tools", "MCP tools", mcp, "used"))
	}
	if mem > 0 {
		categories = append(categories, category("memory_files", "Memory files", mem, "used"))
	}
	categories = append(categories, category("messages", "Messages", msgs, "used"))
	if window > 0 {
		categories = append(categories, category("free_space", "Free space", max(window-total, 0), "free"))
	}
	for _, f := range h.rt.memory.files {
		memoryFiles = append(memoryFiles, map[string]any{"path": f.Path, "type": string(f.Kind), "tokens": approxTokens(f.Content)})
	}
	return categories, memoryFiles, mcpTools
}

// categoryColors are the colors a host may draw each category with.
var categoryColors = map[string]string{
	"system_prompt": "gray", "system_tools": "blue", "mcp_tools": "cyan",
	"memory_files": "yellow", "messages": "purple", "free_space": "dim",
}

func category(id, name string, tokens int, kind string) map[string]any {
	return map[string]any{"id": id, "name": name, "tokens": tokens, "color": categoryColors[id], "kind": kind}
}

// mcpServerOfToolName is the server of an mcp__server__tool name, "" for
// any other tool.
func mcpServerOfToolName(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}
