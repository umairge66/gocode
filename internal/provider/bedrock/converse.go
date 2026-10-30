package bedrock

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/provider"
)

// This file translates between the provider's neutral chat shapes and the
// Bedrock Runtime Converse / ConverseStream JSON:
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html
//
//   - system messages      -> top-level "system": [{text}] (+ {cachePoint}
//     when CacheSystem is set and the model supports prompt caching)
//   - assistant ToolCalls  -> {toolUse:{toolUseId,name,input}} after any text
//   - role:"tool" messages -> a user turn of {toolResult:{toolUseId,content}}
//     blocks (text, plus {image} blocks for tool-produced images)
//   - consecutive same-role turns are merged (Converse requires alternation),
//     with toolResult blocks first in a merged user turn
//   - tools                -> toolConfig.tools[].toolSpec{name,description,
//     inputSchema:{json}}
//   - max_tokens / temperature -> inferenceConfig
//   - Effort (Claude models) -> additionalModelRequestFields.output_config.effort
//   - `[1m]` suffix (Claude)  -> additionalModelRequestFields.anthropic_beta

const (
	defaultMaxTokens = 8192
	context1MBeta    = "context-1m-2025-08-07"
)

type cvRequest struct {
	Messages                     []cvMessage     `json:"messages"`
	System                       []cvSystemBlock `json:"system,omitempty"`
	InferenceConfig              *cvInference    `json:"inferenceConfig,omitempty"`
	ToolConfig                   *cvToolConfig   `json:"toolConfig,omitempty"`
	AdditionalModelRequestFields map[string]any  `json:"additionalModelRequestFields,omitempty"`
}

type cvInference struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

type cvCachePoint struct {
	Type string `json:"type"`
}

type cvSystemBlock struct {
	Text       string        `json:"text,omitempty"`
	CachePoint *cvCachePoint `json:"cachePoint,omitempty"`
}

type cvMessage struct {
	Role    string    `json:"role"`
	Content []cvBlock `json:"content"`
}

// cvBlock is a ContentBlock union: exactly one member is set.
type cvBlock struct {
	Text       *string       `json:"text,omitempty"`
	Image      *cvImage      `json:"image,omitempty"`
	ToolUse    *cvToolUse    `json:"toolUse,omitempty"`
	ToolResult *cvToolResult `json:"toolResult,omitempty"`
	// ReasoningContent appears only in responses (requests never echo it).
	ReasoningContent *cvReasoning `json:"reasoningContent,omitempty"`
}

type cvImage struct {
	Format string        `json:"format"`
	Source cvImageSource `json:"source"`
}

type cvImageSource struct {
	// Bytes is the base64-encoded image (a JSON blob).
	Bytes string `json:"bytes"`
}

type cvToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type cvToolResult struct {
	ToolUseID string                `json:"toolUseId"`
	Content   []cvToolResultContent `json:"content"`
}

type cvToolResultContent struct {
	Text  *string  `json:"text,omitempty"`
	Image *cvImage `json:"image,omitempty"`
}

type cvReasoning struct {
	ReasoningText *struct {
		Text      string `json:"text"`
		Signature string `json:"signature"`
	} `json:"reasoningText,omitempty"`
	RedactedContent string `json:"redactedContent,omitempty"`
}

type cvToolConfig struct {
	Tools []cvTool `json:"tools"`
}

type cvTool struct {
	ToolSpec cvToolSpec `json:"toolSpec"`
}

type cvToolSpec struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	InputSchema cvInputSchema `json:"inputSchema"`
}

type cvInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type cvUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

func (u cvUsage) chatUsage() provider.ChatUsage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens
	}
	return provider.ChatUsage{
		PromptTokens:        u.InputTokens,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         total,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheWriteInputTokens,
	}
}

type cvResponse struct {
	Output struct {
		Message *cvMessage `json:"message"`
	} `json:"output"`
	StopReason string  `json:"stopReason"`
	Usage      cvUsage `json:"usage"`
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// toolInput converts an OpenAI-style arguments string into a JSON object,
// falling back to {} when it is empty or not an object.
func toolInput(args string) json.RawMessage {
	t := strings.TrimSpace(args)
	if t == "" || !strings.HasPrefix(t, "{") || !json.Valid([]byte(t)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(t)
}

// imageFormat maps a media type to a Converse ImageBlock format.
func imageFormat(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	mt = strings.TrimPrefix(mt, "image/")
	switch mt {
	case "jpg", "jpeg", "pjpeg":
		return "jpeg"
	case "png", "gif", "webp":
		return mt
	}
	return "png"
}

// isAnthropicModel reports whether id names a Claude model (base id,
// inference profile or ARN).
func isAnthropicModel(id string) bool {
	return strings.Contains(strings.ToLower(id), "anthropic.claude")
}

// supportsPromptCache reports whether Converse accepts cachePoint blocks for
// id: Claude 3.5 Haiku / 3.7 Sonnet and every Claude 4+ model, and Amazon
// Nova. Older Claude 3 models reject the block, so it is never sent to them.
func supportsPromptCache(id string) bool {
	l := strings.ToLower(id)
	if strings.Contains(l, "amazon.nova") {
		return true
	}
	if !isAnthropicModel(l) {
		return false
	}
	for _, old := range []string{"claude-3-haiku", "claude-3-sonnet", "claude-3-opus", "claude-3-5-sonnet", "claude-instant", "claude-v2"} {
		if strings.Contains(l, old) {
			return false
		}
	}
	return true
}

// buildConverseBody translates a neutral ChatRequest into a Converse body.
func buildConverseBody(req provider.ChatRequest, modelID string) ([]byte, error) {
	body := cvRequest{InferenceConfig: &cvInference{MaxTokens: req.MaxTokens, Temperature: req.Temperature}}
	if body.InferenceConfig.MaxTokens <= 0 {
		body.InferenceConfig.MaxTokens = defaultMaxTokens
	}

	var system []string
	usedTools := map[string]bool{}
	for _, m := range req.Messages {
		var (
			role   string
			blocks []cvBlock
		)
		switch m.Role {
		case provider.RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
			continue
		case provider.RoleAssistant:
			role = "assistant"
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, cvBlock{Text: strPtr(m.Content)})
			}
			for _, tc := range m.ToolCalls {
				usedTools[tc.Name] = true
				blocks = append(blocks, cvBlock{ToolUse: &cvToolUse{ToolUseID: tc.ID, Name: tc.Name, Input: toolInput(tc.Arguments)}})
			}
		case provider.RoleTool:
			role = "user"
			tr := &cvToolResult{ToolUseID: m.ToolCallID}
			if m.Content != "" {
				tr.Content = append(tr.Content, cvToolResultContent{Text: strPtr(m.Content)})
			}
			for _, im := range m.Images {
				tr.Content = append(tr.Content, cvToolResultContent{Image: &cvImage{
					Format: imageFormat(im.MediaType),
					Source: cvImageSource{Bytes: im.Base64},
				}})
			}
			if len(tr.Content) == 0 {
				// toolResult.content is required and blank text is rejected.
				tr.Content = []cvToolResultContent{{Text: strPtr("(no output)")}}
			}
			if m.Name != "" {
				usedTools[m.Name] = true
			}
			blocks = append(blocks, cvBlock{ToolResult: tr})
		default:
			role = "user"
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, cvBlock{Text: strPtr(m.Content)})
			}
		}
		if len(blocks) == 0 {
			continue // Converse rejects empty content
		}
		if n := len(body.Messages); n > 0 && body.Messages[n-1].Role == role {
			body.Messages[n-1].Content = append(body.Messages[n-1].Content, blocks...)
			continue
		}
		body.Messages = append(body.Messages, cvMessage{Role: role, Content: blocks})
	}
	for i := range body.Messages {
		if body.Messages[i].Role != "user" {
			continue
		}
		c := body.Messages[i].Content
		sort.SliceStable(c, func(a, b int) bool { return c[a].ToolResult != nil && c[b].ToolResult == nil })
	}
	if body.Messages == nil {
		body.Messages = []cvMessage{}
	}

	if sys := strings.Join(system, "\n\n"); sys != "" {
		body.System = []cvSystemBlock{{Text: sys}}
		if req.CacheSystem && supportsPromptCache(modelID) {
			body.System = append(body.System, cvSystemBlock{CachePoint: &cvCachePoint{Type: "default"}})
		}
	}

	declared := map[string]bool{}
	var tools []cvTool
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		declared[t.Name] = true
		tools = append(tools, cvTool{ToolSpec: cvToolSpec{Name: t.Name, Description: t.Description, InputSchema: cvInputSchema{JSON: schema}}})
	}
	// Converse rejects toolUse/toolResult history without a toolConfig (e.g.
	// a compaction request that sends no tools): declare permissive stand-ins.
	var missing []string
	for name := range usedTools {
		if name != "" && !declared[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		tools = append(tools, cvTool{ToolSpec: cvToolSpec{Name: name, InputSchema: cvInputSchema{JSON: json.RawMessage(`{"type":"object"}`)}}})
	}
	if len(tools) == 0 && hasToolBlocks(body.Messages) {
		tools = append(tools, cvTool{ToolSpec: cvToolSpec{Name: "tool", InputSchema: cvInputSchema{JSON: json.RawMessage(`{"type":"object"}`)}}})
	}
	if len(tools) > 0 {
		body.ToolConfig = &cvToolConfig{Tools: tools}
	}

	if isAnthropicModel(modelID) {
		extra := map[string]any{}
		if req.Effort != "" {
			extra["output_config"] = map[string]any{"effort": req.Effort}
		}
		if provider.HasContext1MSuffix(req.Model) {
			extra["anthropic_beta"] = []string{context1MBeta}
		}
		if len(extra) > 0 {
			body.AdditionalModelRequestFields = extra
		}
	}
	return json.Marshal(body)
}

func hasToolBlocks(msgs []cvMessage) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.ToolUse != nil || b.ToolResult != nil {
				return true
			}
		}
	}
	return false
}

// finishReason maps a Converse stopReason onto the neutral finish reason.
func finishReason(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence", "pause_turn":
		return provider.FinishStop
	case "tool_use":
		return provider.FinishToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return provider.FinishLength
	case "guardrail_intervened", "content_filtered", "refusal":
		return provider.FinishContentFilter
	}
	return stop
}
