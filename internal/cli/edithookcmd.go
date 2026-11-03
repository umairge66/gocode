package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// `gocode edit-hook --json` (hidden): apply one hook edit, read as JSON from
// stdin, to the user, project or local settings file. It is the write path an
// editor extension uses for its hooks dialog.
//
// The request:
//
//	{"op":"add"|"replace"|"remove", "source":"userSettings"|"projectSettings"|"localSettings",
//	 "event":"<hook event>", "matcher":"<matcher>", "hook":{…},
//	 "target":{"event":…, "matcher":…, "hook":{…}}}   // replace / remove
//
// Every settings source is read (there is no --setting-sources gate), and
// there is one local settings file per project.

const (
	editHookMaxStdin   = 524288
	editHookMaxMatcher = 10000
	editHookMaxHook    = 262144
)

// editHookSessionRefusal is the message for an editor started inside a
// session.
const editHookSessionRefusal = "This setting cannot be changed from an editor started inside a gocode session. Start the editor outside the session and try again."

// hookEvents is the list of known hook events: the events gocode fires
// (hooks.FiredEvents, in lifecycle order) first, then the events a settings
// file may name that gocode never fires. An edit for one of the latter is
// applied with a warning (hookEventWarning).
var hookEvents = append(firedHookEventNames(), "PostToolBatch", "UserPromptExpansion", "StopFailure", "PostCompact", "PreModelSwitch", "PostModelSwitch", "PermissionRequest", "Setup", "TeammateIdle", "TaskCreated", "TaskCompleted", "Elicitation", "ElicitationResult", "ConfigChange", "WorktreeCreate", "WorktreeRemove", "InstructionsLoaded", "CwdChanged", "FileChanged", "DirectoryAdded", "MessageDisplay")

func firedHookEventNames() []string {
	var out []string
	for _, ev := range hooks.FiredEvents() {
		out = append(out, string(ev))
	}
	return out
}

// hookEventWarning is the warning for an edit on an event gocode never
// fires, or "".
func hookEventWarning(ev string) string {
	if hooks.IsFired(hooks.Event(ev)) {
		return ""
	}
	return fmt.Sprintf("edit_hook: gocode never fires %s, so this hook will not run", ev)
}

// hookSchemaKeys lists the fields of each hook type's schema; parsing keeps
// only these (the rest is dropped), which is what the equality checks see.
var hookSchemaKeys = map[string][]string{
	"command":  {"type", "command", "args", "if", "shell", "timeout", "onFailure", "statusMessage", "once", "async", "asyncRewake", "rewakeMessage", "rewakeSummary", "cloud"},
	"prompt":   {"type", "prompt", "if", "timeout", "model", "continueOnBlock", "statusMessage", "once"},
	"mcp_tool": {"type", "server", "tool", "input", "if", "timeout", "statusMessage", "once"},
	"http":     {"type", "url", "if", "timeout", "onFailure", "headers", "allowedEnvVars", "statusMessage", "once", "cloud"},
	"agent":    {"type", "prompt", "if", "timeout", "model", "statusMessage", "once"},
}

// hookRequiredStrings are each type's required string fields.
var hookRequiredStrings = map[string][]string{
	"command":  {"command"},
	"prompt":   {"prompt"},
	"mcp_tool": {"server", "tool"},
	"http":     {"url"},
	"agent":    {"prompt"},
}

var editHookSources = []string{"userSettings", "projectSettings", "localSettings"}

// editHookReply is the JSON reply.
type editHookReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Warning, on a successful edit, is a non-fatal note (the event is one
	// gocode never fires).
	Warning string          `json:"warning,omitempty"`
	Code    string          `json:"code,omitempty"`
	Refusal *editHookRefuse `json:"refusal,omitempty"`
}

type editHookRefuse struct {
	Code string `json:"code"`
	File string `json:"file"`
}

func editHookFail(code, msg string, refusal *editHookRefuse) editHookReply {
	return editHookReply{OK: false, Error: msg, Code: code, Refusal: refusal}
}

// hookEditRequest is the validated edit.
type hookEditRequest struct {
	Op      string
	Source  string
	Event   string
	Matcher string
	Hook    map[string]any
	Target  *hookEditTarget
}

type hookEditTarget struct {
	Event   string
	Matcher string
	Hook    map[string]any
}

// runEditHookCmd is the command action.
func runEditHookCmd(env *Env, t []string) int {
	o, err := scanOpts(argsAfter(t, "edit-hook"), map[string]string{"--json": "json"}, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.pos) > 0 {
		fmt.Fprintf(env.Stderr, "error: too many arguments for 'edit-hook'. Expected 0 arguments but got %d.\n", len(o.pos))
		return 1
	}
	if !o.has("json") {
		fmt.Fprintln(env.Stderr, "error: required option '--json' not specified")
		return 1
	}
	if env.Stdin == nil || env.StdinTTY {
		fmt.Fprintln(env.Stderr, "gocode edit-hook reads one JSON edit from stdin; it is run by the VS Code extension, not by hand")
		return 1
	}
	if envGetenv(env)("GOCODE_CODE_CHILD_SESSION") != "" {
		fmt.Fprintln(env.Stderr, editHookSessionRefusal)
		return 1
	}
	data, rerr := io.ReadAll(io.LimitReader(env.Stdin, editHookMaxStdin+1))
	var edit any
	if rerr != nil || len(data) > editHookMaxStdin || json.Unmarshal(data, &edit) != nil {
		fmt.Fprintln(env.Stderr, "gocode edit-hook: stdin is not a JSON edit")
		return 1
	}
	reply := runHookEdit(env, edit)
	if !reply.OK {
		writeJSONLine(env.Stdout, reply)
		fmt.Fprintln(env.Stderr, reply.Error)
		return 1
	}
	if reply.Warning != "" {
		writeJSONLine(env.Stdout, reply)
		fmt.Fprintln(env.Stderr, reply.Warning)
		return 0
	}
	fmt.Fprintln(env.Stdout, `{"ok":true}`)
	return 0
}

// runHookEdit validates and applies one hook edit request.
func runHookEdit(env *Env, edit any) editHookReply {
	ctx := settingsContext(env)
	fsys := fsfacade.OSFS{}
	if msg := editHookPolicyLock(settingsload.LoadSource(fsys, ctx, settingspolicy.SourcePolicy)); msg != "" {
		return editHookFail("policy_locked", msg, nil)
	}
	obj, ok := edit.(map[string]any)
	if !ok {
		return editHookFail("bad_request", "edit_hook: the edit must be a JSON object", nil)
	}
	req, msg := validateHookEdit(obj)
	if msg != "" {
		return editHookFail("bad_request", msg, nil)
	}
	path, ok := ctx.PathForSource(settingspolicy.Source(req.Source))
	if !ok {
		return editHookFail("source_disabled", fmt.Sprintf("edit_hook: the %s source is disabled for this session (--setting-sources)", req.Source), nil)
	}
	current := settingsload.LoadFile(fsys, path)
	switch current.LoadState {
	case settingspolicy.LoadUnparsable, settingspolicy.LoadMalformed, settingspolicy.LoadUnreadable:
		return editHookFail("file_unloadable", fmt.Sprintf("edit_hook: the %s file has errors and could not be loaded; fix it by hand first", req.Source), &editHookRefuse{Code: "unloadable", File: path})
	}
	if req.Target != nil {
		n := countMatchingHooks(current.Settings, req.Target)
		if n == 0 {
			return editHookFail("target_missing", fmt.Sprintf("edit_hook: that hook is no longer in %s; it may have been changed outside this dialog", req.Source), nil)
		}
		if n > 1 {
			return editHookFail("target_ambiguous", fmt.Sprintf("edit_hook: more than one identical hook is configured for %s in %s; edit the file by hand", req.Event, req.Source), &editHookRefuse{Code: "twins", File: path})
		}
	}
	outcome := ""
	werr := settingsload.UpdateFile(path, func(m map[string]any) (bool, error) {
		var changed bool
		outcome, changed = applyHookEdit(m, req)
		return changed, nil
	})
	if werr != nil {
		return editHookFail("write_failed", "edit_hook: the settings write failed: "+werr.Error(), nil)
	}
	switch outcome {
	case "applied":
		if req.Op == "remove" {
			return editHookReply{OK: true}
		}
		return editHookReply{OK: true, Warning: hookEventWarning(req.Event)}
	case "duplicate":
		return editHookFail("duplicate", fmt.Sprintf("edit_hook: an identical hook is already configured for %s in %s", req.Event, req.Source), nil)
	case "twin-by-headers":
		return editHookFail("twin_by_headers", fmt.Sprintf("edit_hook: a hook that differs from this one only in header values is already configured for %s in %s; this dialog could not tell the two apart afterwards, so edit the file by hand to keep both", req.Event, req.Source), nil)
	case "not-a-list":
		return editHookFail("not_a_list", fmt.Sprintf("edit_hook: the %s entry in %s is not a list; fix the file by hand", req.Event, req.Source), &editHookRefuse{Code: "not-a-list", File: path})
	default:
		return editHookFail("target_missing", fmt.Sprintf("edit_hook: that hook is no longer in %s; it may have been changed outside this dialog", req.Source), nil)
	}
}

// editHookPolicyLock reports managed settings that forbid editing.
func editHookPolicyLock(policy settingspolicy.LoadResult) string {
	switch policy.LoadState {
	case settingspolicy.LoadUnparsable, settingspolicy.LoadMalformed, settingspolicy.LoadUnreadable:
		return "edit_hook: managed settings could not be read, so hooks cannot be edited"
	case settingspolicy.LoadLoaded:
		if b, _ := policy.Settings["disableAllHooks"].(bool); b {
			return "edit_hook: hooks are disabled by managed settings"
		}
		if b, _ := policy.Settings["allowManagedHooksOnly"].(bool); b {
			return "edit_hook: managed settings allow managed hooks only"
		}
	}
	return ""
}

// validateHookEdit validates a hook edit request.
func validateHookEdit(e map[string]any) (hookEditRequest, string) {
	var r hookEditRequest
	op, _ := e["op"].(string)
	if op != "add" && op != "replace" && op != "remove" {
		return r, "edit_hook: op must be add, replace, or remove"
	}
	src, _ := e["source"].(string)
	if !containsStr(editHookSources, src) {
		return r, "edit_hook: source must be userSettings, projectSettings, or localSettings"
	}
	ev, _ := e["event"].(string)
	if !containsStr(hookEvents, ev) {
		return r, "edit_hook: event is not a known hook event"
	}
	matcher, msg := validateHookMatcher(e["matcher"], "matcher")
	if msg != "" {
		return r, msg
	}
	r = hookEditRequest{Op: op, Source: src, Event: ev, Matcher: matcher}
	if op == "add" || op == "replace" {
		h, msg := parseHookObject(e["hook"], "hook")
		if msg != "" {
			return r, msg
		}
		r.Hook = h
	}
	if op == "replace" || op == "remove" {
		tgt, ok := e["target"].(map[string]any)
		if !ok {
			return r, "edit_hook: target must be an object for replace and remove"
		}
		tev, _ := tgt["event"].(string)
		if !containsStr(hookEvents, tev) {
			return r, "edit_hook: target.event is not a known hook event"
		}
		tm, msg := validateHookMatcher(tgt["matcher"], "target.matcher")
		if msg != "" {
			return r, msg
		}
		th, msg := parseHookObject(tgt["hook"], "target.hook")
		if msg != "" {
			return r, msg
		}
		r.Target = &hookEditTarget{Event: tev, Matcher: tm, Hook: th}
	}
	return r, ""
}

// validateHookMatcher validates a matcher: absent/null is "", else a bounded
// string that is not an Object.prototype member name.
func validateHookMatcher(v any, name string) (string, string) {
	if v == nil {
		return "", ""
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Sprintf("edit_hook: %s must be a string", name)
	}
	if len(s) > editHookMaxMatcher {
		return "", fmt.Sprintf("edit_hook: %s is too long", name)
	}
	switch s {
	case "__proto__", "constructor", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "toLocaleString", "toString", "valueOf", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__":
		return "", fmt.Sprintf("edit_hook: %s is not allowed", name)
	}
	return s, ""
}

// parseHookObject validates a hook: an object, no prototype-polluting keys, under
// the size cap, valid against the hook schema. The result keeps only the
// schema's keys.
func parseHookObject(v any, name string) (map[string]any, string) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Sprintf("edit_hook: %s must be an object", name)
	}
	if hasForbiddenKey(obj) {
		return nil, fmt.Sprintf("edit_hook: %s has a key that is not allowed", name)
	}
	if data, _ := json.Marshal(obj); len(data) > editHookMaxHook {
		return nil, fmt.Sprintf("edit_hook: %s is too large", name)
	}
	parsed, path, msg := schemaParseHook(obj)
	if msg != "" {
		where := ""
		if path != "" {
			where = " (" + path + ")"
		}
		return nil, fmt.Sprintf("edit_hook: %s is not a valid hook%s: %s", name, where, msg)
	}
	return parsed, ""
}

func hasForbiddenKey(v any) bool {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if hasForbiddenKey(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if k == "__proto__" || k == "constructor" || k == "prototype" || hasForbiddenKey(e) {
				return true
			}
		}
	}
	return false
}

// schemaParseHook validates a hook against the discriminated union of hook
// schemas (type + required strings + basic field types) and returns it with
// unknown keys stripped. path/msg describe the first issue.
func schemaParseHook(obj map[string]any) (map[string]any, string, string) {
	typ, _ := obj["type"].(string)
	keys, ok := hookSchemaKeys[typ]
	if !ok {
		return nil, "type", "Invalid discriminator value. Expected 'command' | 'prompt' | 'agent' | 'http' | 'mcp_tool'"
	}
	for _, k := range hookRequiredStrings[typ] {
		if _, isStr := obj[k].(string); !isStr {
			return nil, k, "Required"
		}
	}
	if typ == "http" {
		if u, err := url.Parse(obj["url"].(string)); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, "url", "Invalid url"
		}
	}
	if t, present := obj["timeout"]; present {
		if n, isNum := t.(float64); !isNum || n <= 0 {
			return nil, "timeout", "Number must be greater than 0"
		}
	}
	for _, k := range []string{"if", "shell", "statusMessage", "model", "rewakeMessage", "rewakeSummary"} {
		if v, present := obj[k]; present {
			if _, isStr := v.(string); !isStr && containsStr(keys, k) {
				return nil, k, "Expected string"
			}
		}
	}
	if h, present := obj["headers"]; present && typ == "http" {
		hm, isObj := h.(map[string]any)
		if !isObj {
			return nil, "headers", "Expected object"
		}
		for hk, hv := range hm {
			if _, isStr := hv.(string); !isStr {
				return nil, "headers." + hk, "Expected string"
			}
		}
	}
	out := map[string]any{}
	for _, k := range keys {
		if v, present := obj[k]; present {
			out[k] = v
		}
	}
	return out, "", ""
}

// canonicalJSON returns JSON with object keys sorted.
func canonicalJSON(v any) string {
	switch x := v.(type) {
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = canonicalJSON(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			kb, _ := json.Marshal(k)
			parts[i] = string(kb) + ":" + canonicalJSON(x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "null"
		}
		return string(b)
	}
}

// blankHeaderValues returns an http hook with header values
// blanked, so two hooks differing only in header values compare equal.
func blankHeaderValues(h map[string]any) map[string]any {
	if h["type"] != "http" {
		return h
	}
	hm, ok := h["headers"].(map[string]any)
	if !ok {
		return h
	}
	cp := make(map[string]any, len(h))
	for k, v := range h {
		cp[k] = v
	}
	blank := make(map[string]any, len(hm))
	for k := range hm {
		blank[k] = ""
	}
	cp["headers"] = blank
	return cp
}

// hooksEqualIgnoringHeaderValues compares two hooks with header values blanked.
func hooksEqualIgnoringHeaderValues(existing any, want map[string]any) bool {
	obj, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	parsed, _, msg := schemaParseHook(obj)
	if msg != "" {
		return false
	}
	return canonicalJSON(blankHeaderValues(parsed)) == canonicalJSON(blankHeaderValues(want))
}

// hookIdentity returns a hook's canonical form with header names lower-cased.
func hookIdentity(v any) (string, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	parsed, _, msg := schemaParseHook(obj)
	if msg != "" {
		return "", false
	}
	if parsed["type"] == "http" {
		if hm, ok := parsed["headers"].(map[string]any); ok {
			lower := make(map[string]any, len(hm))
			for k, v := range hm {
				lower[strings.ToLower(k)] = v
			}
			cp := make(map[string]any, len(parsed))
			for k, v := range parsed {
				cp[k] = v
			}
			cp["headers"] = lower
			parsed = cp
		}
	}
	return canonicalJSON(parsed), true
}

// groupMatcher returns a group's matcher ("" when absent), or
// ok=false when the entry is not a {hooks:[…]} group.
func groupMatcher(g any) (string, bool) {
	gm, ok := g.(map[string]any)
	if !ok {
		return "", false
	}
	if _, isArr := gm["hooks"].([]any); !isArr {
		return "", false
	}
	switch m := gm["matcher"].(type) {
	case nil:
		return "", true
	case string:
		return m, true
	default:
		return "", false
	}
}

// countMatchingHooks counts the hooks that match the edit target.
func countMatchingHooks(settings map[string]any, tgt *hookEditTarget) int {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return 0
	}
	groups, ok := hooks[tgt.Event].([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, g := range groups {
		if m, ok := groupMatcher(g); !ok || m != tgt.Matcher {
			continue
		}
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			if hooksEqualIgnoringHeaderValues(h, tgt.Hook) {
				n++
			}
		}
	}
	return n
}

// duplicateKind returns "exact", "by-headers" or "" for hook h against
// the groups with the matcher, skipping position skip (group, index).
func duplicateKind(groups []any, matcher string, h map[string]any, skipGroup, skipIndex int) string {
	id, ok := hookIdentity(h)
	if !ok {
		return ""
	}
	byHeaders := false
	for gi, g := range groups {
		if m, ok := groupMatcher(g); !ok || m != matcher {
			continue
		}
		for hi, e := range g.(map[string]any)["hooks"].([]any) {
			if gi == skipGroup && hi == skipIndex {
				continue
			}
			if eid, ok := hookIdentity(e); ok && eid == id {
				return "exact"
			}
			if hooksEqualIgnoringHeaderValues(e, h) {
				byHeaders = true
			}
		}
	}
	if byHeaders {
		return "by-headers"
	}
	return ""
}

func duplicateOutcome(kind string) string {
	if kind == "exact" {
		return "duplicate"
	}
	return "twin-by-headers"
}

// mergeReplacement merges a replacement: the new hook keeps the old entry's
// unknown keys, and an http hook keeps header values the editor sent blank.
func mergeReplacement(old any, next map[string]any) map[string]any {
	out := map[string]any{}
	if om, ok := old.(map[string]any); ok {
		if parsed, _, msg := schemaParseHook(om); msg == "" {
			for k, v := range om {
				if _, known := parsed[k]; !known && k != "__proto__" && k != "constructor" && k != "prototype" {
					out[k] = v
				}
			}
			if parsed["type"] == "http" && next["type"] == "http" {
				oldHeaders, _ := parsed["headers"].(map[string]any)
				if _, has := next["headers"]; !has {
					if oldHeaders != nil {
						out["headers"] = oldHeaders
					}
				} else if nh, ok := next["headers"].(map[string]any); ok {
					merged := make(map[string]any, len(nh))
					for k, v := range nh {
						if s, isStr := v.(string); isStr && s == "" {
							if ov, has := oldHeaders[k]; has {
								v = ov
							} else {
								for ok2, ov := range oldHeaders {
									if strings.EqualFold(ok2, k) {
										v = ov
										break
									}
								}
							}
						}
						merged[k] = v
					}
					nextCopy := make(map[string]any, len(next))
					for k, v := range next {
						nextCopy[k] = v
					}
					nextCopy["headers"] = merged
					next = nextCopy
				}
			}
		}
	}
	for k, v := range next {
		out[k] = v
	}
	if out["type"] == "http" {
		if hm, ok := out["headers"].(map[string]any); ok && len(hm) == 0 {
			delete(out, "headers")
		}
	}
	return out
}

// applyHookEdit applies the edit to the settings object; it reports the
// outcome and whether the object changed.
func applyHookEdit(settings map[string]any, req hookEditRequest) (string, bool) {
	rawHooks, present := settings["hooks"]
	var hooks map[string]any
	if present && rawHooks != nil {
		h, ok := rawHooks.(map[string]any)
		if !ok {
			return "not-a-list", false
		}
		hooks = h
	} else {
		hooks = map[string]any{}
	}
	lists := map[string][]any{}
	list := func(ev string) ([]any, bool) {
		if l, ok := lists[ev]; ok {
			return l, true
		}
		raw, has := hooks[ev]
		if has && raw != nil {
			arr, ok := raw.([]any)
			if !ok {
				return nil, false
			}
			cp := make([]any, len(arr))
			for i, g := range arr {
				if gm, ok := g.(map[string]any); ok {
					if hs, ok := gm["hooks"].([]any); ok {
						ng := make(map[string]any, len(gm))
						for k, v := range gm {
							ng[k] = v
						}
						ng["hooks"] = append([]any{}, hs...)
						cp[i] = ng
						continue
					}
				}
				cp[i] = g
			}
			lists[ev] = cp
			return cp, true
		}
		lists[ev] = []any{}
		return lists[ev], true
	}
	commit := func() {
		for ev, l := range lists {
			if len(l) > 0 {
				hooks[ev] = l
			} else {
				delete(hooks, ev)
			}
		}
		if len(hooks) > 0 {
			settings["hooks"] = hooks
		} else {
			delete(settings, "hooks")
		}
	}

	var newHook map[string]any
	if req.Op == "add" {
		newHook = req.Hook
	} else {
		tl, ok := list(req.Target.Event)
		if !ok {
			return "not-a-list", false
		}
		gi, hi := -1, -1
		for g := 0; g < len(tl) && gi < 0; g++ {
			if m, ok := groupMatcher(tl[g]); !ok || m != req.Target.Matcher {
				continue
			}
			for h, e := range tl[g].(map[string]any)["hooks"].([]any) {
				if hooksEqualIgnoringHeaderValues(e, req.Target.Hook) {
					gi, hi = g, h
					break
				}
			}
		}
		if gi < 0 {
			return "target-missing", false
		}
		group := tl[gi].(map[string]any)
		ghooks := group["hooks"].([]any)
		if req.Op == "replace" && req.Event == req.Target.Event && req.Matcher == req.Target.Matcher {
			repl := mergeReplacement(ghooks[hi], req.Hook)
			if kind := duplicateKind(tl, req.Target.Matcher, repl, gi, hi); kind != "" {
				return duplicateOutcome(kind), false
			}
			ghooks[hi] = repl
			commit()
			return "applied", true
		}
		removed := ghooks[hi]
		ghooks = append(ghooks[:hi:hi], ghooks[hi+1:]...)
		group["hooks"] = ghooks
		if len(ghooks) == 0 {
			tl = append(tl[:gi:gi], tl[gi+1:]...)
			lists[req.Target.Event] = tl
		}
		if req.Op == "remove" {
			commit()
			return "applied", true
		}
		newHook = mergeReplacement(removed, req.Hook)
	}
	dl, ok := list(req.Event)
	if !ok {
		return "not-a-list", false
	}
	if kind := duplicateKind(dl, req.Matcher, newHook, -1, -1); kind != "" {
		return duplicateOutcome(kind), false
	}
	for _, g := range dl {
		if m, ok := groupMatcher(g); ok && m == req.Matcher {
			gm := g.(map[string]any)
			gm["hooks"] = append(gm["hooks"].([]any), newHook)
			commit()
			return "applied", true
		}
	}
	ng := map[string]any{"hooks": []any{newHook}}
	if req.Matcher != "" {
		ng["matcher"] = req.Matcher
	}
	lists[req.Event] = append(dl, ng)
	commit()
	return "applied", true
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
