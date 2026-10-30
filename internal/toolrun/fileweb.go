package toolrun

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/tools/fileedit"
	"github.com/umairge66/gocode/internal/tools/web"
)

// This file adapts the fileedit (Read / Edit / MultiEdit) and web (WebFetch /
// WebSearch) tools to the dispatcher. Each adapter's tool_result text is the
// tool's own model-facing rendering (ReadResult.Text, EditResult.Text,
// MultiEditResult.Text, WebFetchResult.Result, web.FormatResult); errors become
// is_error tool_results wrapped by Dispatch like every other tool.

// ReadAdapter adapts a fileedit.ReadTool. Read-only; the rule content and path
// are the file_path (a filePatternTool, read operation).
//
// A Read of an image or PDF carries structured image content: the tool returns
// ReadResult.Images (one block for an image, one per rendered page for a PDF,
// each already resized/re-encoded to the per-image byte budget by
// fileedit), which the adapter maps onto the tool_result as image blocks
// (ReadResult.Text stays alongside as the caption/summary). The image bytes are
// passed through verbatim — the budget is not re-expanded here. Text reads and
// every other tool return a plain string result, unchanged.
func ReadAdapter(t fileedit.ReadTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in fileedit.ReadInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.FilePath, in.FilePath, pathguard.OpRead, nil
		},
		runImages: func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			var in fileedit.ReadInput
			if err := unmarshal(input, &in); err != nil {
				return "", nil, err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", nil, err
			}
			if len(res.Images) == 0 {
				// Text/notebook read, or an image whose format could not be
				// decoded (fileedit returns a text note, no image blocks).
				return res.Text, nil, nil
			}
			images := make([]agent.ToolResultImage, len(res.Images))
			for i, ri := range res.Images {
				images[i] = agent.ToolResultImage{Base64: ri.Base64, MediaType: ri.MediaType}
			}
			return res.Text, images, nil
		},
		description: t.Prompt(),
		inputSchema: t.InputSchema(),
	}
}

// EditAdapter adapts a fileedit.EditTool. The rule content and path are the
// file_path (a filePatternTool, write operation).
func EditAdapter(t fileedit.EditTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in fileedit.EditInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.FilePath, in.FilePath, pathguard.OpWrite, nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in fileedit.EditInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: t.Prompt(),
		inputSchema: t.InputSchema(),
	}
}

// MultiEditAdapter adapts a fileedit.MultiEditTool. The rule content and path
// are the file_path (a filePatternTool, write operation).
func MultiEditAdapter(t fileedit.MultiEditTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in fileedit.MultiEditInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.FilePath, in.FilePath, pathguard.OpWrite, nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in fileedit.MultiEditInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: t.Prompt(),
		inputSchema: t.InputSchema(),
	}
}

// WebFetchRuleContent returns the permission rule content for a WebFetch URL:
// "domain:<hostname>" (lowercased, as URL.hostname is), or "input:<raw>" when
// the URL cannot be parsed, so a WebFetch(domain:example.com) settings rule
// matches every fetch of that host.
func WebFetchRuleContent(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "input:" + rawURL
	}
	return "domain:" + strings.ToLower(u.Hostname())
}

// WebFetchAdapter adapts a web.WebFetchTool. Read-only; the rule content is
// "domain:<host>" (see WebFetchRuleContent); no filesystem path.
func WebFetchAdapter(t *web.WebFetchTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in web.WebFetchInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return WebFetchRuleContent(in.URL), "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in web.WebFetchInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Result, nil
		},
		description: t.Description(),
		inputSchema: json.RawMessage(t.InputSchema()),
	}
}

// WebSearchAdapter adapts a web.WebSearchTool. Read-only; keyed by the bare
// tool name (no rule content, no path). An unconfigured tool (no backend)
// returns web.ErrMsgNotConfigured as an is_error result.
func WebSearchAdapter(t *web.WebSearchTool, budget func() WebSearchLimiter) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in web.WebSearchInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in web.WebSearchInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			if b := budget(); b != nil && !b.TryConsume() {
				return webSearchCapMessage(b.Limit(), b.RefillsPerHour()), nil
			}
			out, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return web.FormatResult(out), nil
		},
		description: t.Description(),
		inputSchema: json.RawMessage(t.InputSchema()),
	}
}
