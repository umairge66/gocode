package cli

import (
	"context"
	"strings"
	"sync"

	"os"
	"path/filepath"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/web"
)

// This file closes the known -p wiring gap: it advertises the built-in tool
// schemas to the model (so the model can actually emit tool_use) and loads the
// permission rules from the settings files (so toolrun's Decider consults real
// allow/ask/deny rules instead of an empty set). Both the print and the
// interactive paths use these helpers.
//
// There is no telemetry here and nothing is hardcoded: the tool schemas come
// from the dispatcher and the rules come entirely from the user's own settings files via
// internal/settingsload.

// The tool schemas advertised to the model come from the dispatcher itself
// ((*toolrun.Dispatcher).Schemas), so the model is offered exactly the tools the
// dispatcher can run — there is no second hand-written list here.

// webFetchSummaryMaxTokens caps the WebFetch apply-prompt answer.
const webFetchSummaryMaxTokens = 4096

// newWebFetchSummarizer returns the WebFetch Summarizer bound to the session's
// active model caller: it sends web.BuildApplyPrompt(content, prompt) as a
// single user message (no tools, no system prompt) and returns the assistant
// text. The caller is the same agent.ModelCaller the conversation uses (and that
// /model retargets in place), so the request goes through internal/provider
// like every other model call and tests stay offline behind the same seams.
func newWebFetchSummarizer(caller agent.ModelCaller) web.Summarizer {
	if caller == nil {
		return nil
	}
	return web.SummarizerFunc(func(ctx context.Context, content, prompt string) (string, error) {
		resp, err := caller.CallModel(ctx, agent.ModelRequest{
			Messages:  []agent.Message{agent.UserMessage(agent.TextBlock(web.BuildApplyPrompt(content, prompt)))},
			MaxTokens: webFetchSummaryMaxTokens,
		})
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, blk := range resp.Message.Content {
			if blk.Type == agent.BlockText {
				b.WriteString(blk.Text)
			}
		}
		return strings.TrimSpace(b.String()), nil
	})
}

// newSessionDispatcher builds the toolrun dispatcher for a session: the mode,
// the settings permission rules, and the WebFetch summarizer bound to the
// session's model caller, plus any extra options (e.g. the interactive Asker).
func newSessionDispatcher(mode toolreg.Mode, env *Env, caller agent.ModelCaller, opts ...toolrun.Option) *toolrun.Dispatcher {
	search := newSessionSearchEngine(env)
	all := append([]toolrun.Option{
		toolrun.WithWebFetchSummarizer(newWebFetchSummarizer(caller)),
		// Milestone 2: the browser tools relay through the per-user chrome-host
		// control socket. The client dials lazily per call, so building it never
		// touches the network or fails when Chrome is absent — a call then returns
		// the clear "connect the extension" error. The configDir derivation matches
		// the host's (chromehost.DefaultConfigDir) so both resolve the same socket.
		toolrun.WithBrowserClient(chromehost.NewControlClient(sessionChromeConfigDir(env))),
		// Tell the user once per process when the connected extension's
		// version differs from the one built into this gocode.
		toolrun.WithBrowserNotice(sessionExtensionVersionCheck(sessionChromeConfigDir(env)).Notice),
		// A screenshot after each page-changing computer action, unless the
		// "chrome" settings or the environment turn it off (chromesettings.go).
		toolrun.WithBrowserScreenshotAfterActions(screenshotAfterActions(env)),
		// Grep/Glob engine from the "search" settings (bundled ugrep by
		// default; see search.go), and `grep` in Bash running that ugrep.
		toolrun.WithSearchEngine(search),
		toolrun.WithUgrepShadow(sessionUgrepShadow(env, search)),
		// Session file-write checks for the edit tools (sessionfiles.go).
		toolrun.WithSessionFiles(newSessionFileChecks(env)),
		// WebSearch budget for the whole session, shared with subagents.
		toolrun.WithWebSearchBudget(newWebSearchBudget(env)),
	}, opts...)
	// allowManagedPermissionRulesOnly: no "don't ask again" (toolrun.Dispatcher.ManagedRulesOnly).
	all = append([]toolrun.Option{toolrun.WithManagedRulesOnly(managedPermissionRulesOnly(env))}, all...)
	return toolrun.New(mode, loadPermissionRules(env), all...)
}

// managedPermissionRulesOnly reports whether the admin policy sets
// allowManagedPermissionRulesOnly (policySettings
// allowManagedPermissionRulesOnly === true).
func managedPermissionRulesOnly(env *Env) bool {
	return managedRulesOnlyIn(settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder))
}

// managedRulesOnlyIn is managedPermissionRulesOnly over already-loaded
// sources.
func managedRulesOnlyIn(ordered []settingsload.OrderedResult) bool {
	for _, or := range ordered {
		if or.Source == settingspolicy.SourcePolicy && or.Result.LoadState == settingspolicy.LoadLoaded &&
			or.Result.Settings["allowManagedPermissionRulesOnly"] == true {
			return true
		}
	}
	return false
}

// managedOnlyAllowedToolsNotice is the startup warning for --allowedTools
// under allowManagedPermissionRulesOnly.
func managedOnlyAllowedToolsNotice(rules []string) string {
	return "Ignoring --allowedTools " + strings.Join(rules, ", ") + ": permission rules are restricted to managed settings (allowManagedPermissionRulesOnly)."
}

// cliPermissionRules compiles the --allowedTools / --disallowedTools rules
// (source "cliArg"). Under allowManagedPermissionRulesOnly the allow rules are
// dropped — with the startup notice — while the deny rules always apply.
func cliPermissionRules(allowed, disallowed []string, managedOnly bool) (toolreg.RuleSet, []string) {
	var rules toolreg.RuleSet
	for _, spec := range splitToolSpecs(disallowed) {
		if r, ok := parsePermissionRule(spec, toolreg.RuleDeny, "cliArg"); ok {
			rules = append(rules, r)
		}
	}
	allow := splitToolSpecs(allowed)
	if managedOnly {
		if len(allow) > 0 {
			return rules, []string{managedOnlyAllowedToolsNotice(allow)}
		}
		return rules, nil
	}
	for _, spec := range allow {
		if r, ok := parsePermissionRule(spec, toolreg.RuleAllow, "cliArg"); ok {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// splitToolSpecs splits each value on commas and spaces outside
// parentheses, so "Bash(git log:*), Read" is two rules and "Bash(npm run *)"
// stays one.
func splitToolSpecs(values []string) []string {
	var out []string
	for _, v := range values {
		var cur strings.Builder
		inParen := false
		flush := func() {
			if t := strings.TrimSpace(cur.String()); t != "" {
				out = append(out, t)
			}
			cur.Reset()
		}
		for _, c := range v {
			switch {
			case c == '(':
				inParen = true
				cur.WriteRune(c)
			case c == ')':
				inParen = false
				cur.WriteRune(c)
			case (c == ',' || c == ' ') && !inParen:
				flush()
			default:
				if cur.Len() > 0 || strings.TrimSpace(string(c)) != "" {
					cur.WriteRune(c)
				}
			}
		}
		flush()
	}
	return out
}

// sessionChromeConfigDir resolves the gocode root the chrome-host control
// socket is derived from, using the same rule as the native host
// (internal/confighome) so the session and host agree on the socket path.
func sessionChromeConfigDir(env *Env) string {
	getenv := os.Getenv
	if env != nil && env.Getenv != nil {
		getenv = env.Getenv
	}
	return chromehost.DefaultConfigDir(getenv, os.UserHomeDir)
}

// loadPermissionRules reads the user's settings files and returns the compiled
// permission RuleSet (deny, then ask, then allow) the toolrun Decider consults.
// It never fails the caller: on any load/parse trouble it returns the rules it
// could read (possibly none), so a missing or malformed settings file degrades
// to "mode-only" gating rather than an error.
func loadPermissionRules(env *Env) toolreg.RuleSet {
	return permissionRulesFor(fsfacade.OSFS{}, settingsContext(env))
}

// permissionRulesFor compiles the permission rules of the effective sources
// in ctx. The policySettings source is the composed admin policy (MDM /
// registry tiers, managed-settings.json and drop-ins, HKCU fallback), so its
// rules are included like any other source's; when that policy sets
// allowManagedPermissionRulesOnly, only its rules are used — allow, ask and
// deny rules of every other source are ignored.
func permissionRulesFor(fsys fsfacade.FS, ctx *settingsload.Context) toolreg.RuleSet {
	ordered := settingsload.EffectiveSources(fsys, ctx, settingsload.CanonicalSourceOrder)

	managedOnly := managedRulesOnlyIn(ordered)

	var deny, ask, allow []toolreg.Rule
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if managedOnly && or.Source != settingspolicy.SourcePolicy {
			continue
		}
		perms, ok := or.Result.Settings["permissions"].(map[string]any)
		if !ok {
			continue
		}
		src := string(or.Source)
		deny = append(deny, parseRuleList(perms["deny"], toolreg.RuleDeny, src)...)
		ask = append(ask, parseRuleList(perms["ask"], toolreg.RuleAsk, src)...)
		allow = append(allow, parseRuleList(perms["allow"], toolreg.RuleAllow, src)...)
	}

	// Deny is consulted before ask before allow (the Decider also enforces this,
	// but ordering the set keeps Match deterministic within a behavior).
	rules := make(toolreg.RuleSet, 0, len(deny)+len(ask)+len(allow))
	rules = append(rules, deny...)
	rules = append(rules, ask...)
	rules = append(rules, allow...)
	return rules
}

// settingsContext builds a settingsload.Context from the environment: the home
// directory and the current working directory, resolved through the injected
// Env seams so tests can point it at a scratch tree.
func settingsContext(env *Env) *settingsload.Context {
	getenv := func(string) string { return "" }
	if env != nil && env.Getenv != nil {
		getenv = env.Getenv
	}
	ctx := &settingsload.Context{
		HomeDir: trustHome(env),
		Env:     getenv,
	}
	if env != nil && env.Cwd != nil {
		if cwd, err := env.Cwd(); err == nil {
			ctx.Cwd = cwd
		}
	}
	if env != nil {
		ctx.ManagedBaseDirOverride = env.ManagedSettingsDir
		ctx.FlagPath = flagSettingsPath(env.FlagSettings, ctx.Cwd)
		ctx.SourcesSet, ctx.Sources = env.SettingSourcesSet, env.SettingSources
	}
	if ctx.Cwd != "" && env != nil {
		// Workspace trust (trust.go): an untrusted folder's project/local
		// settings lose what runs code or widens permissions, and a committed
		// settings.local.json is MCP-gated like project settings.
		ctx.UntrustedProject = !workspaceTrusted(env)
		ctx.LocalSettingsTracked = localSettingsTracked(ctx)
	}
	return ctx
}

// parseRuleList compiles a settings rule array (each entry "Tool" or
// "Tool(content)") into toolreg.Rule values with the given behavior. Non-string
// and empty entries are skipped.
func parseRuleList(v any, behavior toolreg.RuleBehavior, source string) []toolreg.Rule {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []toolreg.Rule
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			continue
		}
		if r, ok := parsePermissionRule(s, behavior, source); ok {
			out = append(out, r)
		}
	}
	return out
}

// parsePermissionRule parses one rule string of the form "Tool" or
// "Tool(content)" into a toolreg.Rule. It reports false for an empty/invalid
// spec. The content is everything between the first "(" and the final ")".
func parsePermissionRule(spec string, behavior toolreg.RuleBehavior, source string) (toolreg.Rule, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return toolreg.Rule{}, false
	}
	name, content := spec, ""
	if open := strings.Index(spec, "("); open >= 0 && strings.HasSuffix(spec, ")") {
		name = strings.TrimSpace(spec[:open])
		content = spec[open+1 : len(spec)-1]
	}
	if name == "" {
		return toolreg.Rule{}, false
	}
	return toolreg.Rule{
		Behavior: behavior,
		ToolName: name,
		Content:  content,
		Source:   source,
	}, true
}

// extensionVersionChecks holds one extension version check per control-socket
// directory, shared by the session and its subagents so the notice is shown
// once per process.
var extensionVersionChecks sync.Map // string -> *chromehost.ExtensionVersionCheck

func sessionExtensionVersionCheck(configDir string) *chromehost.ExtensionVersionCheck {
	if c, ok := extensionVersionChecks.Load(configDir); ok {
		return c.(*chromehost.ExtensionVersionCheck)
	}
	c, _ := extensionVersionChecks.LoadOrStore(configDir, &chromehost.ExtensionVersionCheck{
		Call:     chromehost.NewControlClient(configDir).Call,
		Expected: chromehost.EmbeddedExtensionVersion(),
	})
	return c.(*chromehost.ExtensionVersionCheck)
}

// flagSettingsPath turns the --settings value into the file the flagSettings
// source reads: a path is made absolute against cwd; an inline JSON object is
// written once to an owner-only temporary file.
func flagSettingsPath(v, cwd string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "{") {
		inlineFlagSettings.once.Do(func() {
			f, err := os.CreateTemp("", "gocode-settings-*.json")
			if err != nil {
				return
			}
			_, werr := f.WriteString(v)
			if cerr := f.Close(); werr == nil && cerr == nil {
				inlineFlagSettings.path = f.Name()
			}
		})
		return inlineFlagSettings.path
	}
	if !filepath.IsAbs(v) && cwd != "" {
		v = filepath.Join(cwd, v)
	}
	return v
}

// inlineFlagSettings holds the temporary file for an inline --settings JSON
// object, written once per process.
var inlineFlagSettings struct {
	once sync.Once
	path string
}
