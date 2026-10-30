package insights

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SessionMeta is the per-session statistics record, cached on disk under
// usage-data/session-meta/<id>.json. Field order and JSON names are part of
// the cached format.
type SessionMeta struct {
	SessionID               string    `json:"session_id"`
	TranscriptMtime         *int64    `json:"transcript_mtime,omitempty"`
	ProjectPath             string    `json:"project_path"`
	StartTime               string    `json:"start_time"`
	DurationMinutes         float64   `json:"duration_minutes"`
	UserMessageCount        int       `json:"user_message_count"`
	AssistantMessageCount   int       `json:"assistant_message_count"`
	ToolCounts              *counts   `json:"tool_counts"`
	Languages               *counts   `json:"languages"`
	GitCommits              int       `json:"git_commits"`
	GitPushes               int       `json:"git_pushes"`
	InputTokens             float64   `json:"input_tokens"`
	OutputTokens            float64   `json:"output_tokens"`
	FirstPrompt             string    `json:"first_prompt"`
	Summary                 string    `json:"summary,omitempty"`
	UserInterruptions       int       `json:"user_interruptions"`
	UserResponseTimes       []float64 `json:"user_response_times"`
	ToolErrors              int       `json:"tool_errors"`
	ToolErrorCategories     *counts   `json:"tool_error_categories"`
	UsesTaskAgent           bool      `json:"uses_task_agent"`
	UsesMCP                 bool      `json:"uses_mcp"`
	UsesWebSearch           bool      `json:"uses_web_search"`
	UsesWebFetch            bool      `json:"uses_web_fetch"`
	LinesAdded              int       `json:"lines_added"`
	LinesRemoved            int       `json:"lines_removed"`
	FilesModified           int       `json:"files_modified"`
	MessageHours            []int     `json:"message_hours"`
	UserMessageTimestamps   []string  `json:"user_message_timestamps"`
	PermissionMode          string    `json:"permission_mode,omitempty"`
	BashWouldPromptCount    *int      `json:"bash_would_prompt_count,omitempty"`
	FileEditToolCount       *int      `json:"file_edit_tool_count,omitempty"`
	DestructiveCommandCount *int      `json:"destructive_command_count,omitempty"`
}

// normalize fills defaults for optional fields of a cached record.
func (m *SessionMeta) normalize() {
	if m.ToolCounts == nil {
		m.ToolCounts = newCounts()
	}
	if m.Languages == nil {
		m.Languages = newCounts()
	}
	if m.ToolErrorCategories == nil {
		m.ToolErrorCategories = newCounts()
	}
	if m.UserResponseTimes == nil {
		m.UserResponseTimes = []float64{}
	}
	if m.MessageHours == nil {
		m.MessageHours = []int{}
	}
	if m.UserMessageTimestamps == nil {
		m.UserMessageTimestamps = []string{}
	}
}

// extToLanguage maps file extensions to language names.
var extToLanguage = map[string]string{
	".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript", ".jsx": "JavaScript",
	".py": "Python", ".rb": "Ruby", ".go": "Go", ".rs": "Rust", ".java": "Java",
	".c": "C", ".h": "C", ".cpp": "C++", ".cc": "C++", ".cxx": "C++", ".hpp": "C++",
	".hh": "C++", ".hxx": "C++", ".ipp": "C++", ".md": "Markdown", ".json": "JSON",
	".yaml": "YAML", ".yml": "YAML", ".sh": "Shell", ".css": "CSS", ".html": "HTML",
}

// Tool names the analysis keys on.
const (
	toolAgent        = "Agent"
	toolTask         = "Task"
	toolEdit         = "Edit"
	toolWrite        = "Write"
	toolNotebookEdit = "NotebookEdit"
	toolBash         = "Bash"
)

// toolErrorRules classify a failed tool result's text.
var toolErrorRules = []struct {
	needles  []string
	category string
}{
	{[]string{"exit code"}, "Command Failed"},
	{[]string{"rejected", "doesn't want"}, "User Rejected"},
	{[]string{"string to replace not found", "no changes"}, "Edit Failed"},
	{[]string{"modified since read"}, "File Changed"},
	{[]string{"exceeds maximum", "too large"}, "File Too Large"},
	{[]string{"file not found", "does not exist"}, "File Not Found"},
}

// toolErrorCategory classifies a tool error (non-string content → "Other").
func toolErrorCategory(content *string) string {
	if content == nil {
		return "Other"
	}
	s := strings.ToLower(*content)
	for _, r := range toolErrorRules {
		for _, n := range r.needles {
			if strings.Contains(s, n) {
				return r.category
			}
		}
	}
	return "Other"
}

// inputString returns in[k] when it is a string, else "".
func inputString(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

// hasUserText reports whether a user message carries typed text.
func hasUserText(m *message) bool {
	if m.contentString != nil {
		return strings.TrimSpace(*m.contentString) != ""
	}
	for _, b := range m.blocks {
		if b.Type == "text" && b.Text != nil {
			return true
		}
	}
	return false
}

const interruptedMarker = "[Request interrupted by user"

// sessionStats are the per-session statistics.
type sessionStats struct {
	toolCounts, languages, toolErrorCategories *counts
	gitCommits, gitPushes                      int
	inputTokens, outputTokens                  float64
	userInterruptions, toolErrors              int
	userResponseTimes                          []float64
	usesTaskAgent, usesMCP                     bool
	usesWebSearch, usesWebFetch                bool
	linesAdded, linesRemoved                   int
	filesModified                              map[string]bool
	messageHours                               []int
	userMessageTimestamps                      []string
	bashWouldPrompt, fileEditTools             int
	destructiveCommands                        int
}

// computeStats walks a log's messages and tallies tool use, languages, git
// activity, response times, errors and permission-prompt estimates.
func (g *generator) computeStats(log *sessionLog) sessionStats {
	s := sessionStats{
		toolCounts: newCounts(), languages: newCounts(), toolErrorCategories: newCounts(),
		filesModified: map[string]bool{}, userResponseTimes: []float64{},
		messageHours: []int{}, userMessageTimestamps: []string{},
	}
	lastAssistantTS := ""
	for _, v := range log.messages {
		ts := v.Timestamp
		if v.Type == "assistant" && v.Message != nil {
			if ts != "" {
				lastAssistantTS = ts
			}
			if u := v.Message.Usage; u != nil {
				s.inputTokens += u.InputTokens
				s.outputTokens += u.OutputTokens
			}
			for _, b := range v.Message.blocks {
				if b.Type != "tool_use" || b.Name == nil {
					continue
				}
				name := *b.Name
				s.toolCounts.add(name, 1)
				if name == toolAgent || name == toolTask {
					s.usesTaskAgent = true
				}
				if strings.HasPrefix(name, "mcp__") {
					s.usesMCP = true
				}
				if name == "WebSearch" {
					s.usesWebSearch = true
				}
				if name == "WebFetch" {
					s.usesWebFetch = true
				}
				in := b.inputObj
				if in == nil {
					continue
				}
				if fp := inputString(in, "file_path"); fp != "" {
					if lang, ok := extToLanguage[strings.ToLower(filepath.Ext(fp))]; ok {
						s.languages.add(lang, 1)
					}
					if name == toolEdit || name == toolWrite {
						s.filesModified[fp] = true
					}
				}
				if name == toolEdit || name == toolWrite || name == toolNotebookEdit {
					s.fileEditTools++
				}
				if name == toolEdit {
					add, rem := diffLineCounts(inputString(in, "old_string"), inputString(in, "new_string"))
					s.linesAdded += add
					s.linesRemoved += rem
				}
				if name == toolWrite {
					if c := inputString(in, "content"); c != "" {
						s.linesAdded += strings.Count(c, "\n") + 1
					}
				}
				cmd := inputString(in, "command")
				if strings.Contains(cmd, "git commit") {
					s.gitCommits++
				}
				if strings.Contains(cmd, "git push") {
					s.gitPushes++
				}
				if name == toolBash && cmd != "" {
					if !g.bashAutoAllowed(cmd) {
						s.bashWouldPrompt++
						if destructiveCategory(cmd) != "" {
							s.destructiveCommands++
						}
					}
				}
			}
		}
		if v.Type == "user" && v.Message != nil {
			m := v.Message
			if hasUserText(m) {
				if ts != "" {
					if t := parseJSDate(ts); !t.IsZero() {
						s.messageHours = append(s.messageHours, t.In(g.loc).Hour())
						s.userMessageTimestamps = append(s.userMessageTimestamps, ts)
					}
				}
				if lastAssistantTS != "" && ts != "" {
					a, u := parseJSDate(lastAssistantTS), parseJSDate(ts)
					if !a.IsZero() && !u.IsZero() {
						d := float64(u.Sub(a).Milliseconds()) / 1000
						if d > 2 && d < 3600 {
							s.userResponseTimes = append(s.userResponseTimes, d)
						}
					}
				}
			}
			for _, b := range m.blocks {
				if b.Type == "tool_result" && len(b.Content) > 0 && b.IsError {
					s.toolErrors++
					s.toolErrorCategories.add(toolErrorCategory(b.contentSt), 1)
				}
			}
			if m.contentString != nil {
				if strings.Contains(*m.contentString, interruptedMarker) {
					s.userInterruptions++
				}
			} else {
				for _, b := range m.blocks {
					if b.Type == "text" && b.Text != nil && strings.Contains(*b.Text, interruptedMarker) {
						s.userInterruptions++
						break
					}
				}
			}
		}
	}
	return s
}

// buildSessionMeta computes a log's SessionMeta.
func (g *generator) buildSessionMeta(log *sessionLog) *SessionMeta {
	s := g.computeStats(log)
	id := log.sessionID
	if id == "" {
		id = "unknown"
	}
	userCount, assistantCount := 0, 0
	for _, e := range log.messages {
		if e.Type == "assistant" {
			assistantCount++
		}
		if e.Type == "user" && e.Message != nil && hasUserText(e.Message) {
			userCount++
		}
	}
	bash, edits, destr := s.bashWouldPrompt, s.fileEditTools, s.destructiveCommands
	return &SessionMeta{
		SessionID:               id,
		ProjectPath:             log.projectPath,
		StartTime:               jsISO(log.created),
		DurationMinutes:         jsRound(float64(log.modified.Sub(log.created).Milliseconds()) / 1000 / 60),
		UserMessageCount:        userCount,
		AssistantMessageCount:   assistantCount,
		ToolCounts:              s.toolCounts,
		Languages:               s.languages,
		GitCommits:              s.gitCommits,
		GitPushes:               s.gitPushes,
		InputTokens:             s.inputTokens,
		OutputTokens:            s.outputTokens,
		FirstPrompt:             log.firstPrompt,
		Summary:                 log.summary,
		UserInterruptions:       s.userInterruptions,
		UserResponseTimes:       s.userResponseTimes,
		ToolErrors:              s.toolErrors,
		ToolErrorCategories:     s.toolErrorCategories,
		UsesTaskAgent:           s.usesTaskAgent,
		UsesMCP:                 s.usesMCP,
		UsesWebSearch:           s.usesWebSearch,
		UsesWebFetch:            s.usesWebFetch,
		LinesAdded:              s.linesAdded,
		LinesRemoved:            s.linesRemoved,
		FilesModified:           len(s.filesModified),
		MessageHours:            s.messageHours,
		UserMessageTimestamps:   s.userMessageTimestamps,
		PermissionMode:          log.permissionMode,
		BashWouldPromptCount:    &bash,
		FileEditToolCount:       &edits,
		DestructiveCommandCount: &destr,
	}
}

// jsISO is Date#toISOString.
func jsISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// betterMeta reports whether a should replace b when deduplicating by
// session id (more user messages wins, then longer duration).
func betterMeta(a, b *SessionMeta) bool {
	if b == nil {
		return true
	}
	if a.UserMessageCount != b.UserMessageCount {
		return a.UserMessageCount > b.UserMessageCount
	}
	return a.DurationMinutes > b.DurationMinutes
}

func (m *SessionMeta) marshal() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	v, err := parseJSValue(data)
	if err != nil {
		return nil, err
	}
	return []byte(stringifyJS(v, "  ")), nil
}

// --- line diff (jsdiff diffLines) ---------------------------------------

// diffLineTokens splits text into jsdiff line tokens: each line keeps its
// trailing newline ("\n" or "\r\n"); a trailing empty piece is dropped.
func diffLineTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// diffLineCounts returns the lines added and removed by a minimal line diff
// of old → new (the same totals as jsdiff's diffLines change objects).
// Computed with Myers' O(ND) algorithm: D = adds + removes and
// removes - adds = N - M.
func diffLineCounts(oldS, newS string) (added, removed int) {
	a, b := diffLineTokens(oldS), diffLineTokens(newS)
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return m, n
	}
	maxD := n + m
	v := make([]int, 2*maxD+2)
	off := maxD + 1
	d := maxD
search:
	for dd := 0; dd <= maxD; dd++ {
		for k := -dd; k <= dd; k += 2 {
			var x int
			if k == -dd || (k != dd && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				d = dd
				break search
			}
		}
	}
	removed = (d + n - m) / 2
	added = d - removed
	return added, removed
}

// --- bash permission estimate -------------------------------------------

// destructivePatterns are the destructive-command warnings, in order.
// git_clean_force uses a JS negative
// lookahead, handled in code below.
var destructivePatterns = []struct {
	re       *regexp.Regexp
	category string
}{
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git_reset_hard"},
	{regexp.MustCompile(`\bgit\s+push\b[^;&|\n]*[ \t](--force|--force-with-lease|-f)\b`), "git_force_push"},
	{nil, "git_clean_force"},
	{regexp.MustCompile(`\bgit\s+checkout\s+(--\s+)?\.[ \t]*($|[;&|\n])`), "git_checkout_dot"},
	{regexp.MustCompile(`\bgit\s+restore\s+(--\s+)?\.[ \t]*($|[;&|\n])`), "git_restore_dot"},
	{regexp.MustCompile(`\bgit\s+stash[ \t]+(drop|clear)\b`), "git_stash_drop"},
	{regexp.MustCompile(`\bgit\s+branch\s+(-D[ \t]|--delete\s+--force|--force\s+--delete)\b`), "git_branch_force_delete"},
	{regexp.MustCompile(`\bgit\s+(commit|push|merge)\b[^;&|\n]*--no-verify\b`), "git_no_verify"},
	{regexp.MustCompile(`\bgit\s+commit\b[^;&|\n]*--amend\b`), "git_commit_amend"},
	{regexp.MustCompile(`(^|[;&|\n][ \t]*)rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f|(^|[;&|\n][ \t]*)rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR]`), "rm_recursive_force"},
	{regexp.MustCompile(`(^|[;&|\n][ \t]*)rm\s+-[a-zA-Z]*[rR]`), "rm_recursive"},
	{regexp.MustCompile(`(^|[;&|\n][ \t]*)rm\s+-[a-zA-Z]*f`), "rm_force"},
	{regexp.MustCompile(`(?i)\b(DROP|TRUNCATE)\s+(TABLE|DATABASE|SCHEMA)\b`), "sql_drop_truncate"},
	{regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+\w+[ \t]*(;|"|'|\n|$)`), "sql_delete_from"},
	{regexp.MustCompile(`\bkubectl\s+delete\b`), "kubectl_delete"},
	{regexp.MustCompile(`\bterraform\s+destroy\b`), "terraform_destroy"},
}

var (
	gitCleanRe       = regexp.MustCompile(`\bgit\s+clean\b`)
	gitCleanDryRunRe = regexp.MustCompile(`-[a-zA-Z]*n|--dry-run`)
	gitCleanForceRe  = regexp.MustCompile(`-[a-zA-Z]*f`)
)

// destructiveCategory returns the first matching destructive-command category
// ("" when none). Commands are inspected up to 10000 bytes.
func destructiveCategory(cmd string) string {
	if len(cmd) > 10000 {
		cmd = cmd[:10000]
	}
	for _, p := range destructivePatterns {
		if p.re == nil {
			if gitCleanForce(cmd) {
				return p.category
			}
			continue
		}
		if p.re.MatchString(cmd) {
			return p.category
		}
	}
	return ""
}

// gitCleanForce emulates /\bgit\s+clean\b(?![^;&|\n]*(?:-[a-zA-Z]*n|--dry-run))[^;&|\n]*-[a-zA-Z]*f/.
func gitCleanForce(cmd string) bool {
	for _, loc := range gitCleanRe.FindAllStringIndex(cmd, -1) {
		rest := cmd[loc[1]:]
		if i := strings.IndexAny(rest, ";&|\n"); i >= 0 {
			rest = rest[:i]
		}
		if gitCleanDryRunRe.MatchString(rest) {
			continue
		}
		if gitCleanForceRe.MatchString(rest) {
			return true
		}
	}
	return false
}

// readOnlyCommands is the conservative built-in stand-in for an offline bash
// permission check that runs the full permission engine. A command
// counts as auto-allowed only if every pipeline segment starts with one of
// these read-only programs and it has no redirection or substitution. Callers
// with access to the real permission engine pass Options.BashAutoAllowed.
var readOnlyCommands = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "rg": true, "find": true, "echo": true, "which": true, "whoami": true,
	"date": true, "file": true, "stat": true, "du": true, "df": true, "tree": true,
	"sort": true, "uniq": true, "cut": true, "diff": true, "true": true, "env": true,
}

var readOnlyGitSubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
	"rev-parse": true, "remote": true, "blame": true, "ls-files": true,
}

var shellSplitRe = regexp.MustCompile(`&&|\|\||[;|\n]`)

// defaultBashAutoAllowed is the built-in offline estimate (see readOnlyCommands).
func defaultBashAutoAllowed(cmd string) bool {
	if strings.ContainsAny(cmd, "<>`") || strings.Contains(cmd, "$(") {
		return false
	}
	for _, seg := range shellSplitRe.Split(cmd, -1) {
		f := strings.Fields(seg)
		if len(f) == 0 {
			continue
		}
		if f[0] == "git" {
			if len(f) < 2 || !readOnlyGitSubcommands[f[1]] {
				return false
			}
			continue
		}
		if !readOnlyCommands[f[0]] {
			return false
		}
	}
	return true
}

// commandCacheLimit bounds the memo of permission estimates (commands longer
// than 10000 bytes are not memoized).
const commandCacheLimit = 5000

// bashAutoAllowed memoizes the permission estimate per command.
func (g *generator) bashAutoAllowed(cmd string) bool {
	g.cacheMu.Lock()
	if v, ok := g.cmdCache[cmd]; ok {
		g.cacheMu.Unlock()
		return v
	}
	g.cacheMu.Unlock()
	allowed := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		if g.opts.BashAutoAllowed != nil {
			return g.opts.BashAutoAllowed(cmd)
		}
		return defaultBashAutoAllowed(cmd)
	}()
	if len(cmd) <= 10000 {
		g.cacheMu.Lock()
		if len(g.cmdCache) >= commandCacheLimit {
			g.cmdCache = map[string]bool{}
		}
		g.cmdCache[cmd] = allowed
		g.cacheMu.Unlock()
	}
	return allowed
}
