package hooks

import (
	"regexp"
	"strings"
)

// nameListMatcher matches a matcher string that is a
// "simple" name or pipe-alternation of word-only names (optionally anchored or
// wrapped in a single group), as opposed to a general regular expression. When
// a matcher looks like this, it is additionally tested against the tool's
// family/alias names.
var nameListMatcher = regexp.MustCompile(`^\^?(?:\((?:\?:)?)?\^?\w+\$?(?:\|\^?\w+\$?)*\)?\$?$`)

// groupNameShape / commaListShape are the shape tests applied before
// treating a matcher as a plain list of tool names: pipes only, or — for the
// events in commaListEvents — also commas, spaces and dashes.
var (
	groupNameShape = regexp.MustCompile(`^[a-zA-Z0-9_|]+$`)
	commaListShape = regexp.MustCompile(`^[a-zA-Z0-9_|, -]+$`)
	listSplit      = regexp.MustCompile(`[|,]`)
)

// commaListEvents are the events whose matchers may use the comma list syntax.
var commaListEvents = map[Event]bool{
	"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true, "PermissionRequest": true,
	"PermissionDenied": true, "UserPromptExpansion": true, "SessionStart": true, "SessionEnd": true,
	"Setup": true, "PreCompact": true, "PostCompact": true, "PreModelSwitch": true,
	"PostModelSwitch": true, "Notification": true, "SubagentStart": true, "SubagentStop": true,
	"Elicitation": true, "ElicitationResult": true, "ConfigChange": true, "InstructionsLoaded": true,
	"DirectoryAdded": true,
}

// nameList returns the tool names a plain name-list matcher spells,
// or ok=false when the matcher is not list-shaped and is a regex instead.
func nameList(raw string, commas bool) (names []string, ok bool) {
	if commas {
		if !commaListShape.MatchString(raw) {
			return nil, false
		}
		for _, n := range listSplit.Split(raw, -1) {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		return names, true
	}
	if !groupNameShape.MatchString(raw) {
		return nil, false
	}
	for _, n := range strings.Split(raw, "|") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, true
}

// Matcher decides whether a configured matcher string selects a given tool name
// (for tool events) or, for non-tool events, whether the group applies at all.
//
// Semantics:
//
//   - An empty matcher or "*" matches everything.
//   - Otherwise the matcher is compiled as a regular expression and tested
//     (unanchored) against the tool name and any alias/legacy names.
//   - A matcher that is not a valid regular expression matches nothing and
//     warns "Invalid regex pattern in hook matcher: <n>".
//
// Matchers are compiled with Go's regexp (RE2), so a few exotic patterns
// (backreferences, lookaround) that a JavaScript RegExp accepts are reported
// as invalid.
//
// A plain name-list matcher ("Write|Edit", or "Write, Edit" for the comma-list
// events) is not a regex: it is turned into an exact name list, matched by
// membership of the tool name or its legacy / alias / family names (MatchFull).
// The MCP/cliOwned family cases are driven by the host's tool info
// (MatchInput); GroupResolver remains as a host override for the list.
type Matcher struct {
	raw      string
	matchAll bool
	re       *regexp.Regexp // nil when matchAll or when raw is not a valid regexp
	valid    bool
}

// GroupResolver resolves a matcher string to the set of tool names it names,
// reporting whether the matcher was a known group.
type GroupResolver func(matcher string) (members []string, ok bool)

// NewMatcher compiles a matcher string. An invalid regular expression produces
// a Matcher whose Match always returns false (and whose Valid reports false),
// so the caller can warn and skip it.
func NewMatcher(raw string) Matcher {
	if raw == "" || raw == "*" {
		return Matcher{raw: raw, matchAll: true, valid: true}
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		return Matcher{raw: raw, valid: false}
	}
	return Matcher{raw: raw, re: re, valid: true}
}

// Valid reports whether the matcher string was an empty/"*" wildcard or a
// well-formed regular expression. An invalid matcher never matches.
func (m Matcher) Valid() bool { return m.valid }

// Raw returns the original matcher string.
func (m Matcher) Raw() string { return m.raw }

// Match reports whether the matcher selects toolName. For lifecycle events that
// carry no tool, callers conventionally pass an empty toolName, which only the
// wildcard matcher ("" or "*") selects.
func (m Matcher) Match(toolName string) bool {
	return m.MatchAny(toolName, nil)
}

// MatchAny reports whether the matcher selects toolName or any of its alias /
// legacy names: the compiled regex is re-tested against each alias.
func (m Matcher) MatchAny(toolName string, aliases []string) bool {
	if m.matchAll {
		return true
	}
	if m.re == nil {
		return false
	}
	if m.re.MatchString(toolName) {
		return true
	}
	for _, a := range aliases {
		if m.re.MatchString(a) {
			return true
		}
	}
	return false
}

// MatchGroup resolves the matcher as an alias group first: if resolver
// reports the matcher is a known group, membership of toolName (or any alias)
// in that group decides the result, without touching the regex path. Otherwise
// it falls back to MatchAny.
func (m Matcher) MatchGroup(toolName string, aliases []string, resolver GroupResolver) bool {
	if m.matchAll {
		return true
	}
	if resolver != nil {
		if members, ok := resolver(m.raw); ok {
			if contains(members, toolName) {
				return true
			}
			for _, a := range aliases {
				if contains(members, a) {
					return true
				}
			}
			return false
		}
	}
	return m.MatchAny(toolName, aliases)
}

// MatchInput carries the per-tool context the full matcher needs
// beyond the matcher string itself.
type MatchInput struct {
	// ToolName is the current tool's canonical name.
	ToolName string
	// FamilyNames are the tool's family / alias names:
	// MCP family names, verb-child names, and the like.
	FamilyNames []string
	// LegacyNames are the tool's former names.
	LegacyNames []string
	// Aliases are any further alias names.
	Aliases []string
	// MCPCliOwned reports whether the tool is a CLI-owned MCP tool, enabling the
	// relaxed family-name matching applied to such tools.
	MCPCliOwned bool
	// GroupResolver, when set and it recognises the matcher, supplies the
	// member list instead of the built-in name-list parse.
	GroupResolver GroupResolver
	// Event is the hook event being matched; it enables the comma-list
	// matcher syntax for the events in commaListEvents.
	Event Event
}

// MatchFull is the complete matcher:
// an empty/"*" matcher matches all; a named alias group matches by membership;
// otherwise the compiled regex is tested against the tool name, and — for a
// name-list-shaped matcher, or a CLI-owned MCP tool, or via legacy/alias names
// — against the tool's other names.
func (m Matcher) MatchFull(in MatchInput) bool {
	if m.matchAll {
		return true
	}
	// Named alias-group path. A group-shaped matcher that the
	// resolver recognises matches by membership of the tool name or a family
	// name, bypassing the regex path entirely.
	if in.GroupResolver != nil && groupNameShape.MatchString(m.raw) {
		if members, ok := in.GroupResolver(m.raw); ok {
			if contains(members, in.ToolName) {
				return true
			}
			for _, f := range in.FamilyNames {
				if contains(members, f) {
					return true
				}
			}
			return false
		}
	}
	// Plain name list: exact membership, never a regex test. A
	// listed legacy name or alias of the tool selects it.
	if names, ok := nameList(m.raw, commaListEvents[in.Event]); ok {
		if contains(names, in.ToolName) {
			return true
		}
		for _, group := range [][]string{in.LegacyNames, in.Aliases, in.FamilyNames} {
			for _, n := range group {
				if contains(names, n) {
					return true
				}
			}
		}
		return false
	}
	if m.re == nil {
		return false
	}
	if m.re.MatchString(in.ToolName) {
		return true
	}
	// A name-list-shaped matcher is also tested against the family names.
	if nameListMatcher.MatchString(m.raw) {
		for _, f := range in.FamilyNames {
			if m.re.MatchString(f) {
				return true
			}
		}
	}
	// CLI-owned MCP tools get relaxed family matching: with more than two family
	// names, any match counts; otherwise only the last (most specific) name.
	if in.MCPCliOwned {
		if len(in.FamilyNames) > 2 {
			for _, f := range in.FamilyNames {
				if m.re.MatchString(f) {
					return true
				}
			}
		} else if len(in.FamilyNames) > 0 {
			if m.re.MatchString(in.FamilyNames[len(in.FamilyNames)-1]) {
				return true
			}
		}
	}
	for _, k := range in.LegacyNames {
		if m.re.MatchString(k) {
			return true
		}
	}
	for _, a := range in.Aliases {
		if m.re.MatchString(a) {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// SelectHooks returns, in configured order, every command hook whose matcher
// group applies to toolName for the given event. It is the small dispatch step
// performed before running hooks: for each matcher group registered on
// the event, test the matcher and, on a hit, append its hooks.
func (c Config) SelectHooks(event Event, toolName string) []CommandHook {
	var out []CommandHook
	for _, g := range c.Groups(event) {
		if NewMatcher(g.Matcher).MatchFull(MatchInput{ToolName: toolName, Event: event}) {
			out = append(out, g.Hooks...)
		}
	}
	return out
}
