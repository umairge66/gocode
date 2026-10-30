package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcpserver"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/browser"
)

// `gocode --gocode-in-chrome-mcp`: the Chrome browser tools served as an MCP
// server over stdio, so any MCP client (another agent, an editor) can drive
// the user's Chrome through gocode's own extension.
//
// Behaviour:
//
//   - an MCP Server {name:"gocode in Chrome", version:"1.0.0"} with
//     capabilities {tools:{listChanged:true}, logging:{}}, connected to a stdio
//     transport; the process exits when stdin ends;
//   - tools/list returns the in-chrome tool definitions minus the
//     browser-picking tools (switch_browser, list_connected_browsers,
//     select_browser) when no bridge is configured; tools/call forwards the
//     call to the browser and returns its content (text and images);
//   - navigate without a tabId opens/uses the session's tab and appends the
//     tabs_context_mcp listing and the note that those tabs are the caller's
//     to close; computer save_to_disk saves screenshots
//     (see browser.ScreenshotSaver); a disconnected browser yields an in-band
//     error telling the user to install / start the extension.
//
// There is no remote relay, pairing, sign-in or telemetry: the extension is
// reached over the same LOCAL control socket the session's browser tools use
// (chromehost.NewControlClient), so tab isolation and the site rules are
// enforced exactly as for the session.
//
// Tools served: the tool names and schemas (chromemcp_tools.json) for the
// tools gocode's extension implements —
// javascript_tool, read_page, find, form_input, computer, browser_batch,
// navigate, resize_window, get_page_text, tabs_context_mcp, tabs_create_mcp,
// tabs_close_mcp. Not served (the extension has no equivalent yet):
// gif_creator, upload_image, read_console_messages, read_network_requests,
// shortcuts_list, shortcuts_execute, file_upload. read_page returns the page
// text; the accessibility-tree options (filter, depth, ref_id, max_chars)
// are accepted and ignored, as is computer's scale (extensions without scale
// support return the full image).
//
// Site rules: the gocodeInChromeDomain allow/deny rules from the settings
// apply. A denied site is refused (here and in the extension); a site with no
// rule is not asked about — this mode has no user to ask, and the MCP client
// gates its own tool calls — so the list can only restrict.

//go:embed chromemcp_tools.json
var chromeMcpToolsJSON []byte

// chromeMcpServerInfo is the identity the server advertises.
var chromeMcpServerInfo = mcp.Implementation{Name: "gocode in Chrome", Version: "1.0.0"}

// chromeNotConnectedMessage is the disconnected-browser error for the local
// extension (no account, no bug-report link).
const chromeNotConnectedMessage = "Browser extension is not connected. Please ensure the gocode browser extension is installed and running (run `gocode chrome install`, then load the extension in Chrome). If this is your first time connecting to Chrome, you may need to restart Chrome for the installation to take effect."

// runChromeMcp is the --gocode-in-chrome-mcp entrypoint. stdout carries only
// MCP frames; diagnostics go to stderr.
func runChromeMcp(env *Env) int {
	if env.Stdin == nil {
		fmt.Fprintln(env.Stderr, "Error: --gocode-in-chrome-mcp requires a stdin stream for the stdio transport.")
		return 1
	}
	rules := toolrun.SiteRulesFrom(loadPermissionRules(env))
	policy := browser.NewSitePolicy(rules, false)
	client := browser.PolicyClient{
		Inner:  chromehost.NewControlClient(sessionChromeConfigDir(env)),
		Policy: func() browser.SitePolicy { return policy },
	}
	srv, err := newChromeMcpServer(client, policy, browser.DefaultScreenshotSaver)
	if err != nil {
		fmt.Fprintf(env.Stderr, "[gocode in Chrome] %v\n", err)
		return 1
	}
	srv.OnError(func(err error) {
		fmt.Fprintf(env.Stderr, "[gocode in Chrome] %v\n", err)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := srv.ServeStdio(ctx, env.Stdin, env.Stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintf(env.Stderr, "[gocode in Chrome] %v\n", err)
		return 1
	}
	return 0
}

// chromeMcpToolDef is one entry of chromemcp_tools.json.
type chromeMcpToolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema mcp.ToolSchema  `json:"inputSchema"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

func chromeMcpTools() ([]chromeMcpToolDef, error) {
	var defs []chromeMcpToolDef
	if err := json.Unmarshal(chromeMcpToolsJSON, &defs); err != nil {
		return nil, fmt.Errorf("tool catalog: %w", err)
	}
	return defs, nil
}

// newChromeMcpServer builds the MCP server over a browser client.
func newChromeMcpServer(c browser.Client, policy browser.SitePolicy, saver *browser.ScreenshotSaver) (*mcpserver.Server, error) {
	defs, err := chromeMcpTools()
	if err != nil {
		return nil, err
	}
	srv := mcpserver.NewServer(chromeMcpServerInfo, mcpserver.WithCapabilities(mcp.ServerCapabilities{
		Tools:   &mcp.ListChangedCapability{ListChanged: true},
		Logging: json.RawMessage(`{}`),
	}))
	// logging capability: accept a level change (diagnostics go to stderr).
	srv.SetRequestHandler("logging/setLevel", func(context.Context, mcp.Request) (any, error) {
		return struct{}{}, nil
	})
	h := chromeMcpHandlers{client: c, policy: policy, saver: saver}
	for _, def := range defs {
		handler := h.handler(def.Name)
		if handler == nil {
			return nil, fmt.Errorf("no handler for tool %s", def.Name)
		}
		srv.RegisterToolFull(mcp.Tool{
			Name:        def.Name,
			Title:       def.Title,
			Description: def.Description,
			InputSchema: def.InputSchema,
			Meta:        def.Meta,
		}, handler)
	}
	return srv, nil
}

type chromeMcpHandlers struct {
	client browser.Client
	policy browser.SitePolicy
	saver  *browser.ScreenshotSaver
}

// decode unmarshals MCP arguments into v (an empty argument object is fine).
func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// toMCP renders a browser result (or error) as MCP content. A tool failure is
// in band (isError), never a protocol error.
func toMCP(res browser.Result, err error) (mcp.CallToolResult, error) {
	if err != nil {
		msg := err.Error()
		if errors.Is(err, chromehost.ErrChromeNotConnected) {
			msg = chromeNotConnectedMessage
		}
		return mcp.CallToolResult{IsError: true, Content: []mcp.ContentBlock{{Type: "text", Text: msg}}}, nil
	}
	out := mcp.CallToolResult{Content: []mcp.ContentBlock{}}
	for _, img := range res.Images {
		out.Content = append(out.Content, mcp.ContentBlock{Type: "image", Data: img.Base64, MimeType: img.MediaType})
	}
	if res.Text != "" || len(res.Images) == 0 {
		out.Content = append(out.Content, mcp.ContentBlock{Type: "text", Text: res.Text})
	}
	return out, nil
}

// checkNavigation refuses a non-web target or a denied site before the call
// goes out (the extension checks again).
func (h chromeMcpHandlers) checkNavigation(urls ...string) error {
	for _, u := range urls {
		if u == "" || browser.IsHistoryNavigation(u) {
			continue
		}
		host, ok := browser.WebHost(u)
		if !ok {
			return errors.New("Can't interact with browser-internal or unparseable URLs. Navigate to a web page first.")
		}
		if h.policy.Check(host) == browser.SiteDenied {
			return errors.New("gocode in Chrome is denied on " + host + ".")
		}
	}
	return nil
}

func (h chromeMcpHandlers) handler(name string) mcpserver.ToolHandler {
	c := h.client
	switch name {
	case "javascript_tool":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.JavascriptInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.JavascriptTool{Client: c}.Execute(ctx, in))
		}
	case "read_page", "get_page_text":
		method := name
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.ReadPageInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.ReadPageTool{Client: c, Method: method}.Execute(ctx, in))
		}
	case "find":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.FindInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.FindTool{Client: c}.Execute(ctx, in))
		}
	case "form_input":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.FormInputInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.FormInputTool{Client: c}.Execute(ctx, in))
		}
	case "computer":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.ComputerInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			if in.SaveTo != "" {
				// An explicit path needs the Write permission check, which this
				// mode has no user to ask for.
				return toMCP(browser.Result{}, errors.New("save_to is not available over MCP; use save_to_disk"))
			}
			return toMCP(browser.ComputerTool{Client: c, Saver: h.saver}.Execute(ctx, in))
		}
	case "browser_batch":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.BatchInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			var urls []string
			for _, a := range in.Actions {
				if a.Name == "browser_batch" {
					return toMCP(browser.Result{}, errors.New("browser_batch: every action needs a string tool name (and batches don't nest)"))
				}
				if a.Name == "navigate" || a.Name == "tabs_create_mcp" || a.Name == "tabs_create" {
					var ni struct {
						URL string `json:"url"`
					}
					_ = json.Unmarshal(a.Input, &ni)
					urls = append(urls, ni.URL)
				}
			}
			if err := h.checkNavigation(urls...); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.BatchTool{Client: c, Saver: h.saver}.Execute(ctx, in))
		}
	case "navigate":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.NavigateInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			if err := h.checkNavigation(in.URL); err != nil {
				return toMCP(browser.Result{}, err)
			}
			res, err := browser.NavigateTool{Client: c}.Execute(ctx, in)
			if err != nil || in.TabID != nil || browser.IsHistoryNavigation(in.URL) {
				return toMCP(res, err)
			}
			// Standalone navigate without a tabId: append the tab listing so the
			// caller has the ids for its next calls.
			if tabs, terr := (browser.TabsContextTool{Client: c}).MCPTabsContext(ctx, browser.TabsContextInput{}); terr == nil {
				res.Text = strings.TrimRight(res.Text, "\n") + "\n\n" + tabs.Text + "\n" + browser.TabCleanupNote("tabs_close_mcp")
			}
			return toMCP(res, nil)
		}
	case "resize_window":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.ResizeWindowInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.ResizeWindowTool{Client: c}.Execute(ctx, in))
		}
	case "tabs_context_mcp":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.TabsContextInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.TabsContextTool{Client: c}.MCPTabsContext(ctx, in))
		}
	case "tabs_create_mcp":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in struct {
				KeepOpen *bool `json:"keep_open,omitempty"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.TabsCreateTool{Client: c}.Execute(ctx, browser.TabsCreateInput{KeepOpen: in.KeepOpen}))
		}
	case "tabs_close_mcp":
		return func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in browser.TabsCloseInput
			if err := decodeArgs(args, &in); err != nil {
				return toMCP(browser.Result{}, err)
			}
			return toMCP(browser.TabsCloseTool{Client: c}.Execute(ctx, in))
		}
	}
	return nil
}
