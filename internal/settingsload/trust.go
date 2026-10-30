package settingsload

import (
	"fmt"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Workspace trust: the repository-controlled settings sources
// (projectSettings = <project>/.gocode/settings.json and localSettings =
// <project>/.gocode/settings.local.json) can run code or widen permissions the
// moment a session starts. Until the user trusts the folder, the CLI sets
// Context.UntrustedProject and EffectiveSources drops exactly those keys from
// the two sources; everything else (deny rules, theme, plan mode, …) still
// applies, and the user / --settings / policy sources are never touched.

// RiskyItem is one setting from a repository-controlled source that is
// withheld until the folder is trusted.
type RiskyItem struct {
	// Source is projectSettings or localSettings.
	Source settingspolicy.Source
	// Kind names the setting group ("hooks", "MCP servers", "permissions", …).
	Kind string
	// Detail is a short human summary ("SessionStart, PreToolUse", "2",
	// "Bash(*), Read"); may be empty.
	Detail string
}

// String renders the item as "<kind>: <detail>".
func (r RiskyItem) String() string {
	if r.Detail == "" {
		return r.Kind
	}
	return r.Kind + ": " + r.Detail
}

// loosePermissionModes are the defaultMode values that widen what runs without
// asking. "default", "plan" and "dontAsk" are not loosening.
var loosePermissionModes = map[string]bool{
	"acceptEdits":       true,
	"bypassPermissions": true,
	"auto":              true,
}

// IsLoosePermissionMode reports whether mode auto-approves more than the
// default mode does.
func IsLoosePermissionMode(mode string) bool { return loosePermissionModes[strings.TrimSpace(mode)] }

// ScanUntrusted lists the settings in s (a projectSettings or localSettings
// document) that are withheld from an untrusted folder. It never modifies s.
func ScanUntrusted(src settingspolicy.Source, s settingspolicy.Settings) []RiskyItem {
	items, _ := untrustedScan(src, s, false)
	return items
}

// StripUntrusted returns s without the settings ScanUntrusted reports. s is
// not modified: changed maps are copied.
func StripUntrusted(src settingspolicy.Source, s settingspolicy.Settings) settingspolicy.Settings {
	_, out := untrustedScan(src, s, true)
	return out
}

// untrustedScan is the single walker behind ScanUntrusted and StripUntrusted,
// so the list shown to the user and the set actually withheld never drift.
func untrustedScan(src settingspolicy.Source, s settingspolicy.Settings, strip bool) ([]RiskyItem, settingspolicy.Settings) {
	if len(s) == 0 {
		return nil, s
	}
	var items []RiskyItem
	out := s
	cloned := false
	drop := func(key string) {
		if !strip {
			return
		}
		if !cloned {
			out = shallowCopy(s)
			cloned = true
		}
		delete(out, key)
	}
	set := func(key string, v any) {
		if !strip {
			return
		}
		if !cloned {
			out = shallowCopy(s)
			cloned = true
		}
		out[key] = v
	}
	add := func(kind, detail string) { items = append(items, RiskyItem{Source: src, Kind: kind, Detail: detail}) }

	// hooks: every event a project/local hook would fire on (command, http,
	// prompt, mcp — all of them act before the user agrees).
	if h, ok := s["hooks"].(map[string]any); ok && len(h) > 0 {
		var events []string
		for ev, v := range h {
			if nonEmpty(v) {
				events = append(events, ev)
			}
		}
		if len(events) > 0 {
			sort.Strings(events)
			add("hooks", strings.Join(events, ", "))
			drop("hooks")
		}
	}
	// statusLine runs its command on every render.
	if v, ok := s["statusLine"]; ok && v != nil {
		detail := ""
		if m, ok := v.(map[string]any); ok {
			if c, ok := m["command"].(string); ok {
				detail = clip(c, 60)
			}
		}
		add("status line command", detail)
		drop("statusLine")
	}
	// mcpServers declared in local settings start without the per-server
	// approval .mcp.json servers need (project-settings servers are approval
	// gated like .mcp.json, so they are left alone).
	if src == settingspolicy.SourceLocal {
		if m, ok := s["mcpServers"].(map[string]any); ok && len(m) > 0 {
			add("MCP servers", fmt.Sprintf("%d (%s)", len(m), strings.Join(sortedMapKeys(m, 4), ", ")))
			drop("mcpServers")
		}
	}
	// .mcp.json approvals.
	var approvals []string
	if b, _ := s["enableAllProjectMcpServers"].(bool); b {
		approvals = append(approvals, "enableAllProjectMcpServers")
		drop("enableAllProjectMcpServers")
	}
	if l := stringItems(s["enabledMcpjsonServers"]); len(l) > 0 {
		approvals = append(approvals, "enabledMcpjsonServers ["+strings.Join(clipList(l, 4), ", ")+"]")
		drop("enabledMcpjsonServers")
	}
	if len(approvals) > 0 {
		add("MCP server approvals", strings.Join(approvals, ", "))
	}
	// permissions: allow / ask rules, extra directories, a loosening mode.
	if p, ok := s["permissions"].(map[string]any); ok {
		np := p
		pc := false
		pdrop := func(key string) {
			if !strip {
				return
			}
			if !pc {
				np = shallowCopy(p)
				pc = true
			}
			delete(np, key)
		}
		var rules []string
		for _, key := range []string{"allow", "ask"} {
			if l := stringItems(p[key]); len(l) > 0 {
				rules = append(rules, l...)
				pdrop(key)
			}
		}
		if len(rules) > 0 {
			add("permissions", strings.Join(clipList(rules, 8), ", "))
		}
		if l := stringItems(p["additionalDirectories"]); len(l) > 0 {
			add("additional directories", strings.Join(clipList(l, 6), ", "))
			pdrop("additionalDirectories")
		}
		if m, ok := p["defaultMode"].(string); ok && IsLoosePermissionMode(m) {
			add("permission mode", strings.TrimSpace(m))
			pdrop("defaultMode")
		}
		if pc {
			set("permissions", np)
		}
	}
	// sandbox loosening.
	if sb, ok := s["sandbox"].(map[string]any); ok {
		nsb, keys := stripSandbox(sb, strip)
		if len(keys) > 0 {
			add("sandbox", strings.Join(keys, ", "))
			set("sandbox", nsb)
		}
	}
	// enabledPlugins: a plugin brings hooks, MCP servers, commands and agents.
	if m, ok := s["enabledPlugins"].(map[string]any); ok {
		var on []string
		for id, v := range m {
			if b, _ := v.(bool); b {
				on = append(on, id)
			}
		}
		if len(on) > 0 {
			sort.Strings(on)
			add("plugins", strings.Join(clipList(on, 4), ", "))
			if strip {
				nm := make(map[string]any, len(m))
				for id, v := range m {
					if b, _ := v.(bool); !b {
						nm[id] = v
					}
				}
				set("enabledPlugins", nm)
			}
		}
	}
	// model / provider overrides (where prompts — and the API key — go).
	for _, key := range []string{"provider", "model", "fallbackModel"} {
		v, ok := s[key]
		if !ok || !nonEmpty(v) {
			continue
		}
		detail := ""
		switch t := v.(type) {
		case string:
			detail = clip(strings.TrimSpace(t), 40)
		case []any:
			detail = strings.Join(clipList(stringItems(t), 3), ", ")
		}
		add(key, detail)
		drop(key)
	}
	return items, out
}

// stripSandbox drops the sandbox keys that loosen the OS sandbox and reports
// their names.
func stripSandbox(sb map[string]any, strip bool) (map[string]any, []string) {
	var keys []string
	out := sb
	cloned := false
	del := func(m map[string]any, key string) map[string]any {
		if !strip {
			return m
		}
		c := shallowCopy(m)
		delete(c, key)
		return c
	}
	ensure := func() {
		if strip && !cloned {
			out = shallowCopy(sb)
			cloned = true
		}
	}
	if b, ok := sb["enabled"].(bool); ok && !b {
		keys = append(keys, "enabled: false")
		ensure()
		out = del(out, "enabled")
	}
	for _, key := range []string{"autoAllowBashIfSandboxed", "allowUnsandboxedCommands", "enableWeakerNestedSandbox"} {
		if b, _ := sb[key].(bool); b {
			keys = append(keys, key)
			ensure()
			out = del(out, key)
		}
	}
	if l := stringItems(sb["excludedCommands"]); len(l) > 0 {
		keys = append(keys, "excludedCommands")
		ensure()
		out = del(out, "excludedCommands")
	}
	if fs, ok := sb["filesystem"].(map[string]any); ok {
		nfs := fs
		for _, key := range []string{"allowWrite", "allowRead"} {
			if l := stringItems(fs[key]); len(l) > 0 {
				keys = append(keys, "filesystem."+key)
				nfs = del(nfs, key)
			}
		}
		if strip && len(nfs) != len(fs) {
			ensure()
			out["filesystem"] = nfs
		}
	}
	if net, ok := sb["network"].(map[string]any); ok {
		nnet := net
		for _, key := range []string{"allowedDomains", "allowUnixSockets"} {
			if l := stringItems(net[key]); len(l) > 0 {
				keys = append(keys, "network."+key)
				nnet = del(nnet, key)
			}
		}
		if b, _ := net["allowLocalBinding"].(bool); b {
			keys = append(keys, "network.allowLocalBinding")
			nnet = del(nnet, "allowLocalBinding")
		}
		if strip && len(nnet) != len(net) {
			ensure()
			out["network"] = nnet
		}
	}
	return out, keys
}

// demoteTrackedLocal treats a git-tracked settings.local.json as project
// settings for MCP purposes: its mcpServers join the project's (so they need
// approval like .mcp.json servers) and its .mcp.json approvals no longer count.
func demoteTrackedLocal(out []OrderedResult) {
	li, pi := -1, -1
	for i, or := range out {
		switch or.Source {
		case settingspolicy.SourceLocal:
			li = i
		case settingspolicy.SourceProject:
			pi = i
		}
	}
	if li < 0 || out[li].Result.LoadState != settingspolicy.LoadLoaded || out[li].Result.Settings == nil {
		return
	}
	local := shallowCopy(out[li].Result.Settings)
	servers, _ := local["mcpServers"].(map[string]any)
	delete(local, "mcpServers")
	delete(local, "enableAllProjectMcpServers")
	delete(local, "enabledMcpjsonServers")
	out[li].Result.Settings = local
	if len(servers) == 0 || pi < 0 {
		return
	}
	proj := settingspolicy.Settings{}
	if out[pi].Result.LoadState == settingspolicy.LoadLoaded && out[pi].Result.Settings != nil {
		proj = shallowCopy(out[pi].Result.Settings)
	}
	merged := map[string]any{}
	if m, ok := proj["mcpServers"].(map[string]any); ok {
		for k, v := range m {
			merged[k] = v
		}
	}
	for k, v := range servers {
		merged[k] = v
	}
	proj["mcpServers"] = merged
	out[pi].Result.Settings = proj
	if out[pi].Result.LoadState != settingspolicy.LoadLoaded {
		out[pi].Result.LoadState = settingspolicy.LoadLoaded
	}
}

// applyTrust applies the Context's trust state to freshly loaded sources.
func applyTrust(ctx *Context, out []OrderedResult) {
	if ctx == nil {
		return
	}
	if ctx.LocalSettingsTracked {
		demoteTrackedLocal(out)
	}
	if !ctx.UntrustedProject {
		return
	}
	for i, or := range out {
		if or.Source != settingspolicy.SourceProject && or.Source != settingspolicy.SourceLocal {
			continue
		}
		if or.Result.Settings != nil {
			out[i].Result.Settings = StripUntrusted(or.Source, or.Result.Settings)
		}
	}
}

func shallowCopy(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func nonEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case bool:
		return t
	}
	return true
}

func stringItems(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedMapKeys(m map[string]any, max int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return clipList(keys, max)
}

// clipList keeps at most max entries (each clipped), noting how many more.
func clipList(l []string, max int) []string {
	out := make([]string, 0, len(l))
	for i, s := range l {
		if i == max {
			out = append(out, fmt.Sprintf("+%d more", len(l)-max))
			break
		}
		out = append(out, clip(s, 50))
	}
	return out
}

// clip shortens s to n runes and strips control characters, so a hostile
// setting cannot paint over the dialog.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
