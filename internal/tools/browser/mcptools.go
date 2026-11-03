package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/chromehost"
)

// Tools the --gocode-in-chrome-mcp server serves beyond the session's set:
// javascript_tool, resize_window, and tabs_context_mcp's JSON listing.

// JavascriptInput is the javascript_tool parameters.
type JavascriptInput struct {
	Action string `json:"action,omitempty"`
	Text   string `json:"text"`
	TabID  *Num   `json:"tabId,omitempty"`
}

// JavascriptTool evaluates JavaScript in a gocode tab (REPL semantics: the value
// of the last expression is returned, top-level await works). It is an ACTION
// on the page and is subject to the site policy like computer.
type JavascriptTool struct{ Client Client }

func (t JavascriptTool) Execute(ctx context.Context, in JavascriptInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if in.Action != "" && in.Action != "javascript_exec" {
		return Result{}, errors.New("javascript_tool: action must be 'javascript_exec'")
	}
	if strings.TrimSpace(in.Text) == "" {
		return Result{}, errors.New("javascript_tool requires text (the code to run)")
	}
	raw, err := c.Call(ctx, chromehost.MethodJavascript, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		TabID     int    `json:"tabId"`
		URL       string `json:"url"`
		Value     string `json:"value"`
		Exception string `json:"exception"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.Exception != "" {
		return Result{}, errors.New("JavaScript error: " + sanitizePageText(r.Exception))
	}
	if r.Value == "" {
		r.Value = "undefined"
	}
	// The value is computed from the page, so it is fenced like page text.
	return Result{Text: wrapUntrusted(tabSource(r.URL, r.TabID), r.Value)}, nil
}

// ResizeWindowInput is the resize_window parameters.
type ResizeWindowInput struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	TabID  *Num `json:"tabId,omitempty"`
}

// ResizeWindowTool resizes the browser window holding a gocode tab.
type ResizeWindowTool struct{ Client Client }

func (t ResizeWindowTool) Execute(ctx context.Context, in ResizeWindowInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if in.Width <= 0 || in.Height <= 0 {
		return Result{}, errors.New("resize_window requires a positive width and height")
	}
	raw, err := c.Call(ctx, chromehost.MethodResizeWindow, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.Width == 0 {
		r.Width, r.Height = in.Width, in.Height
	}
	return Result{Text: fmt.Sprintf("Resized the window to %dx%d.", r.Width, r.Height)}, nil
}

// TabEntry is one tab of the tabs_context_mcp listing.
type TabEntry struct {
	TabID int    `json:"tabId"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// TabsContextInput is the tabs_context_mcp parameters.
type TabsContextInput struct {
	CreateIfEmpty bool `json:"createIfEmpty,omitempty"`
}

// ListTabs returns gocode's tabs and its tab group id (0 when none), creating a
// tab first when createIfEmpty is set and gocode has none.
func (t TabsContextTool) ListTabs(ctx context.Context, in TabsContextInput) ([]TabEntry, int, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return nil, 0, err
	}
	raw, err := c.Call(ctx, chromehost.MethodTabsContext, in)
	if err != nil {
		return nil, 0, err
	}
	var r struct {
		Groups []struct {
			ID int `json:"id"`
		} `json:"groups"`
		Tabs []tabInfo `json:"tabs"`
	}
	_ = json.Unmarshal(raw, &r)
	out := make([]TabEntry, 0, len(r.Tabs))
	for _, tb := range r.Tabs {
		out = append(out, TabEntry{TabID: tb.ID, Title: tb.Title, URL: tb.URL})
	}
	group := 0
	if len(r.Groups) > 0 {
		group = r.Groups[0].ID
	}
	return out, group, nil
}

// MCPTabsContext renders the tabs_context_mcp result: the JSON
// {availableTabs:[{tabId,title,url}], tabGroupId}, fenced because titles and
// URLs are set by the pages.
func (t TabsContextTool) MCPTabsContext(ctx context.Context, in TabsContextInput) (Result, error) {
	tabs, group, err := t.ListTabs(ctx, in)
	if err != nil {
		return Result{}, err
	}
	payload := map[string]any{"availableTabs": tabs}
	if group != 0 {
		payload["tabGroupId"] = group
	}
	b, _ := json.Marshal(payload)
	if len(tabs) == 0 {
		return Result{Text: string(b) + "\n\nNo gocode tabs yet. Call tabs_context_mcp with createIfEmpty:true or tabs_create_mcp to open one."}, nil
	}
	return Result{Text: wrapUntrusted("", string(b))}, nil
}
