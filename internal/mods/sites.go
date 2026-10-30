package mods

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// site is one event's dispatch rules:
// what a rewrite passed to next(e) may change, which of the engine's keys a
// rewrite gets back when it leaves them out, and what a hook's answer must be.
type site struct {
	event string
	// restore lists the keys copied from the received argument into a
	// rewrite that leaves them out.
	restore []string
	// checkArgument refuses a rewrite: "" accepts it.
	checkArgument func(passed, received map[string]any) string
	// check refuses an answer: "" accepts it.
	// fromBelow are the answers next(e) gave this hook.
	check func(result, received map[string]any, fromBelow []map[string]any) string
	// settle normalizes an accepted answer.
	settle func(result map[string]any) map[string]any
}

// sameJSON reports equality: the two values serialize alike.
func sameJSON(a, b any) bool {
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	return ea == nil && eb == nil && string(ja) == string(jb)
}

// changedEnvelope: the first of keys a rewrite changed.
func changedEnvelope(keys []string, passed, received map[string]any) string {
	for _, k := range keys {
		if !sameJSON(passed[k], received[k]) {
			return fmt.Sprintf("a changed %s (the envelope is the engine's; a rewrite keeps %s)", k, strings.Join(keys, ", "))
		}
	}
	return ""
}

func changedPinned(keys []string, passed, received map[string]any, explanation string) string {
	for _, k := range keys {
		if !sameJSON(passed[k], received[k]) {
			return fmt.Sprintf("a changed %s (%s)", k, explanation)
		}
	}
	return ""
}

// denyOr: either { deny } (a string) or what ok accepts, never both.
func denyOr(r map[string]any, what string, ok func(map[string]any) bool) string {
	d, has := r["deny"]
	if !has || d == nil {
		if ok(r) {
			return ""
		}
		return "neither " + what + " nor { deny }"
	}
	if _, isStr := d.(string); !isStr {
		return "a deny that is not a string"
	}
	if ok(r) {
		return "a deny beside " + what
	}
	return ""
}

// lateKey: a refusal key answered after next() answered without it
// (a refusal "in place of next").
func lateKey(key string, r map[string]any, fromBelow []map[string]any) string {
	if v, ok := r[key]; !ok || v == nil {
		return ""
	}
	for _, b := range fromBelow {
		if v, ok := b[key]; !ok || v == nil {
			return fmt.Sprintf("a %s after its next() was answered (%s in place of next)", key, key)
		}
	}
	return ""
}

// stringList: a list of strings, else nil,false.
func stringList(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// keepsContext: a hook adds to the context its next gave it and
// may not leave an entry out.
func keepsContext(ctxv any, below []any) string {
	var have []string
	if ctxv != nil {
		l, ok := stringList(ctxv)
		if !ok {
			return "a context that is not a list of texts"
		}
		have = l
	}
	if slices.Contains(have, "") {
		return "a context with an empty entry"
	}
	for _, b := range below {
		want, _ := stringList(b)
		left := slices.Clone(have)
		for _, w := range want {
			i := slices.Index(left, w)
			if i < 0 {
				return "a context without an entry a hook below attached (a hook adds to the context its next gave it; it may not leave an entry out)"
			}
			left = slices.Delete(left, i, i+1)
		}
	}
	return ""
}

const originChanged = "an origin other than the engine set (next(e) passes e.origin on)"

var (
	// toolCallReserved: the keys of tool.call's envelope.
	toolCallReserved = []string{"tool", "tool_use_id", "agentId", "consent", "$shadowed"}
	toolCheckPinned  = []string{"tool", "input", "tool_use_id", "agentId", "ceiling", "isReadOnly"}
	spawnPinned      = []string{"tool_use_id", "name", "fork", "isTeammate", "workflow", "resume", "parentModel", "permissionMode", "parentAgentId", "provider"}
	checkDecisions   = []string{"allow", "ask", "deny"}
	checkDeciders    = []string{"host", "person"}
)

// spawnArgumentProblem: the spawn fields a rewrite may change must
// stay well formed.
func spawnArgumentProblem(e, received map[string]any) string {
	type rule struct {
		key string
		ok  bool
		msg string
	}
	prompt, _ := e["prompt"].(string)
	_, descOK := e["description"].(string)
	_, typeOK := e["subagentType"].(string)
	_, bgOK := e["background"].(bool)
	model, hasModel := e["model"]
	_, modelStr := model.(string)
	cwd, hasCwd := e["cwd"]
	cwdS, cwdStr := cwd.(string)
	rules := []rule{
		{"prompt", strings.TrimSpace(prompt) != "", "no { prompt } (a non-empty string)"},
		{"description", descOK, "a description that is not a string"},
		{"subagentType", typeOK, "a subagentType that is not a string"},
		{"model", !hasModel || model == nil || modelStr, "a model that is neither a string nor undefined"},
		{"background", bgOK, "a background that is not a boolean"},
		{"cwd", !hasCwd || cwd == nil || (cwdStr && filepath.IsAbs(cwdS)), "a cwd that is not an absolute path"},
	}
	for _, r := range rules {
		if !r.ok && !sameJSON(e[r.key], received[r.key]) {
			return r.msg
		}
	}
	return ""
}

// sites holds the descriptors of the wired events; every other event uses
// genericSite and every op event opSite.
var sites = map[string]*site{
	"tool.call": {
		event:   "tool.call",
		restore: []string{"agentId", "$shadowed", "requestMeta"},
		checkArgument: func(p, r map[string]any) string {
			if s := changedEnvelope(toolCallReserved, p, r); s != "" {
				return s
			}
			if m, ok := p["requestMeta"]; ok && m != nil {
				if _, isObj := m.(map[string]any); !isObj {
					return "a requestMeta that is not an object"
				}
			}
			return ""
		},
		check: func(res, _ map[string]any, below []map[string]any) string {
			if s := denyOr(res, "{ result }", func(m map[string]any) bool { _, ok := m["result"]; return ok }); s != "" {
				return s
			}
			if d, ok := res["deny"]; ok && d != nil {
				return ""
			}
			var ctxs []any
			for _, b := range below {
				if d, ok := b["deny"]; ok && d != nil {
					continue
				}
				if sameJSON(b["result"], res["result"]) {
					ctxs = append(ctxs, b["context"])
				}
			}
			if len(ctxs) == 0 {
				for _, b := range below {
					if d, ok := b["deny"]; !ok || d == nil {
						ctxs = append(ctxs, b["context"])
					}
				}
			}
			return keepsContext(res["context"], ctxs)
		},
		settle: func(r map[string]any) map[string]any {
			out := cloneMap(r)
			if c, ok := out["context"]; ok && c != nil {
				if l, ok := stringList(c); ok {
					out["context"] = toAnySlice(l)
				}
			}
			return out
		},
	},
	"tool.check": {
		event:   "tool.check",
		restore: []string{"tool_use_id", "agentId", "ceiling", "isReadOnly"},
		checkArgument: func(p, r map[string]any) string {
			return changedPinned(toolCheckPinned, p, r, "the tool, its input and the call are the question and are pinned; a hook answers { decision }, it does not ask about another call")
		},
		check: func(res, _ map[string]any, _ []map[string]any) string {
			d, _ := res["decision"].(string)
			if !slices.Contains(checkDecisions, d) {
				return "no { decision } (one of " + strings.Join(checkDecisions, ", ") + ")"
			}
			for _, k := range []string{"reason", "rule", "hook", "setting", "decider"} {
				if v, ok := res[k]; ok && v != nil {
					if _, isStr := v.(string); !isStr {
						return "a reason, rule, hook, setting or decider that is not a string"
					}
				}
			}
			if dc, ok := res["decider"].(string); ok && !slices.Contains(checkDeciders, dc) {
				b, _ := json.Marshal(dc)
				return "a decider that is not one of " + strings.Join(checkDeciders, ", ") + ": " + string(b)
			}
			return ""
		},
		settle: func(r map[string]any) map[string]any {
			out := map[string]any{"decision": r["decision"]}
			for _, k := range []string{"reason", "rule", "hook", "setting", "decider"} {
				if v, ok := r[k]; ok && v != nil {
					out[k] = v
				}
			}
			return out
		},
	},
	"agent.spawn": {
		event:   "agent.spawn",
		restore: []string{"isTeammate", "workflow", "resume", "parentAgentId", "provider"},
		checkArgument: func(p, r map[string]any) string {
			if s := changedPinned(spawnPinned, p, r, "the identity of the spawn and its parent is pinned; a rewrite keeps "+strings.Join(spawnPinned, ", ")); s != "" {
				return s
			}
			return spawnArgumentProblem(p, r)
		},
		check: func(res, _ map[string]any, below []map[string]any) string {
			if s := denyOr(res, "{ model }", func(m map[string]any) bool { _, ok := m["model"].(string); return ok }); s != "" {
				return s
			}
			return lateKey("deny", res, below)
		},
	},
	"prompt.submit": {
		event:   "prompt.submit",
		restore: []string{"command", "isMeta"},
		checkArgument: func(p, r map[string]any) string {
			if _, ok := p["text"].(string); !ok {
				return "no { text }"
			}
			if !sameJSON(p["wait"], r["wait"]) {
				return "a changed wait (pinned)"
			}
			if !sameJSON(p["origin"], r["origin"]) {
				return originChanged
			}
			if s := changedEnvelope([]string{"command", "isMeta"}, p, r); s != "" {
				return s
			}
			if c, ok := p["context"]; ok && c != nil {
				if l, ok := stringList(c); !ok || slices.Contains(l, "") {
					return "a context that is not a list of non-empty texts"
				}
			}
			return ""
		},
		check: func(res, received map[string]any, below []map[string]any) string {
			drop, hasDrop := res["drop"]
			if !hasDrop || drop == nil {
				if _, ok := res["text"].(string); !ok {
					return "neither { text } nor { drop }"
				}
				if o, ok := res["origin"]; ok && o != nil && !sameJSON(o, received["origin"]) {
					return originChanged
				}
				if c, ok := res["context"]; ok && c != nil {
					if l, ok := stringList(c); !ok || slices.Contains(l, "") {
						return "a context that is not a list of non-empty texts"
					}
				}
				return ""
			}
			if _, ok := drop.(string); !ok {
				return "a drop that is not a string"
			}
			return lateKey("drop", res, below)
		},
		settle: func(r map[string]any) map[string]any {
			if d, ok := r["drop"].(string); ok {
				return map[string]any{"drop": d}
			}
			return r
		},
	},
	"command.run": {
		event:   "command.run",
		restore: []string{"presentation"},
		checkArgument: func(p, r map[string]any) string {
			if p["command"] != r["command"] {
				return "a changed command (the engine runs the one it resolved)"
			}
			if !sameJSON(p["presentation"], r["presentation"]) {
				return "a changed presentation (pinned: where the answer shows is a fact)"
			}
			if _, ok := p["args"].(string); !ok {
				return "no { args } (a string)"
			}
			if !sameJSON(p["origin"], r["origin"]) {
				return originChanged
			}
			return ""
		},
		check: func(res, _ map[string]any, below []map[string]any) string {
			if ref, ok := res["ref"]; ok && ref != nil {
				if _, isNum := ref.(float64); !isNum {
					return "a ref that is not the one next(e) gave"
				}
			}
			if t, ok := res["text"]; ok && t != nil {
				if _, isStr := t.(string); !isStr {
					return "a text that is not a string"
				}
			}
			if ec, ok := res["exitCode"]; ok && ec != nil {
				f, isNum := ec.(float64)
				if !isNum || f != float64(int(f)) || f < 0 || f > 255 {
					return "an exitCode that is not a whole number from 0 to 255"
				}
			}
			if c, ok := res["context"]; ok && c != nil {
				var ctxs []any
				for _, b := range below {
					ctxs = append(ctxs, b["context"])
				}
				if len(ctxs) > 1 {
					ctxs = ctxs[len(ctxs)-1:]
				}
				return keepsContext(c, ctxs)
			}
			return ""
		},
	},
	"session.start": {
		event: "session.start",
		checkArgument: func(p, _ map[string]any) string {
			if _, ok := p["cwd"].(string); !ok {
				return "no { cwd }"
			}
			return ""
		},
		check: func(res, _ map[string]any, _ []map[string]any) string {
			if _, ok := res["cwd"].(string); !ok {
				return "no { cwd }"
			}
			return ""
		},
	},
	"turn.start": {
		event:   "turn.start",
		restore: []string{"agentId"},
		checkArgument: func(p, r map[string]any) string {
			if _, ok := p["turnId"].(string); !ok {
				return "no { turnId }"
			}
			if !sameJSON(p["agentId"], r["agentId"]) {
				return "a changed agentId (the loop the turn runs in is pinned)"
			}
			return ""
		},
		check: func(res, _ map[string]any, _ []map[string]any) string {
			if _, ok := res["turnId"].(string); !ok {
				return "no { turnId }"
			}
			return ""
		},
	},
	"turn.complete": {
		event:   "turn.complete",
		restore: []string{"agentId"},
		checkArgument: func(p, r map[string]any) string {
			if _, ok := p["answer"].(string); !ok {
				return "no { answer }"
			}
			if !sameJSON(p["agentId"], r["agentId"]) {
				return "a changed agentId (the loop the turn ran in is pinned)"
			}
			return ""
		},
		check: func(res, _ map[string]any, _ []map[string]any) string {
			if _, ok := res["text"].(string); !ok {
				return "no { text }"
			}
			return ""
		},
	},
}

// opSite: a call on `$` answers { value } or { deny }.
func opSite(event string) *site {
	return &site{
		event: event,
		check: func(res, _ map[string]any, _ []map[string]any) string {
			return denyOr(res, "{ value }", func(m map[string]any) bool { _, ok := m["value"]; return ok })
		},
	}
}

// genericSite is the descriptor of an event with no specific rules
// for: any result object.
func genericSite(event string) *site { return &site{event: event} }

func siteFor(event string) *site {
	if s, ok := sites[event]; ok {
		return s
	}
	if IsOpEvent(event) {
		return opSite(event)
	}
	return genericSite(event)
}

// restoreArgument: keys the rewrite left out come back from the
// received argument.
func (s *site) restoreArgument(passed, received map[string]any) map[string]any {
	var missing []string
	for _, k := range s.restore {
		if _, ok := passed[k]; !ok {
			if _, has := received[k]; has {
				missing = append(missing, k)
			}
		}
	}
	if len(missing) == 0 {
		return passed
	}
	out := cloneMap(passed)
	for _, k := range missing {
		out[k] = received[k]
	}
	return out
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAnySlice(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
