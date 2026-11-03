package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/mods"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// modsDeps is what the hooks modules' host reaches beyond the settings: the
// session's provider and model (for $.model.*), its environment and the
// live model caller.
type modsDeps struct {
	session *printSession
	env     *Env
	live    liveCaller
	// addDirs are the session's --add-dir directories; with
	// permissions.additionalDirectories they widen $.fs's reach.
	addDirs []string
}

// buildModsSession loads the hooks modules of the enabled plugins (the
// plugin hooks-module engine, internal/mods) for a session. It returns nil
// when no module loads; a module that does not load becomes a notice.
//
// The engine sends nothing anywhere: $.telemetry.* are local no-ops,
// $.http.fetch fetches only what a module declared and asked for, and
// $.model.* run on the session's own provider, model and key.
func buildModsSession(r *sessionRuntime, ordered []settingsload.OrderedResult, enabled []*plugins.Plugin, paths sessionPaths, sessionID string, interactive bool, dataDir func(*plugins.Plugin) string, deps modsDeps) *mods.Session {
	if len(enabled) == 0 {
		return nil
	}
	var policy, user, flag map[string]any
	merged := map[string]any{}
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		switch or.Source {
		case settingspolicy.SourcePolicy:
			policy = or.Result.Settings
		case settingspolicy.SourceUser:
			user = or.Result.Settings
		case settingspolicy.SourceFlag:
			flag = or.Result.Settings
		}
		for k, v := range or.Result.Settings {
			merged[k] = v
		}
	}
	// A plugin's option values: user, --settings and managed settings
	// (project settings are not read), sensitive ones from the secure store
	// in the config dir.
	optionSources := plugins.OptionSources{Settings: []map[string]any{user, flag, policy}}
	if paths.configDir != "" {
		optionSources.SecretsFile = filepath.Join(paths.configDir, plugins.PluginSecretsFile)
	}
	policyEnabled, _ := policy["enabledPlugins"].(map[string]any)
	refs := make([]mods.PluginRef, 0, len(enabled))
	for _, p := range enabled {
		ref := mods.PluginRef{Name: p.Name(), ID: p.ID(), Root: p.Root}
		ref.Managed = p.Origin == plugins.OriginManaged || policyEnabled[p.ID()] == true
		if dataDir != nil {
			ref.DataDir = dataDir(p)
		}
		opts, notes, err := plugins.OptionValues(p, optionSources)
		for _, n := range notes {
			modsDebugf("%s", n)
		}
		ref.Options, ref.OptionsErr = opts, err
		refs = append(refs, ref)
	}
	sources := map[string]map[string]any{}
	for _, or := range ordered {
		if or.Result.LoadState == settingspolicy.LoadLoaded {
			sources[strings.TrimSuffix(string(or.Source), "Settings")] = or.Result.Settings
		}
	}
	policyEnv, _ := policy["env"].(map[string]any)
	var workDirs []string
	if deps.env != nil {
		workDirs = additionalWorkingDirs(deps.env, deps.addDirs)
	}
	host := &mods.Host{
		Cwd:            func() string { return paths.cwd },
		AdditionalDirs: func() []string { return workDirs },
		SessionID:      func() string { return sessionID },
		Version:        Version,
		Interactive:    interactive,
		Settings: func(source string) (map[string]any, error) {
			if source == "" {
				return merged, nil
			}
			return sources[source], nil
		},
		PinnedEnv: func(name string) bool {
			for k := range policyEnv {
				if strings.EqualFold(k, name) {
					return true
				}
			}
			return false
		},
		Notify: func(plugin, kind, text string) {
			if kind == "log" || kind == "status" {
				modsDebugf("[%s] $.ui.%s: %s", plugin, kind, text)
				return
			}
			r.emit(fmt.Sprintf("[%s] %s", plugin, text))
		},
		Models: modsModelHost(r, deps),
		MCP: func() mods.MCPHost {
			if r.mcp == nil {
				return nil
			}
			return modsMCP{h: r.mcp}
		},
	}
	s, errs := mods.LoadSession(mods.SessionConfig{
		Plugins:        refs,
		PolicySettings: policy,
		UserSettings:   user,
		MergedSettings: merged,
		Interactive:    interactive,
		Host:           host,
		Logf:           modsDebugf,
	})
	for _, err := range errs {
		r.notices = append(r.notices, "Hooks module: "+err.Error())
	}
	return s
}

// modsMCP is the session's MCP host as $.mcp reaches it.
type modsMCP struct{ h *mcphost.Host }

func (m modsMCP) Tools() []agent.ToolSchema { return m.h.Tools() }

func (m modsMCP) Call(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	return m.h.Call(ctx, name, input)
}

func (m modsMCP) ServerStates() []mods.MCPServerState {
	st := m.h.Status()
	out := make([]mods.MCPServerState, 0, len(st))
	for _, s := range st {
		out = append(out, mods.MCPServerState{Name: s.Name, State: string(s.State), Error: s.Error})
	}
	return out
}

// modsModelHost serves $.model.* on the session's own provider backend, with
// its key: an alias resolves as a subagent's model does, a model outside
// the availableModels allowlist is refused, and every call is logged under
// the plugin that made it. Nothing reaches a gocode-hosted service.
func modsModelHost(r *sessionRuntime, deps modsDeps) mods.ModelHost {
	s := deps.session
	prov := ""
	defaultModel := ""
	if s != nil {
		prov, defaultModel = s.Provider, s.Model
	}
	// The same model policy (availableModels and the managed rules) a
	// skill's or subagent's model passes.
	gate := skillModelGate{provider: prov, defaultModel: defaultModel}
	if deps.env != nil {
		env := deps.env
		gate.policy = func() *modelPolicy { return loadModelPolicy(env, prov) }
	}
	current := func() (*provider.ModelCaller, bool) {
		if deps.live.get == nil {
			return nil, false
		}
		pc, ok := deps.live.get().(*provider.ModelCaller)
		return pc, ok
	}
	return mods.ModelHost{
		Complete: func(ctx context.Context, req mods.ModelRequest) (mods.ModelReply, error) {
			pc, ok := current()
			if !ok || pc == nil || pc.Provider == nil {
				return mods.ModelReply{}, &mods.ModelRefusal{Reason: "this session has no provider to complete with (run /login or /provider)"}
			}
			model := resolveSubagentModel(prov, req.Model, "")
			if model == "" {
				return mods.ModelReply{}, &mods.ModelRefusal{Reason: fmt.Sprintf("model %q is not a model this session's provider (%s) serves", req.Model, providerOrDefault(prov))}
			}
			if !gate.modelAllowed(model) {
				return mods.ModelReply{}, &mods.ModelRefusal{Reason: fmt.Sprintf("model %q is not in this organization's allowlist", req.Model)}
			}
			msgs := []provider.ChatMessage{}
			if req.System != "" {
				msgs = append(msgs, provider.ChatMessage{Role: provider.RoleSystem, Content: req.System})
			}
			msgs = append(msgs, provider.ChatMessage{Role: provider.RoleUser, Content: req.Prompt})
			modsDebugf("$.model.complete (%s): %s via %s, max %d tokens", req.Plugin, model, pc.Provider.Name(), req.MaxTokens)
			start := time.Now()
			resp, err := pc.Provider.Chat(ctx, provider.ChatRequest{Model: model, Messages: msgs, MaxTokens: req.MaxTokens, Effort: req.Effort})
			if err == nil {
				// A plugin's completion is session spend (cost_wire.go).
				if resp.Model != "" {
					model = resp.Model
				}
				r.recordSideQuery(model, provider.APIUsage(resp.Usage, pc.Provider.Name()), time.Since(start))
			}
			if err != nil {
				var api *provider.APIError
				if errors.As(err, &api) {
					return mods.ModelReply{Failure: &mods.ModelFailure{Status: api.Status, Kind: string(api.Category)}}, nil
				}
				return mods.ModelReply{}, err
			}
			return mods.ModelReply{Text: resp.Content, Usage: mods.ModelUsage{
				InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens,
				CacheReadTokens: resp.Usage.CacheReadTokens, CacheCreationTokens: resp.Usage.CacheCreationTokens,
			}}, nil
		},
		Fork: func(ctx context.Context, plugin, prompt string) (mods.ModelReply, bool, error) {
			conv := r.conv
			if conv == nil || len(conv.History) == 0 || deps.live.get == nil {
				return mods.ModelReply{}, false, nil
			}
			history := forkableHistory(append([]agent.Message(nil), conv.History...))
			history = append(history, agent.UserMessage(agent.TextBlock(prompt)))
			modsDebugf("$.model.fork (%s): %d messages, tools denied", plugin, len(history))
			resp, err := r.sideQuery(deps.live).CallModel(ctx, agent.ModelRequest{System: conv.System, Messages: history})
			if err != nil {
				var api *provider.APIError
				if errors.As(err, &api) {
					return mods.ModelReply{Failure: &mods.ModelFailure{Status: api.Status, Kind: string(api.Category)}}, true, nil
				}
				return mods.ModelReply{}, true, err
			}
			return mods.ModelReply{Text: firstText(resp.Message)}, true, nil
		},
		SmallModel: func() string {
			cur := defaultModel
			if pc, ok := current(); ok && pc != nil {
				cur = pc.CurrentModel()
			}
			return resolveSubagentModel(prov, "haiku", cur)
		},
	}
}

func providerOrDefault(p string) string {
	if p == "" {
		return providerOpenRouter
	}
	return p
}

// forkableHistory returns msgs where the trailing assistant messages lose
// their tool calls (a fork answers in text; its tools are all denied).
func forkableHistory(msgs []agent.Message) []agent.Message {
	n := len(msgs)
	var tail []agent.Message
	for n > 0 && msgs[n-1].Role == agent.RoleAssistant {
		m := msgs[n-1]
		n--
		var kept []agent.ContentBlock
		for _, b := range m.Content {
			if b.Type != agent.BlockToolUse {
				kept = append(kept, b)
			}
		}
		if len(kept) > 0 {
			m.Content = kept
			tail = append([]agent.Message{m}, tail...)
		}
	}
	return append(msgs[:n], tail...)
}

// modsDebugf writes the engine's notes to stderr when GOCODE_MODS_DEBUG is
// set; otherwise they are dropped (they never leave the machine).
func modsDebugf(format string, args ...any) {
	if os.Getenv("GOCODE_MODS_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "mods: "+format+"\n", args...)
	}
}
