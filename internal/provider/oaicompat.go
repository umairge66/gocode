package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This file holds the OpenAI chat-completions wire shared by every
// OpenAI-compatible provider (OpenRouter, OpenAI). Each concrete provider
// supplies its endpoint, auth/attribution headers and a couple of body options
// via oaiCompatClient; the request building, response parsing and SSE stream
// accumulation live here exactly once. The registry's OpenAI-compatible
// providers (compatible.go) use the same client, parameterized by their Spec.

// ----------------------------------------------------------------------------
// OpenAI chat-completions wire shapes.
// ----------------------------------------------------------------------------

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools,omitempty"`
	// MaxTokens is the legacy cap (OpenRouter). MaxCompletionTokens is the
	// current OpenAI cap, required by reasoning models (o-series, gpt-5). Only
	// one of the two is ever set.
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *oaiStreamOptions `json:"stream_options,omitempty"`
	// ReasoningEffort is OpenAI's top-level reasoning_effort; Reasoning is
	// OpenRouter's {"effort":…} object. At most one is set, and neither when
	// no effort was requested.
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Reasoning       *oaiReasoning `json:"reasoning,omitempty"`
	// EnableThinking is Alibaba Qwen's enable_thinking, set only when an
	// effort was requested on a ReasoningEnableThinking provider.
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

// oaiReasoning is OpenRouter's unified reasoning object.
type oaiReasoning struct {
	Effort string `json:"effort"`
}

// oaiStreamOptions asks the server to emit a final usage chunk during streaming
// (OpenAI/OpenRouter only report streamed usage when include_usage is set).
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role string `json:"role"`
	// Content is `any`: a bare string for the normal path (text content,
	// serialized exactly as before) or an array of oaiContentPart for a
	// multimodal user message (image_url parts). omitempty drops a nil Content,
	// matching the prior string-with-omitempty behavior.
	Content any `json:"content,omitempty"`
	// ToolCalls is set on assistant turns that requested tools.
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on tool turns, referencing the assistant call answered.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// oaiContentPart is one element of a multimodal content array (OpenAI chat): a
// text part or an image_url part carrying a data URL.
type oaiContentPart struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	ImageURL     *oaiImageURL     `json:"image_url,omitempty"`
	CacheControl *oaiCacheControl `json:"cache_control,omitempty"`
}

// oaiCacheControl is the Anthropic-style prompt-cache breakpoint OpenRouter
// passes through on a content part for Anthropic models.
type oaiCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// oaiImageURL is the image_url payload: a data: URL (data:<mime>;base64,<data>).
type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"` // always "function"
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string (OpenAI encodes the arguments object as
	// a string, not a nested object).
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string          `json:"type"` // always "function"
	Function oaiToolFunction `json:"function"`
}

type oaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Non-streaming response.
type oaiResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
}

type oaiChoice struct {
	Message      oaiRespMessage `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type oaiRespMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	ToolCalls []oaiToolCall `json:"tool_calls"`
}

// Streaming chunk (one SSE `data:` event).
type oaiStreamChunk struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	// Error is a mid-stream failure: OpenRouter ends a 200 stream with
	// {"error":{"code":502,"message":"…"}} (often beside a choice whose
	// finish_reason is "error") when the upstream provider fails.
	Error *struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning is OpenRouter's (and some OpenAI-compatible servers')
			// streamed reasoning text; ReasoningContent is the DeepSeek / vLLM
			// spelling; ReasoningDetails is OpenRouter's structured form, which
			// repeats the same text as Reasoning.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ReasoningDetails []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Summary string `json:"summary"`
			} `json:"reasoning_details"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
	// XGroq carries Groq's streamed usage on the final chunk
	// (x_groq.usage) when the standard usage field is absent.
	XGroq *struct {
		Usage *oaiUsage `json:"usage"`
	} `json:"x_groq"`
}

// ----------------------------------------------------------------------------
// Request building.
// ----------------------------------------------------------------------------

// oaiBodyOptions carries the per-provider differences in the request body.
type oaiBodyOptions struct {
	// maxCompletionTokens sends the cap as max_completion_tokens (OpenAI)
	// instead of max_tokens (OpenRouter).
	maxCompletionTokens bool
	// effort selects how ChatRequest.Effort is encoded (clamped to
	// low/medium/high on both wires).
	effort oaiEffortStyle
	// cacheControl honours ChatRequest.CacheSystem for Anthropic models
	// (OpenRouter): the system message becomes a text part carrying
	// cache_control {"type":"ephemeral"}.
	cacheControl bool
	// noStreamUsage omits stream_options on streaming requests (servers that
	// reject it).
	noStreamUsage bool
	// extraBody are fixed top-level fields merged into the body (never
	// overwriting a field the request set).
	extraBody map[string]any
}

// oaiEffortStyle is the per-provider reasoning-effort encoding.
type oaiEffortStyle int

const (
	// oaiEffortNone drops effort (no reasoning field is sent).
	oaiEffortNone oaiEffortStyle = iota
	// oaiEffortTopLevel sends "reasoning_effort":"<level>" (OpenAI).
	oaiEffortTopLevel
	// oaiEffortObject sends "reasoning":{"effort":"<level>"} (OpenRouter).
	oaiEffortObject
	// oaiEffortEnableThinking sends "enable_thinking":true (Alibaba Qwen).
	oaiEffortEnableThinking
)

// isAnthropicModel reports whether an OpenAI-compatible model id names an
// Anthropic (Claude) model, e.g. "anthropic/claude-sonnet-4".
func isAnthropicModel(model string) bool {
	m := strings.ToLower(WireModelID(model))
	return strings.HasPrefix(m, "anthropic/") || strings.Contains(m, "claude")
}

// buildOAIBody marshals a ChatRequest into the OpenAI chat-completions body.
// The ChatMessage shape is already OpenAI-aligned, so this is a near-direct
// field copy; tool_calls are tagged type:"function" and tool schemas wrapped as
// {type:"function",function:{...}}.
func buildOAIBody(req ChatRequest, stream bool, opts oaiBodyOptions) ([]byte, error) {
	body := oaiRequest{
		Model:       WireModelID(req.Model),
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if opts.maxCompletionTokens {
		body.MaxCompletionTokens = req.MaxTokens
	} else {
		body.MaxTokens = req.MaxTokens
	}
	if stream && !opts.noStreamUsage {
		body.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	if level := clampOpenAIEffort(req.Effort); level != "" {
		switch opts.effort {
		case oaiEffortTopLevel:
			body.ReasoningEffort = level
		case oaiEffortObject:
			body.Reasoning = &oaiReasoning{Effort: level}
		case oaiEffortEnableThinking:
			on := true
			body.EnableThinking = &on
		}
	}

	body.Messages = make([]oaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := oaiMessage{
			Role:       m.Role,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// A bare string content, omitted when empty (matches the prior
		// string-with-omitempty behavior; a nil `any` is dropped).
		if m.Content != "" {
			om.Content = m.Content
		}
		if m.Role == RoleSystem && m.Content != "" && opts.cacheControl && req.CacheSystem && isAnthropicModel(req.Model) {
			om.Content = []oaiContentPart{{Type: "text", Text: m.Content, CacheControl: &oaiCacheControl{Type: "ephemeral"}}}
		}
		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]oaiToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				args := tc.Arguments
				if args == "" {
					args = "{}"
				}
				om.ToolCalls[j] = oaiToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: oaiFunction{Name: tc.Name, Arguments: args},
				}
			}
		}
		body.Messages = append(body.Messages, om)

		// The classic OpenAI tool role (role:"tool") cannot carry image parts,
		// so images a tool produced are delivered as a FOLLOW-UP user message of
		// image_url data-URL parts, right after the tool message that carries
		// the text. A model that cannot see images never reaches here: the
		// adapter has already degraded to a text note (ImagesUnsupported).
		if m.Role == RoleTool && len(m.Images) > 0 {
			parts := make([]oaiContentPart, 0, len(m.Images))
			for _, im := range m.Images {
				parts = append(parts, oaiContentPart{
					Type:     "image_url",
					ImageURL: &oaiImageURL{URL: "data:" + im.MediaType + ";base64," + im.Base64},
				})
			}
			body.Messages = append(body.Messages, oaiMessage{Role: RoleUser, Content: parts})
		}
	}

	if len(req.Tools) > 0 {
		body.Tools = make([]oaiTool, len(req.Tools))
		for i, t := range req.Tools {
			body.Tools[i] = oaiTool{
				Type: "function",
				Function: oaiToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			}
		}
	}

	raw, err := json.Marshal(body)
	if err != nil || len(opts.extraBody) == 0 {
		return raw, err
	}
	return mergeExtraBody(raw, opts.extraBody)
}

// mergeExtraBody adds extra's fields to the JSON object raw, keeping any
// field raw already has.
func mergeExtraBody(raw []byte, extra map[string]any) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := m[k]; ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		m[k] = b
	}
	return json.Marshal(m)
}

// ----------------------------------------------------------------------------
// Transport.
// ----------------------------------------------------------------------------

// oaiCompatClient is the shared OpenAI-compatible transport. A concrete
// provider builds one per call with its endpoint, transport and headers.
type oaiCompatClient struct {
	// name prefixes error messages ("provider/<name>: ...").
	name     string
	endpoint string
	doer     Doer
	// setHeaders adds the provider's auth / attribution headers.
	setHeaders func(h http.Header)
	opts       oaiBodyOptions
}

func (c *oaiCompatClient) errf(format string, args ...any) error {
	return fmt.Errorf("provider/"+c.name+": "+format, args...)
}

// post marshals req, sends it and returns the 2xx response (caller closes).
func (c *oaiCompatClient) post(ctx context.Context, req ChatRequest, stream bool) (*http.Response, error) {
	payload, err := buildOAIBody(req, stream, c.opts)
	if err != nil {
		return nil, c.errf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, c.errf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if c.setHeaders != nil {
		c.setHeaders(httpReq.Header)
	}
	doer := c.doer
	if doer == nil {
		doer = defaultHTTPClient
	}
	resp, err := doer.Do(httpReq)
	if err != nil {
		// Classify dial/TLS/timeout failures into a friendly network error
		// (context.Canceled from a user interrupt passes through unchanged).
		return nil, doError(c.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, httpError(c.name, resp)
	}
	return resp, nil
}

// chat performs one non-streaming completion.
func (c *oaiCompatClient) chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := c.post(ctx, req, false)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	var parsed oaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ChatResponse{}, c.errf("decode response: %w", err)
	}

	out := ChatResponse{ID: parsed.ID, Model: parsed.Model}
	if parsed.Usage != nil {
		out.Usage = parsed.Usage.chatUsage()
	}
	if len(parsed.Choices) > 0 {
		ch := parsed.Choices[0]
		out.Content = ch.Message.Content
		out.FinishReason = ch.FinishReason
		for _, tc := range ch.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ChatToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return out, nil
}

// streamChat performs one streaming completion.
func (c *oaiCompatClient) streamChat(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResponse, error) {
	resp, err := c.post(ctx, req, true)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	return accumulateOAIStream(c.name, resp.Body, onDelta, req.OnReasoning)
}

// httpError lives in errors.go: it classifies a non-2xx response into a typed,
// user-facing *APIError. It is shared by every request path in this package.

// ----------------------------------------------------------------------------
// Stream accumulation.
// ----------------------------------------------------------------------------

// toolCallAccumulator collects an incrementally-streamed tool call keyed by its
// choices[].delta.tool_calls[].index.
type toolCallAccumulator struct {
	id   string
	name string
	args strings.Builder
}

// newSSEScanner returns a line scanner sized for long SSE data lines (a chunk
// may carry a large tool-argument fragment).
func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return scanner
}

// reasoningDelta picks a chunk's reasoning text. OpenRouter sends the same text
// both as `reasoning` and inside `reasoning_details`, so the plain field wins
// and the details are used only when it is absent (taking reasoning.text and
// reasoning.summary entries; encrypted entries carry no readable text).
func reasoningDelta(reasoning, reasoningContent string, details []struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
}) string {
	if reasoning != "" {
		return reasoning
	}
	if reasoningContent != "" {
		return reasoningContent
	}
	var b strings.Builder
	for _, d := range details {
		switch {
		case d.Text != "":
			b.WriteString(d.Text)
		case d.Summary != "":
			b.WriteString(d.Summary)
		}
	}
	return b.String()
}

// accumulateOAIStream folds an OpenAI-compatible SSE stream into a
// ChatResponse. It reads the `data:` lines, accumulates
// choices[].delta.content and the per-index delta.tool_calls[].function.arguments,
// forwards each text delta to onDelta, and stops on `data: [DONE]`.
func accumulateOAIStream(name string, r io.Reader, onDelta, onReasoning func(delta string)) (ChatResponse, error) {
	var (
		content      strings.Builder
		finishReason string
		model        string
		respID       string
		usage        *ChatUsage
		order        []int
		calls        = map[int]*toolCallAccumulator{}
	)

	scanner := newSSEScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		// SSE frames are separated by blank lines; comments begin with ':'.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Ignore non-data fields (event:, id:, retry:).
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return ChatResponse{}, fmt.Errorf("provider/%s: decode stream chunk: %w", name, err)
		}
		if chunk.Error != nil && (chunk.Error.Message != "" || len(chunk.Error.Code) > 0) {
			return ChatResponse{}, oaiStreamError(name, chunk.Error.Code, chunk.Error.Message)
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.ID != "" && respID == "" {
			respID = chunk.ID
		}
		if cu := chunk.Usage; cu != nil || (chunk.XGroq != nil && chunk.XGroq.Usage != nil) {
			if cu == nil {
				cu = chunk.XGroq.Usage
			}
			u := cu.chatUsage()
			usage = &u
		}
		for _, ch := range chunk.Choices {
			if onReasoning != nil {
				if r := reasoningDelta(ch.Delta.Reasoning, ch.Delta.ReasoningContent, ch.Delta.ReasoningDetails); r != "" {
					onReasoning(r)
				}
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(ch.Delta.Content)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc := calls[tc.Index]
				if acc == nil {
					acc = &toolCallAccumulator{}
					calls[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
				}
			}
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("provider/%s: read stream: %w", name, err)
	}

	out := ChatResponse{
		ID:           respID,
		Model:        model,
		Content:      content.String(),
		FinishReason: finishReason,
	}
	if usage != nil {
		out.Usage = *usage
	}
	for _, idx := range order {
		acc := calls[idx]
		out.ToolCalls = append(out.ToolCalls, ChatToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: acc.args.String(),
		})
	}
	return out, nil
}
