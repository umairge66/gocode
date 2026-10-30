package toolsearch

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Tool is one searchable tool: its name, model-facing description and
// whether its definition is deferred.
type Tool struct {
	Name        string
	Description string
	Deferred    bool
}

// Query limits.
const (
	maxQueryChars   = 16384
	maxQueryWords   = 32
	maxSelectNames  = 64
	maxHintChars    = 4096
	DefaultMaxMatch = 5
)

var selectRe = regexp.MustCompile(`(?i)^select:(.+)$`)

// normalizeQuery caps the query: at most maxQueryChars characters, a
// keyword query at most maxQueryWords words, a select at most
// maxSelectNames names.
func normalizeQuery(q string) string {
	q = truncateChars(q, maxQueryChars)
	if !selectRe.MatchString(q) {
		return capWords(q)
	}
	m := selectRe.FindStringSubmatch(q)
	names := strings.Split(m[1], ",")
	if len(names) <= maxSelectNames {
		return q
	}
	return q[:len(q)-len(m[1])] + strings.Join(names[:maxSelectNames], ",")
}

func truncateChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func capWords(q string) string {
	words := strings.Fields(q)
	if len(words) <= maxQueryWords {
		return q
	}
	return strings.Join(words[:maxQueryWords], " ")
}

// Search runs a ToolSearch query over tools and returns the matched tool
// names, best first.
//
//   - "select:A,B" returns each named tool that exists (deferred or not; an
//     MCP tool may also be named by its bare tool name).
//   - Otherwise the query is a keyword search over the deferred tools: an
//     exact tool name returns that tool; an "mcp__server" prefix returns
//     that server's tools; else each word scores against the tool name's
//     parts, its search hint and its description, "+word" making a word
//     required. At most maxResults names are returned (5 when <= 0).
func Search(query string, tools []Tool, maxResults int) []string {
	if maxResults <= 0 {
		maxResults = DefaultMaxMatch
	}
	q := normalizeQuery(query)
	if m := selectRe.FindStringSubmatch(q); m != nil {
		return selectNames(m[1], tools)
	}
	return keywordSearch(q, tools, maxResults)
}

func selectNames(list string, tools []Tool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if t, ok := findTool(tools, name, true); ok {
			add(t.Name)
			continue
		}
		if t, ok := findTool(tools, name, false); ok {
			add(t.Name)
			continue
		}
		// A bare MCP tool name matches every server's tool of that name.
		for _, t := range tools {
			if _, tool, ok := splitMCP(t.Name); ok && (tool == name || normalizeName(tool) == name) {
				add(t.Name)
			}
		}
	}
	return out
}

// findTool finds a tool by exact name, among the deferred tools first.
func findTool(tools []Tool, name string, deferredOnly bool) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name && (!deferredOnly || t.Deferred) {
			return t, true
		}
	}
	return Tool{}, false
}

// normalizeName is how an MCP tool name is spelled in a tool name: anything
// outside [A-Za-z0-9_-] becomes "_".
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// splitMCP splits "mcp__server__tool".
func splitMCP(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := name[len("mcp__"):]
	i := strings.Index(rest, "__")
	if i < 0 {
		return rest, "", true
	}
	return rest[:i], rest[i+2:], true
}

// nameParts is a tool name split for matching: parts are the individual
// words, coarseParts the whole name (server and tool for MCP tools).
type nameParts struct {
	parts, coarse []string
	full          string
	mcp           bool
}

var (
	camelRe    = regexp.MustCompile(`([a-z])([A-Z])`)
	mcpSplitRe = regexp.MustCompile(`[\s_.]+`)
)

func splitName(name string) nameParts {
	if server, tool, ok := splitMCP(name); ok {
		var coarse []string
		for _, s := range []string{server, tool} {
			if s != "" {
				coarse = append(coarse, strings.ToLower(s))
			}
		}
		var parts []string
		for _, c := range coarse {
			for _, p := range mcpSplitRe.Split(c, -1) {
				if p != "" {
					parts = append(parts, p)
				}
			}
		}
		return nameParts{parts: parts, coarse: coarse, full: strings.Join(parts, " "), mcp: true}
	}
	spaced := camelRe.ReplaceAllString(name, "$1 $2")
	spaced = strings.ToLower(strings.ReplaceAll(spaced, "_", " "))
	parts := strings.Fields(spaced)
	return nameParts{parts: parts, coarse: []string{strings.ToLower(name)}, full: strings.Join(parts, " ")}
}

func containsExact(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsSub(list []string, s string) bool {
	for _, x := range list {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func keywordSearch(q string, tools []Tool, maxResults int) []string {
	query := strings.ToLower(strings.TrimSpace(capWords(q)))
	var deferred []Tool
	for _, t := range tools {
		if t.Deferred {
			deferred = append(deferred, t)
		}
	}
	// An exact tool name.
	for _, set := range [][]Tool{deferred, tools} {
		for _, t := range set {
			if strings.ToLower(t.Name) == query {
				return []string{t.Name}
			}
		}
	}
	// An MCP server (or tool name) prefix.
	if strings.HasPrefix(query, "mcp__") && len(query) > len("mcp__") {
		var out []string
		for _, t := range deferred {
			if strings.HasPrefix(strings.ToLower(t.Name), query) {
				out = append(out, t.Name)
				if len(out) == maxResults {
					break
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var required, optional []string
	words := strings.Fields(query)
	for _, w := range words {
		if strings.HasPrefix(w, "+") && len(w) > 1 {
			required = append(required, w[1:])
		} else {
			optional = append(optional, w)
		}
	}
	terms := words
	if len(required) > 0 {
		terms = append(append([]string(nil), required...), optional...)
	}
	res := map[string]*regexp.Regexp{}
	for _, t := range terms {
		if _, ok := res[t]; !ok {
			res[t] = regexp.MustCompile(`\b` + regexp.QuoteMeta(t) + `\b`)
		}
	}
	candidates := deferred
	if len(required) > 0 {
		candidates = nil
		for _, t := range deferred {
			np := splitName(t.Name)
			desc := strings.ToLower(t.Description)
			hint := strings.ToLower(truncateChars(Hint(t.Name), maxHintChars))
			ok := true
			for _, r := range required {
				re := res[r]
				if !(containsExact(np.parts, r) || containsSub(np.parts, r) || containsExact(np.coarse, r) ||
					containsSub(np.coarse, r) || re.MatchString(desc) || (hint != "" && re.MatchString(hint))) {
					ok = false
					break
				}
			}
			if ok {
				candidates = append(candidates, t)
			}
		}
	}
	type scored struct {
		name  string
		score int
	}
	var out []scored
	for _, t := range candidates {
		np := splitName(t.Name)
		desc := strings.ToLower(t.Description)
		hint := strings.ToLower(truncateChars(Hint(t.Name), maxHintChars))
		score := 0
		for _, w := range terms {
			re := res[w]
			switch {
			case containsExact(np.parts, w):
				score += pick(np.mcp, 12, 10)
			case containsSub(np.parts, w):
				score += pick(np.mcp, 6, 5)
			}
			switch {
			case containsExact(np.coarse, w):
				score += pick(np.mcp, 12, 10)
			case containsSub(np.coarse, w):
				score += pick(np.mcp, 4, 3)
			}
			if strings.Contains(np.full, w) && score == 0 {
				score += 3
			}
			if hint != "" && re.MatchString(hint) {
				score += 4
			}
			if re.MatchString(desc) {
				score += 2
			}
		}
		if score > 0 {
			out = append(out, scored{t.Name, score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > maxResults {
		out = out[:maxResults]
	}
	names := make([]string, len(out))
	for i, s := range out {
		names[i] = s.name
	}
	return names
}

func pick(mcp bool, a, b int) int {
	if mcp {
		return a
	}
	return b
}

// searchHints are short capability phrases for the deferrable built-ins,
// matched by keyword searches alongside the description.
var searchHints = map[string]string{
	"NotebookEdit":         "edit Jupyter notebook cells (.ipynb)",
	"TodoWrite":            "manage the session task checklist",
	"WebFetch":             "fetch and extract content from a URL",
	"WebSearch":            "search the web for current information",
	"SendMessage":          "send messages to agent teammates",
	"AmendAgent":           "redirect a running subagent with a refined instruction",
	"MultiEdit":            "apply several edits to one file at once",
	"ListAgents":           "list agents you can SendMessage to",
	"Workflow":             "orchestrate subagents with deterministic JavaScript workflow",
	"AskUserQuestion":      "prompt the user with a multiple-choice question",
	"Skill":                "invoke a slash-command skill",
	"Glob":                 "find files by name pattern or wildcard",
	"Grep":                 "search file contents with regex",
	"Read":                 "read files, images, PDFs, notebooks",
	"EnterPlanMode":        "switch to plan mode to design an approach before coding",
	"ExitPlanMode":         "present plan for approval and start coding (plan mode only)",
	"EnterWorktree":        "create an isolated git worktree and switch into it",
	"ExitWorktree":         "exit a worktree session and return to the original directory",
	"TaskStop":             "kill a running background task",
	"LSP":                  "code intelligence (definitions, references, symbols, hover)",
	"CronCreate":           "schedule a recurring or one-shot prompt",
	"CronDelete":           "cancel a scheduled cron job",
	"CronList":             "list active cron jobs",
	"TaskCreate":           "create a task in the task list",
	"TaskGet":              "retrieve a task by ID",
	"TaskUpdate":           "update a task",
	"TaskList":             "list all tasks",
	"PushNotification":     "send a notification to the user via terminal and optionally mobile",
	"navigate":             "browser: navigate a Chrome tab to a URL",
	"read_page":            "browser: read the text of a Chrome tab",
	"get_page_text":        "browser: extract the readable page text of a Chrome tab",
	"tabs_context":         "browser: list the Chrome tabs",
	"tabs_create":          "browser: open a new Chrome tab",
	"tabs_close":           "browser: close a Chrome tab",
	"screenshot":           "browser: screenshot a Chrome tab",
	"computer":             "browser: click, type, scroll and press keys in a Chrome tab",
	"find":                 "browser: find elements on a page by description",
	"form_input":           "browser: set a form field value",
	"browser_batch":        "browser: run several browser actions in one call",
	"ListMcpResourcesTool": "list resources offered by connected MCP servers",
	"ReadMcpResourceTool":  "read a resource from a connected MCP server",
}

// Hint returns a tool's search hint ("" when it has none).
func Hint(name string) string { return searchHints[name] }
