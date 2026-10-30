package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This file holds the stop-handling the loop performs on a response
// with no tool_use follow-up before it decides the turn is over: the
// malformed-tool-use retry, max_output_tokens recovery, the thinking-only /
// empty-response nudge, pause_turn resumption, refusal and context-window
// stops, plus the tool-input and abort hygiene that keeps every tool_use paired
// with a tool_result.

// Nudge / notice texts.
const (
	// MalformedToolUseNudge is appended (as a meta user turn) when the model
	// stopped with stop_reason "tool_use" but produced no tool_use block; the
	// bad response is dropped and the model is asked again.
	MalformedToolUseNudge = "The previous response failed to produce a valid tool call. Please retry the tool call now."

	// malformedToolUseExhausted is the error surfaced once the single retry
	// also failed.
	malformedToolUseExhausted = "The model's tool call could not be parsed (retry also failed)."

	// MaxOutputTokensNudge is appended after a response cut off by the output
	// cap; the partial response is kept so the model resumes it.
	MaxOutputTokensNudge = "Output token limit hit. Resume directly — no apology, no recap of what you were doing. " +
		"Pick up mid-thought if that is where the cut happened. Break remaining work into smaller pieces."

	// NoVisibleOutputNudge is appended when an end_turn / stop_sequence
	// response carried no visible text (thinking-only or empty); the empty
	// response is dropped and the model is asked once more.
	NoVisibleOutputNudge = "[Your previous response had no visible output. Please continue and produce a user-visible response.]"

	// noContentPlaceholder stands in for an assistant turn with no content so
	// the stored history stays API-valid (the API rejects empty content).
	noContentPlaceholder = "(no content)"

	// apiErrorPrefix prefixes the loop's API-error notices.
	apiErrorPrefix = "API Error"

	// toolUseInterruptedResult is the synthetic tool_result content for a
	// tool_use that never completed because the run was aborted.
	toolUseInterruptedResult = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."

	// interruptedForToolUse is the user-turn marker appended after the
	// synthetic results when the run is aborted during tool execution.
	interruptedForToolUse = "[Request interrupted by user for tool use]"
)

// Limits.
const (
	// maxOutputTokensRecoveryLimit is how many continuation nudges a run of
	// max_tokens responses may get before the loop stops with an error.
	maxOutputTokensRecoveryLimit = 3

	// maxPauseTurnResumes bounds consecutive pause_turn resumptions.
	// A pause_turn response is resumed by re-sending the conversation with no
	// tool results; a fixed cap keeps a misbehaving server from looping
	// forever.
	maxPauseTurnResumes = 10

	// unparsedInputPreview is how much of an unparseable tool input is echoed
	// back to the model.
	unparsedInputPreview = 200
	// unparsedInputKeep is how much raw input is retained at all.
	unparsedInputKeep = 2048
)

// Stop error kinds carried by StopError.Kind.
const (
	StopKindMaxOutputTokens  = "max_output_tokens"
	StopKindRefusal          = "refusal"
	StopKindContextWindow    = "model_context_window_exceeded"
	StopKindMalformedToolUse = "malformed_tool_use"
	// StopKindRepeatedToolCalls: the same tool call was repeated up to the
	// loop guard's stop threshold.
	StopKindRepeatedToolCalls = "repeated_tool_calls"
)

// StopError is the classified error a Result carries when the model stopped
// for a reason the loop cannot recover from (refusal, exhausted output-cap
// recovery, context window, unparseable tool call). Message is the user-facing
// API-error notice; it is returned here instead of being stored in the
// history, because the history is sent back to the model verbatim and the
// notice must not reach the request.
type StopError struct {
	Kind    string
	Message string
}

func (e *StopError) Error() string { return e.Message }

// maxOutputTokensMessage builds the max_tokens notice.
func maxOutputTokensMessage(maxTokens int) string {
	if maxTokens > 0 {
		return fmt.Sprintf("%s: gocode's response exceeded the %d output token maximum.", apiErrorPrefix, maxTokens)
	}
	return apiErrorPrefix + ": gocode's response exceeded the output token maximum."
}

// contextWindowMessage is the notice for a model_context_window_exceeded stop.
const contextWindowMessage = apiErrorPrefix + ": The model has reached its context window limit."

// refusalMessage is the generic refusal notice. The loop does not know the
// model's display name, so it names "gocode"; no "Learn more" link,
// category-specific lines or request/message ids are added.
const refusalMessage = apiErrorPrefix + ": gocode can't help with this. Start a new session to continue."

// unparsedInputError builds the InputValidationError tool_result body for a
// tool_use whose input could not be parsed as JSON.
func unparsedInputError(name string, raw string) string {
	total := utf8.RuneCountInString(raw)
	preview := truncateRunes(truncateRunes(raw, unparsedInputKeep), unparsedInputPreview)
	return fmt.Sprintf("InputValidationError: %s was called with input that could not be parsed as JSON. "+
		"You sent (first %d of %d bytes): %s Common causes: unescaped backslashes in file paths (use / or \\\\), "+
		"unescaped control characters, or truncated output. Retry with valid JSON.",
		name, utf8.RuneCountInString(preview), total, preview)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// sanitizeToolInputs normalizes every tool_use block's input so the assistant
// turn can be stored and re-sent: an empty or "null" input becomes {}, and an
// input that is not valid JSON becomes {} with its raw text recorded (keyed by
// tool_use id) so the dispatch step answers it with an InputValidationError
// instead of running the tool. The message is copied, never mutated in place.
func sanitizeToolInputs(m Message) (Message, map[string]string) {
	var bad map[string]string
	var out []ContentBlock
	for i, b := range m.Content {
		if b.Type != BlockToolUse {
			continue
		}
		trimmed := strings.TrimSpace(string(b.Input))
		var fixed json.RawMessage
		switch {
		case trimmed == "" || trimmed == "null":
			fixed = json.RawMessage(`{}`)
		case !json.Valid([]byte(trimmed)):
			fixed = json.RawMessage(`{}`)
			if bad == nil {
				bad = make(map[string]string)
			}
			bad[b.ID] = string(b.Input)
		default:
			continue
		}
		if out == nil {
			out = append([]ContentBlock(nil), m.Content...)
		}
		out[i].Input = fixed
	}
	if out != nil {
		m.Content = out
	}
	return m, bad
}

// withContent guarantees an assistant turn has at least one content block.
func withContent(m Message) Message {
	if len(m.Content) == 0 {
		m.Content = []ContentBlock{TextBlock(noContentPlaceholder)}
	}
	return m
}

// appendUser appends a user turn, merging into a trailing user turn when there
// is one so the history never holds two consecutive user turns (adjacent user
// messages would be merged before sending anyway).
func appendUser(messages []Message, blocks ...ContentBlock) []Message {
	if n := len(messages); n > 0 && messages[n-1].Role == RoleUser {
		merged := append(append([]ContentBlock(nil), messages[n-1].Content...), blocks...)
		out := append([]Message(nil), messages[:n-1]...)
		return append(out, Message{Role: RoleUser, Content: merged})
	}
	return append(messages, UserMessage(blocks...))
}

// interruptedToolResult is the synthetic result for a tool_use that never
// completed because the run was aborted.
func interruptedToolResult(toolUseID string) ContentBlock {
	return ContentBlock{
		Type:      BlockToolResult,
		ToolUseID: toolUseID,
		Content:   toolUseInterruptedResult,
		IsError:   true,
	}
}

// pairDanglingToolUses appends a synthetic interrupted tool_result for every
// tool_use in the trailing assistant turn that has no matching result, so the
// next request is not rejected for an unanswered tool_use.
func pairDanglingToolUses(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	last := messages[len(messages)-1]
	if last.Role != RoleAssistant {
		return messages
	}
	var results []ContentBlock
	for _, b := range last.ToolUseBlocks() {
		results = append(results, interruptedToolResult(b.ID))
	}
	if len(results) == 0 {
		return messages
	}
	results = append(results, TextBlock(interruptedForToolUse))
	return append(messages, UserMessage(results...))
}
