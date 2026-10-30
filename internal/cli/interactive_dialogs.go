package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/pluginmgr"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the interactive /model, /login and /provider dialog flows onto
// the ui host contract. Each flow runs on its own goroutine (spawned from the
// turn driver's renderLocal when the matching EffectOpenDialog is seen), opens
// one or more ui dialogs, and blocks on a reply channel that the shared
// OnDialogSubmit / OnDialogCancel callbacks fulfil from the single program
// goroutine — mirroring the dialogAsker / dialogPrompter seams. toolrun
// serializes permission/ask dialogs, and these flows are only started from a
// locally-handled builtin effect (never mid tool-call), so at most one of
// asker / prompter / dialogs is ever pending at a time.
//
// Model access is via provider API keys only: /login saves a key to
// <configHome>/auth.json (0600) and retargets the live caller so it works
// immediately without a restart; /provider switches the active provider (and its
// default model), chaining into /login when the chosen provider has no key. The
// entered key is NEVER rendered or echoed. There is no gocode OAuth/account and
// no telemetry.

// bannerHintLine is the welcome-banner hint shown on a fresh interactive session.
const bannerHintLine = ui.DefaultBannerHint

// chooseGreeting picks the start screen's greeting for a session from
// ui.Greetings. Tests replace it to get a fixed greeting.
var chooseGreeting = func() string { return ui.Greetings[rand.IntN(len(ui.Greetings))] }

// interactiveDialogs owns the /model, /login and /provider dialog flows and the
// welcome banner / footer state that tracks the active provider + model.
type interactiveDialogs struct {
	mu      sync.Mutex
	send    func(tea.Msg)
	pending chan dialogReply

	s    *interactiveSession
	conv *conversation.Conversation
	env  *Env

	// authPath is where /login persists keys; resolved identically to the key
	// resolver's AuthFile so a save is found by the banner's own resolver.
	authPath string

	// Identity used to (re)render the banner + footer. Guarded by idMu.
	idMu     sync.Mutex
	appName  string
	version  string
	cwd      string
	provider string
	model    string
	greeting string // chosen once per session (chooseGreeting)

	// listModels fetches the catalog for the /model picker. Nil uses the live
	// provider (provider.ListModels); tests inject a fake.
	listModels func(ctx context.Context) ([]provider.ModelInfo, error)
	// models caches the fetched catalogs so the picker opens at once (see
	// modelcatalog.go).
	models modelCatalogCache

	// pluginMgr drives the /plugin flow (marketplace add, browse & install,
	// manage installed). Nil disables /plugin (the flow reports it is
	// unavailable).
	pluginMgr *pluginmgr.Manager

	// mcpHost is the live MCP host, so an approved project server can be
	// connected mid-session (AddServers). Nil disables live reconnection (the
	// flow then tells the user to restart).
	mcpHost *mcphost.Host
	// refreshTools re-advertises the conversation's tool schemas to the model
	// after new MCP servers connect, so their tools become callable without a
	// restart. Nil skips the refresh.
	refreshTools func()
	// searchEngine is the session's live Grep/Glob engine; /search applies new
	// settings to it (filetools.SearchEngine.Apply). Nil: changes apply next session.
	searchEngine *filetools.SearchEngine

	// store / cwd drive the /resume picker: the LOCAL session transcript store and
	// the working directory whose sessions are listed. Nil store disables /resume
	// (persistence off / no config home).
	store     *sessionstore.Store
	resumeCwd string
	// doResume loads a chosen session id into the live conversation and binds the
	// store so continuation appends to it. It returns the loaded session meta and
	// ok=false on failure. Wired by the host to sessionRuntime.resumeInto.
	doResume func(id string) (sessionstore.Meta, bool)

	// rules is the session's live permission rule set; /permissions adds and
	// removes rules in it as well as in the settings files. Nil: changes
	// apply next session.
	rules *toolrun.SharedRules
	// hookHost is the session's hook host, listed by /hooks. Nil lists the
	// settings files' hooks only.
	hookHost *hookhost.Host
	// addDirs are the session's --add-dir directories (/permissions'
	// workspace view).
	addDirs []string
	// setAutoCompact turns automatic compaction on or off for the live
	// session (/config). Nil: the change applies next session.
	setAutoCompact func(bool)
	// openEditor opens a file in the external editor; nil uses the real
	// editor (editor.go). Tests inject a fake.
	openEditor func(path string) (editorChoice, error)
	// now is the clock for /export's default file name (nil: time.Now).
	now func() time.Time

	// onContextChanged re-sends the context gauge / footer figures after a flow
	// that changes the history or the model window outside a turn (/resume, a
	// model or provider switch). Nil skips it.
	onContextChanged func()
}

// contextChanged runs the onContextChanged hook when wired.
func (d *interactiveDialogs) contextChanged() {
	if d.onContextChanged != nil {
		d.onContextChanged()
	}
}

// newInteractiveDialogs builds the controller from the running session. The
// send func and the identity are set by the caller (bind / seedIdentity) once
// the program and caller exist.
func newInteractiveDialogs(s *interactiveSession, conv *conversation.Conversation, env *Env) *interactiveDialogs {
	d := &interactiveDialogs{
		s:          s,
		conv:       conv,
		env:        env,
		send:       func(tea.Msg) {},
		authPath:   authFilePath(env),
		listModels: s.listModels,
		openEditor: s.openEditor,
		now:        s.now,
	}
	if d.listModels == nil {
		d.listModels = func(ctx context.Context) ([]provider.ModelInfo, error) {
			// List from the live caller's provider when there is one; otherwise
			// (no model chosen yet) straight from the session's provider + key,
			// so the picker works before any model is set.
			if pc, ok := d.conv.Model.(*provider.ModelCaller); ok {
				return provider.ListModels(ctx, pc.CurrentProvider())
			}
			b, err := d.s.print.backend()
			if err != nil {
				return nil, err
			}
			return provider.ListModels(ctx, b)
		}
	}
	return d
}

// bind attaches the program's Send func (called once the program is built).
func (d *interactiveDialogs) bind(send func(tea.Msg)) {
	d.mu.Lock()
	d.send = send
	d.mu.Unlock()
}

// seedIdentity records the initial banner/footer identity.
func (d *interactiveDialogs) seedIdentity(appName, version, cwd, prov, model string) {
	d.idMu.Lock()
	d.appName, d.version, d.cwd, d.provider, d.model = appName, version, cwd, prov, model
	d.greeting = chooseGreeting()
	d.idMu.Unlock()
}

// submit / cancel are the ui.Model dialog seams, mirroring dialogAsker.
func (d *interactiveDialogs) submit(values []string) { d.deliver(dialogReply{values: values}) }
func (d *interactiveDialogs) cancel()                { d.deliver(dialogReply{cancelled: true}) }

func (d *interactiveDialogs) deliver(r dialogReply) {
	d.mu.Lock()
	ch := d.pending
	d.pending = nil
	d.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// openAndWait opens dlg and blocks until the user confirms or dismisses it (or
// ctx is cancelled). ok=false means the wait was abandoned (ctx cancelled).
func (d *interactiveDialogs) openAndWait(ctx context.Context, dlg *ui.Dialog) (dialogReply, bool) {
	ch := make(chan dialogReply, 1)
	d.mu.Lock()
	d.pending = ch
	send := d.send
	d.mu.Unlock()

	send(ui.OpenDialogMsg{Dialog: dlg})
	select {
	case <-ctx.Done():
		d.mu.Lock()
		d.pending = nil
		d.mu.Unlock()
		send(ui.CloseDialogMsg{})
		return dialogReply{}, false
	case r := <-ch:
		return r, true
	}
}

// sendMsg posts a message to the program.
func (d *interactiveDialogs) sendMsg(msg tea.Msg) {
	d.mu.Lock()
	send := d.send
	d.mu.Unlock()
	send(msg)
}

// --- banner + footer -------------------------------------------------------

// sendBanner (re)renders the welcome banner and the footer model from the
// current identity, computing the no-key warning from the SAME resolver the
// banner and `auth status` use (so a key saved by /login clears it).
func (d *interactiveDialogs) sendBanner() {
	d.idMu.Lock()
	b := &ui.Banner{
		AppName:  d.appName,
		Version:  d.version,
		Cwd:      d.cwd,
		Provider: d.provider,
		Model:    d.model,
		Hint:     bannerHintLine,
		Greeting: d.greeting,
	}
	prov, model := d.provider, d.model
	d.idMu.Unlock()
	// The banner's model line carries the explicit effort ("<model> with high
	// effort"); the footer keeps the bare model id.
	if b.Model != "" {
		b.Model += effortModelSuffix(d.effort().current())
	}

	if prov != "" && !d.providerHasKey(prov) {
		b.Warning = "No API key for " + prov + " — run /login"
	} else if strings.TrimSpace(model) == "" {
		b.Warning = "No model selected — run /model"
	}
	d.sendMsg(ui.SetBannerMsg{Banner: b})
	d.sendMsg(ui.SetFooterModelMsg{Model: model})
}

// providerHasKey reports whether a key resolves for prov through the shared
// resolver (env > auth.json > settings > apiKeyHelper).
func (d *interactiveDialogs) providerHasKey(prov string) bool {
	_, _, ok := newKeyResolver(d.env).ResolveProviderKey(auth.Provider(prov))
	return ok
}

// setModel updates the active model in the identity and re-renders.
func (d *interactiveDialogs) setModel(model string) {
	d.idMu.Lock()
	d.model = model
	d.idMu.Unlock()
	d.sendBanner()
	d.contextChanged()
}

// setProviderModel updates the active provider + model in the identity and
// re-renders (the banner warning reflects whether the new provider has a key).
func (d *interactiveDialogs) setProviderModel(prov, model string) {
	d.idMu.Lock()
	d.provider, d.model = prov, model
	d.idMu.Unlock()
	d.sendBanner()
	d.contextChanged()
}

// modelListErrorText is the short message shown when the model catalog
// cannot be fetched. It names the failure in plain words (a provider's
// friendly error message, a timeout, or an unreadable list) and suggests
// /login only when no key is set or the key was rejected. The raw error is
// appended only when GOCODE_DEBUG is set.
func modelListErrorText(err error, hasKey bool, command string, debug bool) string {
	reason := "the provider's model list could not be read"
	suggestLogin := !hasKey
	var apiErr *provider.APIError
	switch {
	case errors.As(err, &apiErr) && strings.TrimSpace(apiErr.Message) != "":
		reason = strings.TrimRight(strings.TrimSpace(apiErr.Message), ".")
		if apiErr.Category == provider.CategoryAuth {
			suggestLogin = true
		}
	case errors.Is(err, context.DeadlineExceeded):
		reason = "the request timed out"
	case errors.Is(err, context.Canceled):
		reason = "the request was cancelled"
	}
	text := "Could not list models: " + reason + "."
	if debug {
		text += " (" + err.Error() + ")"
	}
	if suggestLogin {
		return text + " Set a key with /login, or pass " + command + " <id>."
	}
	return text + " Pass " + command + " <id> to choose a model directly."
}

// --- /model picker ---------------------------------------------------------

// modelPicker fetches the model catalog and opens the "Select model" picker;
// the chosen id is applied through the same path as `/model <id>`.
func (d *interactiveDialogs) modelPicker() {
	if id := d.chooseModel("Select model", "/model"); id != "" {
		d.applyModel(id)
	}
}

// chooseModel fetches the model catalog and opens a model picker titled
// title, listing the models the policy allows. It returns the chosen id, or ""
// when nothing could be listed or the picker was cancelled. command is the
// typed form ("/model", "/fallback") the error hints point at.
func (d *interactiveDialogs) chooseModel(title, command string) string {
	models, refresh, err := d.pickerModels()
	if refresh != nil {
		defer refresh()
	}
	if err != nil {
		d.idMu.Lock()
		prov := d.provider
		d.idMu.Unlock()
		hasKey := prov == "" || d.providerHasKey(prov)
		debug := d.env != nil && d.env.Getenv != nil && d.env.Getenv("GOCODE_DEBUG") != ""
		d.sendMsg(ui.AppendSystemMsg{Text: modelListErrorText(err, hasKey, command, debug)})
		return ""
	}
	if len(models) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No models available. Pass " + command + " <id>."})
		return ""
	}
	info := cmdInfo{conv: d.conv, env: d.env}
	if d.s != nil {
		info.print = d.s.print
	}
	prov := info.currentProvider()
	// availableModels / deniedModels: the picker lists only the models the
	// policy allows.
	models = loadModelPolicy(d.env, prov).pickerModels(models)
	if len(models) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No models available: your organization's settings allow none of this provider's models."})
		return ""
	}
	items := make([]ui.ListItem, 0, len(models))
	for _, m := range models {
		if m.Lifecycle == provider.LifecycleRetired {
			continue // no longer callable (the Anthropic list omits these by default)
		}
		items = append(items, ui.ListItem{Label: m.Name, Value: m.ID, Detail: modelDetail(prov, m)})
	}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog(title, items, 10))
	if !ok || r.cancelled {
		return ""
	}
	return firstChosen(r.values)
}

// applyModel applies the chosen model id through the conversation effect (the
// same path `/model <id>` uses), then updates the footer and echoes a line.
func (d *interactiveDialogs) applyModel(id string) {
	res, err := d.conv.Submit(context.Background(), "/model "+id)
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
		return
	}
	if res.Effect != nil && res.Effect.Kind != builtincmds.EffectSetModel {
		// Refused by the model policy (conversation.ModelGate).
		d.sendMsg(ui.AppendSystemMsg{Text: res.Effect.Text})
		return
	}
	if res.Effect != nil && res.Effect.Model != "" {
		id = res.Effect.Model
	}
	d.s.print.Model = id
	// Started without a model (or key): the real caller may be buildable now.
	// With the key still missing, the placeholder keeps saying what to do.
	if isUnconfigured(d.conv) {
		if _, err := activateCaller(d.conv, d.s.print); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
			return
		}
	}
	d.setModel(id)
	// Persist the picker choice so the model is remembered next session (the
	// typed `/model <id>` form persists via the turn driver; the picker submits
	// off the turn loop, so it must persist here too).
	_ = writeLocalSettingString(d.env, "model", id)
	d.sendMsg(ui.AppendSystemMsg{Text: "Model set to " + id})
	d.sendMsg(ui.ToastMsg{Text: "Model switched to " + id, Kind: ui.ToastSuccess})
	info := cmdInfo{conv: d.conv, env: d.env}
	if d.s != nil {
		info.print = d.s.print
	}
	if n := info.lifecycleNotice(id); n != "" {
		d.sendMsg(ui.AppendSystemMsg{Text: n})
	}
}

// --- /effort picker --------------------------------------------------------

// effort returns the live effort controller (caller + session + settings).
func (d *interactiveDialogs) effort() effortState {
	st := effortState{conv: d.conv, env: d.env}
	if d.s != nil {
		st.print = d.s.print
	}
	return st
}

// effortPicker opens the "Effort" picker (levels + auto, the current one
// marked) for a bare /effort and applies the choice through the same path as
// `/effort <level>` (live + persisted), then refreshes the banner.
func (d *interactiveDialogs) effortPicker() {
	st := d.effort()
	cur := st.current()
	mark := func(level, detail string) string {
		if level == cur {
			return "current · " + detail
		}
		return detail
	}
	items := make([]ui.ListItem, 0, len(provider.EffortLevels)+1)
	for _, l := range provider.EffortLevels {
		items = append(items, ui.ListItem{Label: effortPickerLabels[l], Value: l, Detail: mark(l, effortUsageSummary[l])})
	}
	items = append(items, ui.ListItem{Label: "Auto", Value: "auto", Detail: mark("", "Use the default effort level for your model")})
	r, ok := d.openAndWait(context.Background(), ui.ListDialog("Effort", items, 0, st.currentText(), effortPickerHelp))
	if !ok || r.cancelled {
		return
	}
	v := firstChosen(r.values)
	if v == "" {
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: st.command(v, true)})
	d.sendBanner()
}

// modelDetail renders a model's dimmed trailing description for the picker:
// the context window (the listing's own figure, else the capability table's),
// the vision / effort badges from the capability table, a "deprecated" badge,
// then the description.
func modelDetail(prov string, m provider.ModelInfo) string {
	caps := provider.CapabilitiesFor(prov, m.ID)
	var parts []string
	window := m.ContextLength
	if window <= 0 && caps.Known() {
		window = caps.ContextWindow
	}
	if window > 0 {
		parts = append(parts, formatTokenCount(window)+" ctx")
	}
	if caps.Known() {
		parts = append(parts, capabilityBadges(caps)...)
	}
	if m.Lifecycle == provider.LifecycleDeprecated {
		parts = append(parts, "deprecated")
	}
	if m.Description != "" {
		parts = append(parts, m.Description)
	}
	return strings.Join(parts, " · ")
}

// --- /login ----------------------------------------------------------------

// login runs the /login flow: pick a provider (or use the arg), then prompt for
// and save a key.
func (d *interactiveDialogs) login(arg string) {
	prov, ok := d.resolveProviderArgOrPick(arg, "Select a provider to sign in")
	if !ok {
		return
	}
	d.promptAndSaveKey(prov)
}

// promptAndSaveKey opens the masked key prompt for prov, saves the entered key to
// auth.json (0600), retargets the live caller and clears the banner warning. The
// key is NEVER echoed.
func (d *interactiveDialogs) promptAndSaveKey(prov string) {
	title := "Paste " + strings.ToUpper(prov) + " API key"
	r, ok := d.openAndWait(context.Background(), ui.SecretDialog(title))
	if !ok || r.cancelled {
		return
	}
	key := firstChosen(r.values)
	if strings.TrimSpace(key) == "" {
		d.sendMsg(ui.AppendSystemMsg{Text: "No key entered."})
		return
	}
	if err := auth.SaveProviderKey(d.authPath, auth.Provider(prov), key); err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not save key: " + err.Error()})
		return
	}
	// Retarget the live caller only when the key is for the ACTIVE provider (so it
	// works immediately without restart); a key saved for another provider is just
	// persisted and picked up when the user switches to it. This keeps the banner
	// identity and the caller in sync.
	d.idMu.Lock()
	active := d.provider
	d.idMu.Unlock()
	needModel := false
	if prov == active {
		needModel = d.retargetCaller(prov, key)
	}
	// Re-render the banner: the shared resolver now finds the saved key, so the
	// no-key warning for the active provider clears.
	d.sendBanner()
	d.sendMsg(ui.AppendSystemMsg{Text: "Saved " + strings.ToUpper(prov) + " key"})
	if needModel {
		// There is no default model: with the key in, pick one now.
		d.sendMsg(ui.AppendSystemMsg{Text: "Now pick a model."})
		d.modelPicker()
	}
}

// --- /provider -------------------------------------------------------------

// provider runs the /provider flow: pick a provider (or use the arg), then
// switch to it.
func (d *interactiveDialogs) switchProviderFlow(arg string) {
	prov, ok := d.resolveProviderArgOrPick(arg, "Select a provider")
	if !ok {
		return
	}
	d.switchProvider(prov)
}

// switchProvider makes prov the active provider. There is no default model and
// model ids are provider-specific, so the previous model is cleared and the
// user picks one: the picker opens right away when prov has a key; without a
// key the key prompt comes first (and chains into the picker).
func (d *interactiveDialogs) switchProvider(prov string) {
	d.s.print.Provider = prov
	d.s.print.Model = ""
	d.s.print.providerErr = nil
	d.setProviderModel(prov, "")
	// Persist the switch; drop the old provider's model so the next session
	// doesn't start with an id the new provider doesn't know.
	_ = writeLocalSettingString(d.env, "provider", prov)
	_ = deleteLocalSetting(d.env, "model")

	key, _, hasKey := newKeyResolver(d.env).ResolveProviderKey(auth.Provider(prov))
	d.s.print.APIKey = ""
	if hasKey {
		d.s.print.APIKey = key
	}
	d.conv.Model = placeholderFor(d.s.print)
	d.conv.ModelID = ""
	d.sendBanner()
	if !hasKey {
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Switched to %s — no API key yet.", prov)})
		d.promptAndSaveKey(prov)
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: "Provider set to " + prov + " — now pick a model."})
	d.modelPicker()
}

// --- shared helpers --------------------------------------------------------

// resolveProviderArgOrPick returns the provider named by arg (validated), or
// opens a provider picker when arg is empty. ok=false means an invalid arg or a
// cancelled picker.
func (d *interactiveDialogs) resolveProviderArgOrPick(arg, title string) (string, bool) {
	a := strings.ToLower(strings.TrimSpace(arg))
	if a != "" {
		if isValidProvider(a) {
			return a, true
		}
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Unknown provider %q. Valid: %s.", a, strings.Join(validProviders(), ", "))})
		return "", false
	}
	r, ok := d.openAndWait(context.Background(), d.providerPicker(title))
	if !ok || r.cancelled {
		return "", false
	}
	v := firstChosen(r.values)
	if v == "" || !isValidProvider(v) {
		return "", false
	}
	return v, true
}

// retargetCaller rebuilds the model caller for prov+key and swaps it into the
// live conversation so the new provider/key works immediately (no restart).
// Components that captured the caller at startup follow it through liveCaller.
// It reports needModel=true when the key is in but no model is chosen yet.
func (d *interactiveDialogs) retargetCaller(prov, key string) (needModel bool) {
	d.s.print.Provider = prov
	d.s.print.APIKey = key
	d.s.print.providerErr = nil
	active, err := activateCaller(d.conv, d.s.print)
	if err != nil {
		d.sendMsg(ui.AppendSystemMsg{Text: "Key saved, but could not activate the caller: " + err.Error()})
		return false
	}
	return !active
}

// providerListItems builds the provider picker rows: the id, with the display
// name and key env var(s) as the dimmed detail (every registry provider). The
// current provider's row is marked "current" and a provider with a key set is
// marked "key set" (hasKey nil marks none).
func providerListItems(current string, hasKey func(string) bool) []ui.ListItem {
	specs := provider.Providers()
	out := make([]ui.ListItem, 0, len(specs))
	for _, s := range specs {
		var parts []string
		if s.ID == current {
			parts = append(parts, "current")
		}
		if hasKey != nil && hasKey(s.ID) {
			parts = append(parts, "key set")
		}
		if s.DisplayName != "" && !strings.EqualFold(s.DisplayName, s.ID) {
			parts = append(parts, s.DisplayName)
		}
		if vars := strings.Join(auth.ProviderEnvVars(auth.Provider(s.ID)), "/"); vars != "" {
			parts = append(parts, vars)
		}
		out = append(out, ui.ListItem{Label: s.ID, Value: s.ID, Detail: strings.Join(parts, " · ")})
	}
	return out
}

// providerPicker opens the provider picker with the current provider marked
// and highlighted, and the providers that have a key (environment, auth.json
// or settings — the apiKeyHelper is not run for this) marked.
func (d *interactiveDialogs) providerPicker(title string) *ui.Dialog {
	d.idMu.Lock()
	current := d.provider
	d.idMu.Unlock()
	r := newKeyResolver(d.env)
	r.Helper = nil
	hasKey := func(id string) bool {
		_, _, ok := r.ResolveProviderKey(auth.Provider(id))
		return ok
	}
	items := providerListItems(current, hasKey)
	dlg := ui.ListDialog(title, items, 0)
	for i, it := range items {
		if it.Value == current {
			dlg.Selected = i
			break
		}
	}
	return dlg
}

// isValidProvider reports whether p is one of the selectable providers.
func isValidProvider(p string) bool { return provider.IsKnown(p) }

// firstChosen returns the first chosen value, or "".
func firstChosen(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// --- /resume (session picker) ----------------------------------------------

// resume runs the /resume flow (also the --resume launch path). With a value
// that resolves to a single session (by id or unique id prefix) it resumes that
// one directly; otherwise it opens the interactive picker of this project's
// saved sessions (most-recent first), pre-filtered by the value as a search
// term. Selecting a row loads that session's history into the live conversation
// and continues in it.
func (d *interactiveDialogs) resume(value string) {
	if d.store == nil {
		d.sendMsg(ui.AppendSystemMsg{Text: "Session persistence is disabled; there is nothing to resume."})
		return
	}
	v := strings.TrimSpace(value)
	// A value that names a session id (or a unique id prefix) resumes directly.
	if v != "" {
		if m, ok := d.store.Find(d.resumeCwd, v); ok && (m.ID == v || strings.HasPrefix(m.ID, v)) {
			d.applyResume(m.ID)
			return
		}
	}
	metas, _ := d.store.List(d.resumeCwd)
	if len(metas) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No conversations found in this project."})
		return
	}
	items := make([]ui.ListItem, 0, len(metas))
	for _, m := range metas {
		items = append(items, ui.ListItem{Label: m.Title, Value: m.ID, Detail: resumeDetail(m)})
	}
	dlg := ui.ListDialog("Resume session", items, 10)
	if v != "" {
		dlg.Filter = v // pre-filter by the search term
	}
	r, ok := d.openAndWait(context.Background(), dlg)
	if !ok || r.cancelled {
		return
	}
	id := firstChosen(r.values)
	if id == "" {
		return
	}
	d.applyResume(id)
}

// applyResume loads session id into the live conversation (via the host seam)
// and echoes the outcome. Continuing from here appends to that session.
func (d *interactiveDialogs) applyResume(id string) {
	if d.doResume == nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Resume is unavailable in this session."})
		return
	}
	m, ok := d.doResume(id)
	if !ok {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not resume conversation " + shortID(id) + "."})
		return
	}
	title := m.Title
	if title == "" {
		title = shortID(id)
	}
	// Re-render the just-loaded history into the TUI transcript so the resumed
	// conversation is actually visible (doResume only seeded conv.History). This
	// is the single replay point shared by the interactive /resume picker AND the
	// startup --resume/--continue path (both funnel through applyResume). Each
	// call replays only the session doResume just loaded, so a later /resume that
	// switches sessions replays its own history behind a fresh separator rather
	// than re-replaying an earlier one. It runs on the dialog/startup goroutine
	// and pushes through d.sendMsg (the program's Send), never the UI goroutine.
	replayResumeHistory(d.sendMsg, title, d.conv.History)
	d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Resumed conversation: %s (%d messages). Continuing in this session.", title, m.MessageCount)})
	d.contextChanged()
}

// --- /mcp status + project-server approval ---------------------------------

// mcpApproveAllValue is the sentinel option value for "approve ALL project
// servers (now and later)", which persists enableAllProjectMcpServers rather
// than listing each server in enabledMcpjsonServers.
const mcpApproveAllValue = "\x00approve-all"

// mcpApproveEntryValue is the sentinel ListDialog row value for the leading
// "Approve project servers…" entry, which (re)opens the project-server approval
// dialog. mcpSkippedPrefix marks a non-actionable skipped-server row.
const (
	mcpApproveEntryValue = "\x00approve-entry"
	mcpSkippedPrefix     = "\x00skipped:"
)

// mcp opens the interactive MCP server picker: a ui.ListDialog ("MCP servers")
// whose rows are the configured servers (connected / failed / pending) with a
// "<state> · <N tools>" detail, plus — when project servers await approval — a
// leading "Approve project servers…" entry. Selecting a server row shows that
// server's state and its tool names as a system message
// (e.g. "caveman: connected — caveman_compress, caveman_retrieve"); selecting the
// approve entry reopens the approval dialog. With nothing configured it emits a
// single "No MCP servers configured." line and opens no dialog. Driven by the
// /mcp builtin.
//
// It is built from the existing dialog primitives (ui.ListDialog, exactly as
// /model and /resume do), with the status vocabulary connected / failed /
// pending, N tools. The pending project-server approval flow
// (approveProjectServers) is kept fully reachable through the leading list
// entry rather than auto-opening, so /mcp first shows the complete picture and
// the user chooses to approve. Selecting a live server opens its action menu
// (mcpServerMenu, mcpauth.go): Authenticate / Re-authenticate / Clear
// authentication for OAuth-capable servers, Reconnect, and Show details.
// `gocode mcp list` (non-interactive) and the -p path keep their plain
// renderMCPStatus text and never sign in.
func (d *interactiveDialogs) mcp() {
	g := gatherMCP(d.env, d.s.print)
	pending := pendingProjectServers(g)

	var statuses []mcphost.ServerStatus
	if d.mcpHost != nil {
		statuses = d.mcpHost.Status()
	}

	// Empty state: nothing live/known and nothing skipped. One system line, no
	// empty dialog.
	if len(statuses) == 0 && len(g.Skipped) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No MCP servers configured."})
		return
	}

	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })

	var items []ui.ListItem
	if len(pending) > 0 {
		items = append(items, ui.ListItem{
			Label:  fmt.Sprintf("Approve project servers… (%d pending)", len(pending)),
			Value:  mcpApproveEntryValue,
			Detail: "review & enable project MCP servers",
		})
	}
	for _, st := range statuses {
		items = append(items, ui.ListItem{Label: st.Name, Value: st.Name, Detail: mcpStatusDetail(st)})
	}
	// Skipped servers (pending approval, policy-blocked, …) are not live on the
	// host; list them as non-actionable rows so the picture is complete.
	for _, sk := range g.Skipped {
		items = append(items, ui.ListItem{
			Label:  sk.Name,
			Value:  mcpSkippedPrefix + sk.Name,
			Detail: fmt.Sprintf("skipped · %s", sk.Reason),
		})
	}

	r, ok := d.openAndWait(context.Background(), ui.ListDialog("MCP servers", items, 10))
	if !ok || r.cancelled {
		return
	}
	v := firstChosen(r.values)
	switch {
	case v == "":
		return
	case v == mcpApproveEntryValue:
		if len(pending) > 0 {
			d.approveProjectServers(pending)
		}
	case strings.HasPrefix(v, mcpSkippedPrefix):
		name := strings.TrimPrefix(v, mcpSkippedPrefix)
		for _, sk := range g.Skipped {
			if sk.Name == name {
				d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("%s: skipped (%s) — %s", sk.Name, sk.Scope, sk.Reason)})
				return
			}
		}
	default:
		d.mcpServerMenu(v, statuses)
	}
}

// mcpStatusDetail formats a server's ListDialog detail line: "<state> · <N
// tools>", appending the error for a failed server.
func mcpStatusDetail(st mcphost.ServerStatus) string {
	detail := fmt.Sprintf("%s · %d tools", mcphost.StateLabel(st), st.Tools)
	switch st.State {
	case mcphost.StateFailed, mcphost.StateDisconnected:
		if st.Error != "" {
			detail += " · " + st.Error
		}
	case mcphost.StateNeedsAuth:
		if st.AuthCapable {
			detail += " · select to authenticate"
		}
	}
	return detail
}

// showServerDetail posts the selected server's state and tool names as a system
// message, e.g. "caveman: connected — caveman_compress, caveman_retrieve". A
// server with no tools (not connected, or connected with none) shows just its
// state (plus any failure error).
func (d *interactiveDialogs) showServerDetail(name string, statuses []mcphost.ServerStatus) {
	var st mcphost.ServerStatus
	found := false
	for _, s := range statuses {
		if s.Name == name {
			st, found = s, true
			break
		}
	}
	if !found {
		d.sendMsg(ui.AppendSystemMsg{Text: name + ": unknown server"})
		return
	}
	msg := fmt.Sprintf("%s: %s", st.Name, mcphost.StateLabel(st))
	if tools := serverToolNames(d.mcpHost, name); len(tools) > 0 {
		msg += " — " + strings.Join(tools, ", ")
	} else if (st.State == mcphost.StateFailed || st.State == mcphost.StateDisconnected) && st.Error != "" {
		msg += " — " + st.Error
	}
	d.sendMsg(ui.AppendSystemMsg{Text: msg})
}

// serverToolNames returns the (normalized) tool names the live host exposes for
// a server, derived from the model-facing tool list by stripping the
// "mcp__<server>__" prefix. A nil host or a server with no connected tools
// yields nil. Names are sorted for a stable detail line.
func serverToolNames(h *mcphost.Host, server string) []string {
	if h == nil {
		return nil
	}
	prefix := mcphost.ToolName(server, "")
	var out []string
	for _, sc := range h.Tools() {
		if strings.HasPrefix(sc.Name, prefix) {
			if tool := strings.TrimPrefix(sc.Name, prefix); tool != "" {
				out = append(out, tool)
			}
		}
	}
	sort.Strings(out)
	return out
}

// approveProjectServers opens the multi-select approval dialog for the pending
// project servers ("N new MCP servers found in this project / Select any you
// wish to enable"), persists the choice to the
// local settings file, and connects the approved servers live.
func (d *interactiveDialogs) approveProjectServers(pending []mcphost.Skipped) {
	title := fmt.Sprintf("%d new MCP server(s) found in this project", len(pending))
	if len(pending) == 1 {
		title = "New MCP server found in this project: " + pending[0].Name
	}
	options := []ui.DialogOption{{Label: "Approve ALL project servers (now and later)", Value: mcpApproveAllValue}}
	for _, p := range pending {
		options = append(options, ui.DialogOption{Label: p.Name, Value: p.Name})
	}
	dlg := ui.AskDialog(title, true, options, "Select any you wish to enable; space toggles, enter confirms, esc leaves them pending.")
	// Pre-mark every concrete server (not the "all" sentinel): by default all
	// pending servers are selected.
	for i := range dlg.Marked {
		dlg.Marked[i] = options[i].Value != mcpApproveAllValue
	}

	r, ok := d.openAndWait(context.Background(), dlg)
	if !ok || r.cancelled {
		d.sendMsg(ui.AppendSystemMsg{Text: "Left project MCP servers pending; run /mcp to approve later."})
		return
	}

	names := make(map[string]bool, len(pending))
	for _, p := range pending {
		names[p.Name] = true
	}
	var enable, disable []string
	enableAll := false
	chosen := map[string]bool{}
	for _, v := range r.values {
		if v == mcpApproveAllValue {
			enableAll = true
			continue
		}
		if names[v] {
			chosen[v] = true
		}
	}
	for _, p := range pending {
		switch {
		case enableAll || chosen[p.Name]:
			enable = append(enable, p.Name)
		default:
			disable = append(disable, p.Name)
		}
	}
	if !enableAll && len(enable) == 0 {
		// Nothing selected: persist the rejection so it is not re-prompted.
		if err := writeMCPApproval(d.env, nil, disable, false); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: "Could not save MCP approval: " + err.Error()})
			return
		}
		d.sendMsg(ui.AppendSystemMsg{Text: "No project MCP servers approved."})
		return
	}

	// Persist: enableAll sets enableAllProjectMcpServers; otherwise the chosen
	// names go to enabledMcpjsonServers and the rest to disabledMcpjsonServers.
	var persistEnable []string
	if !enableAll {
		persistEnable = enable
	}
	if err := writeMCPApproval(d.env, persistEnable, disable, enableAll); err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not save MCP approval: " + err.Error()})
		return
	}

	// Re-gather with the just-written settings and connect the newly approved
	// servers live (no restart needed).
	d.connectApproved(enable)
}

// connectApproved re-gathers the MCP set (now that approval is persisted) and
// connects the just-approved servers on the live host, then refreshes the
// model-facing tool schemas so their tools are callable without a restart. When
// there is no live host it falls back to a restart hint.
func (d *interactiveDialogs) connectApproved(names []string) {
	if d.mcpHost == nil {
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Approved %d MCP server(s) — restart to connect.", len(names))})
		return
	}
	g := gatherMCP(d.env, d.s.print)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	configs := map[string]mcp.ServerConfig{}
	for name, cfg := range g.Servers {
		if want[name] {
			configs[name] = cfg
		}
	}
	if len(configs) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "Approved; no connectable servers found (they may be blocked by policy)."})
		return
	}

	// AddServers connects the newly approved servers (already-present names are
	// no-ops) and rebuilds the host's tool table; refreshTools re-advertises them
	// to the model.
	d.mcpHost.AddServers(context.Background(), configs)
	if d.refreshTools != nil {
		d.refreshTools()
	}

	statuses := map[string]mcphost.State{}
	for _, st := range d.mcpHost.Status() {
		statuses[st.Name] = st.State
	}
	names = sortedNames(configs)
	var lines []string
	for _, n := range names {
		if st, ok := statuses[n]; ok {
			lines = append(lines, fmt.Sprintf("%s: %s", n, st))
		}
	}
	msg := fmt.Sprintf("Approved %d project MCP server(s).", len(configs))
	if len(lines) > 0 {
		msg += "\n" + strings.Join(lines, "\n")
	}
	d.sendMsg(ui.AppendSystemMsg{Text: msg})
}

// sortedNames returns the keys of m in sorted order.
func sortedNames(m map[string]mcp.ServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
