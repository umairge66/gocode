package automode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/agent"
)

// The classifier's transcript is one JSON object per line:
//
//	{"user":"<a user turn's text>"}
//	{"assistant":"<the agent's prose the user then replied to>"}
//	{"<ToolName>":<the tool's classifier input>}
//
// Tool results are not included (the classifier judges actions, and a
// result's content is untrusted), except the user's answers to
// AskUserQuestion, which are the user speaking. Assistant prose is included
// only directly before the user turn that replied to it — the referent of a
// terse "yes". The action under review is always the final line.

// TranscriptLimits bounds the transcript sent with each classified call (the
// action under review is never trimmed). The classifier judges one action
// against the user's intent, so what it needs is every user turn (the User
// Intent Rule reads them), the agent's recent activity, and the prose a terse
// reply answers — not the whole session:
//
//   - every user turn is kept, each cut to MaxUserTurnChars (head and tail);
//   - the last RecentToolCalls tool calls are kept (string inputs cut to
//     MaxToolEntryChars), plus any older Write / Edit / NotebookEdit of a
//     file the action names (WRITTEN FILE EXECUTION needs the written body);
//   - assistant prose is kept inside the recent window and before the last
//     user turn, each cut to MaxProseChars;
//   - everything else becomes one "[N earlier entries omitted]" line per
//     dropped run;
//   - the result is held under MaxChars by dropping the oldest kept entries
//     (tool calls and prose first, then user turns other than the latest).
//
// The transcript is trimmed rather than sent whole to cut the per-call token
// cost.
type TranscriptLimits struct {
	MaxChars          int
	RecentToolCalls   int
	MaxUserTurnChars  int
	MaxProseChars     int
	MaxToolEntryChars int
}

// DefaultTranscriptLimits keep a typical transcript near 4k tokens, with a
// hard cap of 16k characters.
var DefaultTranscriptLimits = TranscriptLimits{
	MaxChars:          16000,
	RecentToolCalls:   12,
	MaxUserTurnChars:  2000,
	MaxProseChars:     1000,
	MaxToolEntryChars: 2000,
}

func (l TranscriptLimits) withDefaults() TranscriptLimits {
	d := DefaultTranscriptLimits
	if l.MaxChars > 0 {
		d.MaxChars = l.MaxChars
	}
	if l.RecentToolCalls > 0 {
		d.RecentToolCalls = l.RecentToolCalls
	}
	if l.MaxUserTurnChars > 0 {
		d.MaxUserTurnChars = l.MaxUserTurnChars
	}
	if l.MaxProseChars > 0 {
		d.MaxProseChars = l.MaxProseChars
	}
	if l.MaxToolEntryChars > 0 {
		d.MaxToolEntryChars = l.MaxToolEntryChars
	}
	return d
}

// omittedLine marks a run of n entries left out of the transcript.
func omittedLine(n int) string {
	return "[" + strconv.Itoa(n) + " earlier entries omitted]\n"
}

// clip keeps the head and tail of s within max bytes (rune-safe), noting how
// much was cut.
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := len(s) - max
	note := " …[" + strconv.Itoa(cut) + " chars truncated]… "
	keep := max - len(note)
	if keep < 2 {
		keep = 2
	}
	head, tail := keep*3/4, keep-keep*3/4
	h := s[:head]
	for len(h) > 0 && !utf8.ValidString(h) {
		h = h[:len(h)-1]
	}
	t := s[len(s)-tail:]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[1:]
	}
	return h + note + t
}

// Action is the tool call under review.
type Action struct {
	// Tool is the model-facing tool name.
	Tool string
	// Input is the raw model JSON input.
	Input json.RawMessage
	// ID is the tool_use id (used to stop the transcript at this call).
	ID string
	// Hints are the annotations an MCP tool declares for itself (nil for a
	// built-in tool, or an MCP tool that declares neither hint).
	Hints *ToolHints
}

// ToolHints are an MCP tool's self-declared annotations. They reach the
// classifier as a {"meta":{"mcpAnnotations":…}} line directly above the
// action; the server's claim is unverified, so the prompt lets
// DestructiveHint tighten the judgement and never lets ReadOnlyHint relax it.
type ToolHints struct {
	ReadOnly    bool
	Destructive bool
}

// metaLine renders the action's harness meta line, or "" when there is none.
func (a Action) metaLine() string {
	if a.Hints == nil || (!a.Hints.ReadOnly && !a.Hints.Destructive) {
		return ""
	}
	var ann orderedFields
	if a.Hints.Destructive {
		ann = append(ann, field{"destructiveHint", true})
	}
	if a.Hints.ReadOnly {
		ann = append(ann, field{"readOnlyHint", true})
	}
	return encodeLine("meta", orderedFields{{"mcpAnnotations", ann}})
}

// reservedKeys are entry keys a tool name must not impersonate.
var reservedKeys = map[string]bool{"user": true, "assistant": true, "outcome": true, "id": true, "meta": true}

// entryKey returns the key for a tool entry: a tool name equal (case-insensitively) to an
// entry key is bracketed so it cannot pose as a user turn.
func entryKey(name string) string {
	if reservedKeys[strings.ToLower(strings.TrimSpace(name))] {
		return "[" + name + "]"
	}
	return name
}

// tagRe matches the opening of a transcript-structure tag inside content.
var tagRe = regexp.MustCompile(`(?i)[<\x{FF1C}]([\s/]*(?:transcript|forwarded_user_turns|forwarded_turn)\b)`)

// neutralizeTags defuses structure tags: a "<transcript" (or "</transcript",
// "<forwarded_turn" …) inside content becomes "[transcript…", so content
// cannot close the transcript early or forge a section.
func neutralizeTags(s string) string {
	return tagRe.ReplaceAllString(s, "[$1")
}

// encodeLine renders one transcript entry as compact JSON (key order kept,
// no HTML escaping) with U+2028/2029/0085 escaped and tags
// neutralized, plus the line break.
func encodeLine(key string, value any) string {
	var buf bytes.Buffer
	buf.WriteString("{")
	kb, _ := marshalNoHTML(key)
	buf.Write(kb)
	buf.WriteString(":")
	vb, err := marshalNoHTML(value)
	if err != nil {
		vb = []byte(`""`)
	}
	buf.Write(vb)
	buf.WriteString("}")
	s := strings.NewReplacer(" ", ` `, " ", ` `, "\u0085", `\u0085`).Replace(buf.String())
	return neutralizeTags(s) + "\n"
}

func marshalNoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// orderedFields is a JSON object that keeps its key order.
type orderedFields []field

type field struct {
	k string
	v any
}

func (o orderedFields) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	for i, f := range o {
		if i > 0 {
			buf.WriteString(",")
		}
		kb, _ := marshalNoHTML(f.k)
		buf.Write(kb)
		buf.WriteString(":")
		vb, err := marshalNoHTML(f.v)
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// ClassifierInput returns the built-in tools' classifier input: the
// part of a call's input the classifier judges. Bash is its command, Write is
// "<path>: <content>", Edit shows what it removes and adds, a read or search
// is its path or pattern; any other tool (MCP, WebFetch, Agent, …) is its full
// input. An input that does not parse is passed as its raw text.
func ClassifierInput(tool string, input json.RawMessage) any {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil || m == nil {
		if len(bytes.TrimSpace(input)) == 0 {
			return map[string]any{}
		}
		return string(input)
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	switch tool {
	case "Bash", "PowerShell":
		if c, ok := m["command"].(string); ok {
			return c
		}
	case "Write":
		return str("file_path") + ": " + str("content")
	case "Edit", "MultiEdit":
		if _, ok := m["edits"]; ok {
			return orderedFields{{"file_path", str("file_path")}, {"edits", m["edits"]}}
		}
		o := orderedFields{{"file_path", str("file_path")}, {"adds", str("new_string")}, {"removes", str("old_string")}}
		if b, _ := m["replace_all"].(bool); b {
			o = append(o, field{"replaceAll", true})
		}
		return o
	case "NotebookEdit":
		mode := str("edit_mode")
		if mode == "" {
			mode = "replace"
		}
		return str("notebook_path") + " " + mode + ": " + str("new_source")
	case "Read":
		return str("file_path")
	case "Glob", "Grep":
		if p := str("path"); p != "" {
			return str("pattern") + " in " + p
		}
		return str("pattern")
	case "TodoWrite":
		if todos, ok := m["todos"].([]any); ok {
			return strconv.Itoa(len(todos)) + " items"
		}
	}
	return json.RawMessage(input)
}

// entry kinds.
const (
	entryUser = iota
	entryAssistant
	entryTool
)

type entry struct {
	kind int
	line string
	// path is the file a Write / Edit / NotebookEdit call touched.
	path string
}

// built is a serialized transcript plus what the verdict cache keys on.
type built struct {
	transcript string
	action     string
	// userTurns fingerprints the user's turns: it changes whenever the user
	// sends a new message.
	userTurns string
}

// BuildTranscript serializes msgs into transcript lines (trimmed to
// DefaultTranscriptLimits) and returns the action as the final line. Entries
// after the action's own tool_use (later calls of the same assistant turn)
// are not included.
func BuildTranscript(msgs []agent.Message, act Action) (transcript string, action string) {
	b := buildTranscript(msgs, act, DefaultTranscriptLimits)
	return b.transcript, b.action
}

// BuildTranscriptLimits is BuildTranscript with explicit limits (zero fields
// take the defaults).
func BuildTranscriptLimits(msgs []agent.Message, act Action, lim TranscriptLimits) (transcript string, action string) {
	b := buildTranscript(msgs, act, lim)
	return b.transcript, b.action
}

func writtenPath(name string, input json.RawMessage) string {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
	default:
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func buildTranscript(msgs []agent.Message, act Action, lim TranscriptLimits) built {
	lim = lim.withDefaults()
	var entries []entry
	askIDs := map[string]bool{}
	pendingProse := ""
	stop := false
	userHash := sha256.New()
	userCount := 0
	for _, m := range msgs {
		if stop {
			break
		}
		switch m.Role {
		case "assistant":
			var prose []string
			for _, b := range m.Content {
				switch b.Type {
				case agent.BlockText:
					if t := strings.TrimSpace(b.Text); t != "" {
						prose = append(prose, b.Text)
					}
				case agent.BlockToolUse:
					if act.ID != "" && b.ID == act.ID {
						stop = true
					}
					if stop {
						break
					}
					if b.Name == "AskUserQuestion" {
						askIDs[b.ID] = true
					}
					in := ClassifierInput(b.Name, b.Input)
					if s, ok := in.(string); ok {
						in = clip(s, lim.MaxToolEntryChars)
					}
					entries = append(entries, entry{kind: entryTool, line: encodeLine(entryKey(b.Name), in), path: writtenPath(b.Name, b.Input)})
				}
				if stop {
					break
				}
			}
			if len(prose) > 0 {
				pendingProse = strings.Join(prose, "\n")
			}
		case "user":
			var texts []string
			for _, b := range m.Content {
				switch b.Type {
				case agent.BlockText:
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				case agent.BlockToolResult:
					if askIDs[b.ToolUseID] && !b.IsError && strings.TrimSpace(b.Content) != "" {
						texts = append(texts, "[User answered AskUserQuestion]: "+b.Content)
					}
				}
			}
			if len(texts) == 0 {
				continue
			}
			if pendingProse != "" {
				entries = append(entries, entry{kind: entryAssistant, line: encodeLine("assistant", clip(pendingProse, lim.MaxProseChars))})
			}
			pendingProse = ""
			for _, t := range texts {
				userCount++
				userHash.Write([]byte(t))
				userHash.Write([]byte{0})
				entries = append(entries, entry{kind: entryUser, line: encodeLine("user", clip(t, lim.MaxUserTurnChars))})
			}
		}
	}
	actionLine := encodeLine(entryKey(act.Tool), ClassifierInput(act.Tool, act.Input))
	keep := selectEntries(entries, actionLine, lim)

	var b strings.Builder
	dropped := 0
	for i, e := range entries {
		if !keep[i] {
			dropped++
			continue
		}
		if dropped > 0 {
			b.WriteString(omittedLine(dropped))
			dropped = 0
		}
		b.WriteString(e.line)
	}
	if dropped > 0 {
		b.WriteString(omittedLine(dropped))
	}
	return built{
		transcript: b.String(),
		// A meta line sits directly above the action it describes.
		action:    act.metaLine() + actionLine,
		userTurns: strconv.Itoa(userCount) + ":" + hex.EncodeToString(userHash.Sum(nil)),
	}
}

// selectEntries applies TranscriptLimits: which entries are sent.
func selectEntries(entries []entry, actionLine string, lim TranscriptLimits) []bool {
	keep := make([]bool, len(entries))
	lastUser, lastAssistant := -1, -1
	var tools []int
	for i, e := range entries {
		switch e.kind {
		case entryUser:
			keep[i] = true
			lastUser = i
		case entryAssistant:
			lastAssistant = i
		case entryTool:
			tools = append(tools, i)
		}
	}
	window := len(entries) // index where the recent-call window starts
	if n := len(tools); n > 0 {
		from := max(n-lim.RecentToolCalls, 0)
		window = tools[from]
		for _, i := range tools[from:] {
			keep[i] = true
		}
		// An older write of a file the action names stays: running or
		// importing it inherits the written content.
		for _, i := range tools[:from] {
			if p := entries[i].path; p != "" {
				if base := filepath.Base(p); len(base) >= 3 && strings.Contains(actionLine, base) {
					keep[i] = true
				}
			}
		}
	}
	for i, e := range entries {
		if e.kind == entryAssistant && (i >= window || i == lastAssistant) {
			keep[i] = true
		}
	}

	// size is the rendered length, omitted-entry markers included.
	size := func() int {
		n, run := 0, 0
		for i, e := range entries {
			if !keep[i] {
				run++
				continue
			}
			if run > 0 {
				n += len(omittedLine(run))
				run = 0
			}
			n += len(e.line)
		}
		if run > 0 {
			n += len(omittedLine(run))
		}
		return n
	}
	// Over the cap: drop the oldest kept non-user entries (keeping the last
	// few tool calls), then the oldest user turns except the latest, then
	// whatever non-user entries remain.
	const minRecentCalls = 3
	protected := map[int]bool{}
	for _, i := range tools[max(len(tools)-minRecentCalls, 0):] {
		protected[i] = true
	}
	passes := []func(i int, e entry) bool{
		func(i int, e entry) bool { return e.kind != entryUser && !protected[i] },
		func(i int, e entry) bool { return e.kind == entryUser && i != lastUser },
		func(i int, e entry) bool { return e.kind != entryUser },
	}
	cur := size()
	for _, drop := range passes {
		for i, e := range entries {
			if cur <= lim.MaxChars {
				return keep
			}
			if keep[i] && drop(i, e) {
				keep[i] = false
				cur = size()
			}
		}
	}
	return keep
}
