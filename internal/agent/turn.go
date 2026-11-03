// Package agent implements gocode agent turn loop: the core
// conversation loop that sends a request to the model, receives the assistant
// turn (text + tool_use blocks), executes the requested tools, appends their
// results back as a user turn, and iterates until the model stops.
//
//   - The loop runs one round at a time; each round either asks for another
//     round (the assistant produced at least one tool_use block, so a
//     follow-up is needed) or ends with a terminal reason.
//   - Dispatching a single tool covers lookup by name, JSON/schema/input
//     validation, permission resolution with PreToolUse hook events
//     (permission result / updated input / prevent continuation / stop /
//     defer / additional context), the tool call itself, and mapping the
//     tool's output to a {type:"tool_result", tool_use_id, content, is_error}
//     content block.
//
// This file holds the conversation types (messages, content blocks) and the
// terminal-result type. The loop lives in loop.go; tool dispatch in
// tooldispatch.go.
//
// The loop's canonical "message" carries tool_result blocks (which the model
// never emits but the loop produces) alongside the text/tool_use blocks the
// model returns. internal/apiclient holds the shared wire data model (message,
// content-block and stop-reason shapes); this package reuses its constants.
//
// Content-block unification. apiclient.ContentBlock was
// extended to carry the tool_result fields (tool_use_id / content / is_error)
// and agent.ContentBlock is now a type alias for it, so one content-block type
// serves both the wire and the loop. Extending the wire type is sound rather
// than polluting: tool_result is a legitimate Anthropic content block that the
// loop sends back inside user turns, and the added fields are all `omitempty`,
// so the other block types still serialize byte-for-byte. The loop's canonical
// Message stays a distinct named type here because it carries methods
// (ToolUseBlocks / HasText), which cannot hang off an alias to another package's
// type; callers convert between agent.Message and apiclient.Message by copying
// the (now identical) content slice.
package agent

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/apiclient"
)

// Message roles: Message.role ∈ {"user", "assistant"} (the loop pushes assistant turns from the model and user turns
// carrying tool_result blocks).
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Content-block type discriminants, reused from the wire union.
const (
	BlockText       = apiclient.BlockText       // "text"
	BlockToolUse    = apiclient.BlockToolUse    // "tool_use"
	BlockToolResult = apiclient.BlockToolResult // "tool_result"
	BlockThinking   = apiclient.BlockThinking   // "thinking"
	BlockImage      = apiclient.BlockImage      // "image"
)

// Stop reasons, reused from internal/apiclient.
const (
	StopEndTurn      = apiclient.StopEndTurn      // "end_turn"
	StopToolUse      = apiclient.StopToolUse      // "tool_use"
	StopStopSequence = apiclient.StopStopSequence // "stop_sequence"
	StopMaxTokens    = apiclient.StopMaxTokens    // "max_tokens"
	StopRefusal      = apiclient.StopRefusal      // "refusal"
	StopPauseTurn    = apiclient.StopPauseTurn    // "pause_turn"
	// StopModelContextExceeded: the response hit the model's context window.
	StopModelContextExceeded = apiclient.StopModelContextExceeded // "model_context_window_exceeded"
)

// ContentBlock is one block inside a Message's content array. It is a type
// alias for apiclient.ContentBlock — the single, unified content-block type
// shared by the wire client and the turn loop. A single tagged struct
// (discriminated by Type) carries every variant: the text / tool_use blocks the
// model emits and the tool_result block ({type:"tool_result",tool_use_id,
// content,is_error}) the loop produces inside user turns. Fields irrelevant to a
// given Type are omitted on the wire.
type ContentBlock = apiclient.ContentBlock

// ToolResultImage is one image content block a tool produced, carried on a
// tool_result alongside its text. It is a type alias for
// apiclient.ToolResultImage so the loop and the wire share one type.
type ToolResultImage = apiclient.ToolResultImage

// Message is one conversation turn.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
	// ID is the provider's response id of an assistant turn (message.id;
	// empty when the provider sent none). Compaction groups the
	// history by API round with it. Providers never send it back:
	// requests are built from Role/Content only.
	ID string `json:"id,omitempty"`
}

// ImageSource is the source of a user image block (apiclient.ImageSource).
type ImageSource = apiclient.ImageSource

// ImageBlock builds a user image block from base64 data.
func ImageBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{Type: BlockImage, Source: &ImageSource{Type: "base64", MediaType: mediaType, Data: base64Data}}
}

// TextBlock builds a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ToolUseBlock builds an assistant tool_use block.
func ToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// ToolResultBlock builds a tool_result block from a dispatched tool's result.
func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	if isError {
		content = wrapToolError(content)
	}
	return ContentBlock{
		Type:      BlockToolResult,
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// ToolResultBlockWithImages builds a tool_result that carries image content
// (base64 + media type) alongside its text. It is the structured-content path:
// a Read of an image or PDF produces one or more image blocks the model must
// receive. Images are attached only on success; an is_error result never
// carries images (its content is the wrapped error text). With no images it is
// identical to ToolResultBlock, so the text path is unchanged.
func ToolResultBlockWithImages(toolUseID, content string, isError bool, images []ToolResultImage) ContentBlock {
	b := ToolResultBlock(toolUseID, content, isError)
	if !isError {
		b.Images = images
	}
	return b
}

// AssistantMessage builds an assistant turn.
func AssistantMessage(blocks ...ContentBlock) Message {
	return Message{Role: RoleAssistant, Content: blocks}
}

// UserMessage builds a user turn (e.g. the turn carrying tool_result blocks
// that answers the assistant's tool_use blocks).
func UserMessage(blocks ...ContentBlock) Message {
	return Message{Role: RoleUser, Content: blocks}
}

// ToolUseBlocks returns the tool_use blocks of a message, in order.
func (m Message) ToolUseBlocks() []ContentBlock {
	var out []ContentBlock
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// HasText reports whether the message carries any non-empty text block.
func (m Message) HasText() bool {
	for _, b := range m.Content {
		if b.Type == BlockText && len(trimSpace(b.Text)) > 0 {
			return true
		}
	}
	return false
}

// ModelRequest is what the loop hands the model each round (messages + system
// prompt + tool schemas + output cap).
//
// Request fields deliberately not carried here:
//   - tool_choice: unset on normal main-loop turns (only structured-output /
//     side queries set it), so the field is omitted — the same wire result
//     for all three providers.
//   - metadata: device_id / account_uuid / session_id identifiers are never
//     sent (no-telemetry rule).
//   - thinking: derived from the effort setting by the provider layer, not by
//     the loop.
type ModelRequest struct {
	Messages []Message
	System   string
	Tools    []ToolSchema
	// MaxTokens is the output cap.
	MaxTokens int
	// CacheSystemPrompt asks the provider to cache System as a prompt prefix
	// where caching needs an explicit marker: Anthropic (direct, or an
	// Anthropic model via OpenRouter) gets cache_control {"type":"ephemeral"}
	// on the system block. Providers that cache identical prefixes
	// automatically (OpenAI, DeepSeek, Gemini …) ignore it, as do non-Anthropic
	// models. Set it only when System is byte-identical across calls.
	CacheSystemPrompt bool
	// SystemContext is trailing system text that may change between the
	// calls of one conversation (the available-skills listing). It is sent
	// after System: a provider that caches System as a prefix keeps it
	// outside the cached part, and every other provider receives
	// System + "\n\n" + SystemContext as one system prompt, exactly as if
	// it had been appended to System.
	SystemContext string
	// CacheMessages asks the provider to also cache the conversation itself
	// with rolling breakpoints on the latest messages (Anthropic direct: the
	// last block of the last message and of the previous user turn), so each
	// round reads the previous round's prefix from the cache. Providers that
	// cache automatically, or not at all, ignore it.
	CacheMessages bool
	// DeferredTools are the deferred tools not loaded yet in this
	// conversation (see SelectTools), in a stable order. Tools carries what a
	// provider sends as callable definitions; a provider with native deferred
	// loading additionally sends these (and the Deferred entries of Tools)
	// marked for on-demand loading. Every other provider ignores them.
	DeferredTools []ToolSchema
}

// ToolSchema advertises one tool to the model (its name, description and
// input JSON schema).
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Deferred marks a tool whose definition is withheld from requests until
	// the model loads it with ToolSearch (see SelectTools). It is never sent.
	Deferred bool `json:"-"`
}

// ModelResponse is one assistant turn returned by a ModelCaller. It is the
// loop's view of the accumulated streamed message: the assistant Message plus
// the final stop_reason.
type ModelResponse struct {
	Message    Message
	StopReason string
	// Usage is the provider's token accounting for this response, when the
	// caller reports it (zero otherwise). Informational only.
	Usage ModelUsage
}

// ModelUsage is a response's token accounting. CachedInputTokens counts the
// input tokens served from the provider's prompt cache (Anthropic
// cache_read_input_tokens; OpenAI-compatible
// prompt_tokens_details.cached_tokens); CacheWriteTokens the tokens written to
// it (Anthropic cache_creation_input_tokens).
type ModelUsage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CacheWriteTokens  int
}

// Reason is a terminal outcome of Loop.Run.
type Reason string

const (
	// ReasonCompleted: the model ended the turn without requesting tools.
	ReasonCompleted Reason = "completed"
	// ReasonMaxTurns: the per-run turn budget was exhausted.
	ReasonMaxTurns Reason = "max_turns"
	// ReasonAPIError: the model call failed.
	ReasonAPIError Reason = "api_error"
	// ReasonMalformedToolUse: stop_reason was "tool_use" yet no tool_use block
	// was produced, and the single malformed_tool_use_retry was already spent.
	// Result.Err is a *StopError.
	ReasonMalformedToolUse Reason = "malformed_tool_use_exhausted"
	// ReasonAborted: the context was cancelled mid-flight.
	ReasonAborted Reason = "aborted"
	// ReasonStopHookPrevented: a turn-end (Stop/SubagentStop) hook blocked
	// completion and no continuation was produced.
	ReasonStopHookPrevented Reason = "stop_hook_prevented"
	// ReasonRefusal: the model declined to answer (stop_reason "refusal").
	// Result.Err is a *StopError carrying the refusal notice.
	ReasonRefusal Reason = "refusal"
	// ReasonContextWindowExceeded: the response stopped with
	// "model_context_window_exceeded". It signals the caller to compact before
	// the next turn; Result.Err is a *StopError.
	// The loop has no in-loop reactive compaction, so it stops here and
	// leaves compaction and the retry to the conversation layer.
	ReasonContextWindowExceeded Reason = "context_window_exceeded"
	// ReasonRepeatedToolCalls: the model repeated the exact same tool call
	// up to the loop guard's stop threshold; that call was not run.
	// Result.Err is a *StopError naming the tool and the count.
	ReasonRepeatedToolCalls Reason = "repeated_tool_calls"
	// ReasonMaxBudget: the session's spend cap was reached (Loop.Budget).
	// The response that crossed it is the last assistant turn; Result.Err is
	// a *StopError carrying the cap notice.
	ReasonMaxBudget Reason = "max_budget"
	// ReasonToolDeferred: a PreToolUse hook deferred tool calls of the round
	// (Loop.AllowDefer). The last message is the assistant turn with the
	// round still unanswered; Result.DeferredRound holds the deferred calls
	// and the other calls' results (see defercall.go).
	ReasonToolDeferred Reason = "tool_deferred"
	// ReasonToolDeferredUnavailable: a resumed deferred call's tool is no
	// longer available (an MCP server that is not connected, say); the call
	// is still pending and no model request was made. The conversation layer
	// reports it; the loop never does.
	ReasonToolDeferredUnavailable Reason = "tool_deferred_unavailable"
)

// Result is the terminal outcome of a run: the stop reason, the final message
// list (initial + every assistant and tool-result turn appended along the way),
// the number of model rounds taken, and any underlying error.
type Result struct {
	Reason    Reason
	Messages  []Message
	TurnCount int
	Err       error
	// Deferred is the first call a PreToolUse hook deferred
	// (ReasonToolDeferred); DeferredRound holds every deferred call of the
	// round and the results of its other calls.
	Deferred      *ToolUse
	DeferredRound *DeferredRound
}

// trimSpace is a tiny ASCII-space trimmer kept local to avoid pulling strings
// into the type file; it trims the ASCII whitespace the checks care about.
func trimSpace(s string) string {
	start := 0
	for start < len(s) && isSpace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}
