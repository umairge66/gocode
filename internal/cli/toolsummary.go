package cli

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/toolrun"
)

// This file maps a dispatched tool (its name + raw input JSON) onto the pieces
// the rich UI needs for a tool-call block: a user-facing verb (the header word
// before the parens) and a one-line argument summary (what goes inside them),
// plus a unified-diff body for the edit tools.
//
// Each tool has its own verb and summary: Bash shows the command, Read shows
// the path, the edit tool shows "Update", Write shows "Create". Nothing here
// executes anything; it
// only reads the already-parsed input the model emitted.

// toolVerb returns the user-facing verb shown before the parens in a tool-call
// header; an unrecognized tool (including MCP tools) uses its own name
// verbatim.
func toolVerb(name string) string {
	switch name {
	case "Edit", "MultiEdit":
		// The edit tool renders as "Update".
		return "Update"
	case "Write":
		// Write renders as "Create".
		return "Create"
	case "NotebookEdit":
		return "Edit Notebook"
	case "Glob", "Grep":
		return "Search"
	case "WebFetch":
		return "Fetch"
	case "WebSearch":
		return "Web Search"
	case "TodoWrite":
		return "Update Todos"
	case "EnterPlanMode":
		return "Enter plan mode"
	case "ExitPlanMode":
		return "Exit plan mode"
	default:
		if v, ok := browserVerbs[name]; ok {
			return v
		}
		// MCP tools (mcp__<server>__<tool>) show the readable tool part, e.g.
		// "caveman_toon_encode" rather than the raw "mcp__caveman__caveman_toon_encode".
		if server, tool, ok := splitMCP(name); ok {
			if tool != "" {
				return tool
			}
			return server
		}
		// Read, Bash, Task, TodoWrite, … keep their own name.
		return name
	}
}

// splitMCP parses "mcp__<server>__<tool>" into its server and tool parts. ok is
// false for a non-MCP name. For a server-level name ("mcp__<server>") tool is
// "". Used to render a readable MCP tool-call header instead of the raw
// mcp__-qualified identifier.
func splitMCP(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp__")
	if !found {
		return "", "", false
	}
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[:i], rest[i+2:], true
	}
	return rest, "", true
}

// toolHeader returns the verb and argument summary of a tool-call header. A
// Chrome computer call takes its verb from the action (Type, Click, Key, …).
func toolHeader(name string, input json.RawMessage) (verb, args string) {
	if isBrowserTool(name) {
		return browserHeader(name, input)
	}
	return toolVerb(name), toolArgs(name, input)
}

// toolArgs recovers the one-line argument summary shown inside a tool-call
// header's parens. It parses only the field each tool keys on; a tool with no
// recognized field (or unparseable input) yields "", so the header is just
// "<Verb>()". An MCP tool summarizes to its own fully-qualified name.
func toolArgs(name string, input json.RawMessage) string {
	if strings.HasPrefix(name, "mcp__") {
		// MCP tools have no house-specific field; summarize the actual call
		// arguments (compact one-line JSON) instead of echoing the tool name.
		return mcpArgSummary(input)
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
	case "LSP":
		wd, _ := os.Getwd()
		return lsp.ToolUseSummary(input, wd)
	case "TodoWrite":
		return ""
	case "Agent", "Task":
		// Show "<type>: <description>" (or whichever is present) so the header
		// reads e.g. Agent(general-purpose: explore the directory) instead of
		// an empty Agent().
		typ := str("subagent_type")
		desc := str("description")
		switch {
		case typ != "" && desc != "":
			return typ + ": " + desc
		case desc != "":
			return desc
		case typ != "":
			return typ
		default:
			return firstLine(str("prompt"))
		}
	default:
		// A best-effort: prefer a path, then a command, then a query.
		for _, key := range []string{"file_path", "path", "command", "query", "pattern", "url"} {
			if v := str(key); v != "" {
				return firstLine(v)
			}
		}
		return ""
	}
}

// editDiff builds a unified-diff (path, body) from an edit tool's input, for
// AppendDiffMsg via ui.ParseUnifiedDiff. ok is false for a non-edit tool or
// when the input lacks a usable old→new / content payload, in which case the
// caller shows only the tool block and no diff.
//
// The body is built from the tool INPUT (old_string → new_string, or the
// written content), which is all the observer seam can see: the dispatcher
// flattens the tool's structured result to a text string, so the rich
// FileDiff the edit tool computes internally is not available here. The diff
// therefore shows the changed snippet without surrounding file context or
// absolute line numbers — accurate to the change, lighter than a
// gutter-numbered diff.
func editDiff(name string, input json.RawMessage) (path, body string, ok bool) {
	switch name {
	case "Edit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return "", "", false
		}
		return in.FilePath, diffReplace(in.OldString, in.NewString), true

	case "MultiEdit":
		var in struct {
			FilePath string `json:"file_path"`
			Edits    []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" || len(in.Edits) == 0 {
			return "", "", false
		}
		var b strings.Builder
		for _, e := range in.Edits {
			b.WriteString(diffReplace(e.OldString, e.NewString))
		}
		return in.FilePath, b.String(), true

	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return "", "", false
		}
		// A write replaces the file wholesale; show its content as all-added
		// (the prior content is not available at this seam).
		return in.FilePath, signLines(in.Content, "+"), true

	default:
		return "", "", false
	}
}

// diffReplace renders one old→new replacement as unified-diff lines (old lines
// removed, new lines added) with no "@@" header, so ParseUnifiedDiff groups the
// whole change into a single, header-less hunk.
func diffReplace(oldStr, newStr string) string {
	return signLines(oldStr, "-") + signLines(newStr, "+")
}

// signLines prefixes every line of s with sign ("+"/"-") and a space-free sign
// column, as unified-diff bodies do. A trailing newline does not produce an
// empty final line.
func signLines(s, sign string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(sign)
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}

// mcpArgSummary renders an MCP tool's input as a compact one-line JSON summary
// for the call header's parens, truncated to keep the header short. Empty or
// objectless input yields "" (the header is then just "<tool>()").
func mcpArgSummary(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(input, &v) != nil {
		return ""
	}
	b, err := json.Marshal(v) // re-marshal to minify (drops indentation/spacing)
	if err != nil {
		return ""
	}
	s := string(b)
	if s == "{}" || s == "null" {
		return ""
	}
	const max = 60
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// displayToolResult cleans a tool result for the TRANSCRIPT (what the user
// sees). The model still receives the full raw result; this only affects the
// `⎿` block. The Agent tool's background-launch result is internal metadata
// (the "Async agent launched… do not echo this" instructions meant for the
// model), so it is replaced with a terse, user-facing status line rather than
// shown verbatim.
func displayToolResult(name string, input json.RawMessage, result string) string {
	if line, ok := planToolResult(name, result); ok {
		return line
	}
	if name == agentToolName && strings.HasPrefix(result, "Async agent launched successfully") {
		return "Launched — running in the background; you'll be notified when it finishes."
	}
	if feedback, ok := toolrun.RejectionFeedback(result); ok {
		// A call the user rejected at its permission prompt.
		if feedback != "" {
			return toolrun.RejectedToolUseLabel + ": " + feedback
		}
		return toolrun.RejectedToolUseLabel
	}
	if isBrowserTool(name) {
		return browserResult(name, input, result)
	}
	if strings.Contains(result, "<tool_use_error>") {
		return result
	}
	switch name {
	case "Read":
		if n := countNumberedLines(result); n > 0 {
			return "Read " + countNoun(n, "line", "lines")
		}
	case "Glob":
		return "Found " + countNoun(len(resultEntries(result, "No files found")), "file", "files")
	case "Grep":
		return grepSummary(input, result)
	}
	return result
}

// expandedToolResult is the full result shown when the user expands tool
// output (ctrl+o) and the transcript shows only a summary of it; "" when the
// displayed result is already complete.
func expandedToolResult(name string, input json.RawMessage, result string) string {
	if isBrowserTool(name) {
		return ""
	}
	if displayToolResult(name, input, result) == result {
		return ""
	}
	return result
}

// numberedLineRE matches one line of Read output ("    12\tcontent").
var numberedLineRE = regexp.MustCompile(`(?m)^ *\d+\t`)

// countNumberedLines counts the file lines in a Read result (the line-numbered
// rows; trailing notices are not counted). 0 means the result is not plain file
// text (an image, a PDF, a notice), which is then shown as is.
func countNumberedLines(result string) int {
	return len(numberedLineRE.FindAllStringIndex(result, -1))
}

// resultEntries splits a Glob/Grep result into its entries: one per line, with
// the empty-result text and the trailing "(…)" notices (truncation, search
// engine) left out.
func resultEntries(result, emptyText string) []string {
	if strings.TrimSpace(result) == emptyText {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(result, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || (strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")")) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// grepSummary is the one-line Grep result: "Found N files"
// (files_with_matches), "Found N lines" (content) or "Found N matches across M
// files" (count).
func grepSummary(input json.RawMessage, result string) string {
	var in struct {
		OutputMode string `json:"output_mode"`
	}
	_ = json.Unmarshal(input, &in)
	entries := resultEntries(result, "No matches found")
	switch in.OutputMode {
	case "content":
		return "Found " + countNoun(len(entries), "line", "lines")
	case "count":
		matches := 0
		for _, e := range entries {
			if i := strings.LastIndexByte(e, ':'); i >= 0 {
				n, _ := strconv.Atoi(strings.TrimSpace(e[i+1:]))
				matches += n
			}
		}
		return "Found " + countNoun(matches, "match", "matches") + " across " + countNoun(len(entries), "file", "files")
	default:
		return "Found " + countNoun(len(entries), "file", "files")
	}
}

// countNoun renders "1 line" / "3 lines".
func countNoun(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// firstLine returns s truncated at its first newline (tool-call headers are
// one line; a multi-line Bash command shows only its first line).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
