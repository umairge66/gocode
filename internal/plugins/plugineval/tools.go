package plugineval

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
)

// Tool families the grant logic distinguishes.
var (
	// alwaysAllowedTools are granted to every case without an operator
	// grant.
	alwaysAllowedTools = setOf("Read", "Glob", "Grep", "NotebookRead", "Skill", "AskUserQuestion", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "TaskStop", "Agent", "TodoWrite")
	shellTools         = []string{"Bash", "PowerShell"}
	// neverAvailableTools are never available inside an eval run.
	neverAvailableTools = []string{"Monitor", "EnterWorktree", "ExitWorktree"}
	mcpPlumbingTools    = []string{"WaitForMcpServers", "RefreshMcpTools", "ToolSearch"}
	writeTools          = setOf("Write", "Edit", "NotebookEdit")
	readTools           = setOf("Read", "Glob", "Grep", "LSP")
	fileCreatingTools   = setOf("Write", "Edit", "NotebookEdit", "Bash", "PowerShell")
)

// ShellRefusal: a shell grant is refused because this machine cannot
// confine the shell. A shell is never confined for an eval run, so a
// shell grant is always refused.
const ShellRefusal = "A shell tool (Bash or PowerShell) was granted but this machine cannot confine it (no sandbox backend on this platform, or it is not installed), so the run was refused rather than run unconfined — drop the shell grant, or on Linux/macOS install the backend."

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// rule is a parsed permission rule: Tool or Tool(content).
type rule struct {
	tool    string
	content string
	scoped  bool
}

func parseRule(s string) rule {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '(')
	if i <= 0 || !strings.HasSuffix(s, ")") {
		return rule{tool: s}
	}
	return rule{tool: s[:i], content: s[i+1 : len(s)-1], scoped: true}
}

func (r rule) String() string {
	if !r.scoped {
		return r.tool
	}
	return r.tool + "(" + r.content + ")"
}

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validGrantRule: a bare tool name (no wildcard) and, for an MCP tool,
// no content.
func validGrantRule(r rule) bool {
	if r.tool == "" || strings.ContainsAny(r.tool, "()") {
		return false
	}
	if strings.HasPrefix(r.tool, "mcp__") {
		return !r.scoped && regexp.MustCompile(`^mcp__[A-Za-z0-9_-]+(__[A-Za-z0-9_-]+)?$`).MatchString(r.tool)
	}
	return toolNameRe.MatchString(r.tool)
}

func splitRules(entries []string) []string { return frontmatter.SplitTools(entries) }

// relativeContent: a path pattern that stays below the run directory.
func relativeContent(c string) bool {
	if strings.HasPrefix(c, "/") || strings.HasPrefix(c, "~") || strings.HasPrefix(c, `\`) || regexp.MustCompile(`^[A-Za-z]:`).MatchString(c) {
		return false
	}
	for _, seg := range regexp.MustCompile(`[\\/]`).Split(c, -1) {
		if seg == ".." {
			return false
		}
	}
	return true
}

// grantCovers: an operator grant covers a case entry when the tool
// matches and either side is unscoped or both name the same content. An MCP
// server grant (mcp__server) covers that server's tools.
func grantCovers(g, c rule) bool {
	if g.tool != c.tool {
		if strings.HasPrefix(c.tool, "mcp__") && (g.tool == "mcp__*" || strings.HasPrefix(c.tool, g.tool+"__")) {
			return true
		}
		return false
	}
	return !g.scoped || !c.scoped || g.content == c.content
}

// Grants is the result of grantTools.
type Grants struct {
	Allowed []string
	Denied  []string
}

// grantTools: operator grants (--allow-tools) apply to every
// case (a never-available tool is refused); a case's allowed_tools entry is
// granted when it is always allowed, or covered by an operator grant; a
// read/write rule whose path is absolute or climbs must be granted exactly.
// Disallowed tools are removed last.
func grantTools(caseAllowed, operator, disallowed []string) Grants {
	var allowed, denied []string
	var granted []rule
	for _, entry := range operator {
		rs := splitRules([]string{entry})
		if len(rs) == 0 {
			continue
		}
		ok := true
		for _, r := range rs {
			if !validGrantRule(parseRule(r)) {
				ok = false
			}
		}
		if !ok {
			denied = append(denied, entry)
			continue
		}
		for _, r := range rs {
			pr := parseRule(r)
			if inStrings(neverAvailableTools, pr.tool) {
				denied = append(denied, r)
				continue
			}
			allowed = append(allowed, r)
			granted = append(granted, pr)
		}
	}
	for _, r := range splitRules([]string{strings.Join(caseAllowed, ",")}) {
		pr := parseRule(r)
		if !validGrantRule(pr) && !(pr.scoped && toolNameRe.MatchString(pr.tool)) {
			denied = append(denied, r)
			continue
		}
		pathTool := readTools[pr.tool] || writeTools[pr.tool]
		if pathTool && pr.scoped && !relativeContent(pr.content) && !exactGrant(granted, pr) {
			denied = append(denied, r)
			continue
		}
		if alwaysAllowedTools[pr.tool] {
			allowed = append(allowed, r)
			continue
		}
		covered := false
		for _, g := range granted {
			if grantCovers(g, pr) {
				covered = true
				break
			}
		}
		if !covered {
			denied = append(denied, r)
		}
	}
	dis := setOf(disallowed...)
	var out []string
	for _, a := range dedupe(allowed) {
		if !dis[parseRule(a).tool] {
			out = append(out, a)
		}
	}
	return Grants{Allowed: out, Denied: dedupe(denied)}
}

func exactGrant(granted []rule, r rule) bool {
	for _, g := range granted {
		if g.tool == r.tool && g.scoped && g.content == r.content {
			return true
		}
	}
	return false
}

// checkDisallowList: --disallow-tools / disallowed_tools take bare
// built-in tool names only.
func checkDisallowList(entries []string, catalog []string) (tools, rejected []string) {
	known := setOf(catalog...)
	for n := range alwaysAllowedTools {
		known[n] = true
	}
	for _, n := range shellTools {
		known[n] = true
	}
	for _, n := range neverAvailableTools {
		known[n] = true
	}
	for _, e := range splitRules(entries) {
		r := parseRule(e)
		if !r.scoped && known[r.tool] {
			tools = append(tools, r.tool)
		} else {
			rejected = append(rejected, e)
		}
	}
	return dedupe(tools), dedupe(rejected)
}

func disallowMessage(rejected []string) string {
	quoted := make([]string, len(rejected))
	for i, r := range rejected {
		quoted[i] = fmt.Sprintf("%q", r)
	}
	what := "they are not built-in tools"
	if len(rejected) == 1 {
		what = "it is not a built-in tool"
	}
	return fmt.Sprintf("%s cannot be disallowed: %s that this version of gocode can remove. Use bare names such as Agent or TaskStop; patterns such as Bash(rm:*) and MCP tools are not accepted.", strings.Join(quoted, ", "), what)
}

// shellGranted reports whether a shell tool is among the grants.
func shellGranted(allowed []string) bool {
	for _, a := range allowed {
		if inStrings(shellTools, parseRule(a).tool) {
			return true
		}
	}
	return false
}

// pathRule renders a path-scoped rule in the form the permission
// matcher understands: Tool(<absolute dir>/**) ("*" crosses separators).
func pathRule(tool, dir, suffix string) string {
	return tool + "(" + filepath.ToSlash(dir) + suffix + ")"
}

// childAllowRules builds the child's --allowed-tools: relative read/write
// patterns are anchored at the run directory; an unscoped read-family
// tool is confined to the read roots; an unscoped write-family tool to
// the write roots; add_dirs are readable.
func childAllowRules(allowed []string, cwd string, readRoots, writeRoots, addDirs []string) []string {
	var anchored []string
	for _, a := range allowed {
		r := parseRule(a)
		if (readTools[r.tool] || writeTools[r.tool]) && r.scoped && relativeContent(r.content) {
			c := strings.TrimLeft(strings.TrimPrefix(r.content, "./"), "/")
			mid := "/"
			if !strings.Contains(strings.TrimRight(c, "/"), "/") {
				mid = "/**/"
			}
			anchored = append(anchored, r.tool+"("+filepath.ToSlash(cwd)+mid+c+")")
			continue
		}
		anchored = append(anchored, a)
	}
	var out []string
	readUnscoped := false
	for _, a := range anchored {
		r := parseRule(a)
		if !r.scoped && readTools[r.tool] {
			readUnscoped = true
			continue
		}
		out = append(out, a)
	}
	if readUnscoped {
		for _, root := range append(append([]string{}, readRoots...), addDirs...) {
			for _, t := range []string{"Read", "Glob", "Grep"} {
				out = append(out, pathRule(t, root, "/**"))
			}
		}
	}
	var final []string
	writeSeen := map[string]bool{}
	for _, a := range out {
		r := parseRule(a)
		if !r.scoped && writeTools[r.tool] {
			writeSeen[r.tool] = true
			continue
		}
		final = append(final, a)
	}
	if len(writeSeen) > 0 {
		tools := []string{"Edit"}
		for _, t := range []string{"Write", "NotebookEdit"} {
			if writeSeen[t] {
				tools = append(tools, t)
			}
		}
		for _, root := range writeRoots {
			for _, t := range tools {
				final = append(final, pathRule(t, root, "/**"))
			}
		}
	}
	for _, d := range addDirs {
		for _, t := range []string{"Read", "Glob", "Grep"} {
			final = append(final, pathRule(t, d, "/**"))
		}
	}
	return dedupe(final)
}

// childDisallowRules builds the child's --disallowed-tools: the sandbox's
// config and output trees, the plugins and case under test (no writes; the
// eval directory not even readable, so graders stay hidden), /proc, the shell
// tools unless granted, the never-available tools, every catalog tool that was
// not granted, and the operator/case disallow lists.
func childDisallowRules(sb *Sandbox, c *Case, evalDirs []string, allowed, disallow, catalog []string) []string {
	var out []string
	for _, d := range []string{sb.ConfigDir, sb.OutDir} {
		out = append(out, pathRule("Read", d, "/**"), pathRule("Edit", d, "/**"))
	}
	for _, d := range append(append([]string{}, c.PluginDirs...), c.CaseDir) {
		out = append(out, pathRule("Edit", d, "/**"))
	}
	for _, d := range append(append([]string{}, evalDirs...), c.CaseDir) {
		for _, t := range []string{"Read", "Glob", "Grep"} {
			out = append(out, pathRule(t, d, "/**"))
		}
	}
	if filepath.Separator == '/' {
		out = append(out, "Read(/proc/**)")
	}
	granted := map[string]bool{}
	for _, a := range allowed {
		granted[parseRule(a).tool] = true
	}
	for _, s := range shellTools {
		if !granted[s] {
			out = append(out, s)
		}
	}
	out = append(out, neverAvailableTools...)
	keep := setOf(mcpPlumbingTools...)
	for n := range alwaysAllowedTools {
		keep[n] = true
	}
	for n := range readTools {
		keep[n] = true
	}
	for _, t := range catalog {
		if !keep[t] && !granted[t] && !inStrings(shellTools, t) && !inStrings(neverAvailableTools, t) {
			out = append(out, t)
		}
	}
	out = append(out, disallow...)
	return dedupe(out)
}

func inStrings(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// advisories: graders that cannot pass with the tools the
// run is granted.
func advisories(c *Case, allowed, disallowed []string, scaffolded bool) []Advisory {
	var out []Advisory
	granted := map[string]bool{}
	for _, a := range allowed {
		granted[parseRule(a).tool] = true
	}
	canCreate := false
	for t := range granted {
		if fileCreatingTools[t] || strings.HasPrefix(t, "mcp__") {
			canCreate = true
		}
	}
	for _, g := range c.Graders {
		if !needsCreatedFile(g, scaffolded) || canCreate {
			continue
		}
		var blocked []string
		for _, d := range disallowed {
			if fileCreatingTools[d] {
				blocked = append(blocked, d)
			}
		}
		text := fmt.Sprintf("grader %q cannot pass with the granted tools: it checks a file the run creates, but no tool the run may use can create one (a plugin hook still could; a skill's own allowed-tools does not count inside a run) — add Write (or Edit / Bash) to the case's allowed_tools and grant it with --allow-tools", g.Name)
		if len(blocked) > 0 {
			text = fmt.Sprintf("grader %q cannot pass: it checks a file the run creates, but no tool the run may use can create one (a plugin hook still could), and the case's disallowed_tools or --disallow-tools lists %s — take one off that list and grant it (allowed_tools and --allow-tools), or drop the grader", g.Name, strings.Join(blocked, ", "))
		}
		out = append(out, Advisory{Grader: g.Name, Text: text})
	}
	for _, g := range c.Graders {
		var needed []string
		switch {
		case g.Type == "tool_used" && (g.Min == nil || *g.Min >= 1):
			needed = []string{g.Tool}
		case g.Type == "tool_order":
			needed = dedupe([]string{g.Before.Tool, g.After.Tool})
		}
		for _, t := range needed {
			if granted[t] || alwaysAllowedTools[t] {
				if inStrings(disallowed, t) {
					out = append(out, Advisory{Grader: g.Name, Text: fmt.Sprintf("grader %q cannot pass: it needs a %s call in the trace, but %s is in the case's disallowed_tools or --disallow-tools, so no call can occur — change that list (and grant the tool if it is not granted), drop the grader, or invert it (max: 0) if absence is the point", g.Name, t, t)})
				}
				continue
			}
			if inStrings(mcpPlumbingTools, t) || strings.HasPrefix(t, "mcp__") && granted[t] {
				continue
			}
			how := fmt.Sprintf("add %s to the case's allowed_tools and grant it with --allow-tools %s", t, t)
			if inStrings(neverAvailableTools, t) {
				how = fmt.Sprintf("%s is never available inside an eval run — drop the grader, or invert it (max: 0) if absence is the point", t)
			}
			out = append(out, Advisory{Grader: g.Name, Text: fmt.Sprintf("grader %q cannot pass with the granted tools: it needs a %s call in the trace, but %s is not granted, so the run withholds it from the model and no call (not even a refused one) can occur — %s", g.Name, t, t, how)})
		}
	}
	return out
}

func needsCreatedFile(g Grader, scaffolded bool) bool {
	switch g.Type {
	case "file_exists":
		return g.Exists
	case "regex":
		if g.Target.Kind == "file" {
			return !scaffolded
		}
		return g.Target.Kind == "files" && !(g.Match == "not_contains" || g.Match == "count:0")
	case "llm":
		return g.Focus.Kind == "file" && !scaffolded
	}
	return false
}
