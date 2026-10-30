package mods

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// classicEventNames are the classic hook events; each is also a
// mods event spelled "classic.<Name>".
var classicEventNames = []string{
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch", "Notification",
	"UserPromptSubmit", "UserPromptExpansion", "SessionStart", "SessionEnd", "Stop",
	"StopFailure", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
	"PreModelSwitch", "PostModelSwitch", "PermissionRequest", "PermissionDenied", "Setup",
	"TeammateIdle", "TaskCreated", "TaskCompleted", "Elicitation", "ElicitationResult",
	"ConfigChange", "WorktreeCreate", "WorktreeRemove", "InstructionsLoaded", "CwdChanged",
	"FileChanged", "DirectoryAdded", "MessageDisplay",
}

// OpEvents are the calls on `$` the host serves, each also an event the hooks
// above the caller see.
var OpEvents = []string{
	"model.complete", "model.classify", "model.fork", "audio.play", "audio.speak",
	"mcp.call", "mcp.connect", "session.cwd", "session.root", "session.model",
	"session.turns", "session.id", "session.messages", "session.repo", "session.surface",
	"session.surfaces", "session.authorize", "session.identity", "session.usage",
	"session.version", "turn.abort", "prompt.read", "flag.value", "tool.list",
	"tool.register", "command.list", "command.register", "config.list", "agent.list",
	"agent.register", "ui.toast", "ui.status", "ui.log", "ui.notice", "ui.invalidate",
	"ui.open", "ui.close", "ui.panes", "ui.selection", "ui.copy", "ui.view", "ui.notify",
	"ui.blit", "fs.read", "fs.write", "fs.list", "fs.exists", "fs.stat", "fs.ancestors",
	"store.get", "store.set", "store.delete", "store.keys", "state.get", "state.set",
	"clock.now", "clock.sleep", "clock.after", "clock.every", "http.fetch", "process.run",
	"process.spawn", "settings.read", "env.get", "env.set",
}

// engineEvents are the engine's own events (between the classic events and
// the op events).
var engineEvents = []string{
	"tool.call", "tool.check", "ui.render", "ui.resolve", "ui.press", "ui.input",
	"ui.select", "ui.message", "ui.fault", "ui.scroll", "ui.focus", "agent.offer",
	"agent.spawn", "prompt.submit", "prompt.fill", "prompt.suggest", "prompt.edit",
	"prompt.change", "prompt.autocomplete", "prompt.section", "prompt.context",
	"prompt.attachment", "prompt.mention", "prompt.compose", "tool.describe",
	"command.run", "command.describe", "config.set", "config.describe", "telemetry.log",
	"telemetry.mark", "skill.prompt", "attribution.text", "session.start",
	"session.receive", "session.append", "session.send", "session.compact",
	"session.attach", "session.detach", "session.measure", "session.end",
	"plugin.register", "turn.start", "turn.step", "turn.complete", "engine.create",
}

// AllEvents is every event name a hook may register on: the
// classic events, the engine's events and the op events.
var AllEvents = func() []string {
	out := make([]string, 0, len(classicEventNames)+len(engineEvents)+len(OpEvents))
	for _, n := range classicEventNames {
		out = append(out, "classic."+n)
	}
	out = append(out, engineEvents...)
	return append(out, OpEvents...)
}()

// streamingEvents are the events whose hook is an async generator.
var streamingEvents = []string{"turn.step", "process.spawn"}

// IsEvent reports whether name is a known event.
func IsEvent(name string) bool { return slices.Contains(AllEvents, name) }

// IsOpEvent reports whether name is a call on `$` the host serves.
func IsOpEvent(name string) bool { return slices.Contains(OpEvents, name) }

// IsStreamingEvent reports whether name is a streaming event.
func IsStreamingEvent(name string) bool { return slices.Contains(streamingEvents, name) }

func isTelemetryEvent(name string) bool { return strings.HasPrefix(name, "telemetry.") }

// Tier is where a plugin's hooks stand in a chain: the managed
// plugins an administrator prepends, everything a person installs, the managed
// plugins appended, the built-in plugins, and the engine's own innermost link.
type Tier string

const (
	TierPrepend Tier = "prepend"
	TierUser    Tier = "user"
	TierAppend  Tier = "append"
	TierBuiltin Tier = "builtin"
	TierCore    Tier = "core"
)

// tierOrder is the chain order, outermost first.
var tierOrder = []Tier{TierPrepend, TierUser, TierAppend, TierBuiltin, TierCore}

// tierRank is a tier's position in tierOrder.
func tierRank(t Tier) int { return slices.Index(tierOrder, t) }

// PluginTiers are the tiers a plugin can sit in (kit PLUGIN_TIERS).
var PluginTiers = []Tier{TierPrepend, TierUser, TierAppend, TierBuiltin}

// IsPluginTier is the kit's isPluginTier.
func IsPluginTier(s string) bool { return slices.Contains(PluginTiers, Tier(s)) }

// nextToTargets: the tiers a hook of a tier may jump to with next.to.
// Only a managed hook may: prepend to append/builtin/core, append to core.
func nextToTargets(t Tier) []Tier {
	switch t {
	case TierPrepend:
		return []Tier{TierAppend, TierBuiltin, TierCore}
	case TierAppend:
		return []Tier{TierCore}
	}
	return nil
}

// denyShapes: for each event that can be refused, the result a refusal
// with the given text takes. An event absent here (or mapped to nil) cannot be
// refused, so a `.catch` answering { deny } there is not a result it reads.
var denyShapes = map[string]func(string) map[string]any{
	"tool.call":       func(t string) map[string]any { return map[string]any{"deny": t} },
	"tool.check":      func(t string) map[string]any { return map[string]any{"decision": "deny", "reason": t} },
	"agent.offer":     func(string) map[string]any { return map[string]any{"isOffered": false} },
	"agent.spawn":     func(t string) map[string]any { return map[string]any{"deny": t} },
	"prompt.submit":   func(t string) map[string]any { return map[string]any{"drop": t} },
	"prompt.mention":  func(t string) map[string]any { return map[string]any{"deny": t} },
	"command.run":     func(t string) map[string]any { return map[string]any{"text": t} },
	"config.set":      func(t string) map[string]any { return map[string]any{"deny": t} },
	"telemetry.log":   func(t string) map[string]any { return map[string]any{"deny": t} },
	"telemetry.mark":  func(t string) map[string]any { return map[string]any{"deny": t} },
	"plugin.register": func(t string) map[string]any { return map[string]any{"refuse": t} },
	"session.receive": func(t string) map[string]any { return map[string]any{"consumed": t} },
	"session.append":  func(t string) map[string]any { return map[string]any{"deny": t} },
	"session.send":    func(t string) map[string]any { return map[string]any{"isDelivered": false, "reason": t} },
	"session.compact": func(t string) map[string]any { return map[string]any{"skip": t} },
	"ui.scroll":       func(t string) map[string]any { return map[string]any{"deny": t} },
	"ui.focus":        func(t string) map[string]any { return map[string]any{"deny": t} },
}

// DenyShape returns event's refusal result carrying text.
// Every op event refuses with { deny } (its site is opSite).
func DenyShape(event, text string) (map[string]any, bool) {
	if f, ok := denyShapes[event]; ok {
		return f(text), true
	}
	if IsOpEvent(event) {
		return map[string]any{"deny": text}, true
	}
	return nil, false
}

// defaultResults: what an event with nothing beneath its hooks answers
// (the engine's observing events: their core only reads the result back).
var defaultResults = map[string]func(map[string]any) map[string]any{
	"session.start":  func(e map[string]any) map[string]any { return map[string]any{"cwd": e["cwd"]} },
	"session.attach": func(e map[string]any) map[string]any { return map[string]any{"clientId": e["clientId"]} },
	"session.detach": func(e map[string]any) map[string]any { return map[string]any{"clientId": e["clientId"]} },
	"session.measure": func(e map[string]any) map[string]any {
		return map[string]any{"changed": e["changed"]}
	},
	"session.end":   func(e map[string]any) map[string]any { return map[string]any{"sessionId": e["sessionId"]} },
	"prompt.change": func(e map[string]any) map[string]any { return map[string]any{"text": e["text"]} },
	"turn.start":    func(e map[string]any) map[string]any { return map[string]any{"turnId": e["turnId"]} },
	"turn.complete": func(e map[string]any) map[string]any {
		out := map[string]any{"text": e["answer"]}
		if u, ok := e["usage"]; ok && u != nil {
			out["usage"] = u
		}
		return out
	},
}

// identPart is one dot-separated segment of an event name (an
// identifier start then identifier continues, default-ignorables excluded).
var identPart = regexp.MustCompile(`^[\p{L}\p{Nl}$_][\p{L}\p{Nl}\p{Mn}\p{Mc}\p{Nd}\p{Pc}$\x{200C}\x{200D}]*$`)

// pattern is a parsed `on` pattern.
type pattern struct {
	text      string
	negated   bool
	kind      string // "every", "prefix", "name"
	prefixOrN string
}

// parsePattern: an event name, "*", "<prefix>.*", or "!" before
// either.
func parsePattern(s string) (pattern, error) {
	neg := strings.HasPrefix(s, "!")
	r := strings.TrimPrefix(s, "!")
	if neg && r == "*" {
		return pattern{}, fmt.Errorf("%q is not a pattern: it selects no event", s)
	}
	if neg && strings.HasPrefix(r, "!") {
		return pattern{}, fmt.Errorf("%q is not a pattern: \"!\" negates once", s)
	}
	every := r == "*"
	prefix := strings.HasSuffix(r, ".*")
	body := r
	if prefix {
		body = strings.TrimSuffix(r, ".*")
	}
	if !every {
		for _, seg := range strings.Split(body, ".") {
			if !identPart.MatchString(seg) {
				return pattern{}, fmt.Errorf("%q is not an event name or a pattern (\"*\", \"<prefix>.*\", or \"!\" before either)", s)
			}
		}
	}
	p := pattern{text: s, negated: neg}
	switch {
	case every:
		p.kind = "every"
	case prefix:
		p.kind, p.prefixOrN = "prefix", body
	default:
		p.kind, p.prefixOrN = "name", r
	}
	return p, nil
}

// selects reports whether p selects event, where a telemetry
// event is selected for a plugin that is not built in only by a telemetry
// pattern.
func (p pattern) selects(event string, builtin bool) bool {
	var hit bool
	switch p.kind {
	case "every":
		hit = true
	case "prefix":
		hit = strings.HasPrefix(event, p.prefixOrN+".")
	default:
		hit = event == p.prefixOrN
	}
	if p.negated {
		hit = !hit
	}
	if !hit {
		return false
	}
	return builtin || !isTelemetryEvent(event) || isTelemetryEvent(strings.TrimPrefix(p.text, "!"))
}
