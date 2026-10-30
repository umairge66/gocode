package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/browser"
)

// fakeChrome answers browser verbs and records what was sent.
type fakeChrome struct {
	results map[string]json.RawMessage
	err     error
	sent    []string
	params  []json.RawMessage
}

func (f *fakeChrome) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.sent = append(f.sent, method)
	raw, _ := params.(json.RawMessage)
	f.params = append(f.params, raw)
	if f.err != nil {
		return nil, f.err
	}
	return f.results[method], nil
}

func chromeMcpHandle(t *testing.T, fc *fakeChrome, policy browser.SitePolicy, method string, params any) json.RawMessage {
	t.Helper()
	client := browser.PolicyClient{Inner: fc, Policy: func() browser.SitePolicy { return policy }}
	srv, err := newChromeMcpServer(client, policy, &browser.ScreenshotSaver{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(params)
	resp := srv.Handle(context.Background(), mcp.Request{JSONRPC: mcp.JSONRPCVersion, ID: json.RawMessage(`1`), Method: method, Params: p})
	if resp.Error != nil {
		t.Fatalf("%s: rpc error %+v", method, resp.Error)
	}
	return resp.Result
}

func callChromeTool(t *testing.T, fc *fakeChrome, policy browser.SitePolicy, name string, args string) mcp.CallToolResult {
	t.Helper()
	raw := chromeMcpHandle(t, fc, policy, "tools/call", map[string]any{"name": name, "arguments": json.RawMessage(args)})
	var res mcp.CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func textOf(res mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func TestChromeMcpToolList(t *testing.T) {
	raw := chromeMcpHandle(t, &fakeChrome{}, browser.SitePolicy{}, "tools/list", nil)
	var list struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	want := "javascript_tool,read_page,find,form_input,computer,browser_batch,navigate,resize_window,get_page_text,tabs_context_mcp,tabs_create_mcp,tabs_close_mcp"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v", names)
	}
	for _, tl := range list.Tools {
		if tl.Name == "navigate" || tl.Name == "tabs_create_mcp" {
			p, _ := json.Marshal(tl.InputSchema.Properties["keep_open"])
			if !strings.Contains(string(p), `"type":"boolean"`) || !strings.Contains(string(p), browser.KeepOpenDescription) {
				t.Errorf("%s keep_open = %s", tl.Name, p)
			}
		}
		switch tl.Name {
		case "computer":
			for _, p := range []string{"action", "coordinate", "text", "repeat", "modifiers", "save_to_disk", "tabId"} {
				if _, ok := tl.InputSchema.Properties[p]; !ok {
					t.Errorf("computer schema lacks %s", p)
				}
			}
			if strings.Join(tl.InputSchema.Required, ",") != "action,tabId" {
				t.Errorf("computer required = %v", tl.InputSchema.Required)
			}
		case "read_page":
			if !strings.Contains(string(tl.Meta), "maxResultSizeChars") {
				t.Errorf("read_page _meta = %s", tl.Meta)
			}
		case "tabs_context_mcp":
			if tl.Title != "Tabs Context" {
				t.Errorf("tabs_context_mcp title = %q", tl.Title)
			}
		case "tabs_create_mcp":
			if !strings.Contains(tl.Description, "Tabs you create are yours to clean up: close each one with tabs_close_mcp") {
				t.Errorf("tabs_create_mcp description = %q", tl.Description)
			}
			if !strings.Contains(tl.Description, "set keep_open instead of leaving it open") {
				t.Errorf("tabs_create_mcp description = %q", tl.Description)
			}
		case "navigate":
			if !strings.Contains(tl.Description, "A tab opened for you this way is yours to clean up") {
				t.Errorf("navigate description = %q", tl.Description)
			}
			if !strings.Contains(tl.Description, "set keep_open instead of leaving it open") {
				t.Errorf("navigate description = %q", tl.Description)
			}
		case "tabs_close_mcp":
			if !strings.Contains(tl.Description, "Use to clean up tabs you're done with.") {
				t.Errorf("tabs_close_mcp description = %q", tl.Description)
			}
		}
	}
	init := chromeMcpHandle(t, &fakeChrome{}, browser.SitePolicy{}, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	if !strings.Contains(string(init), `"name":"gocode in Chrome"`) || !strings.Contains(string(init), `"logging":{}`) {
		t.Fatalf("initialize = %s", init)
	}
}

func TestChromeMcpCallsReachTheLocalExtension(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz"))
	fc := &fakeChrome{results: map[string]json.RawMessage{
		chromehost.MethodComputer:     json.RawMessage(`{"dataUrl":"data:image/png;base64,` + png + `"}`),
		chromehost.MethodTabsContext:  json.RawMessage(`{"groups":[{"id":7}],"tabs":[{"id":3,"url":"https://e.example/","title":"E"}]}`),
		chromehost.MethodNavigate:     json.RawMessage(`{"tabId":3,"url":"https://e.example/"}`),
		chromehost.MethodJavascript:   json.RawMessage(`{"tabId":3,"value":"42"}`),
		chromehost.MethodResizeWindow: json.RawMessage(`{"width":800,"height":600}`),
	}}
	pol := browser.NewSitePolicy(browser.SiteRules{Deny: []string{"bank.example"}}, false)

	shot := callChromeTool(t, fc, pol, "computer", `{"action":"screenshot","tabId":3,"save_to_disk":true}`)
	if shot.IsError || len(shot.Content) != 2 || shot.Content[0].Type != "image" || !strings.Contains(textOf(shot), browser.SavedPathPrefix) {
		t.Fatalf("screenshot = %+v", shot)
	}
	if !strings.Contains(string(fc.params[len(fc.params)-1]), `"sitePolicy"`) {
		t.Fatalf("the site policy was not sent: %s", fc.params[len(fc.params)-1])
	}

	tabs := callChromeTool(t, fc, pol, "tabs_context_mcp", `{"createIfEmpty":true}`)
	if tabs.IsError || !strings.Contains(textOf(tabs), `"availableTabs":[{"tabId":3,"title":"E","url":"https://e.example/"}]`) || !strings.Contains(textOf(tabs), `"tabGroupId":7`) {
		t.Fatalf("tabs_context_mcp = %s", textOf(tabs))
	}
	if !strings.Contains(string(fc.params[len(fc.params)-1]), `"createIfEmpty":true`) {
		t.Fatalf("createIfEmpty not forwarded: %s", fc.params[len(fc.params)-1])
	}

	nav := callChromeTool(t, fc, pol, "navigate", `{"url":"e.example"}`)
	if nav.IsError || !strings.Contains(textOf(nav), "availableTabs") {
		t.Fatalf("navigate without tabId should append the tab listing: %s", textOf(nav))
	}
	if !strings.Contains(textOf(nav), browser.TabCleanupNote("tabs_close_mcp")) {
		t.Fatalf("navigate without tabId should say the tabs are the caller's to close: %s", textOf(nav))
	}

	// keep_open reaches the extension with navigate and tabs_create_mcp.
	callChromeTool(t, fc, pol, "navigate", `{"url":"e.example","tabId":3,"keep_open":true}`)
	if p := string(fc.params[len(fc.params)-1]); !strings.Contains(p, `"keep_open":true`) {
		t.Fatalf("navigate keep_open not forwarded: %s", p)
	}
	callChromeTool(t, fc, pol, "tabs_create_mcp", `{"keep_open":true}`)
	if p := string(fc.params[len(fc.params)-1]); !strings.Contains(p, `"keep_open":true`) || fc.sent[len(fc.sent)-1] != chromehost.MethodTabsCreate {
		t.Fatalf("tabs_create_mcp keep_open not forwarded: %s", p)
	}
	callChromeTool(t, fc, pol, "tabs_create_mcp", `{}`)
	if p := string(fc.params[len(fc.params)-1]); strings.Contains(p, "keep_open") {
		t.Fatalf("tabs_create_mcp sent keep_open unasked: %s", p)
	}

	js := callChromeTool(t, fc, pol, "javascript_tool", `{"action":"javascript_exec","text":"6*7","tabId":3}`)
	if js.IsError || !strings.Contains(textOf(js), "42") || !strings.Contains(textOf(js), "untrusted-web-content") {
		t.Fatalf("javascript_tool = %s", textOf(js))
	}
	rs := callChromeTool(t, fc, pol, "resize_window", `{"width":800,"height":600,"tabId":3}`)
	if rs.IsError || !strings.Contains(textOf(rs), "800x600") {
		t.Fatalf("resize_window = %s", textOf(rs))
	}

	// Denied site and non-web URLs are refused before reaching the extension.
	before := len(fc.sent)
	for _, args := range []string{`{"url":"https://bank.example/"}`, `{"url":"chrome://settings"}`} {
		if res := callChromeTool(t, fc, pol, "navigate", args); !res.IsError {
			t.Fatalf("navigate %s allowed", args)
		}
	}
	if res := callChromeTool(t, fc, pol, "browser_batch", `{"actions":[{"name":"navigate","input":{"url":"bank.example","tabId":3}}]}`); !res.IsError {
		t.Fatal("batch navigate to a denied site allowed")
	}
	if len(fc.sent) != before {
		t.Fatalf("refused calls reached the extension: %v", fc.sent[before:])
	}
	// save_to needs a Write check this mode cannot ask for.
	if res := callChromeTool(t, fc, pol, "computer", `{"action":"screenshot","tabId":3,"save_to":"/tmp/x.png"}`); !res.IsError {
		t.Fatal("save_to accepted over MCP")
	}
	// Missing required arguments are rejected in band.
	if res := callChromeTool(t, fc, pol, "computer", `{"action":"screenshot"}`); !res.IsError {
		t.Fatal("missing tabId accepted")
	}
}

func TestChromeMcpNotConnected(t *testing.T) {
	fc := &fakeChrome{err: chromehost.ErrChromeNotConnected}
	res := callChromeTool(t, fc, browser.SitePolicy{}, "read_page", `{"tabId":1}`)
	if !res.IsError || !strings.Contains(textOf(res), "gocode chrome install") {
		t.Fatalf("not connected = %+v", res)
	}
}

// TestChromeMcpServesStdio runs the real entrypoint over pipes: initialize and
// tools/list answer on stdout, and the process ends cleanly when stdin closes.
// No Chrome is involved (no tool is called).
func TestChromeMcpServesStdio(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	env := testEnvWithIO(t, inR, outW)
	done := make(chan int, 1)
	go func() { done <- runChromeMcp(env); outW.Close() }()

	br := bufio.NewReader(outR)
	send := func(s string) { _, _ = io.WriteString(inW, s+"\n") }
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "gocode in Chrome") {
		t.Fatalf("initialize reply %q %v", line, err)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	line, err = br.ReadString('\n')
	if err != nil || !strings.Contains(line, `"tabs_context_mcp"`) || strings.Contains(line, "switch_browser") {
		t.Fatalf("tools/list reply %q %v", line, err)
	}
	inW.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// testEnvWithIO is an Env whose home, config dir and cwd are private temp dirs.
func testEnvWithIO(t *testing.T, in io.Reader, out io.Writer) *Env {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	vars := map[string]string{"HOME": home, "GOCODE_CONFIG_DIR": home + "/.gocode"}
	return &Env{
		Getenv: func(k string) string { return vars[k] },
		Setenv: func(string, string) {},
		Stdout: out,
		Stderr: io.Discard,
		Stdin:  in,
		Cwd:    func() (string, error) { return cwd, nil },
	}
}

// The site rules come from the ordinary permissions settings.
func TestChromeSiteRulesFromSettings(t *testing.T) {
	env := testEnvWithIO(t, strings.NewReader(""), io.Discard)
	cwd, _ := env.Cwd()
	dir := cwd + "/.gocode"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"permissions":{"allow":["gocodeInChromeDomain(github.com)","Bash(ls:*)"],"deny":["gocodeInChromeDomain(bank.example)"]}}`
	if err := os.WriteFile(dir+"/settings.json", []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := browser.NewSitePolicy(toolrun.SiteRulesFrom(loadPermissionRules(env)), false)
	if pol.Check("github.com") != browser.SiteAllowed || pol.Check("bank.example") != browser.SiteDenied || pol.Check("x.example") != browser.SiteUnknown {
		t.Fatalf("policy from settings = %+v", pol)
	}
}
