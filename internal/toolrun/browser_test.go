package toolrun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

type fakeBrowserClient struct {
	byMethod map[string]json.RawMessage
	err      error
}

func (f *fakeBrowserClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byMethod[method], nil
}

func dispatchBrowser(t *testing.T, mode toolreg.Mode, c *fakeBrowserClient, name string, input string) agent.ToolResult {
	t.Helper()
	d := New(mode, nil, WithBrowserClient(c))
	res, err := d.Dispatch(context.Background(), agent.ToolUse{Name: name, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("Dispatch(%s) returned loop-fatal error: %v", name, err)
	}
	return res
}

type recordingBrowserClient struct {
	fakeBrowserClient
	params []json.RawMessage
}

func (r *recordingBrowserClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(params)
	r.params = append(r.params, b)
	return r.fakeBrowserClient.Call(ctx, method, params)
}

// The computer schema declares coordinates as numbers: a fractional one is
// rounded, not refused as a type error.
func TestDispatch_ComputerAcceptsFractionalCoordinates(t *testing.T) {
	c := &recordingBrowserClient{fakeBrowserClient: fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodComputer: json.RawMessage(`{"action":"left_click","ok":true}`),
	}}}
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c))
	res, err := d.Dispatch(context.Background(), agent.ToolUse{Name: string(protocol.ToolBrowserComputer),
		Input: json.RawMessage(`{"action":"left_click","coordinate":[120.6,48.2],"scroll_amount":2.5}`)})
	if err != nil || res.IsError {
		t.Fatalf("dispatch: %v %+v", err, res)
	}
	if len(c.params) == 0 || !strings.Contains(string(c.params[0]), `"coordinate":[121,48]`) {
		t.Fatalf("sent %s", c.params)
	}
}

func TestDispatch_BrowserReadsMapResults(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodReadPage:    json.RawMessage(`{"text":"page body"}`),
		chromehost.MethodTabsContext: json.RawMessage(`{"tabs":[{"id":3,"url":"https://z","title":"Z","active":true}]}`),
	}}
	// Default mode: reads lean auto-allow, so these run without a prompter.
	rp := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserReadPage), `{}`)
	if rp.IsError || !strings.Contains(rp.Content, "page body") {
		t.Fatalf("read_page result = %+v", rp)
	}
	tc := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserTabsContext), `{}`)
	if tc.IsError || !strings.Contains(tc.Content, "https://z") {
		t.Fatalf("tabs_context result = %+v", tc)
	}
}

func TestDispatch_ScreenshotFlowsThroughImagePath(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz"))
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodScreenshot: json.RawMessage(`{"dataUrl":"data:image/png;base64,` + png + `"}`),
	}}
	res := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserScreenshot), `{}`)
	if res.IsError {
		t.Fatalf("screenshot result is error: %q", res.Content)
	}
	if len(res.Images) != 1 {
		t.Fatalf("want 1 image on the tool_result, got %d", len(res.Images))
	}
	if res.Images[0].MediaType != "image/png" || res.Images[0].Base64 != png {
		t.Fatalf("image block = %+v", res.Images[0])
	}
}

func TestDispatch_BrowserActionsRunUnderBypass(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodNavigate:   json.RawMessage(`{"tabId":5,"url":"https://e"}`),
		chromehost.MethodTabsCreate: json.RawMessage(`{"tabId":6,"url":"https://f"}`),
		chromehost.MethodTabsClose:  json.RawMessage(`{"closed":6}`),
	}}
	// navigate also needs the site allowed: the in-chrome tools are excluded
	// from bypass allow for a site with no gocodeInChromeDomain rule.
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "gocodeInChromeDomain", Content: "e"}}
	d := New(toolreg.ModeBypassPermissions, rules, WithBrowserClient(c))
	nav, err := d.Dispatch(context.Background(), agent.ToolUse{Name: string(protocol.ToolBrowserNavigate), Input: json.RawMessage(`{"url":"https://e"}`)})
	if err != nil || nav.IsError || !strings.Contains(nav.Content, "5") {
		t.Fatalf("navigate result = %+v (%v)", nav, err)
	}
	cl := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserTabsClose), `{"tabId":6}`)
	if cl.IsError || !strings.Contains(cl.Content, "6") {
		t.Fatalf("tabs_close result = %+v", cl)
	}
}

func TestDispatch_BrowserActionGatedLikeBashInDefault(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodNavigate: json.RawMessage(`{"tabId":1,"url":"https://e"}`),
	}}
	// Default mode, no prompter: an action's "ask" resolves to a denial is_error
	// result — exactly like Bash — and the tool is NOT executed.
	res := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserNavigate), `{"url":"https://e"}`)
	if !res.IsError {
		t.Fatalf("navigate in default mode should be gated (denied), got %+v", res)
	}
}

func TestDispatch_NotConnectedIsNonFatalError(t *testing.T) {
	c := &fakeBrowserClient{err: chromehost.ErrChromeNotConnected}
	res := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserReadPage), `{}`)
	if !res.IsError {
		t.Fatalf("not-connected should yield an is_error result, got %+v", res)
	}
	if !strings.Contains(res.Content, "extension") {
		t.Fatalf("not-connected content = %q, want the connect instruction", res.Content)
	}
}

func TestDispatch_ComputerActionsMapResults(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz"))
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodComputer: json.RawMessage(`{"action":"left_click","tabId":1,"ok":true}`),
	}}
	// A click action -> ok text, no image.
	click := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserComputer), `{"action":"left_click","coordinate":[10,20],"tabId":1}`)
	if click.IsError || !strings.Contains(click.Content, "left_click") {
		t.Fatalf("computer click result = %+v", click)
	}
	if len(click.Images) != 0 {
		t.Fatalf("click should produce no image, got %d", len(click.Images))
	}
	// A type action -> ok.
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"type","tabId":1,"ok":true}`)
	typ := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserComputer), `{"action":"type","text":"hi","tabId":1}`)
	if typ.IsError || !strings.Contains(typ.Content, "type") {
		t.Fatalf("computer type result = %+v", typ)
	}
	// A screenshot action -> image Result through the image path.
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"screenshot","dataUrl":"data:image/png;base64,` + png + `"}`)
	shot := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserComputer), `{"action":"screenshot","tabId":1}`)
	if shot.IsError || len(shot.Images) != 1 || shot.Images[0].Base64 != png {
		t.Fatalf("computer screenshot result = %+v", shot)
	}
}

func TestDispatch_FindMapsMatches(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodFind: json.RawMessage(`{"tabId":1,"matches":[{"ref":"ref_1","text":"Add to cart","role":"button","x":100,"y":200}],"truncated":false}`),
	}}
	// find is a read: it runs in default mode without a prompter.
	res := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserFind), `{"query":"cart","tabId":1}`)
	if res.IsError || !strings.Contains(res.Content, "ref_1") || !strings.Contains(res.Content, "Add to cart") {
		t.Fatalf("find result = %+v", res)
	}
}

func TestDispatch_FormInputMapsOK(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodFormInput: json.RawMessage(`{"ref":"ref_2","ok":true}`),
	}}
	res := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserFormInput), `{"ref":"ref_2","value":"hello","tabId":1}`)
	if res.IsError || !strings.Contains(res.Content, "ref_2") {
		t.Fatalf("form_input result = %+v", res)
	}
}

func TestDispatch_BrowserBatchMapsArray(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz"))
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodBrowserBatch: json.RawMessage(`{"results":[` +
			`{"name":"computer","ok":true},` +
			`{"name":"computer","ok":true,"dataUrl":"data:image/png;base64,` + png + `"}` +
			`]}`),
	}}
	res := dispatchBrowser(t, toolreg.ModeBypassPermissions, c, string(protocol.ToolBrowserBatch),
		`{"actions":[{"name":"computer","input":{"action":"left_click","coordinate":[1,2],"tabId":1}},{"name":"computer","input":{"action":"screenshot","tabId":1}}]}`)
	if res.IsError {
		t.Fatalf("browser_batch errored: %q", res.Content)
	}
	if !strings.Contains(res.Content, "2 of 2") {
		t.Fatalf("browser_batch summary = %q", res.Content)
	}
	if len(res.Images) != 1 || res.Images[0].Base64 != png {
		t.Fatalf("browser_batch should surface 1 interleaved image, got %d", len(res.Images))
	}
}

func TestDispatch_ComputerGatedLikeBashInDefault(t *testing.T) {
	c := &fakeBrowserClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodComputer: json.RawMessage(`{"action":"left_click","ok":true}`),
	}}
	// Default mode, no prompter: an action's "ask" resolves to a denial is_error
	// result (like Bash); the tool is NOT executed.
	res := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserComputer), `{"action":"left_click","coordinate":[1,2],"tabId":1}`)
	if !res.IsError {
		t.Fatalf("computer in default mode should be gated (denied), got %+v", res)
	}
	// browser_batch is likewise gated as an action.
	c.byMethod[chromehost.MethodBrowserBatch] = json.RawMessage(`{"results":[]}`)
	bres := dispatchBrowser(t, toolreg.ModeDefault, c, string(protocol.ToolBrowserBatch), `{"actions":[{"name":"computer","input":{"action":"left_click","coordinate":[1,2],"tabId":1}}]}`)
	if !bres.IsError {
		t.Fatalf("browser_batch in default mode should be gated (denied), got %+v", bres)
	}
}

func TestDispatch_BrowserToolsAdvertisedWhenDisconnected(t *testing.T) {
	// No client injected -> disconnected client, but the tools must still be
	// advertised (present in Schemas) so the session starts with Chrome absent.
	d := New(toolreg.ModeDefault, nil)
	have := map[string]bool{}
	for _, s := range d.Schemas() {
		have[s.Name] = true
	}
	for _, name := range []protocol.Tool{
		protocol.ToolBrowserNavigate, protocol.ToolBrowserReadPage, protocol.ToolBrowserGetPageText,
		protocol.ToolBrowserTabsContext, protocol.ToolBrowserTabsCreate, protocol.ToolBrowserTabsClose,
		protocol.ToolBrowserScreenshot,
		protocol.ToolBrowserComputer, protocol.ToolBrowserFind, protocol.ToolBrowserFormInput, protocol.ToolBrowserBatch,
	} {
		if !have[string(name)] {
			t.Fatalf("browser tool %q not advertised in Schemas", name)
		}
	}
	// And a call returns the not-connected error, not a crash.
	res, err := d.Dispatch(context.Background(), agent.ToolUse{Name: string(protocol.ToolBrowserReadPage), Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("loop-fatal error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "extension") {
		t.Fatalf("disconnected read result = %+v", res)
	}
}
