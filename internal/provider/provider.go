// Package provider defines a model-provider abstraction for gocode and ships
// three concrete providers: OpenRouter and OpenAI (which
// share the OpenAI chat-completions wire in oaicompat.go) and Anthropic (the
// Messages API, anthropic.go).
//
// The abstraction is deliberately transport- and vendor-neutral: a Provider
// speaks in this package's own ChatRequest/ChatResponse shapes (model,
// messages, tools, max_tokens, temperature, stream), and each concrete provider
// translates those to and from its own wire protocol. OpenRouter (openrouter.go)
// is an OpenAI-compatible Chat Completions endpoint, so its implementation
// builds the OpenAI chat-completions JSON.
//
// The bridge into the existing agent turn loop lives in adapter.go: a ModelCaller
// wraps a Provider plus a model id and translates between agent.ModelRequest /
// agent.ModelResponse (Anthropic-shaped content blocks: text / tool_use /
// tool_result) and this package's neutral chat shapes (OpenAI-shaped roles and
// tool_calls). That adapter is where the Anthropic<->OpenAI translation happens;
// nothing here touches internal/apiclient's Messages wire types.
//
// Scope rules honored here: provider API keys only (no OAuth, no gateway), no
// telemetry, and stdlib dependencies only (net/http, encoding/json, bufio).
package provider

import (
	"context"
	"encoding/json"
)

// Role values for ChatMessage. These mirror the OpenAI chat-completions roles
// because that is the lingua franca the first provider speaks; the adapter maps
// the agent loop's Anthropic roles onto them.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish-reason values a provider reports on ChatResponse. They mirror the
// OpenAI finish_reason union; the adapter maps them onto the agent loop's stop
// reasons.
const (
	FinishStop          = "stop"
	FinishToolCalls     = "tool_calls"
	FinishLength        = "length"
	FinishContentFilter = "content_filter"
)

// Provider is a model backend. It speaks in this package's neutral chat shapes;
// a concrete provider translates them to and from its own wire protocol.
type Provider interface {
	// Name returns a short, stable identifier for the provider (e.g.
	// "openrouter"). It is not a model id.
	Name() string

	// Chat performs one non-streaming completion and returns the full response.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// StreamChat performs one streaming completion. Each incremental assistant
	// text delta is forwarded to onDelta (which may be nil) as it arrives; the
	// returned ChatResponse is the fully accumulated result (content, tool
	// calls, finish reason and usage), identical in shape to what Chat returns.
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string)) (ChatResponse, error)
}

// ChatRequest is the provider-neutral request. It carries only what the agent
// loop needs today; a provider ignores fields it does not support.
type ChatRequest struct {
	// Model is the provider-specific model id (e.g. "openai/gpt-4o").
	Model string
	// Messages is the conversation, already flattened into role-tagged turns
	// (system / user / assistant / tool). The adapter produces these from the
	// agent loop's content-block messages.
	Messages []ChatMessage
	// Tools advertises the callable tools.
	Tools []ChatTool
	// MaxTokens caps the generated tokens. 0 means "unset" (omitted on the wire).
	MaxTokens int
	// Temperature is the sampling temperature; nil means unset.
	Temperature *float64
	// Stream requests a streaming response. The adapter sets this to match the
	// Provider method it calls; providers may also consult it.
	Stream bool
	// Effort is the reasoning-effort level (one of EffortLevels), or "" for
	// unset. Each provider encodes it on its own wire — Anthropic
	// output_config.effort (+ the effort beta), OpenAI reasoning_effort,
	// OpenRouter reasoning.effort (the latter two clamped to low/medium/high).
	// Empty sends nothing, so the request is byte-identical to one without it.
	Effort string
	// OnReasoning, when set on a streaming request, receives each incremental
	// reasoning/thinking delta (Anthropic thinking_delta; OpenAI-compatible
	// delta.reasoning / reasoning_content / reasoning_details). It is never sent
	// on the wire.
	OnReasoning func(delta string)
	// OnToolUseStart and OnToolInputDelta, when set on a streaming request,
	// are told each tool_use block as it starts (id, name) and each partial
	// JSON fragment of its input (Anthropic content_block_start tool_use /
	// input_json_delta). Providers that do not stream tool calls ignore them.
	// They are never sent on the wire.
	OnToolUseStart   func(id, name string)
	OnToolInputDelta func(id, partialJSON string)
	// CacheSystem marks the system prompt as a cacheable prefix
	// (agent.ModelRequest.CacheSystemPrompt). Anthropic sends cache_control
	// {"type":"ephemeral"} on the system block; OpenRouter does the same on the
	// system message's text part for an Anthropic model only. Every other
	// provider/model ignores it (their caching is automatic), so their body
	// is unchanged.
	CacheSystem bool
	// SystemCacheSplit, with CacheSystem, is the byte length of the stable
	// prefix of the system prompt (agent.ModelRequest.System) when trailing
	// text that may change follows it (SystemContext). A provider with
	// explicit breakpoints caches only that prefix and sends the rest as a
	// separate, uncached part; 0 caches the whole system prompt. Providers
	// that do not cache explicitly ignore it, so their body is unchanged.
	SystemCacheSplit int
	// CacheMessages asks for rolling prompt-cache breakpoints on the latest
	// messages (agent.ModelRequest.CacheMessages): Anthropic marks the last
	// block of the last message and of the previous user turn; Bedrock
	// Converse adds cachePoint blocks in the same two places (models with
	// prompt caching); OpenRouter marks the text of the trailing user or tool
	// message for an Anthropic model. Every other provider ignores it.
	CacheMessages bool
	// DeferredTools are the deferred tools the conversation has not loaded
	// (agent.ModelRequest.DeferredTools). Only a provider with native
	// deferred loading sends them (marked for on-demand loading); the others
	// send Tools alone.
	DeferredTools []ChatTool
}

// ChatMessage is one role-tagged turn in the neutral shape.
//
// The mapping to OpenAI is one-to-one:
//   - system/user: Content holds the text.
//   - assistant: Content holds any text, ToolCalls holds the requested calls.
//   - tool: Content holds the tool's result and ToolCallID references the
//     assistant tool call it answers.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ChatToolCall // assistant turns only
	ToolCallID string         // tool turns only: the call this answers
	Name       string         // optional function/tool name (tool turns)
	// Images, when non-empty, are image parts a tool produced (tool turns) or
	// the user attached (user turns). Each provider encodes them in its own
	// wire shape: Anthropic embeds a tool's images in the tool_result content
	// array; OpenAI/OpenRouter (whose tool role cannot hold images) deliver
	// them as a follow-up user message of image_url parts. A user turn's
	// images follow its text. Empty (the default) means a text-only turn,
	// encoded exactly as before.
	Images []ChatImage
	// LoadedTools, on a tool turn, names the deferred tools the result loads
	// (a ToolSearch result). A provider with native deferred loading sends
	// them as tool_reference blocks instead of Content; the others send
	// Content.
	LoadedTools []string
}

// ChatImage is one image part carried by a ChatMessage: base64-encoded data
// plus its media type. It is the provider-neutral form; each concrete provider
// encodes it into its own wire representation.
type ChatImage struct {
	Base64    string
	MediaType string
	// URL, set instead of Base64, is an image the provider fetches itself
	// (user images only; a provider that cannot fetch URLs sends a note).
	URL string
}

// ChatToolCall is one tool call requested by the assistant (or the neutral form
// of an OpenAI tool_call). Arguments is the raw JSON-encoded argument string, as
// OpenAI represents function arguments.
type ChatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatTool is one tool advertised to the model. Parameters is the JSON Schema
// for the tool's arguments.
type ChatTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	// Deferred marks a deferred tool the conversation has loaded (see
	// ChatRequest.DeferredTools); providers without native deferred loading
	// treat it like any other tool.
	Deferred bool
}

// ChatResponse is the provider-neutral response.
type ChatResponse struct {
	// ID is the provider's response id (Anthropic message id, OpenAI-compatible
	// completion id), "" when absent. It becomes the assistant turn's
	// agent.Message.ID.
	ID string
	// Model is the model id the provider reports having served the request.
	Model string
	// Content is the assistant's text (possibly empty when only tools were called).
	Content string
	// ToolCalls are the assistant's requested tool calls, in order.
	ToolCalls []ChatToolCall
	// FinishReason is the neutral finish reason (stop / tool_calls / length /
	// content_filter), passed through from the provider.
	FinishReason string
	// Usage is the token accounting for the turn.
	Usage ChatUsage
}

// ChatUsage is the neutral token accounting of one response, normalised to
// the Anthropic split whatever the wire format was: PromptTokens,
// CacheReadTokens and CacheCreationTokens are disjoint parts of the input, and
// CompletionTokens counts every generated token (reasoning included). Each
// wire parser converts its provider's fields (OpenAI prompt_tokens with
// prompt_tokens_details.cached_tokens, DeepSeek prompt_cache_hit_tokens /
// prompt_cache_miss_tokens, Bedrock cacheReadInputTokens, …) into this shape.
type ChatUsage struct {
	// PromptTokens is the uncached input token count (Anthropic
	// input_tokens; OpenAI prompt_tokens minus the cached and cache-written
	// tokens it includes).
	PromptTokens int
	// CompletionTokens is the output token count, reasoning included.
	CompletionTokens int
	// TotalTokens is the provider's total (input of every kind + output).
	TotalTokens int
	// CacheReadTokens / CacheCreationTokens are the input tokens served from /
	// written to the provider's prompt cache.
	CacheReadTokens     int
	CacheCreationTokens int
	// CacheCreation1hTokens is the part of CacheCreationTokens written to a
	// 1-hour cache (Anthropic cache_creation.ephemeral_1h_input_tokens).
	CacheCreation1hTokens int
	// ReasoningTokens is the part of CompletionTokens spent on reasoning
	// (informational).
	ReasoningTokens int
	// CostUSD is the amount the provider reported charging for the request
	// (OpenRouter usage.cost, xAI usage.cost_in_usd_ticks); valid only when
	// CostReported is set.
	CostUSD      float64
	CostReported bool
}

// InputTotal is every input token of the response (uncached + cache read +
// cache write).
func (u ChatUsage) InputTotal() int {
	return u.PromptTokens + u.CacheReadTokens + u.CacheCreationTokens
}
