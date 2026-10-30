package hooks

import (
	"encoding/json"
	"strings"
)

// The "if" guard lets a hook entry gate itself on the current tool call. Its
// grammar is the permission-rule mini-language shared with the
// permission system: a rule is either a bare tool name ("Bash") or a call form
// "ToolName(content)" (for example "Bash(git commit:*)"). A hook whose guard
// does not match the tool call is skipped; a guard on a non-tool event cannot
// be evaluated and the hook is skipped with a warning.
//
// Evaluation: parse the rule as ToolName(content); if the canonicalised tool
// name is not one of the current tool's names the guard fails; a rule without
// content matches on the tool name alone; otherwise the per-tool content
// matcher decides.
//
// The per-tool content matcher and the tool-rename table live outside this
// package; the grammar is modelled here and the content matcher and the
// canonicaliser are taken as injectable seams.

// ruleKind classifies a parsed rule string.
type ruleKind int

const (
	ruleBare ruleKind = iota
	ruleCall
	ruleMalformed
)

// parsedRule is the result of parseRuleShape.
type parsedRule struct {
	kind       ruleKind
	toolName   string
	rawContent string
}

// parseRuleShape classifies a rule string: a string with no parentheses is "bare"; a
// string of the form "<name>(<content>)" with a single trailing ")" and no
// parentheses in the name is a "call"; anything else is "malformed".
func parseRuleShape(s string) parsedRule {
	if !strings.ContainsAny(s, "()") {
		return parsedRule{kind: ruleBare}
	}
	n := strings.IndexByte(s, '(')
	t := strings.LastIndexByte(s, ')')
	if n == -1 && t == -1 {
		return parsedRule{kind: ruleBare}
	}
	name := ""
	if n >= 0 {
		name = s[:n]
	}
	if n == -1 || t <= n || t != len(s)-1 || strings.ContainsAny(name, "()") {
		return parsedRule{kind: ruleMalformed}
	}
	return parsedRule{kind: ruleCall, toolName: name, rawContent: s[n+1 : t]}
}

// Rule is the interpreted guard: a target tool name and optional content.
type Rule struct {
	ToolName    string
	RuleContent string
	HasContent  bool
}

// unescapeRuleContent unescapes rule content: "\(" -> "(", "\)" -> ")", "\\" -> "\".
func unescapeRuleContent(s string) string {
	s = strings.ReplaceAll(s, `\(`, "(")
	s = strings.ReplaceAll(s, `\)`, ")")
	return strings.ReplaceAll(s, `\\`, `\`)
}

// ParseRule parses a guard rule string. canon canonicalises a tool name
// (resolving a legacy/renamed name to its current one); a nil canon is identity.
// A non-call string, or a call whose content is "" or "*", yields a
// tool-name-only rule.
func ParseRule(s string, canon func(string) string) Rule {
	if canon == nil {
		canon = func(x string) string { return x }
	}
	p := parseRuleShape(s)
	if p.kind != ruleCall || p.toolName == "" {
		return Rule{ToolName: canon(s)}
	}
	if p.rawContent == "" || p.rawContent == "*" {
		return Rule{ToolName: canon(p.toolName)}
	}
	return Rule{ToolName: canon(p.toolName), RuleContent: unescapeRuleContent(p.rawContent), HasContent: true}
}

// GuardContext carries everything the evaluator needs about the current event.
type GuardContext struct {
	// IsToolEvent is true for PreToolUse/PostToolUse/PostToolUseFailure/
	// PermissionRequest/PermissionDenied — the only events whose input carries a
	// tool name, so the only events an "if" guard can be evaluated for.
	IsToolEvent bool

	// Names is the set of names the current tool answers to, each canonicalised:
	// its canonical name plus any alias/legacy names. A rule matches when its
	// (canonicalised) tool name is in this set.
	Names []string

	// ToolInput is the tool_input the content matcher inspects.
	ToolInput json.RawMessage

	// Canon canonicalises a tool name. Nil is identity.
	Canon func(string) string

	// ContentMatcher decides whether a rule's content matches the tool input.
	// Nil means no content can match, so a rule with content never runs (the
	// conservative default).
	ContentMatcher func(ruleContent string, input json.RawMessage) bool
}

// EvalIf evaluates a hook's "if" guard. It returns whether the hook should run
// and whether the guard was evaluable at all. An empty guard always runs. A
// guard on a non-tool event is not evaluable (evaluable=false); the caller
// skips the hook and warns ("Hook if condition ... cannot be evaluated for
// non-tool event ...").
func EvalIf(ifExpr string, gc GuardContext) (run, evaluable bool) {
	if strings.TrimSpace(ifExpr) == "" {
		return true, true
	}
	if !gc.IsToolEvent {
		return false, false
	}
	canon := gc.Canon
	if canon == nil {
		canon = func(x string) string { return x }
	}
	rule := ParseRule(ifExpr, canon)
	// The rule's tool name must be among the current tool's names.
	target := canon(rule.ToolName)
	matched := false
	for _, n := range gc.Names {
		if canon(n) == target {
			matched = true
			break
		}
	}
	if !matched {
		return false, true
	}
	if !rule.HasContent {
		return true, true
	}
	if gc.ContentMatcher == nil {
		return false, true
	}
	return gc.ContentMatcher(rule.RuleContent, gc.ToolInput), true
}
