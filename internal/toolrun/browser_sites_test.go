package toolrun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// scriptedBrowser answers per method; errs are returned (and consumed) first.
type scriptedBrowser struct {
	results map[string]json.RawMessage
	errs    map[string][]error
	calls   []recordedCall
}

type recordedCall struct {
	method string
	params map[string]json.RawMessage
}

func (s *scriptedBrowser) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	var p map[string]json.RawMessage
	if raw, ok := params.(json.RawMessage); ok {
		_ = json.Unmarshal(raw, &p)
	}
	s.calls = append(s.calls, recordedCall{method: method, params: p})
	if q := s.errs[method]; len(q) > 0 {
		s.errs[method] = q[1:]
		return nil, q[0]
	}
	return s.results[method], nil
}

func (s *scriptedBrowser) policyOf(i int) struct {
	Allowed        []string `json:"allowed"`
	Denied         []string `json:"denied"`
	RequireAllowed bool     `json:"requireAllowed"`
} {
	var p struct {
		Allowed        []string `json:"allowed"`
		Denied         []string `json:"denied"`
		RequireAllowed bool     `json:"requireAllowed"`
	}
	_ = json.Unmarshal(s.calls[i].params["sitePolicy"], &p)
	return p
}

// recordingPrompter answers every prompt with resp and records the requests.
type recordingPrompter struct {
	resp PermissionResponse
	reqs []PermissionRequest
}

func (p *recordingPrompter) Prompt(_ context.Context, req PermissionRequest) (PermissionResponse, error) {
	p.reqs = append(p.reqs, req)
	return p.resp, nil
}

func siteRule(b toolreg.RuleBehavior, host string) toolreg.Rule {
	return toolreg.Rule{Behavior: b, ToolName: "gocodeInChromeDomain", Content: host}
}

func run(t *testing.T, d *Dispatcher, name, input string) agent.ToolResult {
	t.Helper()
	res, err := d.Dispatch(context.Background(), agent.ToolUse{Name: name, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("Dispatch(%s): %v", name, err)
	}
	return res
}

func TestSites_NavigateAllowedByRuleRunsWithoutPrompt(t *testing.T) {
	c := &scriptedBrowser{results: map[string]json.RawMessage{chromehost.MethodNavigate: json.RawMessage(`{"tabId":5,"url":"https://github.com/x"}`)}}
	d := New(toolreg.ModeDefault, toolreg.RuleSet{siteRule(toolreg.RuleAllow, "github.com")}, WithBrowserClient(c))
	res := run(t, d, "navigate", `{"url":"https://GitHub.com/x"}`)
	if res.IsError {
		t.Fatalf("navigate to an allowed site was refused: %s", res.Content)
	}
	pol := c.policyOf(0)
	if !pol.RequireAllowed || len(pol.Allowed) != 1 || pol.Allowed[0] != "github.com" {
		t.Fatalf("policy sent to the extension = %+v", pol)
	}
}

func TestSites_DeniedSiteRefusedInEveryMode(t *testing.T) {
	for _, mode := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModeBypassPermissions, toolreg.ModeAuto} {
		c := &scriptedBrowser{}
		rules := toolreg.RuleSet{siteRule(toolreg.RuleDeny, "bank.example"), siteRule(toolreg.RuleAllow, "bank.example")}
		d := New(mode, rules, WithBrowserClient(c))
		for _, in := range []struct{ tool, input string }{
			{"navigate", `{"url":"https://bank.example/login"}`},
			{"tabs_create", `{"url":"bank.example"}`},
			{"browser_batch", `{"actions":[{"name":"navigate","input":{"url":"http://bank.example","tabId":1}}]}`},
		} {
			res := run(t, d, in.tool, in.input)
			if !res.IsError || !strings.Contains(res.Content, "denied on bank.example") {
				t.Fatalf("mode %s %s: want denied, got %+v", mode, in.tool, res)
			}
		}
		if len(c.calls) != 0 {
			t.Fatalf("mode %s: a denied navigation reached the extension", mode)
		}
	}
}

func TestSites_NonWebURLRefused(t *testing.T) {
	c := &scriptedBrowser{}
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c))
	for _, u := range []string{"chrome://settings", "javascript:alert(1)", "file:///etc/passwd", "https://user:pw@example.com/"} {
		res := run(t, d, "navigate", `{"url":`+string(siteJSON(u))+`}`)
		if !res.IsError || !strings.Contains(res.Content, "browser-internal") {
			t.Fatalf("navigate %q: want non-web refusal, got %+v", u, res)
		}
	}
	if len(c.calls) != 0 {
		t.Fatal("a non-web navigation reached the extension")
	}
}

func siteJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestSites_UnknownSiteAsksWithSiteRuleAndAllowsThisCall(t *testing.T) {
	c := &scriptedBrowser{results: map[string]json.RawMessage{chromehost.MethodNavigate: json.RawMessage(`{"tabId":5,"url":"https://example.com/"}`)}}
	p := &recordingPrompter{resp: PermissionResponse{Kind: PermissionAllowedOnce}}
	// Even bypass asks for a site with no rule.
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c), WithPermissionPrompter(p))
	res := run(t, d, "navigate", `{"url":"example.com"}`)
	if res.IsError {
		t.Fatalf("approved navigation failed: %s", res.Content)
	}
	if len(p.reqs) != 1 || len(p.reqs[0].Suggested) == 0 || p.reqs[0].Suggested[0] != "gocodeInChromeDomain(example.com)" {
		t.Fatalf("prompt = %+v", p.reqs)
	}
	// The approved host is allowed for this call in the policy the extension sees.
	if pol := c.policyOf(0); len(pol.Allowed) != 1 || pol.Allowed[0] != "example.com" {
		t.Fatalf("policy for the approved call = %+v", pol)
	}
	// Allow-once does not grant the next call.
	run(t, d, "navigate", `{"url":"example.com/2"}`)
	if len(p.reqs) != 2 {
		t.Fatalf("allow-once was remembered: %d prompts", len(p.reqs))
	}
}

func TestSites_UnknownSiteWithoutPrompterIsDeniedWithHowTo(t *testing.T) {
	c := &scriptedBrowser{}
	d := New(toolreg.ModeDefault, nil, WithBrowserClient(c))
	res := run(t, d, "navigate", `{"url":"https://example.com"}`)
	if !res.IsError || !strings.Contains(res.Content, `gocodeInChromeDomain(example.com)`) {
		t.Fatalf("want how-to-allow denial, got %+v", res)
	}
	if len(c.calls) != 0 {
		t.Fatal("unapproved navigation reached the extension")
	}
}

func TestSites_ExtensionNeedsApprovalPromptsAndRetries(t *testing.T) {
	needs := errors.New("chrome extension error: needs_approval: gocode in Chrome needs permission to act on shop.example [chrome_site=shop.example]")
	c := &scriptedBrowser{
		results: map[string]json.RawMessage{chromehost.MethodComputer: json.RawMessage(`{"ok":true}`)},
		errs:    map[string][]error{chromehost.MethodComputer: {needs}},
	}
	rule := siteRule(toolreg.RuleAllow, "shop.example")
	p := &recordingPrompter{resp: PermissionResponse{Kind: PermissionAllowedAlways, Rule: &rule}}
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c), WithPermissionPrompter(p))
	res := run(t, d, "computer", `{"action":"left_click","coordinate":[1,2],"tabId":3}`)
	if res.IsError {
		t.Fatalf("approved click failed: %s", res.Content)
	}
	if len(p.reqs) != 1 || p.reqs[0].Suggested[0] != "gocodeInChromeDomain(shop.example)" {
		t.Fatalf("prompt = %+v", p.reqs)
	}
	if len(c.calls) != 2 {
		t.Fatalf("want the call retried once, got %d calls", len(c.calls))
	}
	if pol := c.policyOf(1); len(pol.Allowed) != 1 || pol.Allowed[0] != "shop.example" {
		t.Fatalf("retry policy = %+v", pol)
	}
	// "Don't ask again" added the rule: later calls carry it without prompting.
	run(t, d, "computer", `{"action":"left_click","coordinate":[1,2],"tabId":3}`)
	if len(p.reqs) != 1 {
		t.Fatal("asked again after don't-ask-again")
	}
	if pol := c.policyOf(2); len(pol.Allowed) != 1 {
		t.Fatalf("policy after the rule was added = %+v", pol)
	}
}

func TestSites_ExtensionNeedsApprovalDeniedByUser(t *testing.T) {
	needs := errors.New("needs_approval: x [chrome_site=shop.example]")
	c := &scriptedBrowser{errs: map[string][]error{chromehost.MethodFormInput: {needs}}}
	p := &recordingPrompter{resp: PermissionResponse{Kind: PermissionDenied}}
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c), WithPermissionPrompter(p))
	res := run(t, d, "form_input", `{"ref":"ref_1","value":"x","tabId":3}`)
	if !res.IsError || len(c.calls) != 1 {
		t.Fatalf("denied site approval should fail without retry: %+v calls=%d", res, len(c.calls))
	}
}

func TestSites_BatchResumesAtTheStepThatNeededApproval(t *testing.T) {
	c := &scriptedBrowser{results: map[string]json.RawMessage{}}
	first := json.RawMessage(`{"results":[{"name":"read_page","ok":true},{"name":"computer","error":"needs_approval: act [chrome_site=shop.example]"}]}`)
	second := json.RawMessage(`{"results":[{"name":"computer","ok":true},{"name":"computer","ok":true}]}`)
	calls := 0
	client := browserFunc(func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		calls++
		var p struct {
			Actions []json.RawMessage `json:"actions"`
		}
		_ = json.Unmarshal(params.(json.RawMessage), &p)
		if calls == 1 {
			if len(p.Actions) != 3 {
				t.Errorf("first batch call has %d actions", len(p.Actions))
			}
			return first, nil
		}
		if len(p.Actions) != 2 {
			t.Errorf("resumed batch has %d actions, want 2", len(p.Actions))
		}
		return second, nil
	})
	_ = c
	p := &recordingPrompter{resp: PermissionResponse{Kind: PermissionAllowedOnce}}
	d := New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(client), WithPermissionPrompter(p))
	res := run(t, d, "browser_batch", `{"actions":[{"name":"read_page","input":{"tabId":1}},{"name":"computer","input":{"action":"type","text":"a","tabId":1}},{"name":"computer","input":{"action":"key","text":"Enter","tabId":1}}]}`)
	if res.IsError || !strings.Contains(res.Content, "ran 3 of 3") {
		t.Fatalf("batch result = %+v", res)
	}
	if len(p.reqs) != 1 || calls != 2 {
		t.Fatalf("prompts=%d calls=%d", len(p.reqs), calls)
	}
}

type browserFunc func(ctx context.Context, method string, params any) (json.RawMessage, error)

func (f browserFunc) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return f(ctx, method, params)
}

func TestSites_ReadToolsAskNothing(t *testing.T) {
	c := &scriptedBrowser{results: map[string]json.RawMessage{chromehost.MethodReadPage: json.RawMessage(`{"text":"hi","url":"https://x.example/"}`)}}
	d := New(toolreg.ModeDefault, nil, WithBrowserClient(c))
	if res := run(t, d, "read_page", `{}`); res.IsError {
		t.Fatalf("read_page on an unknown site: %s", res.Content)
	}
}

func TestScreenshotSaveTo_GoesThroughWriteCheck(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz"))
	shot := json.RawMessage(`{"dataUrl":"data:image/png;base64,` + png + `"}`)
	dir := t.TempDir()
	target := filepath.Join(dir, "shots", "page.png")

	// A Write deny rule refuses before anything is captured.
	c := &scriptedBrowser{results: map[string]json.RawMessage{chromehost.MethodScreenshot: shot, chromehost.MethodComputer: shot}}
	deny := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Write"}}
	d := New(toolreg.ModeBypassPermissions, deny, WithBrowserClient(c))
	res := run(t, d, "screenshot", `{"save_to":`+string(siteJSON(target))+`}`)
	if !res.IsError || len(c.calls) != 0 {
		t.Fatalf("save_to under a Write deny rule: %+v calls=%d", res, len(c.calls))
	}

	// Default mode with no prompter: the Write asks, so it is refused.
	d = New(toolreg.ModeDefault, nil, WithBrowserClient(c))
	if res := run(t, d, "screenshot", `{"save_to":`+string(siteJSON(target))+`}`); !res.IsError {
		t.Fatalf("save_to without approval was allowed: %+v", res)
	}

	// A relative path is refused.
	d = New(toolreg.ModeBypassPermissions, nil, WithBrowserClient(c))
	if res := run(t, d, "screenshot", `{"save_to":"rel.png"}`); !res.IsError {
		t.Fatalf("relative save_to accepted: %+v", res)
	}

	// Allowed (via the prompt): the file is written and the image still returned.
	p := &recordingPrompter{resp: PermissionResponse{Kind: PermissionAllowedOnce}}
	d = New(toolreg.ModeDefault, nil, WithBrowserClient(c), WithPermissionPrompter(p))
	res = run(t, d, "computer", `{"action":"screenshot","tabId":1,"save_to":`+string(siteJSON(target))+`}`)
	if res.IsError || len(res.Images) != 1 || !strings.Contains(res.Content, "Screenshot saved to: "+target) {
		t.Fatalf("computer screenshot save_to = %+v", res)
	}
	// computer itself is gated like Bash in default mode; the save_to path is
	// then asked as a Write.
	if len(p.reqs) != 2 || p.reqs[1].Tool != protocol.ToolWrite {
		t.Fatalf("want the computer prompt then a Write prompt, got %+v", p.reqs)
	}
	got, err := os.ReadFile(target)
	if err != nil || !strings.HasPrefix(string(got), "\x89PNG") {
		t.Fatalf("saved file = %q, %v", got, err)
	}
	// save_to / save_to_disk never reach the extension.
	last := c.calls[len(c.calls)-1]
	if _, ok := last.params["save_to"]; ok {
		t.Fatal("save_to was sent to the extension")
	}
}
