// Package apiclient is the shared wire/data-model package for model
// conversations: the Message and ContentBlock shapes (text, tool_use,
// tool_result, thinking, ...), the content-block and stop-reason constants,
// and the token Usage record. It is used by internal/agent, compaction, cost,
// conversation and provider.
//
// It contains no network client: model access goes exclusively through the
// provider-key-backed clients in internal/provider (OpenRouter, Anthropic,
// OpenAI). The shapes mirror the Anthropic Messages content-block wire format.
package apiclient

import "encoding/json"

// ----------------------------------------------------------------------------
// Message + content-block shapes (both request input and response output).
// ----------------------------------------------------------------------------

// Message is one conversation turn. Role is "user" or "assistant".
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
	// ID is the API response id of an assistant turn. It is
	// local bookkeeping — compaction groups messages by API round with it
	// — and never sent on the wire.
	ID string `json:"-"`
}

// Content-block type discriminants, as they appear on the wire.
const (
	BlockText             = "text"
	BlockToolUse          = "tool_use"
	BlockServerToolUse    = "server_tool_use"
	BlockMCPToolUse       = "mcp_tool_use"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
	BlockToolResult       = "tool_result"
	BlockImage            = "image"
)

// ContentBlock is the union of content-block variants carried in a Message's
// content array. A single tagged struct
// (discriminated by Type) is used so (de)serialization stays faithful to the
// wire while remaining ergonomic in Go; fields not relevant to a given Type are
// omitted on the wire.
//
// This is the one content-block type shared across gocode: the agent turn
// loop aliases it (see internal/agent) so a single type serves both the wire
// and the loop. tool_result is a first-class Anthropic content block — it is
// carried in the user turns the loop sends back after executing tools — so
// representing it here keeps the wire type a faithful, complete model of the
// request/response content array rather than polluting it. Every tool_result
// field is `omitempty`, so blocks of the other types serialize byte-for-byte as
// before.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text      string     `json:"text,omitempty"`
	Citations []Citation `json:"citations,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	// tool_use / server_tool_use / mcp_tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (input-only: produced by the turn loop, never emitted by the
	// model). ToolUseID references the tool_use block being answered.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content is the tool's textual result. The wire also allows a structured
	// content array; the mappers emit a string for the common path,
	// which is what we model here.
	Content string `json:"content,omitempty"`
	// IsError marks a failed/denied tool call.
	IsError bool `json:"is_error,omitempty"`
	// Images, when non-empty, are image parts the tool produced alongside its
	// text (e.g. a Read of an image or PDF page). They travel with the
	// tool_result as its structured content; providers encode them into their
	// own wire shape (an Anthropic image block, an OpenAI image_url data URL).
	// A nil slice (the default) means a text-only tool_result, serialized
	// byte-for-byte as before — every other block type leaves this empty.
	Images []ToolResultImage `json:"tool_result_images,omitempty"`

	// image (input-only: a user turn's attached image). Source is the
	// Anthropic image source, {type:"base64",media_type,data}; providers
	// encode it into their own image part.
	Source *ImageSource `json:"source,omitempty"`

	// LoadedTools names deferred tools whose full definitions this block
	// loads into the conversation: the matches of a ToolSearch tool_result,
	// or (on a compact summary) the tools loaded before the compaction. A
	// provider with native deferred loading sends a tool_result's names as
	// tool_reference blocks; every other block type ignores it on the wire.
	LoadedTools []string `json:"loaded_tools,omitempty"`
	// AnnouncedTools, on the text block of a deferred-tools reminder, is the
	// full set of deferred tool names announced to the model as of that
	// reminder. It is bookkeeping only and never sent to a provider.
	AnnouncedTools []string `json:"announced_tools,omitempty"`
}

// ToolResultImage is one image attached to a tool_result: base64-encoded image
// data plus its media type (e.g. "image/png"). It mirrors the structured image
// content the models consume — {type:"image",source:{type:"base64",media_type,
// data}} for Anthropic, {type:"image_url",image_url:{url:"data:<mime>;base64,
// <data>"}} for OpenAI/OpenRouter — in a provider-neutral form the request
// assembly carries through and each provider encodes.
type ToolResultImage struct {
	Base64    string `json:"data"`
	MediaType string `json:"media_type"`
}

// ImageSource is the source of a user image block: base64 data (Type
// "base64") or a URL (Type "url").
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Citation is a text-block citation, appended by citations_delta events.
type Citation struct {
	Type           string `json:"type,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	DocumentIndex  int    `json:"document_index,omitempty"`
	DocumentTitle  string `json:"document_title,omitempty"`
	StartCharIndex int    `json:"start_char_index,omitempty"`
	EndCharIndex   int    `json:"end_char_index,omitempty"`
}

// Stop reasons returned by the model.
const (
	StopEndTurn              = "end_turn"
	StopMaxTokens            = "max_tokens"
	StopStopSequence         = "stop_sequence"
	StopToolUse              = "tool_use"
	StopPauseTurn            = "pause_turn"
	StopRefusal              = "refusal"
	StopModelContextExceeded = "model_context_window_exceeded"
)

// Usage is the token-usage accounting.
type Usage struct {
	InputTokens              int             `json:"input_tokens,omitempty"`
	OutputTokens             int             `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens,omitempty"`
	ServerToolUse            json.RawMessage `json:"server_tool_use,omitempty"`
	OutputTokensDetails      json.RawMessage `json:"output_tokens_details,omitempty"`

	// The fields below are local accounting attached by the provider layer;
	// they are never part of a request or response body.

	// CacheCreation1hInputTokens is the part of CacheCreationInputTokens
	// written to the 1-hour cache.
	CacheCreation1hInputTokens int `json:"-"`
	// Provider is the registry id of the provider that served the response.
	Provider string `json:"-"`
	// ReportedCostUSD is the amount the provider reported charging for the
	// response; valid only when CostReported is set.
	ReportedCostUSD float64 `json:"-"`
	CostReported    bool    `json:"-"`
}
