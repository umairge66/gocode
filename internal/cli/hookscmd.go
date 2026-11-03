package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/ui"
)

// /hooks: a read-only view of the configured hooks — events, then matchers,
// then the hooks of a matcher, then one hook's details and where it is
// configured. Hooks are added or changed by editing settings.json (or asking
// gocode to).

// hookEventInfo is one hook event and what it fires on.
type hookEventInfo struct {
	event   hooks.Event
	summary string
}

// hookViewEvents are the events gocode runs hooks for, in lifecycle order.
// Other configured events follow them in the view, marked as not fired.
var hookViewEvents = []hookEventInfo{
	{hooks.EventPreToolUse, "Before tool execution"},
	{hooks.EventPermissionDenied, "When a tool call is denied permission"},
	{hooks.EventPostToolUse, "After tool execution"},
	{hooks.EventPostToolUseFailure, "After a tool call fails"},
	{hooks.EventNotification, "When gocode asks for permission or waits for input"},
	{hooks.EventUserPromptSubmit, "When the user submits a prompt"},
	{hooks.EventSessionStart, "When a new session is started"},
	{hooks.EventStop, "Right before gocode concludes its response"},
	{hooks.EventSubagentStart, "When a subagent (Agent tool call) is started"},
	{hooks.EventSubagentStop, "Right before a subagent (Agent tool call) concludes its response"},
	{hooks.EventPreCompact, "Before conversation compaction"},
	{hooks.EventSessionEnd, "When a session is ending"},
}

// unfiredEventSummary describes an event gocode does not fire.
const unfiredEventSummary = "Not fired by gocode: hooks configured here never run"

// hookViewList returns the events the /hooks view lists: hookViewEvents,
// then any other configured event (gocode fires none of them), sorted.
func hookViewList(byEvent map[hooks.Event][]hookhost.ConfiguredHook) []hookEventInfo {
	list := append([]hookEventInfo(nil), hookViewEvents...)
	known := map[hooks.Event]bool{}
	for _, e := range list {
		known[e.event] = true
	}
	var extra []string
	for ev := range byEvent {
		if !known[ev] {
			extra = append(extra, string(ev))
		}
	}
	sort.Strings(extra)
	for _, ev := range extra {
		list = append(list, hookEventInfo{hooks.Event(ev), unfiredEventSummary})
	}
	return list
}

// hooksReadOnlyNote heads the /hooks views.
const hooksReadOnlyNote = "This menu is read-only. To add or change a hook, edit settings.json or ask gocode."

// configuredHooks returns the session's registered hooks.
func (d *interactiveDialogs) configuredHooks() []hookhost.ConfiguredHook {
	if d.hookHost == nil {
		return nil
	}
	return d.hookHost.Configured()
}

// hooksView runs /hooks.
func (d *interactiveDialogs) hooksView() {
	all := d.configuredHooks()
	byEvent := map[hooks.Event][]hookhost.ConfiguredHook{}
	for _, h := range all {
		byEvent[h.Event] = append(byEvent[h.Event], h)
	}
	for {
		withHooks := 0
		var items []ui.ListItem
		for _, ev := range hookViewList(byEvent) {
			n := len(byEvent[ev.event])
			detail := ev.summary
			if n > 0 {
				withHooks++
				detail = countNoun(n, "hook", "hooks") + " · " + ev.summary
			}
			items = append(items, ui.ListItem{Label: string(ev.event), Value: string(ev.event), Detail: detail})
		}
		body := []string{hooksReadOnlyNote}
		switch {
		case d.hookHost != nil && d.hookHost.Disabled():
			body = append(body, "Managed settings switch all hooks off: no hook runs in this session.")
		case d.hookHost != nil && d.hookHost.ManagedOnly():
			body = append(body, "Only hooks from managed settings run here (allowManagedHooksOnly or disableAllHooks).")
		}
		if len(all) == 0 {
			body = append(body, "No hooks configured.")
		} else {
			body = append(body, countNoun(len(all), "hook", "hooks")+" on "+countNoun(withHooks, "event", "events"))
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog("Hooks", items, 0, body...))
		if !ok || r.cancelled {
			d.sendMsg(ui.AppendSystemMsg{Text: "Hooks dialog dismissed"})
			return
		}
		ev := hooks.Event(firstChosen(r.values))
		if ev == "" {
			return
		}
		d.hookEventView(ev, byEvent[ev])
	}
}

// hookEventSummary returns an event's description.
func hookEventSummary(ev hooks.Event) string {
	for _, e := range hookViewEvents {
		if e.event == ev {
			return e.summary
		}
	}
	return unfiredEventSummary
}

// hookEventView shows an event's matchers (or straight its hooks when none of
// them has a matcher).
func (d *interactiveDialogs) hookEventView(ev hooks.Event, hs []hookhost.ConfiguredHook) {
	if len(hs) == 0 {
		d.openAndWait(context.Background(), &ui.Dialog{Kind: ui.DialogAsk, Title: string(ev),
			Body:    []string{hookEventSummary(ev), "No hooks configured for this event", "To add hooks, edit settings.json directly or ask gocode"},
			Options: []ui.DialogOption{{Label: "Back", Value: "back"}}})
		return
	}
	var matchers []string
	byMatcher := map[string][]hookhost.ConfiguredHook{}
	for _, h := range hs {
		m := h.Matcher
		if m == "*" {
			m = ""
		}
		if _, ok := byMatcher[m]; !ok {
			matchers = append(matchers, m)
		}
		byMatcher[m] = append(byMatcher[m], h)
	}
	if len(matchers) == 1 && matchers[0] == "" {
		d.hookListView(ev, "", byMatcher[""])
		return
	}
	sort.Strings(matchers)
	for {
		items := make([]ui.ListItem, len(matchers))
		for i, m := range matchers {
			label, detail := m, countNoun(len(byMatcher[m]), "hook", "hooks")
			if m == "" {
				label = "(all)"
				detail = "matches everything · " + detail
			}
			items[i] = ui.ListItem{Label: label, Value: strconv.Itoa(i), Detail: detail}
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog(string(ev)+" - Matchers", items, 12, hookEventSummary(ev)))
		if !ok || r.cancelled {
			return
		}
		i, err := strconv.Atoi(firstChosen(r.values))
		if err != nil || i < 0 || i >= len(matchers) {
			return
		}
		d.hookListView(ev, matchers[i], byMatcher[matchers[i]])
	}
}

// hookListView lists a matcher's hooks; choosing one shows its details.
func (d *interactiveDialogs) hookListView(ev hooks.Event, matcher string, hs []hookhost.ConfiguredHook) {
	title := string(ev)
	if matcher != "" {
		title += " - Matcher: " + matcher
	} else if ev == hooks.EventPreToolUse || ev == hooks.EventPostToolUse {
		title += " - Matcher: (all)"
	}
	for {
		items := make([]ui.ListItem, len(hs))
		for i, h := range hs {
			items[i] = ui.ListItem{Label: hookTarget(h.Hook), Value: strconv.Itoa(i), Detail: hookSourceLabel(h)}
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog(title, items, 12, hookEventSummary(ev)))
		if !ok || r.cancelled {
			return
		}
		i, err := strconv.Atoi(firstChosen(r.values))
		if err != nil || i < 0 || i >= len(hs) {
			return
		}
		d.openAndWait(context.Background(), &ui.Dialog{Kind: ui.DialogAsk,
			Title:   hookDetailTitle(hs[i]),
			Body:    d.hookDetailLines(hs[i]),
			Options: []ui.DialogOption{{Label: "Back", Value: "back"}}})
	}
}

// hookTypeLabel names a hook's type.
func hookTypeLabel(t hooks.HookType) string {
	switch t {
	case hooks.HookTypePrompt:
		return "Prompt hook"
	case hooks.HookTypeHTTP:
		return "HTTP hook"
	case hooks.HookTypeMCPTool:
		return "MCP tool hook"
	case hooks.HookTypeMCPPrompt:
		return "MCP prompt hook"
	}
	return "Command hook"
}

// hookTarget is the one-line "what it runs" of a hook.
func hookTarget(h hooks.CommandHook) string {
	switch h.NormalizeType() {
	case hooks.HookTypePrompt:
		return firstLine(h.Prompt)
	case hooks.HookTypeHTTP:
		return h.URL
	case hooks.HookTypeMCPTool:
		return h.Server + " · " + h.Tool
	case hooks.HookTypeMCPPrompt:
		return h.Server + " · " + h.PromptName
	}
	return firstLine(strings.TrimSpace(strings.Join(append([]string{h.Command}, h.Args...), " ")))
}

// hookSourceLabel names where a hook is configured.
func hookSourceLabel(h hookhost.ConfiguredHook) string {
	switch h.Source {
	case hookhost.SourcePlugin:
		if h.Plugin != "" {
			return "Plugin (" + h.Plugin + ")"
		}
		return "Plugin"
	case hookhost.SourceSkill:
		return "Skill (registered this session)"
	}
	return capitalize(permSourceLabel(string(h.Source)))
}

// hookDetailTitle is the detail view's title.
func hookDetailTitle(h hookhost.ConfiguredHook) string {
	m := h.Matcher
	if m == "" || m == "*" {
		m = "(all)"
	}
	return string(h.Event) + " · " + m
}

// hookDetailLines describes one hook: its type, what it runs, its options,
// where it comes from and how to change it.
func (d *interactiveDialogs) hookDetailLines(h hookhost.ConfiguredHook) []string {
	hk := h.Hook
	lines := []string{hookTypeLabel(hk.NormalizeType())}
	switch hk.NormalizeType() {
	case hooks.HookTypePrompt:
		lines = append(lines, "Prompt: "+hk.Prompt)
		if hk.Model != "" {
			lines = append(lines, "Model: "+hk.Model)
		}
	case hooks.HookTypeHTTP:
		lines = append(lines, "URL: "+hk.URL)
	case hooks.HookTypeMCPTool:
		lines = append(lines, "MCP tool: "+hk.Server+" · "+hk.Tool)
	case hooks.HookTypeMCPPrompt:
		lines = append(lines, "MCP prompt: "+hk.Server+" · "+hk.PromptName)
	default:
		lines = append(lines, "Command: "+strings.TrimSpace(strings.Join(append([]string{hk.Command}, hk.Args...), " ")))
	}
	if hk.If != "" {
		lines = append(lines, "If: "+hk.If)
	}
	if hk.TimeoutSeconds > 0 {
		lines = append(lines, fmt.Sprintf("Timeout: %ds", hk.TimeoutSeconds))
	}
	if hk.StatusMessage != "" {
		lines = append(lines, "Status message: "+hk.StatusMessage)
	}
	var flags []string
	if hk.Once {
		flags = append(flags, "once")
	}
	if hk.Async {
		flags = append(flags, "async")
	}
	if hk.AsyncRewake {
		flags = append(flags, "async (rewake)")
	}
	if len(flags) > 0 {
		lines = append(lines, "Runs: "+strings.Join(flags, ", "))
	}
	lines = append(lines, "Source: "+hookSourceLabel(h))
	switch h.Source {
	case hookhost.SourcePlugin:
		lines = append(lines, "This hook comes from a plugin and stops running when that plugin is disabled (/plugin).")
	case hookhost.SourceSkill:
		lines = append(lines, "This hook was registered by a skill while gocode was running, not by a settings file, and can't be changed here.")
	case settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
		lines = append(lines, "This hook is set by your organization's managed settings and can't be changed here.")
	default:
		file := "settings.json"
		if p, ok := settingsContext(d.env).PathForSource(h.Source); ok {
			file = displayPath(d.env, p)
		}
		lines = append(lines, "To change or remove this hook, edit "+file+" or ask gocode.")
	}
	return lines
}
