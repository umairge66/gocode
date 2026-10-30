package ui

import (
	"strings"
	"time"
)

// This file carries the rich transcript view items that a
// plain [Message] cannot express on its own: a tool-call block and a diff block.
// They hang off a [Message] as optional payloads (Message.Tool / Message.Diff),
// so the transcript stays an ordered []Message — existing AppendMessage /
// AppendUser / AppendAssistant and every host that reads vs.Transcript keep
// working unchanged — while a tool call or a diff slots into the stream in order.
//
// Look:
//
//   - Tool-call header: "<Name>(<args>)", e.g. Bash(go test ./...),
//     Read(main.go), Update(internal/ui/model.go).
//   - Result truncation affordance: "… (+N more lines)" and the dim continuation
//     gutter "└─ · · · +N more".
//   - Diff: each changed line carries a full-line background — diffAdded (dark
//     green) / diffRemoved (dark red) — with a "+"/"-" sign, and brighter
//     diffAddedWord / diffRemovedWord for the sign/word spans.

// DefaultToolResultLines is the number of result lines a tool-call block shows
// before the "… (+N more lines)" affordance: a small window.
const DefaultToolResultLines = 8

// toolUseErrorTag is the model-facing wrapper the dispatcher puts around a failed
// tool result; it is stripped for display so the user sees only the inner text.
const toolUseErrorTag = "tool_use_error"

// userRejectionPrefix is the start of the model-facing message a tool result
// carries when the user declined the tool call. The full text instructs the
// model to STOP; the UI shows the short label instead (see RejectedToolLabel).
const userRejectionPrefix = "The user doesn't want to proceed with this tool use"

// RejectedToolLabel is the short, user-facing line shown in place of the long
// model-facing rejection body when the user declines a tool call.
const RejectedToolLabel = "User rejected tool use"

// ToolCall is a tool-invocation view item: a header line rendered as
// "<Name>(<Args>)" followed by the tool's result, indented and truncated.
//
// It is appended as a Message with Role RoleToolUse and a non-nil Tool payload;
// the host updates the same block in place (ID match) once the tool returns.
type ToolCall struct {
	// ID is a stable identifier used to update the block in place after the tool
	// returns (UpdateToolCall matches on it). "" means append-only.
	ID string
	// Name is the tool's user-facing verb shown before the parens (e.g. "Bash",
	// "Read", "Update", "Write", "Search").
	Name string
	// Args is the one-line argument summary shown inside the header parens (the
	// command, the path, the query …). It is already summarized by the host.
	Args string
	// ResultText is the tool's textual result, rendered indented under the header.
	ResultText string
	// IsError marks a failed tool call: the result is drawn in the error color.
	IsError bool
	// Collapsed hides the result body entirely (only the header is drawn).
	Collapsed bool
	// MaxLines caps the result lines shown before the "… (+N more lines)"
	// affordance. 0 uses DefaultToolResultLines; a negative value shows every
	// line (no truncation).
	MaxLines int
	// Ended marks a tool call that has returned (its result/error attached). It
	// is set by the ui layer (Reduce, on an UpdateToolCall), NOT by the host: a
	// ToolStart appends the block with Ended=false and the single ToolEnd update
	// flips it true. It is the finalization signal the Model uses to commit the
	// block to the terminal's native scrollback (tea.Println) and drop it from
	// the live region.
	Ended bool
	// FullText is the complete result shown when expanded (ctrl+o) when
	// ResultText is only a summary (e.g. "Read 40 lines" for the file text). ""
	// means ResultText is already the full result.
	FullText string
	// Expanded shows the full result with no line cap. The ui sets it on every
	// tool call from ViewState.ExpandResults (ctrl+o).
	Expanded bool

	// Started and Finished time the call (the Model stamps them as the start
	// and end messages arrive); the modern style shows the duration.
	Started  time.Time
	Finished time.Time

	// Shell marks a command the user ran from the composer's shell mode: the
	// header reads "! <command>" (Args) instead of "<Name>(<Args>)".
	Shell bool
}

// Duration is how long the call ran (0 while running or when untimed).
func (t *ToolCall) Duration() time.Duration {
	if t == nil || t.Started.IsZero() || t.Finished.IsZero() || t.Finished.Before(t.Started) {
		return 0
	}
	return t.Finished.Sub(t.Started)
}

// CanExpand reports whether ctrl+o would show more of this result: a summary
// with a fuller text behind it, or lines cut by the line cap.
func (t *ToolCall) CanExpand() bool {
	if t == nil || t.Expanded || t.IsRejection() {
		return false
	}
	if t.FullText != "" && t.FullText != t.ResultText {
		return true
	}
	_, overflow := t.VisibleResultLines()
	return overflow > 0
}

// Header returns the "<Name>(<Args>)" header string for a tool call.
func (t *ToolCall) Header() string {
	if t == nil {
		return ""
	}
	if t.Shell {
		return shellModeChar + " " + t.Args
	}
	if t.Args == "" {
		// A call with nothing to summarize reads "Screenshot", not "Screenshot()".
		return t.Name
	}
	return t.Name + "(" + t.Args + ")"
}

// resultLimit resolves the effective result-line cap.
func (t *ToolCall) resultLimit() int {
	if t.MaxLines == 0 {
		return DefaultToolResultLines
	}
	return t.MaxLines
}

// stripToolUseError removes a surrounding <tool_use_error …>…</tool_use_error>
// wrapper (the model-facing marker the dispatcher adds to a failed result) so
// only the inner text is shown. Text without the wrapper is returned unchanged.
func stripToolUseError(s string) string {
	t := strings.TrimSpace(s)
	open := "<" + toolUseErrorTag
	close := "</" + toolUseErrorTag + ">"
	if !strings.HasPrefix(t, open) || !strings.HasSuffix(t, close) {
		return s
	}
	// Skip past the opening tag (which may carry attributes: "<tag ...>").
	gt := strings.IndexByte(t, '>')
	if gt < 0 || gt >= len(t)-len(close) {
		return s
	}
	inner := t[gt+1 : len(t)-len(close)]
	return strings.TrimSpace(inner)
}

// IsRejection reports whether this tool result is the user-declined-the-tool
// message (a failed call whose body is the model-facing rejection text). Such a
// result is shown as the short RejectedToolLabel rather than the long body.
func (t *ToolCall) IsRejection() bool {
	if t == nil || !t.IsError {
		return false
	}
	inner := stripToolUseError(t.ResultText)
	return strings.HasPrefix(strings.TrimSpace(inner), userRejectionPrefix)
}

// DisplayResultText is the user-facing result text: the <tool_use_error> wrapper
// stripped off, and a known user-rejection body collapsed to RejectedToolLabel.
// The raw ResultText (what the model receives) is never altered.
func (t *ToolCall) DisplayResultText() string {
	if t == nil {
		return ""
	}
	if t.IsRejection() {
		return RejectedToolLabel
	}
	return stripToolUseError(t.ResultText)
}

// VisibleResultLines returns the result lines to draw and the number of hidden
// trailing lines (the "+N" in the "… (+N more lines)" affordance). A negative
// MaxLines shows every line (overflow 0). The lines are the display text
// (wrapper stripped, rejection collapsed), never the raw model-facing body.
func (t *ToolCall) VisibleResultLines() (lines []string, overflow int) {
	if t == nil {
		return nil, 0
	}
	text := t.DisplayResultText()
	if t.Expanded && t.FullText != "" && !t.IsRejection() {
		text = stripToolUseError(t.FullText)
	}
	if text == "" {
		return nil, 0
	}
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if t.Expanded {
		return all, 0
	}
	limit := t.resultLimit()
	if limit < 0 || len(all) <= limit {
		return all, 0
	}
	return all[:limit], len(all) - limit
}

// DiffLineKind classifies a diff line.
type DiffLineKind int

const (
	// DiffContext is an unchanged context line (no sign).
	DiffContext DiffLineKind = iota
	// DiffAdd is an added line ("+", drawn on the diffAdded background).
	DiffAdd
	// DiffRemove is a removed line ("-", drawn on the diffRemoved background).
	DiffRemove
)

// DiffLine is one line of a unified-diff hunk.
type DiffLine struct {
	Kind DiffLineKind
	Text string
	// OldLine / NewLine are 1-based source line numbers (0 = none), shown in the
	// diff gutter. Optional: 0/0 omits the number column.
	OldLine int
	NewLine int
}

// DiffHunk is a contiguous group of diff lines with an optional "@@ … @@" header.
type DiffHunk struct {
	Header string
	Lines  []DiffLine
}

// Diff is a unified-diff view item for an edit tool (Update/Write): a file path
// and one or more hunks of +/- lines. It is appended as a Message with Role
// RoleToolResult and a non-nil Diff payload.
type Diff struct {
	// Path is the edited file path shown in the diff header.
	Path string
	// Hunks are the diff's change groups in order.
	Hunks []DiffHunk
}

// Stat returns the added/removed line counts across all hunks (the "+N -N"
// summary drawn in diffAddedWord/diffRemovedWord).
func (d *Diff) Stat() (added, removed int) {
	if d == nil {
		return 0, 0
	}
	for _, h := range d.Hunks {
		for _, ln := range h.Lines {
			switch ln.Kind {
			case DiffAdd:
				added++
			case DiffRemove:
				removed++
			}
		}
	}
	return added, removed
}

// ParseUnifiedDiff builds a Diff from a standard unified-diff body (the text a
// host gets from `git diff`/`diff -u`). Lines starting with "+" are additions,
// "-" removals, " " context; "@@ … @@" starts a new hunk; "+++"/"---"/"diff "/
// "index " file headers are skipped. It is a convenience for hosts that already
// hold a unified diff; a host may also build Diff/DiffHunk directly.
func ParseUnifiedDiff(path, body string) *Diff {
	d := &Diff{Path: path}
	var cur *DiffHunk
	oldNum, newNum := 0, 0
	for _, raw := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(raw, "@@"):
			d.Hunks = append(d.Hunks, DiffHunk{Header: raw})
			cur = &d.Hunks[len(d.Hunks)-1]
			oldNum, newNum = parseHunkStarts(raw)
		case strings.HasPrefix(raw, "+++"), strings.HasPrefix(raw, "---"),
			strings.HasPrefix(raw, "diff "), strings.HasPrefix(raw, "index "):
			// file headers — skipped
		default:
			if cur == nil {
				d.Hunks = append(d.Hunks, DiffHunk{})
				cur = &d.Hunks[len(d.Hunks)-1]
			}
			switch {
			case strings.HasPrefix(raw, "+"):
				cur.Lines = append(cur.Lines, DiffLine{Kind: DiffAdd, Text: raw[1:], NewLine: newNum})
				newNum++
			case strings.HasPrefix(raw, "-"):
				cur.Lines = append(cur.Lines, DiffLine{Kind: DiffRemove, Text: raw[1:], OldLine: oldNum})
				oldNum++
			default:
				text := raw
				if strings.HasPrefix(raw, " ") {
					text = raw[1:]
				}
				cur.Lines = append(cur.Lines, DiffLine{Kind: DiffContext, Text: text, OldLine: oldNum, NewLine: newNum})
				oldNum++
				newNum++
			}
		}
	}
	return d
}

// parseHunkStarts extracts the old/new starting line numbers from a
// "@@ -a,b +c,d @@" header; it returns 0,0 when the header is unparseable.
func parseHunkStarts(header string) (oldStart, newStart int) {
	fields := strings.Fields(header)
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			oldStart = leadingInt(f[1:])
		} else if strings.HasPrefix(f, "+") {
			newStart = leadingInt(f[1:])
		}
	}
	return oldStart, newStart
}

// leadingInt parses the integer prefix of s up to a comma or end.
func leadingInt(s string) int {
	n := 0
	seen := false
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		seen = true
	}
	if !seen {
		return 0
	}
	return n
}
