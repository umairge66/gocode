package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Permission-rule validation for `gocode doctor`. A bad permissions.allow /
// deny / ask entry is listed as an invalid-settings entry ("<file> ›
// permissions.allow.0: <message>"). The settings loader checks only key names
// and basic types, so the doctor runs this rule validation itself.
//
// Not covered: Bash wildcard-placement warnings and the escaped-parenthesis
// ambiguity check.

// ruleProblem is one validation result.
type ruleProblem struct {
	Error      string
	Suggestion string
	Examples   []string
	// Warning marks a rule that is valid but will not do what it looks like.
	Warning bool
}

// message joins the parts the way fa does.
func (p ruleProblem) message() string {
	m := p.Error
	if p.Suggestion != "" {
		m += ". " + p.Suggestion
	}
	if len(p.Examples) > 0 {
		m += ". Examples: " + strings.Join(p.Examples, ", ")
	}
	return m
}

var (
	ruleFilePatternTools = []string{"Read", "Write", "Edit", "Glob", "NotebookRead", "NotebookEdit", "Cd"}
	ruleBashPrefixTools  = []string{"Bash"}
)

// ruleShape classifies a rule: kind "bare", "call" (with tool and content) or
// "malformed".
func ruleShape(rule string) (kind, tool, content string) {
	if !strings.ContainsAny(rule, "()") {
		return "bare", "", ""
	}
	open := strings.Index(rule, "(")
	closeIdx := strings.LastIndex(rule, ")")
	if open == -1 && closeIdx == -1 {
		return "bare", "", ""
	}
	if open == -1 || closeIdx <= open || closeIdx != len(rule)-1 || strings.ContainsAny(rule[:open], "()") {
		return "malformed", "", ""
	}
	return "call", rule[:open], rule[open+1 : closeIdx]
}

// splitMCPToolName parses "mcp__<server>[__<tool>]".
func splitMCPToolName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp__")
	if !found || rest == "" {
		return "", "", false
	}
	server, tool, _ = strings.Cut(rest, "__")
	return server, tool, true
}

// wildcardAllowProblem reports a problem with a wildcard tool name in an
// allow rule.
func wildcardAllowProblem(tool string) *ruleProblem {
	if !strings.Contains(tool, "*") {
		return nil
	}
	if server, _, ok := splitMCPToolName(tool); ok && !strings.Contains(server, "*") {
		return nil
	}
	return &ruleProblem{
		Error:      fmt.Sprintf("Wildcard tool name %q is not supported in allow rules", tool),
		Suggestion: "An allow pattern must name the scope it widens — globs are permitted only in the tool position after a literal mcp__<server>__ prefix. Deny and ask rules accept wildcards anywhere",
		Examples:   []string{"mcp__puppeteer__*", "mcp__github__get_*"},
	}
}

func formatRule(tool, content string) string {
	if content == "" {
		return tool
	}
	return tool + "(" + content + ")"
}

// validatePermissionRule validates one rule for its behavior (allow / deny /
// ask). It returns nil for
// a rule with nothing to report.
func validatePermissionRule(rule, behavior string) *ruleProblem {
	if strings.TrimSpace(rule) == "" {
		return &ruleProblem{Error: "Permission rule cannot be empty"}
	}
	kind, callTool, raw := ruleShape(rule)
	if kind == "malformed" {
		return &ruleProblem{Error: "Malformed Tool(content) rule", Suggestion: `Rules take the form Tool or Tool(content) and must end at the closing ")"; parentheses inside the content are literal`}
	}
	if kind == "call" && raw == "" {
		if callTool == "" {
			return &ruleProblem{Error: "Empty parentheses with no tool name", Suggestion: "Specify a tool name before the parentheses"}
		}
		return &ruleProblem{Error: "Empty parentheses", Suggestion: fmt.Sprintf("Either specify a pattern or use just %q without parentheses", callTool), Examples: []string{callTool, callTool + "(some-pattern)"}}
	}
	tool, content := rule, ""
	if kind == "call" {
		tool = callTool
		if raw != "*" {
			content = raw
		}
	}
	if server, mcpTool, ok := splitMCPToolName(tool); ok {
		if kind == "call" {
			p := &ruleProblem{Error: "MCP rules do not support patterns in parentheses"}
			if mcpTool != "" && !strings.Contains(mcpTool, "*") {
				alt := tool + "__*"
				p.Suggestion = fmt.Sprintf("Use %q without parentheses. For all tools of a server, use \"mcp__%s__*\" for the server %q, or %q for a server named \"%s__%s\"", tool, server, server, alt, server, mcpTool)
				p.Examples = []string{"mcp__" + server, "mcp__" + server + "__*", "mcp__" + server + "__" + mcpTool, alt}
			} else {
				p.Suggestion = fmt.Sprintf("Use %q without parentheses, or use \"mcp__%s__*\" for all tools", tool, server)
				p.Examples = []string{"mcp__" + server, "mcp__" + server + "__*"}
				if mcpTool != "" && mcpTool != "*" {
					p.Examples = append(p.Examples, "mcp__"+server+"__"+mcpTool)
				}
			}
			return p
		}
		if behavior == "allow" {
			return wildcardAllowProblem(tool)
		}
		return nil
	}
	if tool == "" {
		return &ruleProblem{Error: "Tool name cannot be empty"}
	}
	if behavior == "allow" {
		if p := wildcardAllowProblem(tool); p != nil {
			return p
		}
	}
	if !strings.Contains(tool, "_") {
		r, size := utf8.DecodeRuneInString(tool)
		if unicode.ToUpper(r) != r {
			return &ruleProblem{Error: "Tool names must start with uppercase", Suggestion: fmt.Sprintf("Use %q", string(unicode.ToUpper(r))+tool[size:])}
		}
	}
	if content != "" {
		switch tool {
		case "WebSearch":
			if strings.ContainsAny(content, "*?") {
				return &ruleProblem{Error: "WebSearch does not support wildcards", Suggestion: "Use exact search terms without * or ?", Examples: []string{"WebSearch(gocode ai)", "WebSearch(typescript tutorial)"}}
			}
		case "WebFetch":
			if strings.Contains(content, "://") || strings.HasPrefix(content, "http") {
				return &ruleProblem{Error: "WebFetch permissions use domain format, not URLs", Suggestion: `Use "domain:hostname" format`, Examples: []string{"WebFetch(domain:example.com)", "WebFetch(domain:github.com)"}}
			}
			if !strings.HasPrefix(content, "domain:") {
				return &ruleProblem{Error: `WebFetch permissions must use "domain:" prefix`, Suggestion: `Use "domain:hostname" format`, Examples: []string{"WebFetch(domain:example.com)", "WebFetch(domain:*.google.com)"}}
			}
		}
	}
	if containsStr(ruleBashPrefixTools, tool) && content == ":*" {
		return &ruleProblem{Error: "Prefix cannot be empty before :*", Suggestion: "Specify a command prefix before :*", Examples: []string{"Bash(npm *)", "Bash(git *)"}}
	}
	if containsStr(ruleFilePatternTools, tool) && strings.Contains(content, ":*") {
		return &ruleProblem{Error: `The ":*" syntax is only for Bash prefix rules`, Suggestion: `Use glob patterns like "*" or "**" for file matching`, Examples: []string{tool + "(*.ts) - matches .ts files", tool + "(src/**) - matches all files in src", tool + "(**/*.test.ts) - matches test files"}}
	}
	if content != "" && !strings.Contains(content, ":*") {
		via := ""
		switch tool {
		case "Write", "NotebookEdit", "MultiEdit":
			via = "Edit"
		case "Glob":
			via = "Read"
		}
		if via != "" {
			what := "reading"
			if via == "Edit" {
				what = "editing"
			}
			return &ruleProblem{Warning: true, Error: fmt.Sprintf("%s is not matched by file permission checks — only %s(path) rules are. Use %s instead (%s rules cover all file-%s tools).", formatRule(tool, content), via, formatRule(via, content), via, what)}
		}
	}
	return nil
}
