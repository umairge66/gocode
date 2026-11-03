package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/umairge66/gocode/internal/toolsearch"
)

// Anthropic Messages API defaults.
const (
	anthropicBaseURL          = "https://api.anthropic.com"
	anthropicDefaultVersion   = "2023-06-01"
	anthropicDefaultMaxTokens = 8192
)

// Anthropic is a Provider for the Anthropic Messages API
// (POST {BaseURL}/v1/messages). It is self-contained (it does not use
// internal/apiclient) and authenticates with a provider API key sent as
// x-api-key — no OAuth, no gateway.
//
// Translation from the neutral (OpenAI-shaped) chat:
//   - system messages      -> top-level "system" string (joined by blank lines)
//   - assistant ToolCalls  -> {type:"tool_use",id,name,input} after any text
//   - role:"tool" messages -> a user turn of {type:"tool_result",...} blocks
//   - consecutive same-role turns are merged (the API requires alternation),
//     with tool_result blocks first in a merged user turn
//   - tools                -> {name,description,input_schema}
//   - max_tokens is required (8192 when unset)
type Anthropic struct {
	// APIKey is sent as "x-api-key" (required).
	APIKey string
	// BaseURL is the API root. Empty means https://api.anthropic.com; tests
	// point it at an httptest.Server. "/v1/messages" is appended.
	BaseURL string
	// Version is the anthropic-version header. Empty means "2023-06-01".
	Version string
	// HTTP is the transport. Nil means the credential-safe default client
	// (no cross-origin redirects; see baseurl.go).
	HTTP Doer
	// NativeToolSearch sends deferred tools with defer_loading and ToolSearch
	// results as tool_reference blocks (see buildAnthropicBody). The registry
	// sets it for the real API endpoint, or for a custom base URL when
	// ENABLE_TOOL_SEARCH asks for it explicitly.
	NativeToolSearch bool

	// nativeRefused latches native deferred loading off once the API
	// rejected it.
	nativeRefused atomic.Bool
}

var _ Provider = (*Anthropic)(nil)

// Name identifies the provider.
func (a *Anthropic) Name() string { return "anthropic" }

// ----------------------------------------------------------------------------
// Messages API wire shapes.
// ----------------------------------------------------------------------------

type antRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is the system prompt: a bare string, or (CacheSystem) one text
	// block carrying cache_control. nil omits it.
	System      any          `json:"system,omitempty"`
	Messages    []antMessage `json:"messages"`
	Tools       []antTool    `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
	// OutputConfig carries the reasoning effort ({"effort":"high"}); nil (no
	// effort) omits it so the body is unchanged.
	OutputConfig *antOutputConfig `json:"output_config,omitempty"`
}

// antOutputConfig is the Messages API output_config object (effort only).
type antOutputConfig struct {
	Effort string `json:"effort"`
}

// antSystemBlock is a system prompt text block (used when it carries a
// prompt-cache breakpoint).
type antSystemBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	CacheControl *antCacheControl `json:"cache_control,omitempty"`
}

// antCacheControl is the prompt-cache breakpoint {"type":"ephemeral"}.
type antCacheControl struct {
	Type string `json:"type"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

// antBlock is a request content block (text / tool_use / tool_result).
//
// Content holds the tool_result payload. It is an `any` so it can be either a
// bare string (text-only tool_result — the common path, serialized exactly as
// before) or an array of antToolResultPart (text + image blocks) when a tool
// produced images. omitempty drops a nil Content, matching the prior
// string-with-omitempty behavior for an empty tool_result.
type antBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *antImageSource `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
}

// antToolResultPart is one element of a structured tool_result content array:
// a text part, or an image part {type:"image",source:{type:"base64",
// media_type,data}}.
type antToolResultPart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source *antImageSource `json:"source,omitempty"`
}

// antImageSource is the base64 image source of an image content block.
type antImageSource struct {
	Type      string `json:"type"` // "base64" or "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// antUserImage encodes a user-attached image block.
func antUserImage(im ChatImage) antBlock {
	if im.URL != "" && im.Base64 == "" {
		return antBlock{Type: "image", Source: &antImageSource{Type: "url", URL: im.URL}}
	}
	return antBlock{Type: "image", Source: &antImageSource{Type: "base64", MediaType: im.MediaType, Data: im.Base64}}
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// DeferLoading keeps the definition out of the prompt until a
	// tool_reference block in a tool_result loads it.
	DeferLoading bool `json:"defer_loading,omitempty"`
}

// antToolReference is a tool_result content block that loads a deferred
// tool's definition.
type antToolReference struct {
	Type     string `json:"type"` // "tool_reference"
	ToolName string `json:"tool_name"`
}

// antUsage is the Messages API usage object. input_tokens is the uncached
// input only; cache_read_input_tokens and cache_creation_input_tokens are
// disjoint from it, and cache_creation splits the writes by cache TTL.
type antUsage struct {
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	CacheReadInputTokens     int               `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int               `json:"cache_creation_input_tokens"`
	CacheCreation            *antCacheCreation `json:"cache_creation,omitempty"`
}

// antCacheCreation is usage.cache_creation.
type antCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

func (u antUsage) chatUsage() ChatUsage {
	out := ChatUsage{
		PromptTokens:        u.InputTokens,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	}
	if u.CacheCreation != nil {
		out.CacheCreation1hTokens = min(u.CacheCreation.Ephemeral1hInputTokens, u.CacheCreationInputTokens)
	}
	return out
}

// antRespBlock is a response content block.
type antRespBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type antResponse struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Content    []antRespBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      antUsage       `json:"usage"`
}

// antStreamEvent is the union of every SSE event payload the Messages API sends.
type antStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string   `json:"id"`
		Model string   `json:"model"`
		Usage antUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *antRespBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens              *int              `json:"input_tokens"`
		OutputTokens             *int              `json:"output_tokens"`
		CacheReadInputTokens     *int              `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int              `json:"cache_creation_input_tokens"`
		CacheCreation            *antCacheCreation `json:"cache_creation"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ----------------------------------------------------------------------------
// Request building.
// ----------------------------------------------------------------------------

// antToolInput converts an OpenAI-style arguments string into a tool_use input
// object, falling back to {} when it is empty or not a JSON object.
func antToolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// buildAnthropicBody translates a neutral ChatRequest into a Messages API body.
//
// native selects native deferred loading: a ToolSearch result's loaded names
// become tool_reference blocks, and every deferred tool the request carries —
// loaded through such a block, or not loaded yet — is sent with
// defer_loading, after the always-loaded tools and sorted by name, so the
// tools array stays the same from request to request. A deferred tool that is
// loaded but has no tool_reference in the request (it was loaded before a
// compaction) is sent as a plain definition. Without native, the tools are
// sent exactly as the request lists them and a ToolSearch result is its text.
func buildAnthropicBody(req ChatRequest, stream, native bool) ([]byte, error) {
	body := antRequest{
		Model:       WireModelID(req.Model),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = anthropicDefaultMaxTokens
	}
	if req.Effort != "" {
		body.OutputConfig = &antOutputConfig{Effort: req.Effort}
	}

	// tool_reference blocks may only name tools the request defines.
	defined := map[string]bool{}
	if native {
		for _, t := range req.Tools {
			defined[t.Name] = true
		}
		for _, t := range req.DeferredTools {
			defined[t.Name] = true
		}
	}
	var system []string
	for _, m := range req.Messages {
		var (
			role   string
			blocks []antBlock
		)
		switch m.Role {
		case RoleSystem:
			if m.Content != "" {
				system = append(system, m.Content)
			}
			continue
		case RoleAssistant:
			role = RoleAssistant
			if m.Content != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, antBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: antToolInput(tc.Arguments),
				})
			}
		case RoleTool:
			role = RoleUser
			tr := antBlock{Type: "tool_result", ToolUseID: m.ToolCallID}
			var refs []antToolReference
			if native {
				for _, n := range m.LoadedTools {
					if defined[n] {
						refs = append(refs, antToolReference{Type: "tool_reference", ToolName: n})
					}
				}
			}
			if len(refs) > 0 {
				tr.Content = refs
			} else if len(m.Images) > 0 {
				// Structured content array: the text (if any) followed by one
				// image block per image.
				parts := make([]antToolResultPart, 0, len(m.Images)+1)
				if m.Content != "" {
					parts = append(parts, antToolResultPart{Type: "text", Text: m.Content})
				}
				for _, im := range m.Images {
					parts = append(parts, antToolResultPart{
						Type:   "image",
						Source: &antImageSource{Type: "base64", MediaType: im.MediaType, Data: im.Base64},
					})
				}
				tr.Content = parts
			} else if m.Content != "" {
				// Text-only: a bare string, exactly as before (empty stays nil
				// so omitempty drops it, matching the prior behavior).
				tr.Content = m.Content
			}
			blocks = append(blocks, tr)
		default: // user
			role = RoleUser
			if m.Content != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, im := range m.Images {
				blocks = append(blocks, antUserImage(im))
			}
		}
		if len(blocks) == 0 {
			// The API rejects empty content; an empty turn carries nothing.
			continue
		}
		if n := len(body.Messages); n > 0 && body.Messages[n-1].Role == role {
			body.Messages[n-1].Content = append(body.Messages[n-1].Content, blocks...)
			continue
		}
		body.Messages = append(body.Messages, antMessage{Role: role, Content: blocks})
	}
	// tool_result blocks must lead a user turn.
	for i := range body.Messages {
		if body.Messages[i].Role != RoleUser {
			continue
		}
		c := body.Messages[i].Content
		sort.SliceStable(c, func(a, b int) bool {
			return c[a].Type == "tool_result" && c[b].Type != "tool_result"
		})
	}
	if body.Messages == nil {
		body.Messages = []antMessage{}
	}
	if sys := strings.Join(system, "\n\n"); sys != "" {
		if req.CacheSystem {
			body.System = []antSystemBlock{{Type: "text", Text: sys, CacheControl: &antCacheControl{Type: "ephemeral"}}}
		} else {
			body.System = sys
		}
	}

	toAnt := func(t ChatTool, deferLoading bool) antTool {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		return antTool{Name: t.Name, Description: t.Description, InputSchema: schema, DeferLoading: deferLoading}
	}
	if !native {
		for _, t := range req.Tools {
			body.Tools = append(body.Tools, toAnt(t, false))
		}
		return json.Marshal(body)
	}
	referenced := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			for _, n := range m.LoadedTools {
				if defined[n] {
					referenced[n] = true
				}
			}
		}
	}
	var deferred []antTool
	for _, t := range req.Tools {
		if t.Deferred && referenced[t.Name] {
			deferred = append(deferred, toAnt(t, true))
			continue
		}
		body.Tools = append(body.Tools, toAnt(t, false))
	}
	for _, t := range req.DeferredTools {
		deferred = append(deferred, toAnt(t, true))
	}
	sort.SliceStable(deferred, func(i, j int) bool { return deferred[i].Name < deferred[j].Name })
	body.Tools = append(body.Tools, deferred...)
	return json.Marshal(body)
}

// hasDeferral reports whether the request defers any tool.
func hasDeferral(req ChatRequest) bool {
	if len(req.DeferredTools) > 0 {
		return true
	}
	for _, t := range req.Tools {
		if t.Deferred {
			return true
		}
	}
	return false
}

// useNativeToolSearch reports whether req is sent with native deferred
// loading: the provider allows it, the API has not refused it this session,
// the request defers tools, and the model reads tool_reference blocks.
func (a *Anthropic) useNativeToolSearch(req ChatRequest) bool {
	return a.NativeToolSearch && !a.nativeRefused.Load() && hasDeferral(req) &&
		toolsearch.ModelSupportsToolReference(req.Model)
}

// isToolSearchRefusal reports whether err is the API rejecting the native
// deferred-loading request shape.
func isToolSearchRefusal(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	hay := strings.ToLower(ae.Detail + " " + ae.Message)
	for _, k := range []string{"tool_reference", "defer_loading", "tool search", "tool_search", toolsearch.NativeBeta} {
		if strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// anthropicFinish maps a Messages API stop_reason onto the neutral finish reason.
func anthropicFinish(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence", "pause_turn":
		return FinishStop
	case "tool_use":
		return FinishToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return FinishLength
	case "refusal":
		return FinishContentFilter
	default:
		return stop
	}
}

// ----------------------------------------------------------------------------
// Transport.
// ----------------------------------------------------------------------------

// post sends req. A request using native deferred loading that the API
// rejects for its shape is sent once more without it, and native loading
// stays off for the rest of the session (the conversation's tool lists then
// carry every loaded definition).
func (a *Anthropic) post(ctx context.Context, req ChatRequest, stream bool) (*http.Response, error) {
	native := a.useNativeToolSearch(req)
	resp, err := a.postShape(ctx, req, stream, native)
	if native && isToolSearchRefusal(err) {
		a.nativeRefused.Store(true)
		return a.postShape(ctx, req, stream, false)
	}
	return resp, err
}

// postShape sends req with or without native deferred loading.
func (a *Anthropic) postShape(ctx context.Context, req ChatRequest, stream, native bool) (*http.Response, error) {
	payload, err := buildAnthropicBody(req, stream, native)
	if err != nil {
		return nil, fmt.Errorf("provider/anthropic: marshal request: %w", err)
	}
	base := a.BaseURL
	if base == "" {
		base = anthropicBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("provider/anthropic: build request: %w", err)
	}
	version := a.Version
	if version == "" {
		version = anthropicDefaultVersion
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", version)
	if a.APIKey != "" {
		httpReq.Header.Set("x-api-key", a.APIKey)
	}
	if req.Effort != "" {
		// output_config.effort is gated behind the effort beta.
		addBetaHeader(httpReq.Header, anthropicEffortBeta)
	}
	if native {
		addBetaHeader(httpReq.Header, toolsearch.NativeBeta)
	}
	if HasContext1MSuffix(req.Model) {
		// The `[1m]` suffix is client-side: the 1M window is requested with the
		// context-1m beta (harmless on natively-1M models).
		addBetaHeader(httpReq.Header, anthropicContext1MBeta)
	}
	if stream {
		httpReq.Header.Set("accept", "text/event-stream")
	} else {
		httpReq.Header.Set("accept", "application/json")
	}

	var doer Doer = defaultHTTPClient
	if a.HTTP != nil {
		doer = a.HTTP
	}
	resp, err := doer.Do(httpReq)
	if err != nil {
		// Classify dial/TLS/timeout failures into a friendly network error
		// (context.Canceled from a user interrupt passes through unchanged).
		return nil, doError("anthropic", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, httpError("anthropic", resp)
	}
	return resp, nil
}

// Chat performs one non-streaming Messages API call.
func (a *Anthropic) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := a.post(ctx, req, false)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	var parsed antResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("provider/anthropic: decode response: %w", err)
	}
	out := ChatResponse{
		ID:           parsed.ID,
		Model:        parsed.Model,
		FinishReason: anthropicFinish(parsed.StopReason),
		Usage:        parsed.Usage.chatUsage(),
	}
	var text strings.Builder
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			out.ToolCalls = append(out.ToolCalls, ChatToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	out.Content = text.String()
	return out, nil
}

// antBlockAccumulator collects one streamed content block by index.
type antBlockAccumulator struct {
	typ   string
	text  strings.Builder
	id    string
	name  string
	input strings.Builder
	// startInput is the input object carried on content_block_start (usually
	// {}), used when no input_json_delta arrives.
	startInput json.RawMessage
}

// StreamChat performs one streaming Messages API call, folding the SSE events
// (message_start, content_block_start/delta/stop, message_delta, message_stop,
// ping, error) into a ChatResponse and forwarding text deltas to onDelta.
func (a *Anthropic) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string)) (ChatResponse, error) {
	resp, err := a.post(ctx, req, true)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	var (
		out    ChatResponse
		usage  antUsage
		order  []int
		blocks = map[int]*antBlockAccumulator{}
	)

	scanner := newSSEScanner(resp.Body)
scan:
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			// Blank separators, comments and event:/id:/retry: fields; the
			// event type is repeated inside the data payload.
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev antStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return ChatResponse{}, fmt.Errorf("provider/anthropic: decode stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				out.ID = ev.Message.ID
				out.Model = ev.Message.Model
				usage = ev.Message.Usage
			}
		case "content_block_start":
			acc := &antBlockAccumulator{}
			if cb := ev.ContentBlock; cb != nil {
				acc.typ = cb.Type
				acc.id = cb.ID
				acc.name = cb.Name
				acc.startInput = cb.Input
				if cb.Type == "text" && cb.Text != "" {
					acc.text.WriteString(cb.Text)
					if onDelta != nil {
						onDelta(cb.Text)
					}
				}
				if cb.Type == "tool_use" && req.OnToolUseStart != nil {
					req.OnToolUseStart(cb.ID, cb.Name)
				}
			}
			if _, seen := blocks[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			blocks[ev.Index] = acc
		case "content_block_delta":
			acc := blocks[ev.Index]
			if acc == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				acc.text.WriteString(ev.Delta.Text)
				if onDelta != nil && ev.Delta.Text != "" {
					onDelta(ev.Delta.Text)
				}
			case "input_json_delta":
				acc.input.WriteString(ev.Delta.PartialJSON)
				if req.OnToolInputDelta != nil && acc.typ == "tool_use" && ev.Delta.PartialJSON != "" {
					req.OnToolInputDelta(acc.id, ev.Delta.PartialJSON)
				}
			case "thinking_delta":
				// Shown live only; the block itself is assembled as before.
				if req.OnReasoning != nil && ev.Delta.Thinking != "" {
					req.OnReasoning(ev.Delta.Thinking)
				}
			}
		case "content_block_stop", "ping":
			// Nothing to do: blocks are finalized at the end.
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.FinishReason = anthropicFinish(ev.Delta.StopReason)
			}
			if u := ev.Usage; u != nil {
				if u.OutputTokens != nil {
					usage.OutputTokens = *u.OutputTokens
				}
				if u.InputTokens != nil {
					usage.InputTokens = *u.InputTokens
				}
				if u.CacheReadInputTokens != nil {
					usage.CacheReadInputTokens = *u.CacheReadInputTokens
				}
				if u.CacheCreationInputTokens != nil {
					usage.CacheCreationInputTokens = *u.CacheCreationInputTokens
				}
				if u.CacheCreation != nil {
					usage.CacheCreation = u.CacheCreation
				}
			}
		case "message_stop":
			break scan
		case "error":
			// A mid-stream error event (e.g. overloaded_error, rate_limit_error):
			// classify it into the same friendly, typed error as a status error so
			// a mid-turn failure surfaces actionable guidance rather than a raw
			// "overloaded_error: Overloaded" string.
			if ev.Error != nil {
				return ChatResponse{}, streamErrorFromType("anthropic", ev.Error.Type, ev.Error.Message)
			}
			return ChatResponse{}, fmt.Errorf("provider/anthropic: stream error: %s", data)
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("provider/anthropic: read stream: %w", err)
	}

	var text strings.Builder
	for _, idx := range order {
		acc := blocks[idx]
		switch acc.typ {
		case "text":
			text.WriteString(acc.text.String())
		case "tool_use":
			args := acc.input.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
				if len(acc.startInput) > 0 {
					args = string(acc.startInput)
				}
			}
			out.ToolCalls = append(out.ToolCalls, ChatToolCall{ID: acc.id, Name: acc.name, Arguments: args})
		}
	}
	out.Content = text.String()
	out.Usage = usage.chatUsage()
	return out, nil
}
