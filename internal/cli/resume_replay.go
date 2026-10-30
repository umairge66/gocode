package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// This file re-renders a RESUMED conversation's history into the interactive TUI
// transcript. The backend already seeds the loaded messages into
// conv.History (see sessionRuntime.resumeInto → Conversation.SeedHistory), but
// seeding alone leaves the transcript empty: the user who runs /resume (or
// launches with --resume / --continue) would see nothing of the conversation
// they picked. replayResumeHistory walks the loaded history in order and emits
// the SAME ui host messages the live turn driver emits (AppendUserMsg /
// AppendAssistantMsg / AppendToolCallMsg / UpdateToolCallMsg), so the past
// exchange reads top-to-bottom exactly as it originally did, followed by the
// "Resumed conversation" note.
//
// It is a pure re-render: it touches only the UI (via the program's Send seam)
// and never the conversation state, persistence, the picker or the flags. It is
// interactive-only; the -p / PlainRenderer path has no TUI to re-render and is
// unaffected.

// resumeSeparator formats the leading system line that marks where a resumed
// conversation's replayed history begins. It is emitted before the replayed
// messages so a /resume that switches sessions mid-run shows a clear boundary
// (the terminal commits finished transcript items to native scrollback, so the
// earlier transcript cannot be cleared — a boundary line is the clean,
// correct behavior; each /resume replays only its own newly-loaded session, so
// histories are never replayed twice).
func resumeSeparator(title string) string {
	if strings.TrimSpace(title) == "" {
		return "— Resumed conversation —"
	}
	return "— Resumed conversation: " + title + " —"
}

// compactedDivider is the line shown where a compacted conversation's history
// begins: the compact_boundary row renders "✻ Conversation compacted";
// there is no " (ctrl+o for history)" hint because there is no
// transcript-mode toggle.
const compactedDivider = "\u273B Conversation compacted"

// replayResumeHistory re-renders a resumed conversation's history into the TUI
// transcript via send (the program's Send func), in history order, preceded by
// a resume separator line. It handles these content-block kinds:
//
//   - user-role text block        → ui.AppendUserMsg (the sent-command block)
//   - shell-mode exchange blocks  → one "! <command>" block with its output,
//     as shown when the command ran (see shellReplay)
//   - assistant-role text block   → ui.AppendAssistantMsg (final Markdown render)
//   - assistant tool_use block    → ui.AppendToolCallMsg (header "<verb>(<args>)")
//   - tool_result block           → ui.UpdateToolCallMsg (attaches the result to
//     the matching tool_use block, paired by id)
//
// Deliberately skipped (no sensible static transcript form):
//
//   - thinking / redacted_thinking blocks — the UI only ever shows reasoning as
//     a live stream; a resumed turn has no stream to attach it to.
//   - image blocks — the transcript renderer draws only text.
//   - empty / whitespace-only text blocks — nothing to show.
//   - a tool_result whose tool_use_id was never seen in this history — pairing
//     it would append a header-less "()" block, so it is dropped.
//   - a leading compact-summary turn — replaced by the "Conversation compacted"
//     divider (see compactedDivider).
//
// send must be the program's Send (the established safe seam for pushing to the
// UI off the render goroutine); it is never called on the UI goroutine.
func replayResumeHistory(send func(tea.Msg), title string, history []agent.Message) {
	if send == nil {
		return
	}
	send(ui.AppendSystemMsg{Text: resumeSeparator(title)})
	replayHistory(send, history)
}

// replayHistory renders history into the TUI transcript (see
// replayResumeHistory), without a separator: /rewind redraws the conversation
// it kept on a cleared screen.
func replayHistory(send func(tea.Msg), history []agent.Message) {
	if send == nil {
		return
	}

	// A compacted session loads from its last compact_boundary, so the history
	// starts with the compact-summary turn. The boundary is
	// shown as a divider and the summary itself is not rendered in the main
	// transcript (isVisibleInTranscriptOnly); the pre-compaction turns are not
	// loaded, so nothing is replayed above the divider.
	if len(history) > 0 && compaction.IsSummaryMessage(compaction.Message{Role: history[0].Role, Content: history[0].Content}) {
		send(ui.AppendSystemMsg{Text: compactedDivider})
		history = history[1:]
	}

	// toolMeta remembers each tool_use block's verb+args by id so a later
	// tool_result (which carries only the id) can re-render the block's header
	// when it attaches the result (UpdateToolCall replaces the block wholesale).
	type toolMeta struct {
		name, args string
		tool       string          // raw tool name
		input      json.RawMessage // raw tool input
	}
	seen := map[string]toolMeta{}
	var shell shellReplay

	for _, msg := range history {
		for _, b := range msg.Content {
			switch b.Type {
			case agent.BlockText:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				if msg.Role == agent.RoleUser {
					if st, ok := conversation.ParseShellText(b.Text); ok {
						shell.replay(send, st)
						continue
					}
					send(ui.AppendUserMsg{Text: b.Text})
				} else {
					send(ui.AppendAssistantMsg{Text: b.Text})
				}
			case agent.BlockToolUse:
				verb, args := toolHeader(b.Name, b.Input)
				seen[b.ID] = toolMeta{name: verb, args: args, tool: b.Name, input: b.Input}
				send(ui.AppendToolCallMsg{Tool: ui.ToolCall{
					ID:   b.ID,
					Name: verb,
					Args: args,
				}})
			case agent.BlockToolResult:
				meta, ok := seen[b.ToolUseID]
				if !ok {
					// No matching tool_use earlier in the history — dropping avoids a
					// header-less block.
					continue
				}
				send(ui.UpdateToolCallMsg{Tool: ui.ToolCall{
					ID:         b.ToolUseID,
					Name:       meta.name,
					Args:       meta.args,
					ResultText: displayToolResult(meta.tool, meta.input, b.Content),
					FullText:   expandedToolResult(meta.tool, meta.input, b.Content),
					IsError:    b.IsError,
				}})
			default:
				// thinking / redacted_thinking / image and any other kind: skipped
				// (documented above).
			}
		}
	}
}

// shellReplay renders the recorded blocks of shell-mode exchanges: the
// <bash-input> block opens a "! <command>" block, the following output block
// (or interruption marker) fills in its result, and the caveat is not shown.
type shellReplay struct {
	n    int
	open *ui.ToolCall
}

func (r *shellReplay) replay(send func(tea.Msg), st conversation.ShellText) {
	switch st.Kind {
	case conversation.ShellTextInput:
		r.n++
		tc := ui.ToolCall{ID: fmt.Sprintf("replay-shell-%d", r.n), Name: "!", Args: st.Command, Shell: true}
		r.open = &tc
		send(ui.AppendToolCallMsg{Tool: tc})
	case conversation.ShellTextOutput:
		if r.open == nil {
			return
		}
		tc := *r.open
		tc.ResultText = joinNonEmpty(st.Stdout, st.Stderr)
		if tc.ResultText == "" {
			tc.ResultText = "(No output)"
		}
		tc.IsError = conversation.ShellOutputFailed(st.Stderr)
		send(ui.UpdateToolCallMsg{Tool: tc})
		r.open = nil
	case conversation.ShellTextInterrupted:
		if r.open == nil {
			return
		}
		tc := *r.open
		tc.ResultText = "Interrupted"
		tc.IsError = true
		send(ui.UpdateToolCallMsg{Tool: tc})
		r.open = nil
	}
}
