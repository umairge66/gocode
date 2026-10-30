package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/projectmemory"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/systemprompt"
	"github.com/umairge66/gocode/internal/toolreg"
)

// projectMemoryHeader prefixes the loaded gocode.md memory where it is appended
// to a system prompt. It is appended under a clear header after the base
// prompt (identity + guidance + env), giving the layout
// base + project-memory + skills.
const projectMemoryHeader = "# Project memory"

// sessionMemory is the memory a session loaded at startup.
type sessionMemory struct {
	// block is the gocode.md memory (with its header) appended to the main
	// system prompt, or "".
	block string
	// managedBlock is the Managed-tier part of block (with its header), for
	// agent definitions with omitgocodeMd, or "".
	managedBlock string
	// nested hands out the conditional rules and nested-directory memory as
	// files are touched; nil when memory is disabled.
	nested *projectmemory.Nested
}

// disableMemoryEnv turns off gocode.md memory and rules entirely.
const disableMemoryEnv = "GOCODE_CODE_DISABLE_GOCODE_MDS"

// buildSystemPrompt assembles the main agent's base system prompt and loads the
// project memory for this session. It returns the full base+memory system
// prompt (what conv.System is set to) and the loaded memory (what the
// subagent MemoryFor seam and the nested-memory hook use), so both the main
// agent and its subagents carry the same gocode.md memory and rules.
//
// Everything that varies at runtime — cwd, git state, platform, date, model,
// provider, and the session facts in feat — is resolved here and handed to the
// pure systemprompt.Build; memory is read through projectmemory.Load on the
// real filesystem. No telemetry, no network.
func buildSystemPrompt(ctx context.Context, env *Env, s *printSession, paths sessionPaths, feat promptFeatures) (system string, memory sessionMemory) {
	info := systemprompt.Info{
		Cwd:             paths.cwd,
		Platform:        runtime.GOOS,
		Shell:           shellName(env),
		Date:            time.Now().Format("2006-01-02"),
		Model:           sessionModelID(s),
		Provider:        sessionProviderName(s),
		Headless:        feat.headless,
		BrowserTools:    feat.browserTools,
		Hooks:           feat.hooks,
		SkipPermissions: s != nil && s.Mode == toolreg.ModeBypassPermissions,
		GrepViaBash:     feat.grepViaBash,
	}
	if paths.cwd != "" {
		info.IsGitRepo = isGitRepo(paths.cwd)
		if info.IsGitRepo {
			info.GitBranch = cheapGitBranch(ctx, paths.cwd)
		}
	}
	system = systemprompt.Build(info)

	if envutil.EnvTruthy(envGet(env)(disableMemoryEnv)) {
		return system, memory
	}
	cfg := projectmemory.Config{
		Cwd:        paths.cwd,
		Home:       paths.home,
		ConfigDir:  paths.configDir,
		ManagedDir: settingsContext(env).ManagedBaseDir(),
		FS:         fsfacade.OSFS{},
	}
	mem := projectmemory.Load(cfg)
	if text := strings.TrimSpace(mem.Text); text != "" {
		memory.block = projectMemoryHeader + "\n\n" + text
	}
	if managed := strings.TrimSpace(projectmemory.RenderEntries(projectmemory.ManagedOnly(mem.Entries))); managed != "" {
		memory.managedBlock = projectMemoryHeader + "\n\n" + managed
	}
	memory.nested = projectmemory.NewNested(cfg, mem.Sources)

	if memory.block != "" {
		system += "\n\n" + memory.block
	}
	return system, memory
}

// promptFeatures are the session facts that select optional system prompt
// guidance.
type promptFeatures struct {
	// headless is a non-interactive session (-p, stream-json, plugin eval).
	headless bool
	// browserTools: the built-in browser tools are offered.
	browserTools bool
	// hooks: at least one hook (settings, plugin, or hooks module) is
	// configured.
	hooks bool
	// grepViaBash: content search is `grep` in the Bash tool (ugrep), and the
	// Grep tool is not offered.
	grepViaBash bool
}

// envGet returns the session's env lookup.
func envGet(env *Env) func(string) string {
	if env != nil && env.Getenv != nil {
		return env.Getenv
	}
	return os.Getenv
}

// agentMemoryEnabled reports whether persistent agent memory is on: not in
// simple mode, not when GOCODE_CODE_DISABLE_AUTO_MEMORY is truthy (a falsy
// value such as "0" forces it on), and not when the autoMemoryEnabled setting
// is false.
func agentMemoryEnabled(env *Env) bool {
	get := envGet(env)
	if envutil.EnvTruthy(get("GOCODE_CODE_SIMPLE")) {
		return false
	}
	if v := get("GOCODE_CODE_DISABLE_AUTO_MEMORY"); envutil.EnvTruthy(v) {
		return false
	} else if envutil.EnvFalsy(v) {
		return true
	}
	if on, ok := mergedEffectiveSettings(env)["autoMemoryEnabled"].(bool); ok {
		return on
	}
	return true
}

// subagentMemoryFor returns the Agent tool's MemoryFor seam: an agent with a
// `memory` scope first gets its "Persistent Agent Memory" section, then the
// session's gocode.md memory unless the definition omits it (built-in and
// policy agents drop it entirely; others keep the managed instructions).
func subagentMemoryFor(env *Env, paths sessionPaths, mem sessionMemory) func(subagent.AgentType) string {
	enabled := agentMemoryEnabled(env)
	extra := envGet(env)("GOCODE_COWORK_MEMORY_EXTRA_GUIDELINES")
	return func(a subagent.AgentType) string {
		var parts []string
		if enabled && projectmemory.ValidAgentMemoryScope(a.Memory) {
			parts = append(parts, projectmemory.AgentMemoryPrompt(projectmemory.AgentMemoryConfig{
				AgentType:      a.Name,
				Scope:          a.Memory,
				ConfigDir:      paths.configDir,
				Cwd:            paths.cwd,
				ExtraGuideline: extra,
				FS:             fsfacade.OSFS{},
				MkdirAll:       func(dir string) error { return os.MkdirAll(dir, 0o755) },
			}))
		}
		project := mem.block
		if a.OmitProjectMemory {
			project = mem.managedBlock
			if a.BuiltIn || a.Source == "policySettings" {
				project = ""
			}
		}
		if project != "" {
			parts = append(parts, project)
		}
		return strings.Join(parts, "\n\n")
	}
}

// nestedMemoryHooks wraps the loop hooks so that the files the agent reads,
// edits or writes bring in their conditional rules and nested-directory
// memory: PostToolUse records the touched path, and at the end of the
// tool-result round each new source is appended as a system-reminder user
// message.
type nestedMemoryHooks struct {
	inner  agent.PermissionHook
	nested *projectmemory.Nested
	cwd    string

	mu      sync.Mutex
	touched []string
}

var (
	_ agent.PermissionHook = (*nestedMemoryHooks)(nil)
	_ agent.TurnEndHook    = (*nestedMemoryHooks)(nil)
)

// withNestedMemory wraps inner; a nil tracker returns inner unchanged.
func withNestedMemory(inner agent.PermissionHook, nested *projectmemory.Nested, cwd string) agent.PermissionHook {
	if nested == nil {
		return inner
	}
	return &nestedMemoryHooks{inner: inner, nested: nested, cwd: cwd}
}

func (h *nestedMemoryHooks) PreToolUse(ctx context.Context, call agent.ToolUse) (agent.PermissionDecision, error) {
	if h.inner == nil {
		return agent.PermissionDecision{Behavior: agent.BehaviorAllow}, nil
	}
	return h.inner.PreToolUse(ctx, call)
}

func (h *nestedMemoryHooks) PostToolUse(ctx context.Context, call agent.ToolUse, result agent.ToolResult) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, call, result)
	}
	if result.IsError {
		return
	}
	switch protocol.Tool(call.Name) {
	case protocol.ToolRead, protocol.ToolEdit, protocol.ToolWrite:
	default:
		return
	}
	var in struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(call.Input, &in) != nil || strings.TrimSpace(in.FilePath) == "" {
		return
	}
	p := in.FilePath
	if !filepath.IsAbs(p) && h.cwd != "" {
		p = filepath.Join(h.cwd, p)
	}
	h.mu.Lock()
	h.touched = append(h.touched, p)
	h.mu.Unlock()
}

func (h *nestedMemoryHooks) OnTurnEnd(ctx context.Context, messages []agent.Message) (bool, []agent.Message) {
	cont := true
	var extra []agent.Message
	if th, ok := h.inner.(agent.TurnEndHook); ok {
		cont, extra = th.OnTurnEnd(ctx, messages)
	}
	h.mu.Lock()
	touched := h.touched
	h.touched = nil
	h.mu.Unlock()
	if !cont {
		return cont, extra
	}
	var blocks []agent.ContentBlock
	for _, p := range touched {
		for _, e := range h.nested.ForFile(p) {
			blocks = append(blocks, agent.TextBlock("<system-reminder>\n"+projectmemory.RenderNested(e)+"\n</system-reminder>"))
		}
	}
	if len(blocks) > 0 {
		extra = append(extra, agent.UserMessage(blocks...))
	}
	return cont, extra
}

// sessionModelID returns the configured model id, or "" when none is set.
func sessionModelID(s *printSession) string {
	if s == nil {
		return ""
	}
	return s.Model
}

// sessionProviderName returns the active provider name, defaulting to
// OpenRouter when the session did not select one (matching newCaller).
func sessionProviderName(s *printSession) string {
	if s != nil && s.Provider != "" {
		return s.Provider
	}
	return providerOpenRouter
}

// shellName returns the base name of the user's shell from $SHELL, or "" when
// unknown. It never fails and performs no I/O beyond an env read.
func shellName(env *Env) string {
	get := envGet(env)
	sh := strings.TrimSpace(get("SHELL"))
	if sh == "" {
		return ""
	}
	return filepath.Base(sh)
}

// isGitRepo reports whether dir is inside a git working tree, detected offline
// by walking ancestors for a .git entry (file or directory — a worktree/submodule
// uses a .git file). It runs no subprocess, so it never prompts or blocks.
func isGitRepo(dir string) bool {
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}
