package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/updater"
)

// This file renders the content for the informational slash
// commands — /status, /context, /config and /doctor — as plain text shown in the
// transcript (interactive) and on stdout (-p). None of it touches the network
// and there is no telemetry: every value comes from the already-resolved session
// state (provider/model/key source), the local cost ledger, the on-disk settings
// and the connected MCP host.
//
// Each one is rendered as a clean text panel. Where a figure is approximated
// (notably /context token counts) the output says so.

// cmdInfo bundles the session state the informational commands read. The zero
// value is not usable; mcp may be nil (no MCP host / tests).
type cmdInfo struct {
	conv  *conversation.Conversation
	env   *Env
	print *printSession
	mcp   *mcphost.Host
	lsp   *sessionLSP
}

// currentProvider returns the active provider name, preferring the live caller
// (so a mid-session /provider switch is reflected), then the resolved session.
func (c cmdInfo) currentProvider() string {
	if c.conv != nil {
		if pc, ok := c.conv.Model.(*provider.ModelCaller); ok && pc.CurrentProvider() != nil {
			if n := pc.CurrentProvider().Name(); n != "" {
				return n
			}
		}
	}
	if c.print != nil && c.print.Provider != "" {
		return c.print.Provider
	}
	return providerOpenRouter
}

// currentModel returns the active model id, preferring the live caller, then the
// conversation's selected model, then the resolved session default.
func (c cmdInfo) currentModel() string {
	if c.conv != nil {
		if pc, ok := c.conv.Model.(*provider.ModelCaller); ok && pc.CurrentModel() != "" {
			return pc.CurrentModel()
		}
		if c.conv.ModelID != "" {
			return c.conv.ModelID
		}
	}
	if c.print != nil {
		return c.print.Model
	}
	return ""
}

// capabilities returns the capability-table entry for the active provider and
// model (provider.CapabilitiesFor — built-in table, cached OpenRouter catalog,
// or the conservative default; never a network call).
func (c cmdInfo) capabilities() provider.Capabilities {
	return provider.CapabilitiesFor(c.currentProvider(), c.currentModel())
}

// formatTokenCount renders a token count compactly: 1000000 → "1M",
// 1047576 → "1.05M", 200000 → "200k", 16384 → "16.4k", 512 → "512".
func formatTokenCount(n int) string {
	trim := func(f float64, prec int) string {
		s := strconv.FormatFloat(f, 'f', prec, 64)
		if strings.Contains(s, ".") {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		return s
	}
	switch {
	case n >= 1000000:
		return trim(float64(n)/1e6, 2) + "M"
	case n >= 1000:
		return trim(float64(n)/1e3, 1) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// capabilityBadges lists the notable capabilities ("vision", "effort") for the
// /status line and the /model picker detail.
func capabilityBadges(c provider.Capabilities) []string {
	var out []string
	if c.Vision {
		out = append(out, "vision")
	}
	if c.Effort {
		out = append(out, "effort")
	}
	return out
}

// lifecycleNotice is the one-line deprecation / retirement warning for model
// on the active provider ("" when the model is active or nothing is known).
// It reads the cached Anthropic /v1/models facts only (never the network).
func (c cmdInfo) lifecycleNotice(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	return provider.LifecycleNotice(c.currentProvider(), model)
}

// windowSummary is "1M tokens (built-in)" — the window and where it came from.
func windowSummary(c provider.Capabilities) string {
	return fmt.Sprintf("%s tokens (%s)", formatTokenCount(c.ContextWindow), c.Source)
}

// keySource reports the resolved key source for the active provider (the env var
// name, auth.json, settings or apiKeyHelper) WITHOUT revealing the key, and
// whether a key resolved at all.
func (c cmdInfo) keySource() (source string, ok bool) {
	_, source, ok = newKeyResolver(c.env).ResolveProviderKey(auth.Provider(c.currentProvider()))
	return source, ok
}

// baseURL returns the active provider's user-configured base URL: the one the
// session resolved (so /status matches what the caller uses), else a fresh
// resolution from the environment and settings.
func (c cmdInfo) baseURL() providerBaseURL {
	p := c.currentProvider()
	if c.print != nil && c.print.baseURLs != nil {
		return c.print.baseURLs[p]
	}
	return resolveProviderBaseURL(c.env, p)
}

// mcpCounts returns the number of connected and total configured MCP servers.
func (c cmdInfo) mcpCounts() (connected, total int) {
	if c.mcp == nil {
		return 0, 0
	}
	st := c.mcp.Status()
	total = len(st)
	for _, s := range st {
		if s.State == mcphost.StateConnected {
			connected++
		}
	}
	return connected, total
}

// effectiveSettings merges the active settings sources (low→high precedence,
// last wins) into one map, so the informational commands can read a value the
// same way the session resolved it. It never fails: unreadable sources are
// skipped. The user settings path (where to change them) is returned too.
func (c cmdInfo) effectiveSettings() (settings map[string]any, userPath string) {
	settings = map[string]any{}
	ctx := settingsContext(c.env)
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		for k, v := range or.Result.Settings {
			settings[k] = v
		}
	}
	if p, ok := ctx.PathForSource(settingspolicy.SourceUser); ok {
		userPath = p
	}
	return settings, userPath
}

// settingString returns a settings value as a string, or "" when absent.
func settingString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// settingBool returns a settings bool and whether it was present.
func settingBool(m map[string]any, key string) (val, present bool) {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

// approxTokens is a rough token estimate (≈ 4 chars/token) used by /context. It
// is an APPROXIMATION, not the model's real tokenizer — there is no local
// tokenizer, so the breakdown is labelled approximate.
func approxTokens(s string) int { return (len([]rune(s)) + 3) / 4 }

// messageText flattens a message's text/tool content for a rough token estimate.
func messageText(m agent.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Text != "" {
			b.WriteString(blk.Text)
		}
		if blk.Content != "" {
			b.WriteString(blk.Content)
		}
		if len(blk.Input) > 0 {
			b.Write(blk.Input)
		}
	}
	return b.String()
}

// statusText renders /status: the session status
// (version, cwd, provider, model, key source, MCP count), honestly reporting
// that gocode uses provider API keys with no account/email.
func (c cmdInfo) statusText() string {
	var b strings.Builder
	b.WriteString("gocode status\n")
	fmt.Fprintf(&b, "  Version:       %s\n", updateCurrentVersion())
	if cwd := sessionCwd(c.env); cwd != "" {
		fmt.Fprintf(&b, "  Directory:     %s\n", cwd)
	}
	fmt.Fprintf(&b, "  Provider:      %s\n", providerWithName(c.currentProvider()))
	// The configured endpoint, shown only when one is set, with embedded
	// credentials masked.
	if bu := c.baseURL(); bu.configured() {
		fmt.Fprintf(&b, "  %s: %s\n", provider.BaseURLLabel(c.currentProvider()), bu.display())
	}
	if m := c.currentModel(); m != "" {
		fmt.Fprintf(&b, "  Model:         %s\n", m)
	} else {
		b.WriteString("  Model:         not set — run /model\n")
	}
	fmt.Fprintf(&b, "  Permission mode: %s\n", c.permissionMode())
	if n := c.lifecycleNotice(c.currentModel()); n != "" {
		fmt.Fprintf(&b, "  Lifecycle:     %s\n", n)
	}
	fmt.Fprintf(&b, "  Fallback model: %s\n", fallbackControl{conv: c.conv, print: c.print, env: c.env}.statusValue())
	caps := c.capabilities()
	fmt.Fprintf(&b, "  Context:       %s\n", windowSummary(caps))
	if badges := capabilityBadges(caps); len(badges) > 0 {
		fmt.Fprintf(&b, "  Capabilities:  %s\n", strings.Join(badges, ", "))
	}
	fmt.Fprintf(&b, "  Effort:        %s\n", effortState{conv: c.conv, print: c.print, env: c.env}.statusValue())
	if source, ok := c.keySource(); ok {
		fmt.Fprintf(&b, "  API key:       set (source: %s)\n", source)
	} else {
		b.WriteString("  API key:       not set — run /login\n")
	}
	connected, total := c.mcpCounts()
	fmt.Fprintf(&b, "  MCP servers:   %d connected / %d configured\n", connected, total)
	fmt.Fprintf(&b, "  LSP servers:   %s\n", c.lsp.statusValue())
	fmt.Fprintf(&b, "  Search:        %s\n", searchEngineSummary(c.env))
	// allowManagedPermissionRulesOnly (the "Permission rules" status row).
	var grants *allowedToolsGate
	if c.print != nil {
		grants = c.print.grants
	}
	for i, line := range managedRulesStatusLines(c.env != nil && managedPermissionRulesOnly(c.env), grants) {
		label := "  Permission rules: "
		if i > 0 {
			label = "                    "
		}
		b.WriteString(label + line + "\n")
	}
	// gocode authenticates with provider API keys only (no gocode account / OAuth),
	// so there is no account email to show.
	b.WriteString("  Account:       provider API key (no account)")
	return b.String()
}

// permissionMode is the session's live permission mode ("default" when unset).
func (c cmdInfo) permissionMode() string {
	if c.conv != nil && c.conv.Mode != "" {
		return string(c.conv.Mode)
	}
	if c.print != nil && c.print.Mode != "" {
		return string(c.print.Mode)
	}
	return "default"
}

// contextText renders /context: an APPROXIMATE context-usage breakdown.
func (c cmdInfo) contextText() string {
	var sys, tools, msgs int
	var msgCount, toolCount, deferredCount int
	if c.conv != nil {
		sys = approxTokens(c.conv.System)
		// Only the definitions the next request carries count; deferred tools
		// not loaded yet cost their name in a reminder, not a definition.
		sent, deferred := agent.SelectTools(c.conv.ToolSchemas, c.conv.History)
		for _, ts := range sent {
			tools += approxTokens(ts.Name) + approxTokens(ts.Description) + approxTokens(string(ts.InputSchema))
			toolCount++
		}
		deferredCount = len(deferred)
		for _, m := range c.conv.History {
			msgs += approxTokens(messageText(m))
			msgCount++
		}
	}
	// The headline number is the SAME one the autocompact trigger uses: the last
	// response's real reported tokens (input + cache + output) plus an estimate
	// of anything added since. Before the first response it is an estimate
	// (system prompt + tools + history). The per-part rows are ≈4 chars/token.
	used := sys + tools + msgs
	measured := false
	if c.conv != nil {
		if c.conv.ContextTokensMeasured() {
			used, measured = c.conv.ContextTokens(), true
		} else {
			used = sys + tools + c.conv.ContextTokens()
		}
	}

	var b strings.Builder
	b.WriteString("Context usage\n")
	if m := c.currentModel(); m != "" {
		fmt.Fprintf(&b, "  Model:            %s\n", m)
	}
	fmt.Fprintf(&b, "  System prompt:    ~%d tokens\n", sys)
	if deferredCount > 0 {
		fmt.Fprintf(&b, "  Tools:            ~%d tokens (%d tools; %d more deferred, loaded on demand)\n", tools, toolCount, deferredCount)
	} else {
		fmt.Fprintf(&b, "  Tools:            ~%d tokens (%d tools)\n", tools, toolCount)
	}
	fmt.Fprintf(&b, "  Conversation:     ~%d tokens (%d messages)\n", msgs, msgCount)
	if measured {
		fmt.Fprintf(&b, "  Context used:     %d tokens (measured: last response + estimate since)\n", used)
	} else {
		fmt.Fprintf(&b, "  Context used:     ~%d tokens (estimated — no model response yet)\n", used)
	}

	if c.conv != nil && c.conv.Cost != nil {
		l := c.conv.Cost
		fmt.Fprintf(&b, "  Session I/O:      ↑ %d (%d cached) ↓ %d tokens (cumulative, from the cost ledger)\n",
			l.TotalInputTokens()+l.TotalCacheReadInputTokens()+l.TotalCacheCreationInputTokens(),
			l.TotalCacheReadInputTokens(), l.TotalOutputTokens())
	}
	caps := c.capabilities()
	fmt.Fprintf(&b, "  Context window:   %s tokens (source: %s)\n", formatTokenCount(caps.ContextWindow), caps.Source)
	if caps.ContextWindow > 0 {
		left := max(caps.ContextWindow-used, 0)
		fmt.Fprintf(&b, "  Remaining:        ~%s tokens (~%d%% used)\n", formatTokenCount(left), used*100/caps.ContextWindow)
	}
	var raw string
	if c.env != nil && c.env.Getenv != nil {
		raw = c.env.Getenv(provider.MaxOutputEnv)
	}
	if mo := provider.ResolveMaxOutput(caps, raw); mo.Tokens > 0 {
		fmt.Fprintf(&b, "  Max output:       %s tokens", formatTokenCount(mo.Tokens))
		switch {
		case mo.Note != "":
			fmt.Fprintf(&b, " (%s)", mo.Note)
		case mo.FromEnv:
			fmt.Fprintf(&b, " (from %s)", provider.MaxOutputEnv)
		}
	} else {
		b.WriteString("  Max output:       provider default")
	}
	return b.String()
}

// usageStubText is the /usage (/cost /stats) EffectPrintText sentinel. The cli
// detects it and substitutes the live cost report. It aliases the builtincmds
// constant so the sentinel the built-in emits and the one the host matches can
// never drift apart.
const usageStubText = builtincmds.UsageStubText

// usageText renders /usage (aliases /cost, /stats): the /cost body
// (cost.CostSummary over the session ledger — total cost, API/wall durations,
// code changes, and the per-model input/output/cache breakdown), followed by the
// cumulative session token totals and the assistant turn count. It is the real
// substitute for the usageStubText sentinel the built-in emits.
func (c cmdInfo) usageText() string {
	if c.conv == nil || c.conv.Cost == nil {
		return "Session usage is unavailable (no cost ledger wired for this session)."
	}
	l := c.conv.Cost
	turns := 0
	for _, m := range c.conv.History {
		if m.Role == agent.RoleAssistant {
			turns++
		}
	}
	var b strings.Builder
	b.WriteString("Session usage\n")
	b.WriteString(c.conv.CostSummary())
	b.WriteString("\n")
	// With no recorded usage the summary's own "Usage:" line already shows the
	// zero totals; the token totals line would repeat it.
	if len(l.ModelUsage()) > 0 {
		fmt.Fprintf(&b, "Total tokens:          %d input, %d output, %d cache read, %d cache write\n",
			l.TotalInputTokens(), l.TotalOutputTokens(), l.TotalCacheReadInputTokens(), l.TotalCacheCreationInputTokens())
	}
	fmt.Fprintf(&b, "Assistant turns:       %d", turns)
	return b.String()
}

// configText renders /config: the current effective settings and where to change
// them (the user settings.json path). Secrets are never printed.
func (c cmdInfo) configText() string {
	settings, userPath := c.effectiveSettings()

	theme := settingString(settings, "theme")
	if ui, _ := settings["ui"].(map[string]any); ui != nil {
		if t, _ := ui["theme"].(string); t != "" {
			theme = t + " (ui.theme)"
		}
	}
	if theme == "" {
		theme = "auto (default: follows the terminal background)"
	}
	mode := "default"
	if c.conv != nil && c.conv.Mode != "" {
		mode = string(c.conv.Mode)
	}
	autoupdate := "default (enabled)"
	if v, present := settingBool(settings, "autoUpdates"); present {
		if v {
			autoupdate = "on"
		} else {
			autoupdate = "off"
		}
	}
	repo := settingString(settings, updater.RepoSettingsKey)
	resolved, _, err := updater.ResolveRepo(func(k string) string {
		if c.env != nil && c.env.Getenv != nil {
			return c.env.Getenv(k)
		}
		return ""
	}, repo)
	repoLine := "not configured"
	if err == nil && resolved != "" {
		repoLine = resolved
	}

	var b strings.Builder
	b.WriteString("gocode configuration\n")
	fmt.Fprintf(&b, "  Provider:        %s   (change: /provider, --provider or GOCODE_PROVIDER)\n", c.currentProvider())
	if m := c.currentModel(); m != "" {
		fmt.Fprintf(&b, "  Model:           %s   (change: /model or --model)\n", m)
	}
	fmt.Fprintf(&b, "  Theme:           %s\n", theme)
	fmt.Fprintf(&b, "  Permission mode: %s\n", mode)
	fmt.Fprintf(&b, "  Autoupdate:      %s\n", autoupdate)
	fmt.Fprintf(&b, "  Update repo:     %s   (%s or settings %q)\n", repoLine, updater.RepoEnvVar, updater.RepoSettingsKey)
	fmt.Fprintf(&b, "  Search:          %s   (settings \"search\": useUgrep, ugrepPath, preferSystemUgrep, includeHidden; \"respectGitignore\")\n", searchEngineSummary(c.env))
	if userPath != "" {
		fmt.Fprintf(&b, "  Settings file:   %s\n", userPath)
	}
	if dir, ok := settingsContext(c.env).UserConfigDir(); ok {
		fmt.Fprintf(&b, "  Data directory:  %s   (settings, keys, sessions, history, plans)\n", dir)
	}
	b.WriteString("Run /config to change these, or edit the settings file.")
	return b.String()
}

// doctorText renders /doctor: fast, offline pass/fail diagnostics. It makes no
// network call — "provider reachable" is reported as key-presence only, matching
// the task's offline constraint.
func (c cmdInfo) doctorText() string {
	mark := func(ok bool) string {
		if ok {
			return "✓"
		}
		return "✗"
	}

	var b strings.Builder
	b.WriteString("gocode doctor\n")
	fmt.Fprintf(&b, "  %s Version: %s\n", mark(true), updateCurrentVersion())
	fmt.Fprintf(&b, "  %s OS/arch: %s/%s\n", mark(true), runtime.GOOS, runtime.GOARCH)

	prov := c.currentProvider()
	_, keyOK := c.keySource()
	fmt.Fprintf(&b, "  %s API key for %s: %s\n", mark(keyOK), prov, presentWord(keyOK))
	if bu := c.baseURL(); bu.configured() {
		fmt.Fprintf(&b, "  %s %s: %s\n", mark(bu.Err == nil), provider.BaseURLLabel(prov), bu.display())
		if bu.Err != nil {
			fmt.Fprintf(&b, "    %v\n", bu.Err)
		}
	}

	connected, total := c.mcpCounts()
	fmt.Fprintf(&b, "  %s MCP servers: %d connected / %d configured\n", mark(connected == total), connected, total)

	search := searchEngineSummary(c.env)
	fmt.Fprintf(&b, "  %s Search: %s (bundled: %s)\n", mark(true), search, bundledUgrepSummary())

	settings, userPath := c.effectiveSettings()
	repo := settingString(settings, updater.RepoSettingsKey)
	resolved, _, rerr := updater.ResolveRepo(func(k string) string {
		if c.env != nil && c.env.Getenv != nil {
			return c.env.Getenv(k)
		}
		return ""
	}, repo)
	repoOK := rerr == nil && resolved != ""
	switch {
	case repoOK:
		fmt.Fprintf(&b, "  %s Update repo: %s\n", mark(true), resolved)
	case rerr != nil && !errors.Is(rerr, updater.ErrRepoNotConfigured):
		// A configured value that does not resolve is a real problem.
		fmt.Fprintf(&b, "  %s Update repo: %v\n", mark(false), rerr)
	default:
		// No update repo is optional: informational, not a failure.
		fmt.Fprintf(&b, "  · Update repo: not configured (optional; set %s or settings %q to enable updates)\n", updater.RepoEnvVar, updater.RepoSettingsKey)
	}

	// Config file readable: EffectiveSources already read it without error when a
	// path resolved; report whether a user settings path exists and parsed.
	cfgOK := userPath != ""
	if cfgOK {
		fmt.Fprintf(&b, "  %s Config file: %s\n", mark(true), userPath)
	} else {
		fmt.Fprintf(&b, "  %s Config file: no user settings path\n", mark(false))
	}
	if dir, ok := settingsContext(c.env).UserConfigDir(); ok {
		fmt.Fprintf(&b, "  %s Data directory: %s\n", mark(true), dir)
	} else {
		fmt.Fprintf(&b, "  %s Data directory: none (set HOME, XDG_CONFIG_HOME or GOCODE_CONFIG_DIR)\n", mark(false))
	}

	b.WriteString("Diagnostics are offline (no network check).")
	return b.String()
}

func presentWord(ok bool) string {
	if ok {
		return "present"
	}
	return "missing"
}

// contextStubText is the old /context EffectPrintText stub. The cli layer
// detects it and substitutes the real (approximate) breakdown, since /context is
// a type:"local" builtin registered in internal/builtincmds (read-only here) and
// the Effect carries no command name to key off otherwise.
const contextStubText = "Context usage is reported by the host."

// providerWithName is the provider id followed by its registry display name
// when that says more than the id ("zai (Z.ai)", "huggingface (Hugging Face
// Inference Providers)"); "openrouter" / "groq" stay bare.
func providerWithName(id string) string {
	if s, ok := provider.Lookup(id); ok && s.DisplayName != "" && !strings.EqualFold(s.DisplayName, id) {
		return id + " (" + s.DisplayName + ")"
	}
	return id
}
