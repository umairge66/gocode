package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file wires the non-interactive print path (`-p/--print`) end-to-end:
// it builds the selected provider's model caller (provider.OpenRouter /
// provider.Anthropic / provider.OpenAI → provider.NewModelCaller), the real
// tool dispatcher gated by the permission mode, and the conversation
// orchestrator, then runs one turn through conversation.Submit and prints the
// assistant text.
//
// Everything that could touch the network or the real credential store sits
// behind the printSession seams, so the path is exercised in tests with an
// injected provider transport (BaseURL/HTTP) or a fake model caller, and no
// real request is ever made unless a real key is present. There is NO telemetry
// here and no secret is hardcoded: the endpoint comes from internal/provider and
// the provider API key is resolved from the environment / settings through
// internal/auth (<PROVIDER>_API_KEY > settings > apiKeyHelper).
// There is NO gocode OAuth/gateway — provider API keys only.

// Provider backend ids for the model-caller switch, selected by
// `--provider` > GOCODE_PROVIDER > the settings "provider" key > openrouter.
const (
	providerOpenRouter = "openrouter"
	providerAnthropic  = "anthropic"
	providerOpenAI     = "openai"
)

// validProviders lists the selectable providers, in display / auto-detection
// order: the provider registry (internal/provider/registry.go) — openrouter,
// anthropic, openai, then the OpenAI-compatible providers and any provider
// registered by another package.
func validProviders() []string { return provider.ProviderIDs() }

// envProvider is the environment variable that selects the provider when no
// `--provider` flag is given.
const envProvider = "GOCODE_PROVIDER"

// There is no built-in default model. The model comes from `--model`, the
// persisted "model" setting (written by /model), or /model in a session; with
// none of those the caller builder returns errNoModel (setup.go).

// errNoCredentials is returned by the default model-caller builder when no
// OpenRouter API key could be resolved. It is reported to the user as a clear,
// actionable message (not a panic and not an opaque network error) — no gocode
// OAuth flow. noCredentialsError gives the per-provider variant.
var errNoCredentials = errors.New(
	"set OPENROUTER_API_KEY (or run with a provider key)")

// noCredentialsError names the selected provider's key env var.
func noCredentialsError(p string) error {
	if p == "" || p == providerOpenRouter {
		return errNoCredentials
	}
	if envs := auth.ProviderEnvVars(auth.Provider(p)); len(envs) > 0 {
		return errors.New("set " + strings.Join(envs, " or ") + " (or run with a provider key)")
	}
	return errors.New("no credentials for provider " + p + " (run /login or configure the provider)")
}

// unknownProviderError reports an unrecognized provider selection.
func unknownProviderError(p, source string) error {
	return fmt.Errorf("unknown provider %q (from %s); valid providers: %s",
		p, source, strings.Join(validProviders(), ", "))
}

// printSession carries the resolved settings and the injectable seams the
// print path needs. The zero value is not usable; build one with
// newPrintSession (or set the fields directly in a test).
type printSession struct {
	// Model is the provider-specific model id sent on every request.
	Model string
	// Mode is the permission mode the tool dispatcher runs under.
	Mode toolreg.Mode
	// Provider selects the model backend the default newCaller builds
	// (openrouter / anthropic / openai). Empty is treated as openrouter.
	Provider string
	// providerErr is set when the provider selection was invalid; buildCaller
	// reports it before any caller is built.
	providerErr error

	// Effort is the reasoning-effort level the default caller sends ("" =
	// unset/auto): --effort > persisted "effortLevel" > unset. /effort updates
	// it alongside the live caller so a rebuilt caller keeps it. effortErr is
	// an invalid --effort value, reported by buildCaller.
	Effort    string
	effortErr error

	// FallbackModels is the model fallback chain: --fallback-model (a
	// comma-separated list) > the settings "fallbackModel" array, normalized
	// by provider.ParseFallbackModels. The fallbacks run on the session's
	// provider with its key.
	FallbackModels []string

	// BaseURL overrides the provider endpoint ("" → the provider's real
	// endpoint). For OpenRouter this is the full chat-completions URL; for
	// Anthropic / OpenAI it is the API root. Tests point it at httptest.
	BaseURL string
	// baseURLs are the user-configured per-provider base URLs
	// (ANTHROPIC_BASE_URL, OPENAI_BASE_URL,
	// OPENROUTER_BASE_URL, from the environment or a trusted settings "env"
	// block), used when BaseURL is empty. Keyed by provider so a key is only
	// ever sent to its own provider's configured endpoint.
	baseURLs map[string]providerBaseURL
	// getenv reads the session environment for a provider constructor that
	// needs more than a key (region, project, …); nil means none.
	getenv func(string) string
	// HTTP is the injectable transport handed to the provider. Nil means the
	// provider's own credential-safe default client. It only ever reaches the real
	// network once a real key is present (see newCaller).
	HTTP provider.Doer
	// AllowedTools / DisallowedTools are the --allowedTools /
	// --disallowedTools values, compiled into "cliArg" rules when the session
	// is built (cliPermissionRules).
	AllowedTools    []string
	DisallowedTools []string
	// SystemPrompt / AppendSystemPrompt are the --system-prompt and
	// --append-system-prompt texts (the -file forms already read); AgentsJSON
	// is the --agents object. See promptflags.go.
	SystemPrompt       string
	AppendSystemPrompt string
	AgentsJSON         string

	// grants decides whether skill/command allowed-tools may pre-approve
	// tools under allowManagedPermissionRulesOnly (toolgrants.go); set by
	// buildSession.
	grants *allowedToolsGate

	// restrictedModel is the user-specified model (--model or the "model"
	// setting) the model policy refused at startup; the session shows the
	// restricted-model notice for it.
	restrictedModel string

	// modeNotice is the startup notice for a permission mode that was
	// refused (e.g. "Bypass permissions mode was
	// disabled by settings"); startupRefusal, when set, ends the session
	// before it starts (the root/sudo bypass refusal). See bypassmode.go.
	modeNotice     string
	startupRefusal string
	// bypassAllowed: the session may switch to bypassPermissions later (it
	// started in it, or with --allow-dangerously-skip-permissions).
	bypassAllowed bool
	// modeChosen reports that a startup permission mode was asked for: a
	// --permission-mode / --dangerously-skip-permissions flag, or a usable
	// settings permissions.defaultMode. An interactive session given none
	// starts in auto mode when auto mode is available (automodedefault.go).
	modeChosen bool
	// modeFromFallback marks an interactive session that started in auto mode
	// because no mode was chosen.
	modeFromFallback bool

	// Referer / Title are the optional OpenRouter attribution headers.
	Referer string
	Title   string

	// APIKey is the resolved provider API key (<PROVIDER>_API_KEY >
	// settings > apiKeyHelper). When empty the default
	// newCaller refuses with the provider's no-key message rather than issuing
	// an unauthenticated request. KeySource records where it came from.
	APIKey    string
	KeySource string

	// Session-construction inputs threaded from the root flags (see
	// DispatchDefaults): --mcp-config, --strict-mcp-config, --plugin-dir and
	// --plugin-dir-no-mcp.
	MCPConfig       []string
	StrictMCPConfig bool
	// Chrome is the --chrome / --no-chrome choice ("on", "off" or ""); see
	// browserToolsEnabled.
	Chrome          string
	PluginDirs      []string
	PluginDirsNoMCP []string
	// AddDirs are the --add-dir directories (raw flag values).
	AddDirs []string

	// MaxRetries is the retry budget for transient model-request failures
	// (GOCODE_MAX_RETRIES, default 10; 0 disables), applied to every caller the
	// session builds. retrySleep is a test seam for the backoff wait (nil = real
	// timer). retryObs is the live retry observer (status line / stderr), read
	// at call time under retryMu; see retry.go.
	MaxRetries int
	// MaxTurns is the validated --max-turns limit for a print run (0 = none;
	// GOCODE_CODE_MAX_TURNS then applies). See maxturns.go.
	MaxTurns int
	// MaxBudgetUSD is the validated --max-budget-usd cap (0 = none); see
	// maxbudget.go for how it combines with the settings and environment.
	MaxBudgetUSD float64
	retrySleep   retrySleepFunc
	retryMu      sync.Mutex
	retryObs     retryObserver

	// resume is the parsed --continue/--resume/--session-id intent for this run,
	// threaded from the dispatch defaults. It drives session persistence and
	// startup resume for both -p and the interactive REPL.
	resume resumeIntent

	// mcpTransport, when set, builds every MCP server transport (tests inject
	// in-memory servers); nil uses the real stdio/HTTP transports.
	mcpTransport mcphost.TransportFactory
	// hookRunner, when set, runs hook commands (tests inject a fake); nil
	// runs them as real processes.
	hookRunner hookhost.CommandRunner
	// shellRunner, when set, executes a custom command's `!`-blocks (tests inject
	// a fake so command expansion runs offline); nil uses the real project Bash
	// exec path (commands.DefaultShellRunner). See configureSession.
	shellRunner commands.ShellRunner
	// skillShellDisabled, when set, replaces the disableSkillShellExecution
	// settings lookup (tests); nil reads the session's settings sources
	// (settingsload.SkillShellExecutionDisabled). See configureSession.
	skillShellDisabled func() bool
	// hookHTTPDoer, when set, is the HTTP client the hook host's `http` hooks POST
	// through (tests inject a fake); nil uses a default *http.Client with a sane
	// timeout. It only ever posts to the hook's own configured URL — no telemetry.
	hookHTTPDoer hooks.HTTPDoer
	// onHookOptions, when set, observes the hookhost.Options the session was built
	// with before the Host is constructed (tests assert the seams are populated).
	onHookOptions func(hookhost.Options)
	// onSession, when set, observes the built session runtime (tests).
	onSession func(*sessionRuntime)

	// newCaller builds the model caller. The default builds the selected
	// provider wrapped in provider.NewModelCaller; tests inject a fake caller.
	// This is the single seam that decides whether a network call can happen.
	newCaller func(s *printSession) (agent.ModelCaller, error)
}

// buildCaller reports an invalid provider selection, then delegates to the
// newCaller seam.
func (s *printSession) buildCaller() (agent.ModelCaller, error) {
	if s.providerErr != nil {
		return nil, s.providerErr
	}
	if s.effortErr != nil {
		return nil, s.effortErr
	}
	return s.newCaller(s)
}

// newPrintSession resolves the print-mode settings from the dispatch defaults
// and the environment, wiring the default (real) model-caller builder.
func newPrintSession(env *Env, d *DispatchDefaults) *printSession {
	p, perr := resolveProvider(env, d)
	s := &printSession{
		Mode:        modeFromDefaults(d),
		Provider:    p,
		providerErr: perr,
		newCaller:   defaultNewCaller,
		MaxRetries:  provider.MaxRetriesFromEnv(envGetenv(env)),
	}
	// Persisted model choice (settings "model") overrides the provider default,
	// so a model picked with /model is remembered across sessions. The --model
	// flag still wins over it.
	if v := loadModelSetting(env); v != "" {
		s.Model = v
	}
	if d != nil && d.Model != "" {
		s.Model = d.Model
	}
	// Reasoning effort: --effort > persisted "effortLevel" > unset.
	s.Effort, s.effortErr = resolveEffort(env, d)
	s.FallbackModels = resolveFallbackModels(env, d)
	// The availableModels / deniedModels policy (modelpolicy.go): a refused
	// --model or "model" setting steps down within its family or is dropped
	// (the startup notice reports it), enforceAvailableModels supplies the
	// model when none was chosen, and refused fallback models are left out.
	pol := loadModelPolicy(env, p)
	s.Model, s.restrictedModel = pol.startupModel(s.Model)
	s.FallbackModels = pol.fallbackModels(s.FallbackModels)
	// The settings permissions.defaultMode applies when no --permission-mode
	// flag was given; the flag wins.
	s.modeChosen = d != nil && strings.TrimSpace(d.PermissionMode) != ""
	var settingsWarning string
	if !s.modeChosen {
		m, warn := resolveSettingsPermissionMode(env)
		settingsWarning = warn
		if m != "" {
			s.Mode = modeFromDefaults(&DispatchDefaults{PermissionMode: m})
			s.modeChosen = true
		}
	}
	// disableBypassPermissionsMode and the root/sudo refusal (bypassmode.go).
	s.Mode, s.modeNotice, s.startupRefusal = gateBypassMode(env, d, s.Mode)
	s.bypassAllowed = s.Mode == toolreg.ModeBypassPermissions || (d != nil && d.AllowBypass)
	if s.modeNotice == msgBypassDisabledNotice {
		// A refused bypass mode leaves only the settings mode as a choice.
		m := toolreg.Mode(loadPermissionModeSetting(env))
		s.modeChosen = m != "" && m != toolreg.ModeBypassPermissions
	}
	s.modeNotice = joinNotices(settingsWarning, s.modeNotice)
	// Auto mode disabled by settings (disableAutoMode): a session asked to start
	// in auto mode starts in default mode instead.
	if s.Mode == toolreg.ModeAuto && autoModeDisabledBySettings(env) {
		s.Mode = toolreg.ModeDefault
		s.modeNotice = joinNotices(s.modeNotice, autoModeUnavailableText(autoModeReasonSettings))
	}
	if d != nil {
		s.MCPConfig = d.MCPConfig
		s.StrictMCPConfig = d.StrictMCPConfig
		s.Chrome = d.Chrome
		s.PluginDirs = d.PluginDirs
		s.PluginDirsNoMCP = d.PluginDirsNoMCP
		s.AddDirs = d.AddDirs
		s.resume = resumeIntentFrom(d)
		s.AllowedTools = d.AllowedTools
		s.DisallowedTools = d.DisallowedTools
		s.MaxTurns = d.MaxTurnsLimit
		s.SystemPrompt, s.AppendSystemPrompt, s.AgentsJSON = d.SystemPrompt, d.AppendSystemPrompt, d.AgentsJSON
		s.MaxBudgetUSD = d.MaxBudgetUSDLimit
	}
	if perr == nil {
		s.resolveAuth(env)
	}
	s.baseURLs = resolveProviderBaseURLs(env)
	if env != nil && env.Getenv != nil {
		s.getenv = env.Getenv
	}
	return s
}

// loadModelSetting returns the persisted "model" id from the effective settings
// (last writer wins across the source order), or "" when unset. Mirrors
// loadProviderSetting so a /model choice saved to local settings is picked up at
// the next session start.
func loadModelSetting(env *Env) string {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	var out string
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if v, ok := or.Result.Settings["model"].(string); ok && strings.TrimSpace(v) != "" {
			out = strings.TrimSpace(v)
		}
	}
	return out
}

// loadFallbackModelSetting returns the settings "fallbackModel" array (last
// writer wins across the source order: arrays of this key replace rather than
// merge), or nil when unset. Non-string entries are
// ignored. A plain string (a single model, or a comma-separated chain) is
// accepted too, and an empty string or array clears what a lower-priority
// source set (`/fallback off` writes "" to local settings).
func loadFallbackModelSetting(env *Env) []string {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	var out []string
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if str, ok := or.Result.Settings["fallbackModel"].(string); ok {
			out = []string{str}
			continue
		}
		arr, ok := or.Result.Settings["fallbackModel"].([]any)
		if !ok {
			continue
		}
		var models []string
		for _, v := range arr {
			if s, ok := v.(string); ok {
				models = append(models, s)
			}
		}
		out = models
	}
	return out
}

// loadPermissionModeSetting returns the startup permission mode the settings
// ask for (see resolveSettingsPermissionMode), or "" when they ask for none.
func loadPermissionModeSetting(env *Env) string {
	m, _ := resolveSettingsPermissionMode(env)
	return m
}

// Warnings for a settings defaultMode a repository-controlled source may not
// grant.
const (
	msgSettingsAutoUntrusted   = `settings defaultMode "auto" ignored — only policy/user/flag settings may grant auto mode (projectSettings and localSettings are repo-controllable). Remove "defaultMode" from the project's .gocode/settings.json or .gocode/settings.local.json, or set it in your user settings with /config.`
	msgSettingsBypassUntrusted = `settings defaultMode "bypassPermissions" ignored — only policy/user/flag settings may grant bypass mode (projectSettings and localSettings are repo-controllable). Remove "defaultMode" from the project's .gocode/settings.json or .gocode/settings.local.json.`
)

// resolveSettingsPermissionMode reads the effective settings' nested
// "permissions.defaultMode" (last writer wins across the source order; "manual"
// is an alias of "default") and returns the mode it asks for, or "" when unset.
//
// bypassPermissions and auto are honored only when a trusted source (user,
// --settings, policy) itself sets that mode: projectSettings and
// localSettings are repo-controllable (a cloned repository can ship both
// files), so a repository must not widen what runs without asking. An
// untrusted bypassPermissions yields "default"; an untrusted auto is ignored
// ("", as if unset). Either way warning says so.
func resolveSettingsPermissionMode(env *Env) (mode, warning string) {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	var out string
	trusted := map[string]bool{}
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		perms, ok := or.Result.Settings["permissions"].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := perms["defaultMode"].(string); ok && strings.TrimSpace(v) != "" {
			out = normalizePermissionModeName(v)
			if trustedBaseURLSources[or.Source] {
				trusted[out] = true
			}
		}
	}
	switch toolreg.Mode(out) {
	case toolreg.ModeBypassPermissions:
		if !trusted[out] {
			return string(toolreg.ModeDefault), msgSettingsBypassUntrusted
		}
	case toolreg.ModeAuto:
		if !trusted[out] {
			return "", msgSettingsAutoUntrusted
		}
	}
	return out, ""
}

// normalizePermissionModeName trims a permission mode name and maps the
// "manual" alias to "default".
func normalizePermissionModeName(v string) string {
	v = strings.TrimSpace(v)
	if v == "manual" {
		return string(toolreg.ModeDefault)
	}
	return v
}

// joinNotices joins the non-empty notices, one per line.
func joinNotices(notices ...string) string {
	var out []string
	for _, n := range notices {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "\n")
}

// resolveProvider picks the provider: `--provider` > GOCODE_PROVIDER > the
// settings "provider" key > the first provider whose API-key variable is set
// (provider.Detect, registry order) > openrouter. Values are trimmed and
// lower-cased; an unrecognized value yields an error listing the valid
// providers.
func resolveProvider(env *Env, d *DispatchDefaults) (string, error) {
	var (
		raw    string
		source string
	)
	if d != nil && strings.TrimSpace(d.Provider) != "" {
		raw, source = d.Provider, "--provider"
	} else if env != nil && env.Getenv != nil && strings.TrimSpace(env.Getenv(envProvider)) != "" {
		raw, source = env.Getenv(envProvider), envProvider
	} else if v := loadProviderSetting(env); v != "" {
		raw, source = v, `settings "provider"`
	} else if id, _, ok := provider.Detect(envGetenv(env)); ok {
		// Auto-detection: the first provider (registry order: openrouter >
		// anthropic > openai > the rest) whose API-key variable is set.
		return id, nil
	} else {
		return providerOpenRouter, nil
	}
	p := strings.ToLower(strings.TrimSpace(raw))
	if provider.IsKnown(p) {
		return p, nil
	}
	return p, unknownProviderError(strings.TrimSpace(raw), source)
}

// loadProviderSetting returns the settings files' top-level "provider" string
// (later, higher-precedence sources win), or "" when unset/unreadable. All
// canonical sources (user, project, local, flag, policy) are consulted: a nil
// allow-list would activate only the flag/policy sources.
func loadProviderSetting(env *Env) string {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	var out string
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if v, ok := or.Result.Settings["provider"].(string); ok && strings.TrimSpace(v) != "" {
			out = strings.TrimSpace(v)
		}
	}
	return out
}

// resolveAuth populates APIKey for the selected provider from the environment
// and settings via internal/auth's provider-key resolver:
// <PROVIDER>_API_KEY > settings ("apiKeys") > apiKeyHelper. A
// miss leaves APIKey empty, so the default caller refuses gracefully. There is
// NO gocode OAuth/gateway and no network refresh here — provider API keys only.
func (s *printSession) resolveAuth(env *Env) {
	resolver := newKeyResolver(env)
	p := s.Provider
	if p == "" {
		p = providerOpenRouter
	}
	if key, source, ok := resolver.ResolveProviderKey(auth.Provider(p)); ok {
		s.APIKey = key
		s.KeySource = source
	}
}

// backend builds the authenticated provider backend for the session's provider
// and key (no model needed — it is also what the /model picker lists models
// from). It refuses when no key is present.
func (s *printSession) backend() (provider.Provider, error) {
	p := s.Provider
	if p == "" {
		p = providerOpenRouter
	}
	// The endpoint: the injected BaseURL seam (handed to the provider struct
	// verbatim), else this provider's own configured base URL (never another
	// provider's; the API root — OpenRouter's constructor converts it to the
	// chat endpoint its struct takes), else the default.
	cfg := s.baseURLs[p]
	spec, known := provider.Lookup(p)
	if !known {
		return nil, unknownProviderError(p, "session")
	}
	backend, err := spec.New(provider.Config{
		APIKey:       s.APIKey,
		BaseURL:      cfg.URL,
		ExactBaseURL: s.BaseURL,
		HTTP:         s.HTTP,
		Getenv:       s.getenv,
		Referer:      s.Referer,
		Title:        s.Title,
	})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p, err)
	}
	if s.APIKey == "" && !spec.KeyOptional {
		return nil, noCredentialsError(p)
	}
	if s.BaseURL == "" && cfg.Err != nil {
		// A configured but unusable base URL is an error, not a silent
		// fallback to the default endpoint.
		return nil, fmt.Errorf("invalid %s base URL: %w", p, cfg.Err)
	}
	return backend, nil
}

// defaultNewCaller builds the default model caller for the selected provider —
// provider.OpenRouter, provider.Anthropic or provider.OpenAI, authenticated
// with the resolved provider key and honoring the injectable BaseURL/HTTP
// transport seams — wrapped in provider.NewModelCaller. It refuses when no key
// is present so a key-less invocation fails with a clear, provider-specific
// message instead of making an unauthenticated request.
func defaultNewCaller(s *printSession) (agent.ModelCaller, error) {
	backend, err := s.backend()
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		return nil, errNoModel
	}
	mc := provider.NewModelCaller(backend, model)
	mc.SetEffort(s.Effort)
	// Retry budget + the live retry hooks (status line / stderr). Every caller
	// swap (/login, /provider) goes through here, so the hooks follow it.
	s.applyRetryPolicy(mc)
	return mc, nil
}

// loadProviderKeys builds a ProviderKeyStore from the user's settings files'
// top-level "apiKeys" map ({"apiKeys":{"openrouter":"sk-or-..."}}). It never
// fails: unreadable or malformed settings yield an empty store, so provider-key
// resolution simply falls through to the environment / apiKeyHelper. This is
// only the settings source consulted by ResolveProviderKey (after the env).
// All canonical sources are consulted (a nil allow-list would activate only the
// flag/policy sources, so a user-settings key would never be found).
func loadProviderKeys(env *Env) auth.ProviderKeyStore {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	store := auth.MapProviderKeyStore{}
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		// Keys come only from sources the user (or their admin) controls. A
		// repository's own .gocode/settings.json or settings.local.json could
		// otherwise ship someone else's key, and the user's conversation would
		// be sent on that account. Same trust line as settings env blocks.
		if !trustedKeySource(or.Source) {
			continue
		}
		keys, ok := or.Result.Settings["apiKeys"].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range keys {
			if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
				store[auth.Provider(k)] = str
			}
		}
	}
	return store
}

// trustedKeySource reports whether a settings source may supply API keys: the
// user's own settings, --settings, and managed policy — never project or local
// settings that come with a repository.
func trustedKeySource(src settingspolicy.Source) bool {
	switch src {
	case settingspolicy.SourceUser, settingspolicy.SourceFlag, settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
		return true
	}
	return false
}

// modeFromDefaults maps the parsed dispatch-default permission mode to a
// toolreg.Mode. `--dangerously-skip-permissions` is parsed upstream into
// PermissionMode=="bypassPermissions", so it selects ModeBypassPermissions
// here; `--permission-mode <m>` selects the named mode ("manual" is an alias
// of "default"); anything else (and the absence of a flag) is ModeDefault.
func modeFromDefaults(d *DispatchDefaults) toolreg.Mode {
	if d == nil {
		return toolreg.ModeDefault
	}
	switch toolreg.Mode(normalizePermissionModeName(d.PermissionMode)) {
	case toolreg.ModeBypassPermissions:
		return toolreg.ModeBypassPermissions
	case toolreg.ModeAcceptEdits:
		return toolreg.ModeAcceptEdits
	case toolreg.ModePlan:
		return toolreg.ModePlan
	case toolreg.ModeAuto:
		return toolreg.ModeAuto
	case toolreg.ModeDontAsk:
		return toolreg.ModeDontAsk
	default:
		return toolreg.ModeDefault
	}
}

// runPrint is the `-p/--print` entrypoint: it resolves the prompt (from the
// positional argument or piped stdin), builds the print session, and runs one
// turn to completion.
func runPrint(env *Env, d *DispatchDefaults, rest []string) int {
	o, rest, msg := parsePrintOutputFlags(rest)
	if msg == "" {
		msg = o.validate(env)
	}
	if msg != "" {
		fmt.Fprintln(env.Stderr, msg)
		return 1
	}
	var schema map[string]any
	if o.schemaSet {
		if schema, msg = headless.ParseJSONSchema(o.jsonSchema); msg != "" {
			fmt.Fprintln(env.Stderr, msg)
			return 1
		}
	}
	if o.inputFormat == headless.FormatStreamJSON {
		if !o.verbose {
			fmt.Fprintln(env.Stderr, headless.ErrStreamJSONNeedsVerbose)
			return 1
		}
		return runHeadlessStream(env, newPrintSession(env, d), o, schema, env.Stdin)
	}
	prompt := promptFromArgs(env, rest)
	if prompt == "" {
		fmt.Fprintln(env.Stderr, "gocode -p: no prompt provided (pass a prompt argument or pipe one on stdin).")
		return 1
	}
	switch o.format() {
	case headless.FormatStreamJSON:
		if !o.verbose {
			fmt.Fprintln(env.Stderr, headless.ErrStreamJSONNeedsVerbose)
			return 1
		}
		return runHeadlessPrint(env, newPrintSession(env, d), o, schema, singlePrompt(prompt))
	case headless.FormatJSON:
		return runHeadlessPrint(env, newPrintSession(env, d), o, schema, singlePrompt(prompt))
	}
	if schema != nil {
		return runHeadlessPrint(env, newPrintSession(env, d), o, schema, singlePrompt(prompt))
	}
	return runPrintSession(env, newPrintSession(env, d), prompt)
}

// promptFromArgs returns the print prompt: the first positional token, or, when
// there is none and stdin is piped (not a TTY), the trimmed contents of stdin.
func promptFromArgs(env *Env, rest []string) string {
	if p := firstPositional(rest); p != "" {
		return p
	}
	if env.Stdin != nil && !env.StdinTTY {
		data, err := io.ReadAll(env.Stdin)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// runPrintSession runs one conversation turn for the given prompt and prints
// the assistant text. It returns the process exit code: 0 on success, 1 on any
// failure (no credential, API error, expansion error). It never panics.
func runPrintSession(env *Env, s *printSession, prompt string) int {
	return withPrintSession(env, s, true, func(ctx context.Context, env *Env, rt *sessionRuntime) int {
		conv := rt.conv
		// Keep the autocompact window pointed at the active model, then snapshot
		// the history so a /compact can report a real before/after result.
		rt.syncCompactionWindow()
		preCompactTokens, preCompactMsgs := compactionSnapshot(conv.History)

		res, err := conv.Submit(ctx, prompt)
		for _, n := range res.Notices {
			fmt.Fprintln(env.Stderr, "gocode -p: "+n)
		}
		if err != nil {
			fmt.Fprintln(env.Stderr, "gocode -p: "+err.Error())
			if s := errorSuggestion(err); s != "" {
				fmt.Fprintln(env.Stderr, "gocode -p: "+s)
			}
			return 1
		}
		if res.Err != nil {
			fmt.Fprintln(env.Stderr, "gocode -p: "+res.Err.Error())
			if s := errorSuggestion(res.Err); s != "" {
				fmt.Fprintln(env.Stderr, "gocode -p: "+s)
			}
			return 1
		}
		if res.HookBlocked {
			// A UserPromptSubmit hook refused the prompt; the model was not called.
			fmt.Fprintln(env.Stderr, "gocode -p: "+renderLocalEffect(res.Effect))
			return 1
		}

		// Surface the Result discriminator: a locally-handled builtin effect (e.g.
		// /clear, /model) prints its rendered outcome with no model round-trip;
		// /exit prints nothing and exits 0; a normal model turn prints the reply.
		switch res.Kind {
		case conversation.ResultExit:
			return 0
		case conversation.ResultLocal:
			if txt := printLocalResultText(env, s, rt, res, preCompactTokens, preCompactMsgs); txt != "" {
				fmt.Fprintln(env.Stdout, txt)
			}
			return 0
		default:
			// Background commands still running after the turn are awaited;
			// each completion notification runs one more turn, and the last
			// turn's reply is the one printed.
			final, code := res.AssistantText, 0
			rt.awaitBackgroundShells(ctx, env, func(ctx context.Context) bool {
				r2, ran, err := conv.ResumeWithQueuedContext(ctx)
				for _, n := range r2.Notices {
					fmt.Fprintln(env.Stderr, "gocode -p: "+n)
				}
				if err == nil {
					err = r2.Err
				}
				if err != nil {
					fmt.Fprintln(env.Stderr, "gocode -p: "+err.Error())
					code = 1
					return false
				}
				if ran {
					final = r2.AssistantText
				}
				return true
			})
			fmt.Fprintln(env.Stdout, final)
			return code
		}
	})
}

// withPrintSession runs body inside a fully built print-mode session: it
// reports startup refusals and notices, builds the model caller (a failure is
// reported on stderr and exits 1), builds the session — transcript
// persistence included, unless --no-session-persistence — applies a
// --continue/--resume, and closes the session on every exit path. A shutdown
// signal aborts the run; the process then exits with the signal's code
// (SIGINT 0, SIGTERM 143, SIGHUP 129). safeStdout strips terminal control
// sequences from model text printed to a terminal (printsafe.go); the JSON
// formats escape them instead.
func withPrintSession(env *Env, s *printSession, safeStdout bool, body func(ctx context.Context, env *Env, rt *sessionRuntime) int) (code int) {
	if s.startupRefusal != "" {
		fmt.Fprintln(env.Stderr, s.startupRefusal)
		return 1
	}
	if s.modeNotice != "" {
		fmt.Fprintln(env.Stderr, "gocode -p: "+s.modeNotice)
	}
	if safeStdout {
		env = terminalSafeEnv(env)
	}
	if s.restrictedModel != "" {
		fmt.Fprintln(env.Stderr, "gocode -p: "+restrictedModelNotice(s.restrictedModel, s.Model))
	}
	caller, err := s.buildCaller()
	if err != nil {
		fmt.Fprintln(env.Stderr, "gocode -p: "+err.Error())
		if isSetupError(err) && apiKeyHelperConfigured(env) {
			fmt.Fprintln(env.Stderr, "gocode -p: "+apiKeyHelperNotice)
		}
		return 1
	}
	// Retries are silent in -p apart from one stderr note each; stdout carries
	// only the answer.
	s.setRetryObserver(printRetryObserver(env.Stderr))

	// One session-construction path (session.go): plugins, hooks, MCP, the
	// dispatcher (built-ins + MCP + Agent/Skill), the conversation. The
	// session is closed (SessionEnd hooks, MCP servers shut down) on every
	// exit path, including an interrupt.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	sigs := watchShutdownSignals(envGetenv(env))
	defer sigs.Stop()
	sigCode := make(chan int, 1)
	go func() {
		select {
		case c := <-sigs.Codes():
			armShutdownFailsafe(c)
			sigCode <- c
			stop()
		case <-ctx.Done():
		}
	}()
	defer func() {
		select {
		case c := <-sigCode:
			code = c
		default:
		}
	}()
	// A -p run is recorded like an interactive one: a fresh session writes its
	// transcript as it goes, and a --resume/--continue appends to (or, with
	// --fork-session, copies) the session it loads.
	rt := buildSession(ctx, env, s, caller, sessionConfig{
		sessionID:     s.resume.sessionID,
		persistNew:    true,
		noPersistence: s.resume.noPersistence,
		cleanupDays:   defaultCleanupDays,
	})
	defer rt.Close("other")
	if code, stop := sandboxStartupCheck(env, rt.sandbox); stop {
		return code
	}
	// Surface automatic (pre-turn) compaction in -p too (to stderr so it doesn't
	// pollute the prompt's stdout result).
	rt.conv.OnAutoCompact = func(summarized, before, after int) {
		freed := before - after
		if freed < 0 {
			freed = 0
		}
		fmt.Fprintf(env.Stderr, "gocode: auto-compacted — summarized %d messages, freed ~%d tokens.\n", summarized, freed)
	}
	for _, n := range rt.start(ctx) {
		fmt.Fprintln(env.Stderr, "gocode -p: "+n)
	}
	// A background MCP server that fails is reported with the turn's notices.
	rt.reportBackgroundMCP(false, rt.conv.Notify, nil)
	// Resume/continue: load the requested session's history into the conversation
	// so the prompt runs in its context (and appends to it).
	if n := rt.applyPrintResume(s.resume); n != "" {
		fmt.Fprintln(env.Stderr, "gocode -p: "+n)
	}
	return body(ctx, env, rt)
}

// printLocalResultText renders a locally handled slash command's outcome as
// print mode shows it ("" when there is nothing to show).
func printLocalResultText(env *Env, s *printSession, rt *sessionRuntime, res conversation.Result, preCompactTokens, preCompactMsgs int) string {
	conv := rt.conv
	// /help prints the full slash-command list (same source as the
	// interactive autocomplete menu); other local effects render as before.
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectOpenDialog && res.Effect.Dialog == "help" {
		return helpTextForConversation(conv)
	}
	// The informational commands print the same content the interactive host
	// shows (no dialog in -p). /mcp reuses the shared status renderer.
	info := cmdInfo{conv: conv, env: env, print: s, mcp: rt.mcp, lsp: rt.lsp}
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectOpenDialog {
		switch res.Effect.Dialog {
		case "status":
			return info.statusText()
		case "tasks":
			return tasksCommand(conv.BackgroundTasks, res.Effect.Args)
		case "config":
			return info.configText()
		case "doctor":
			return info.doctorText()
		case "effort":
			// -p: no picker and no persistence (the level applies to this
			// session only).
			return effortState{conv: conv, print: s, env: env}.command(res.Effect.Args, false)
		case "rename":
			return renameCommand(conv, res.Effect.Args)
		case "tag":
			return tagCommand(conv, res.Effect.Args)
		case "mcp":
			// Pass the gathered set so pending/skipped project servers are
			// listed (not hidden). -p never prompts; approval guidance is
			// surfaced via the startup notices.
			return renderMCPStatus(&rt.gathered, rt.mcp)
		}
	}
	// /version prints a stub; substitute the running version.
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectPrintText && res.Effect.Text == builtincmds.VersionStubText {
		return versionLine()
	}
	// /context prints a stub; substitute the real (approximate) breakdown.
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectPrintText && res.Effect.Text == contextStubText {
		return info.contextText()
	}
	// /usage (/cost /stats) prints a stub; substitute the real cost report.
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectPrintText && res.Effect.Text == usageStubText {
		return info.usageText()
	}
	// /compact actually summarized (the Compactor is wired): report the real
	// before/after result instead of the static "Conversation compacted."
	if res.Effect != nil && res.Effect.Kind == builtincmds.EffectTriggerCompaction {
		return compactionReport(res, preCompactTokens, preCompactMsgs)
	}
	return renderLocalEffect(res.Effect)
}

// hasPrintFlag reports whether the argument list carries the `-p`/`--print`
// print-mode flag.
func hasPrintFlag(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--print" {
			return true
		}
	}
	return false
}
