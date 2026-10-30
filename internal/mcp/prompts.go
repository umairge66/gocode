package mcp

import "context"

// MethodPromptsGet is the prompts/get request method.
const MethodPromptsGet = "prompts/get"

// PromptArgument describes one argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one entry of a server's prompt catalog.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// ListPromptsParams are the (optional) params of prompts/list.
type ListPromptsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListPromptsResult is the result of prompts/list.
type ListPromptsResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// GetPromptParams are the params of prompts/get.
type GetPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// PromptMessage is one message of a prompts/get result.
type PromptMessage struct {
	Role    string       `json:"role"`
	Content ContentBlock `json:"content"`
}

// GetPromptResult is the result of prompts/get.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// ListPrompts fetches one page of the server's prompt catalog (nil params:
// the first page).
func (c *Client) ListPrompts(ctx context.Context, params *ListPromptsParams) (*ListPromptsResult, error) {
	var p any
	if params != nil {
		p = params
	}
	var result ListPromptsResult
	if err := c.request(ctx, MethodPromptsList, p, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetPrompt renders one prompt with its arguments.
func (c *Client) GetPrompt(ctx context.Context, params GetPromptParams) (*GetPromptResult, error) {
	var result GetPromptResult
	if err := c.request(ctx, MethodPromptsGet, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
