package agenttool

import (
	"encoding/json"
	"strings"
)

// Activity summary for a subagent's current tool call.
//
// This mirrors the verb/argument summarizer style of internal/cli/toolsummary.go
// (which internal/agenttool cannot import without a cycle): it reads only the
// already-parsed tool input the model emitted and produces a short, natural
// activity phrase for Task.Activity plus a one-line argument summary for the
// captured tool TranscriptEntry. Nothing here executes anything.
//
// The gerund phrasing ("Searching", "Reading", "Running") gives a per-agent
// activity column; free-form model-authored wording cannot be derived from a
// tool's input, so a verb + argument summary is used instead.

// summarizeActivity returns (activity, args) for a tool call: activity is the
// human-readable current-action phrase, args is the one-line argument summary.
func summarizeActivity(name string, input json.RawMessage) (activity, args string) {
	args = activityArgs(name, input)
	verb := activityVerb(name)
	if args == "" {
		return verb, ""
	}
	return verb + " " + args, args
}

// activityVerb maps a tool name to a gerund action phrase. An unrecognized tool
// (including MCP tools) uses its own name.
func activityVerb(name string) string {
	switch name {
	case "Bash":
		return "Running"
	case "Read":
		return "Reading"
	case "Edit", "MultiEdit":
		return "Editing"
	case "Write":
		return "Writing"
	case "NotebookEdit":
		return "Editing notebook"
	case "Glob", "Grep":
		return "Searching"
	case "WebFetch":
		return "Fetching"
	case "WebSearch":
		return "Searching the web"
	case "Agent", "Task":
		return "Delegating"
	default:
		return name
	}
}

// activityArgs recovers the one-line argument summary, keying on the field each
// tool uses (mirrors internal/cli/toolsummary.go toolArgs). An MCP tool
// summarizes to its own fully-qualified name.
func activityArgs(name string, input json.RawMessage) string {
	if strings.HasPrefix(name, "mcp__") {
		return name
	}

	var in map[string]json.RawMessage
	if len(input) > 0 {
		_ = json.Unmarshal(input, &in)
	}
	str := func(key string) string {
		raw, ok := in[key]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}

	switch name {
	case "Bash":
		return firstLine(str("command"))
	case "Read", "Edit", "MultiEdit", "Write", "NotebookEdit":
		if p := str("file_path"); p != "" {
			return p
		}
		return str("notebook_path")
	case "Glob", "Grep":
		if p := str("pattern"); p != "" {
			return p
		}
		return str("query")
	case "WebFetch":
		return str("url")
	case "WebSearch":
		return str("query")
	case "Agent", "Task":
		return str("description")
	default:
		for _, key := range []string{"file_path", "path", "command", "query", "pattern", "url"} {
			if v := str(key); v != "" {
				return firstLine(v)
			}
		}
		return ""
	}
}

// firstLine returns s truncated at its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
