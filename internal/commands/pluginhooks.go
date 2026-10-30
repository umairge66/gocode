package commands

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file implements the plugin-only steps of the markdown command builder
// that the generic BuildCommand path does not need:
//
//   - the npm provenance sanitizer applied to a plugin installed from an
//     npm registry, and
//   - the hooks validation of plugin skills/commands:
//     a PreToolUse/PermissionRequest hook that cannot be loaded may be what
//     guards the permissions declared beside it, so the skill then loads with
//     no hooks and no allowed-tools.
//
// Neither step runs a process or touches the network: the sanitizer only rewrites a
// recorded tarball URL, and the hooks checks are pure structural checks over
// the parsed frontmatter.

// NpmResolution is the recorded provenance of a plugin installed from an npm
// registry: the package name and the tarball URL npm resolved it to.
type NpmResolution struct {
	PackageName string `json:"packageName"`
	TarballURL  string `json:"tarballUrl"`
}

// SanitizeNpmResolution checks a recorded resolution: the tarball URL must be an
// absolute http(s) URL whose authority holds no backslash (WHATWG and other
// parsers disagree on such hosts); credentials, query and fragment are stripped so a
// registry token embedded in the URL never reaches the session. It reports
// false when the resolution must be dropped. An already-clean resolution is
// returned unchanged.
func SanitizeNpmResolution(r NpmResolution) (NpmResolution, bool) {
	if authorityHasBackslash(r.TarballURL) {
		return NpmResolution{}, false
	}
	u, err := url.Parse(strings.TrimSpace(r.TarballURL))
	if err != nil || u.Host == "" {
		return NpmResolution{}, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return NpmResolution{}, false
	}
	if u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(r.TarballURL, "#") {
		return r, true
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return NpmResolution{PackageName: r.PackageName, TarballURL: u.String()}, true
}

// specialSchemes are the WHATWG special schemes whose authority may be preceded
// by any run of slashes or backslashes.
var specialSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "ftp": true}

// authorityHasBackslash reports true when the URL's authority (or the
// slashes before it of a special scheme) carries a backslash.
func authorityHasBackslash(raw string) bool {
	t := strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20")
	t = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(t)
	n := strings.Index(t, "://")
	if n == -1 {
		return false
	}
	rest := t[n+3:]
	if specialSchemes[strings.ToLower(t[:n])] {
		lead := rest[:len(rest)-len(strings.TrimLeft(rest, "/\\"))]
		if strings.Contains(lead, "\\") {
			return true
		}
		rest = rest[len(lead):]
	}
	if i := strings.IndexAny(rest, "/?#"); i != -1 {
		rest = rest[:i]
	}
	return strings.Contains(rest, "\\")
}

// HookEventNames are the hook events a `hooks` object may name.
var HookEventNames = []string{
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch", "Notification",
	"UserPromptSubmit", "UserPromptExpansion", "SessionStart", "SessionEnd", "Stop",
	"StopFailure", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
	"PreModelSwitch", "PostModelSwitch", "PermissionRequest", "PermissionDenied", "Setup",
	"TeammateIdle", "TaskCreated", "TaskCompleted", "Elicitation", "ElicitationResult",
	"ConfigChange", "WorktreeCreate", "WorktreeRemove", "InstructionsLoaded", "CwdChanged",
	"FileChanged", "DirectoryAdded", "MessageDisplay",
}

var hookEventSet = func() map[string]bool {
	m := make(map[string]bool, len(HookEventNames))
	for _, e := range HookEventNames {
		m[e] = true
	}
	return m
}()

// guardEvents are the events whose hooks may guard permissions.
var guardEvents = map[string]bool{"PreToolUse": true, "PermissionRequest": true}

// unscannedFrontmatterKeys are the frontmatter keys the guard scan never
// descends into.
var unscannedFrontmatterKeys = map[string]bool{"metadata": true, "mcpServers": true, "lspServers": true}

// hookTypes are the hook entry types.
var hookTypes = map[string]bool{"command": true, "prompt": true, "agent": true, "http": true, "mcp_tool": true}

// guardUnloadable is the shared consequence text.
const guardUnloadable = "a PreToolUse/PermissionRequest hook that cannot be loaded may be what guards the permissions declared beside it, so nothing it sits in is applied until the entry is fixed or removed"

// hasGuardEvent reports whether v is an object with a non-empty PreToolUse/PermissionRequest.
func hasGuardEvent(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, e := range m {
		if !guardEvents[k] || e == nil {
			continue
		}
		if arr, ok := e.([]any); ok && len(arr) == 0 {
			continue
		}
		return true
	}
	return false
}

// looksLikeMatcher reports whether v is an object shaped like a hook matcher group.
func looksLikeMatcher(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, hasMatcher := m["matcher"]
	if !hasMatcher {
		for k := range m {
			if hookEventSet[k] {
				return false
			}
		}
	}
	switch h := m["hooks"].(type) {
	case []any:
		return len(h) > 0
	case map[string]any:
		_, typed := h["type"].(string)
		return hasMatcher || typed
	}
	return false
}

// scanForGuard is a bounded search for a guard hook anywhere in v.
func scanForGuard(v any, depth int, matchersCount bool, unscanned map[string]bool, inHooksList bool) bool {
	if hasGuardEvent(v) || (matchersCount && looksLikeMatcher(v)) {
		return true
	}
	if depth == 0 {
		return false
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if scanForGuard(e, depth-1, matchersCount, unscanned, inHooksList) {
				return true
			}
		}
	case map[string]any:
		if _, typed := t["type"].(string); inHooksList && typed {
			return false
		}
		for _, k := range sortedKeys(t) {
			if unscanned[k] {
				continue
			}
			e := t[k]
			_, isList := e.([]any)
			if scanForGuard(e, depth-1, matchersCount && !hookEventSet[k], unscanned, k == "hooks" && isList) {
				return true
			}
		}
	}
	return false
}

// guardOutsideHooks reports a PreToolUse/PermissionRequest
// hook declared anywhere in the frontmatter except under `hooks`.
func guardOutsideHooks(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	if hasGuardEvent(raw) {
		return true
	}
	for _, k := range sortedKeys(raw) {
		if k == "hooks" || unscannedFrontmatterKeys[k] {
			continue
		}
		if scanForGuard(raw[k], 3, !hookEventSet[k], unscannedFrontmatterKeys, false) {
			return true
		}
	}
	return false
}

// containsGuardHooks reports whether v holds PreToolUse/PermissionRequest hooks.
func containsGuardHooks(v any) bool {
	if looksLikeMatcher(v) {
		return true
	}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if containsGuardHooks(e) {
				return true
			}
		}
		return false
	}
	return hasGuardEvent(v)
}

// HookValidation is the outcome of ValidatePluginHooks: notes, unloadable
// guards, and the cleaned hooks object.
type HookValidation struct {
	// Hooks is a cleaned deep copy: entries dropped with a note are
	// removed. Nil when the input was not an object.
	Hooks map[string]any
	// Notes are the dropped-entry warnings ("...; entry ignored").
	Notes []string
	// UnloadableGuards are the problems that make the whole hooks object
	// unloadable (a broken guard hook).
	UnloadableGuards []string
	// SchemaError is the schema failure of the cleaned object, if any.
	SchemaError string
}

// Loadable reports whether the hooks may be applied.
func (v HookValidation) Loadable() bool {
	return len(v.UnloadableGuards) == 0 && v.SchemaError == ""
}

// ValidatePluginHooks validates a hooks object and then schema-checks it: it
// drops unknown events and malformed non-guard entries with a note, and marks
// malformed guard entries (PreToolUse/PermissionRequest) as unloadable.
func ValidatePluginHooks(hooks any) HookValidation {
	if looksLikeMatcher(hooks) || anyMatcherInList(hooks) {
		kind := "a single matcher"
		if _, isArr := hooks.([]any); isArr {
			kind = "an array of matchers"
		}
		return HookValidation{UnloadableGuards: []string{"hooks: must be an object mapping event names to matcher arrays; received " + kind}}
	}
	src, ok := hooks.(map[string]any)
	if !ok {
		return HookValidation{SchemaError: "hooks: expected an object, received " + describeJSType(hooks)}
	}
	out := HookValidation{Hooks: map[string]any{}}
	for _, ev := range sortedKeys(src) {
		val := deepCopy(src[ev])
		label := quoteKey(ev)
		if !hookEventSet[ev] {
			if scanForGuard(val, 3, !isList(val), nil, false) {
				out.UnloadableGuards = append(out.UnloadableGuards, fmt.Sprintf("hooks.%s: not a hook event, but it holds PreToolUse/PermissionRequest hooks", label))
				continue
			}
			out.Notes = append(out.Notes, fmt.Sprintf("hooks.%s: unknown hook event; entry ignored", label))
			continue
		}
		arr, ok := val.([]any)
		if !ok {
			if (guardEvents[ev] && val != nil) || scanForGuard(val, 3, false, nil, false) {
				out.UnloadableGuards = append(out.UnloadableGuards, fmt.Sprintf("hooks.%s: must be an array of matchers; received %s", label, describeJSType(val)))
				continue
			}
			out.Notes = append(out.Notes, fmt.Sprintf("hooks.%s: must be an array of matchers; received %s; entry ignored", label, describeJSType(val)))
			continue
		}
		kept, stripped, guards := validateMatchers(arr, guardEvents[ev])
		for _, s := range stripped {
			out.Notes = append(out.Notes, fmt.Sprintf("hooks.%s.%s; entry ignored", label, s))
		}
		for _, g := range guards {
			out.UnloadableGuards = append(out.UnloadableGuards, fmt.Sprintf("hooks.%s.%s", label, g))
		}
		out.Hooks[ev] = kept
	}
	if len(out.UnloadableGuards) == 0 {
		out.SchemaError = hooksSchemaError(out.Hooks)
	}
	return out
}

// validateMatchers validates one event's matcher array. For a guard event
// nothing is stripped: every problem is an unloadable guard. Otherwise bad
// matchers/entries are stripped (with a "path: problem" note) and only a
// misplaced guard hook is unloadable.
func validateMatchers(arr []any, guard bool) (kept []any, stripped, guards []string) {
	kept = make([]any, 0, len(arr))
	for i, d := range arr {
		problems := func(path, problem string) {
			if guard {
				guards = append(guards, path+": "+problem)
			} else {
				stripped = append(stripped, path+": "+problem)
			}
		}
		if scanForGuard(d, 3, false, nil, false) {
			// Always reported: a guard hook where a matcher was expected.
			guards = append(guards, fmt.Sprintf("%d: holds PreToolUse/PermissionRequest hooks where a matcher was expected", i))
			continue
		}
		m, ok := d.(map[string]any)
		if !ok {
			problems(fmt.Sprint(i), "Hook matcher must be an object; received "+describeJSType(d))
			continue
		}
		hl, ok := m["hooks"].([]any)
		if !ok {
			problems(fmt.Sprint(i), "Hook matcher \"hooks\" must be an array of hook entries; received "+describeJSType(m["hooks"]))
			continue
		}
		var entries []any
		var entryProblems []string
		for j, h := range hl {
			if p := validateHookEntry(h); p != "" {
				entryProblems = append(entryProblems, fmt.Sprintf("%d.hooks.%d: %s", i, j, p))
				continue
			}
			entries = append(entries, h)
		}
		if mv, has := m["matcher"]; has && mv != nil {
			if _, isStr := mv.(string); !isStr {
				problems(fmt.Sprint(i), "Invalid hook matcher (matcher: Expected string, received "+receivedTypeName(mv)+")")
				continue
			}
		}
		for _, p := range entryProblems {
			if guard {
				guards = append(guards, p)
			} else {
				stripped = append(stripped, p)
			}
		}
		if guard {
			kept = append(kept, m)
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if entries == nil {
			entries = []any{}
		}
		cp["hooks"] = entries
		kept = append(kept, cp)
	}
	return kept, stripped, guards
}

// validateHookEntry checks a hook entry plus the per-type fields of the hook
// union (BashCommandHookSchema, PromptHookSchema, AgentHookSchema, HttpHookSchema,
// McpToolHookSchema). It returns "" for a valid entry. The per-type checks
// cover the required fields and the types of the common optional ones; unknown
// keys pass through.
func validateHookEntry(e any) string {
	m, ok := e.(map[string]any)
	if !ok {
		return "Hook entry must be an object; received " + describeJSType(e)
	}
	tv, has := m["type"]
	t, isStr := tv.(string)
	if !isStr {
		if !has {
			return "Hook entry has no \"type\""
		}
		return "Hook entry \"type\" must be a string; received " + describeJSType(tv)
	}
	if !hookTypes[t] {
		return fmt.Sprintf("Unknown hook type %q", t)
	}
	var issues []string
	str := func(k string, required bool) {
		v, ok := m[k]
		if !ok || v == nil {
			if required {
				issues = append(issues, k+": Required")
			}
			return
		}
		if _, ok := v.(string); !ok {
			issues = append(issues, fmt.Sprintf("%s: Expected string, received %s", k, receivedTypeName(v)))
		}
	}
	positive := func(k string) {
		if v, ok := m[k]; ok && v != nil {
			n, isNum := toFloat(v)
			if !isNum {
				issues = append(issues, fmt.Sprintf("%s: Expected number, received %s", k, receivedTypeName(v)))
			} else if n <= 0 {
				issues = append(issues, k+": Number must be greater than 0")
			}
		}
	}
	boolean := func(k string) {
		if v, ok := m[k]; ok && v != nil {
			if _, ok := v.(bool); !ok {
				issues = append(issues, fmt.Sprintf("%s: Expected boolean, received %s", k, receivedTypeName(v)))
			}
		}
	}
	positive("timeout")
	str("statusMessage", false)
	boolean("once")
	switch t {
	case "command":
		str("command", true)
		if v, ok := m["args"]; ok && v != nil {
			arr, isArr := v.([]any)
			if !isArr {
				issues = append(issues, "args: Expected array, received "+receivedTypeName(v))
			} else {
				for i, a := range arr {
					if _, ok := a.(string); !ok {
						issues = append(issues, fmt.Sprintf("args.%d: Expected string, received %s", i, receivedTypeName(a)))
					}
				}
			}
		}
		if v, ok := m["shell"]; ok && v != nil {
			if s, _ := v.(string); s != "bash" && s != "powershell" {
				issues = append(issues, "shell: Invalid enum value. Expected 'bash' | 'powershell'")
			}
		}
		boolean("async")
		boolean("asyncRewake")
	case "prompt":
		str("prompt", true)
		str("model", false)
		boolean("continueOnBlock")
	case "agent":
		str("prompt", true)
		str("model", false)
	case "http":
		str("url", true)
		if s, ok := m["url"].(string); ok {
			if u, err := url.Parse(s); err != nil || u.Scheme == "" {
				issues = append(issues, "url: Invalid url")
			}
		}
	case "mcp_tool":
		str("server", true)
		str("tool", true)
		if v, ok := m["input"]; ok && v != nil {
			if _, ok := v.(map[string]any); !ok {
				issues = append(issues, "input: Expected object, received "+receivedTypeName(v))
			}
		}
	}
	if len(issues) > 0 {
		return fmt.Sprintf("Invalid %s hook (%s)", t, strings.Join(issues, "; "))
	}
	return ""
}

// hooksSchemaError is the schema check of the cleaned hooks: a record of known
// events to arrays of {matcher?: string, hooks: entry[]}. It returns "" when
// the object conforms.
func hooksSchemaError(h map[string]any) string {
	for _, ev := range sortedKeys(h) {
		arr, ok := h[ev].([]any)
		if !ok {
			return fmt.Sprintf("%s: Expected array, received %s", ev, receivedTypeName(h[ev]))
		}
		for i, d := range arr {
			m, ok := d.(map[string]any)
			if !ok {
				return fmt.Sprintf("%s.%d: Expected object, received %s", ev, i, receivedTypeName(d))
			}
			if mv, has := m["matcher"]; has && mv != nil {
				if _, isStr := mv.(string); !isStr {
					return fmt.Sprintf("%s.%d.matcher: Expected string, received %s", ev, i, receivedTypeName(mv))
				}
			}
			hl, ok := m["hooks"].([]any)
			if !ok {
				return fmt.Sprintf("%s.%d.hooks: Expected array, received %s", ev, i, receivedTypeName(m["hooks"]))
			}
			for j, e := range hl {
				if p := validateHookEntry(e); p != "" {
					return fmt.Sprintf("%s.%d.hooks.%d: %s", ev, i, j, p)
				}
			}
		}
	}
	return ""
}

// PluginCommandOptions carries the session facts the builder consults that a markdown
// file cannot supply.
type PluginCommandOptions struct {
	// HooksRestricted reports that hooks are switched off by policy
	// (disableAllHooks) or limited to managed hooks (allowManagedHooksOnly), so
	// a skill's guard hooks would not run — its allowed-tools are dropped too.
	HooksRestricted bool
	// NpmResolution is the plugin's recorded npm provenance (nil when the
	// plugin was not installed from npm). It is sanitized before it is kept.
	NpmResolution *NpmResolution
}

// isSkillFile reports a SKILL.md (any case) file.
func isSkillFile(path string) bool {
	return strings.EqualFold(filepath.Base(path), "skill.md")
}

// applyPluginChecks runs the plugin-only steps on a freshly built command.
//
// A guard hook outside `hooks`, or (for a non-skill command) a malformed or
// guard-bearing `hooks`, is an error: the command loads with no hooks and no
// allowed-tools. For a skill with `hooks`, validation notes become warnings;
// unloadable or schema-invalid hooks likewise drop hooks and allowed-tools,
// otherwise the hooks are kept. Allowed-tools are also dropped when the kept
// hooks contain guard hooks and hooks are restricted by policy.
func applyPluginChecks(c *Command, raw map[string]any, opts PluginCommandOptions) {
	if opts.NpmResolution != nil {
		if r, ok := SanitizeNpmResolution(*opts.NpmResolution); ok {
			c.NpmResolution = &r
		}
	}
	skill := isSkillFile(c.FilePath)
	hooks, hasHooks := raw["hooks"]
	if hooks == nil {
		hasHooks = false
	}
	misplaced := !skill && hasHooks && commandHooksMalformed(hooks)
	unloadable := false
	if guardOutsideHooks(raw) || misplaced {
		unloadable = true
		c.Warnings = append(c.Warnings, fmt.Sprintf("Plugin skill '%s': PreToolUse/PermissionRequest is declared at the frontmatter top level, outside \"hooks\" — %s (the skill loads with no hooks and no allowed-tools)", c.Name, guardUnloadable))
	} else if skill && hasHooks {
		v := ValidatePluginHooks(hooks)
		for _, n := range v.Notes {
			c.Warnings = append(c.Warnings, fmt.Sprintf("Plugin skill '%s': %s", c.Name, n))
		}
		if !v.Loadable() {
			unloadable = true
			why := strings.Join(v.UnloadableGuards, "; ")
			if why == "" {
				why = v.SchemaError
			}
			if why == "" {
				why = "hooks unreadable"
			}
			c.Warnings = append(c.Warnings, fmt.Sprintf("Plugin skill '%s': %s — %s (the skill loads with no hooks and no allowed-tools)", c.Name, why, guardUnloadable))
		} else {
			c.Hooks = v.Hooks
		}
	}
	guardsOff := c.Hooks != nil && containsGuardHooks(map[string]any(c.Hooks)) && opts.HooksRestricted
	if unloadable || guardsOff {
		c.AllowedTools = nil
		c.Hooks = nil
	}
	if skill {
		c.ProgressMessage = "loading"
		c.LoadedFrom = "plugin"
	}
}

// commandHooksMalformed is the test for a non-skill command's `hooks`:
// not an object, an array holding a non-object, or guard-bearing.
func commandHooksMalformed(h any) bool {
	switch t := h.(type) {
	case map[string]any:
	case []any:
		for _, e := range t {
			if _, ok := e.(map[string]any); !ok {
				return true
			}
		}
	default:
		return true
	}
	return containsGuardHooks(h) || scanForGuard(h, 3, true, nil, false)
}

func anyMatcherInList(v any) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if looksLikeMatcher(e) {
			return true
		}
	}
	return false
}

func isList(v any) bool { _, ok := v.([]any); return ok }

// describeJSType names a value's JS-style type: "null", "undefined", "an array", "an
// object", "a string", "a number", "a boolean".
func describeJSType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	default:
		if _, ok := toFloat(v); ok {
			return "a number"
		}
		return "an object"
	}
}

// receivedTypeName is the received-type name for a schema issue message.
func receivedTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case string:
		return "string"
	case bool:
		return "boolean"
	default:
		if _, ok := toFloat(v); ok {
			return "number"
		}
		return "unknown"
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// quoteKeyRe matches keys printed bare.
var quoteKeyRe = regexp.MustCompile(`^[A-Za-z0-9_$-]+$`)

// quoteKey renders an event key safely for a diagnostic path.
func quoteKey(k string) string {
	if quoteKeyRe.MatchString(k) {
		return k
	}
	return fmt.Sprintf("%q", k)
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, e := range t {
			a[i] = deepCopy(e)
		}
		return a
	default:
		return v
	}
}
