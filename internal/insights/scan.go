package insights

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sessionFile is one transcript found on disk.
type sessionFile struct {
	sessionID string
	path      string
	mtime     int64 // unix milliseconds, like fs.Stats#mtimeMs
	size      int64
}

// sessionIDPattern is the session-id (UUID) validator.
var sessionIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// listSessionFiles enumerates <configHome>/projects/<dir>/<uuid>.jsonl,
// newest mtime first. Only the local filesystem is scanned; there is no
// hosted storage backend.
func listSessionFiles(configHome string) []sessionFile {
	root := filepath.Join(configHome, "projects")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []sessionFile
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !sessionIDPattern.MatchString(id) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, sessionFile{
				sessionID: id,
				path:      filepath.Join(dir, name),
				mtime:     info.ModTime().UnixMilli(),
				size:      info.Size(),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].mtime > out[j].mtime })
	return out
}

// block is one content block of a transcript message (the subset of fields
// the analysis reads).
type block struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	Name      *string         `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	inputObj  map[string]any
	contentSt *string
}

// message is a transcript line's `message` ({role, content, usage?}).
// content may be a string (older format) or a block array (what sessionstore
// writes).
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		InputTokens  float64 `json:"input_tokens"`
		OutputTokens float64 `json:"output_tokens"`
	} `json:"usage"`

	contentString *string
	blocks        []block
}

// logEntry is one transcript line. Field names follow the shared
// Claude-Code-style schema written by internal/sessionstore.
type logEntry struct {
	Type                      string   `json:"type"`
	UUID                      string   `json:"uuid"`
	ParentUUID                *string  `json:"parentUuid"`
	Timestamp                 string   `json:"timestamp"`
	SessionID                 string   `json:"sessionId"`
	Cwd                       string   `json:"cwd"`
	IsMeta                    bool     `json:"isMeta"`
	IsSidechain               bool     `json:"isSidechain"`
	IsCompactSummary          bool     `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool     `json:"isVisibleInTranscriptOnly"`
	Message                   *message `json:"message"`
	Summary                   string   `json:"summary"`
	LeafUUID                  string   `json:"leafUuid"`
	PermissionMode            string   `json:"permissionMode"`
}

// sessionLog is one reconstructed conversation.
type sessionLog struct {
	sessionID      string
	messages       []*logEntry
	created        time.Time
	modified       time.Time
	firstPrompt    string
	summary        string
	projectPath    string
	permissionMode string
}

// decode prepares a message's content for analysis.
func (m *message) decode() {
	if m == nil || len(m.Content) == 0 {
		return
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		m.contentString = &s
		return
	}
	var raw []json.RawMessage
	if json.Unmarshal(m.Content, &raw) != nil {
		return
	}
	for _, r := range raw {
		var b block
		if json.Unmarshal(r, &b) != nil {
			continue // non-object element: dropped
		}
		if len(b.Input) > 0 {
			var obj map[string]any
			if json.Unmarshal(b.Input, &obj) == nil {
				b.inputObj = obj
			}
		}
		if len(b.Content) > 0 {
			var cs string
			if json.Unmarshal(b.Content, &cs) == nil {
				b.contentSt = &cs
			}
		}
		m.blocks = append(m.blocks, b)
	}
}

// readSessionLogs parses one transcript into its conversation log.
//
// sessionstore writes each session as one linear chain (rather than a
// uuid/parentUuid tree with several leaves), so the file's user/assistant lines in order form the single log; created is the
// first such line's timestamp and modified the last one's. `summary` lines are
// matched by leafUuid (or taken as-is when they carry none, as sessionstore
// writes them) and `permission-mode` lines supply the session's last mode.
func readSessionLogs(path string) ([]*sessionLog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []*logEntry
	summaries := map[string]string{}
	var bareSummary, permMode, sessionID string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e logEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "summary":
			if e.LeafUUID != "" {
				summaries[e.LeafUUID] = e.Summary
			} else if e.Summary != "" {
				bareSummary = e.Summary
			}
			continue
		case "permission-mode":
			if e.PermissionMode != "" {
				permMode = e.PermissionMode
			}
			continue
		case "user", "assistant":
		default:
			continue
		}
		if e.Message == nil || e.IsSidechain {
			continue
		}
		e.Message.decode()
		if sessionID == "" && e.SessionID != "" {
			sessionID = e.SessionID
		}
		entries = append(entries, &e)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	first, leaf := entries[0], entries[len(entries)-1]
	summary := summaries[leaf.UUID]
	if summary == "" {
		summary = bareSummary
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return []*sessionLog{{
		sessionID:      sessionID,
		messages:       entries,
		created:        parseJSDate(first.Timestamp),
		modified:       parseJSDate(leaf.Timestamp),
		firstPrompt:    firstPrompt(entries),
		summary:        summary,
		projectPath:    first.Cwd,
		permissionMode: permMode,
	}}, nil
}

// invalidTime marks an unparseable timestamp (JS `new Date(x)` → NaN).
var invalidTime = time.Time{}

func parseJSDate(s string) time.Time {
	if s == "" {
		return invalidTime
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return invalidTime
}

var (
	commandNameRe = regexp.MustCompile(`<command-name>([\s\S]*?)</command-name>`)
	commandArgsRe = regexp.MustCompile(`<command-args>([\s\S]*?)</command-args>`)
	bashInputRe   = regexp.MustCompile(`<bash-input>([\s\S]*?)</bash-input>`)
	// syntheticPromptRe matches synthetic (tag-wrapped or interrupt) prompts.
	syntheticPromptRe = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)
)

// firstPrompt is the session's first real user prompt, newlines folded, capped
// at 200 UTF-16 units plus an ellipsis ("No prompt" when none). Built-in slash
// commands should be skipped, but the command registry is not visible here, so
// a command without arguments is skipped and one with arguments is rendered
// "name args" — an approximation of that rule.
func firstPrompt(entries []*logEntry) string {
	found := ""
	ok := false
outer:
	for _, e := range entries {
		if e.Type != "user" || e.IsMeta || e.IsCompactSummary || e.Message == nil {
			continue
		}
		var texts []string
		if e.Message.contentString != nil {
			texts = append(texts, *e.Message.contentString)
		}
		for _, b := range e.Message.blocks {
			if b.Type == "text" && b.Text != nil && *b.Text != "" {
				texts = append(texts, *b.Text)
			}
		}
		for _, t := range texts {
			if t == "" {
				continue
			}
			if m := commandNameRe.FindStringSubmatch(t); m != nil {
				args := ""
				if a := commandArgsRe.FindStringSubmatch(t); a != nil {
					args = strings.TrimSpace(a[1])
				}
				if args == "" {
					continue
				}
				found, ok = m[1]+" "+args, true
				break outer
			}
			if m := bashInputRe.FindStringSubmatch(t); m != nil && m[1] != "" {
				found, ok = "! "+m[1], true
				break outer
			}
			if syntheticPromptRe.MatchString(t) {
				continue
			}
			found, ok = t, true
			break outer
		}
	}
	if !ok {
		return "No prompt"
	}
	r := strings.TrimSpace(strings.ReplaceAll(found, "\n", " "))
	if utf16Len(r) > 200 {
		r = strings.TrimSpace(truncateUTF16(r, 200)) + "…"
	}
	return r
}

// strippedTagsRe matches the tag names stripped from prompts.
var strippedTagsRe = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, t := range []string{"bash-input", "bash-stdout", "bash-stderr", "bash-exit-code", "local-command-stdout", "local-command-stderr", "local-command-caveat"} {
		// JS: <(tag)>[\s\S]*?(?:</\1>|$) — RE2 has no backreference, so one
		// regexp per tag.
		out = append(out, regexp.MustCompile(`<`+t+`>[\s\S]*?(?:</`+t+`>|$)`))
	}
	return out
}()

// stripTags removes system-injected tag blocks from user text.
func stripTags(s string) string {
	// Tags never nest in practice, so per-tag passes are equivalent to one
	// pass alternating all tags (leftmost match first).
	for _, re := range strippedTagsRe {
		s = re.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

// userTexts is the non-empty, tag-stripped text of a user message.
func userTexts(m *message) []string {
	var raw []string
	if m.contentString != nil {
		raw = []string{*m.contentString}
	} else {
		for _, b := range m.blocks {
			if b.Type == "text" && b.Text != nil {
				raw = append(raw, *b.Text)
			}
		}
	}
	var out []string
	for _, t := range raw {
		if s := stripTags(t); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// messageText joins a message's text blocks.
func messageText(m *message) string {
	if m.contentString != nil {
		return *m.contentString
	}
	var parts []string
	for _, b := range m.blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// isHumanUserMessage reports whether a user line is a real human prompt
// (not a tool result, meta line, compact summary or command/bash output).
func isHumanUserMessage(e *logEntry) bool {
	if e.Type != "user" || e.Message == nil {
		return false
	}
	if len(e.Message.blocks) > 0 && e.Message.blocks[0].Type == "tool_result" {
		return false
	}
	if e.IsMeta || e.IsCompactSummary || e.IsVisibleInTranscriptOnly {
		return false
	}
	n := strings.TrimSpace(messageText(e.Message))
	for _, tag := range []string{"local-command-stdout", "local-command-stderr", "bash-stdout", "bash-stderr", "task-notification", "tick"} {
		if strings.Contains(n, "<"+tag+">") {
			return false
		}
	}
	return true
}

// transcriptLines renders a log as "[User]: …" / "[Assistant]: …" /
// "[Tool: name]" lines. Queued-command attachments are not rendered; the
// transcripts carry none.
func transcriptLines(entries []*logEntry, userChars, assistantChars int) []string {
	var out []string
	for _, e := range entries {
		switch {
		case e.Type == "user" && e.Message != nil:
			if e.IsCompactSummary {
				for _, t := range userTexts(e.Message) {
					out = append(out, "[Summary of earlier conversation]: "+truncateUTF16(t, userChars))
				}
				continue
			}
			if !isHumanUserMessage(e) {
				continue
			}
			for _, t := range userTexts(e.Message) {
				out = append(out, "[User]: "+truncateUTF16(t, userChars))
			}
		case e.Type == "assistant" && e.Message != nil:
			if e.Message.contentString != nil {
				out = append(out, "[Assistant]: "+truncateUTF16(*e.Message.contentString, assistantChars))
				continue
			}
			for _, b := range e.Message.blocks {
				if b.Type == "text" && b.Text != nil {
					out = append(out, "[Assistant]: "+truncateUTF16(*b.Text, assistantChars))
				} else if b.Type == "tool_use" && b.Name != nil {
					out = append(out, "[Tool: "+*b.Name+"]")
				}
			}
		}
	}
	return out
}
