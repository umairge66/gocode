package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/browser"
)

// This file adapts the milestone-2 browser-control tools (internal/tools/browser)
// to the dispatcher. Each adapter resolves the chrome-host control client, relays
// a browser verb to the extension, and maps the reply to a tool_result. When the
// client returns chromehost.ErrChromeNotConnected the run func returns that error
// and Dispatch surfaces it as a clear, non-fatal is_error tool_result (the user
// is told to load/connect the extension) — it never crashes the session.
//
// screenshot uses the runImages seam so its decoded PNG flows through the SAME
// structured-image delivery path as a Read of an image (ReadResult.Images ->
// agent.ToolResultImage), so the model actually sees the capture.

// disconnectedClient is the default browser.Client installed when the host wired
// none: every call reports Chrome is not connected, so the browser tools stay
// advertised and dispatchable without a live socket.
type disconnectedClient struct{}

func (disconnectedClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return nil, chromehost.ErrChromeNotConnected
}

// browserClient returns the dispatcher's injected client, or the disconnected
// client when none was wired.
func (d *Dispatcher) browserClient() browser.Client {
	if d.BrowserClient != nil {
		return d.BrowserClient
	}
	return disconnectedClient{}
}

// registerBrowserTools registers (or re-registers) all browser adapters
// (milestones 2 + 3) against the current browser client.
//
// Every call carries the session's per-site policy (the gocodeInChromeDomain
// allow/deny rules, see sites.go in internal/tools/browser) so the extension
// enforces it too. navigate / tabs_create / browser_batch navigations are
// checked up front (refineSites); a call on a page whose site has no allow rule
// is refused by the extension as "needs approval", prompted for here, and
// retried once the user allows it (withSiteApproval).
func (d *Dispatcher) registerBrowserTools() {
	raw := d.browserClient()
	c := browser.PolicyClient{Inner: raw, Policy: d.sitePolicy}
	autoShot := !d.BrowserNoScreenshotAfterActions
	batch := browser.BatchTool{Client: c, Approve: d.approveSite(protocol.ToolBrowserBatch), AutoScreenshot: autoShot}
	computer := BrowserComputerAdapter(browser.ComputerTool{Client: c, AutoScreenshot: autoShot})
	batchAdapter := BrowserBatchAdapter(batch)
	if !autoShot {
		computer.description = computerDescriptionNoAutoShot
		batchAdapter.description = batchDescriptionNoAutoShot
	}

	d.Register(protocol.ToolBrowserNavigate, d.siteChecked(protocol.ToolBrowserNavigate, BrowserNavigateAdapter(browser.NavigateTool{Client: c, ListTabs: true})))
	d.Register(protocol.ToolBrowserReadPage, d.siteChecked(protocol.ToolBrowserReadPage, BrowserReadPageAdapter(protocol.ToolBrowserReadPage, browser.ReadPageTool{Client: c, Method: string(protocol.ToolBrowserReadPage)})))
	d.Register(protocol.ToolBrowserGetPageText, d.siteChecked(protocol.ToolBrowserGetPageText, BrowserReadPageAdapter(protocol.ToolBrowserGetPageText, browser.ReadPageTool{Client: c, Method: string(protocol.ToolBrowserGetPageText)})))
	d.Register(protocol.ToolBrowserTabsContext, BrowserTabsContextAdapter(browser.TabsContextTool{Client: c}))
	d.Register(protocol.ToolBrowserTabsCreate, d.siteChecked(protocol.ToolBrowserTabsCreate, BrowserTabsCreateAdapter(browser.TabsCreateTool{Client: c})))
	d.Register(protocol.ToolBrowserTabsClose, BrowserTabsCloseAdapter(browser.TabsCloseTool{Client: c}))
	d.Register(protocol.ToolBrowserScreenshot, d.screenshotPathChecked(d.siteChecked(protocol.ToolBrowserScreenshot, BrowserScreenshotAdapter(browser.ScreenshotTool{Client: c}))))
	d.Register(protocol.ToolBrowserComputer, d.screenshotPathChecked(d.siteChecked(protocol.ToolBrowserComputer, computer)))
	d.Register(protocol.ToolBrowserFind, d.siteChecked(protocol.ToolBrowserFind, BrowserFindAdapter(browser.FindTool{Client: c})))
	d.Register(protocol.ToolBrowserFormInput, d.siteChecked(protocol.ToolBrowserFormInput, BrowserFormInputAdapter(browser.FormInputTool{Client: c})))
	d.Register(protocol.ToolBrowserBatch, d.siteChecked(protocol.ToolBrowserBatch, batchAdapter))

	if d.BrowserNotice != nil {
		for _, tool := range []protocol.Tool{
			protocol.ToolBrowserNavigate, protocol.ToolBrowserReadPage, protocol.ToolBrowserGetPageText,
			protocol.ToolBrowserTabsContext, protocol.ToolBrowserTabsCreate, protocol.ToolBrowserTabsClose,
			protocol.ToolBrowserScreenshot, protocol.ToolBrowserComputer, protocol.ToolBrowserFind,
			protocol.ToolBrowserFormInput, protocol.ToolBrowserBatch,
		} {
			d.adapters[tool] = withNotice(d.adapters[tool], d.BrowserNotice)
		}
	}
}

// withNotice appends notice's message (when it returns one) to a successful
// result of a, so a one-time message such as "reload the extension" reaches
// the user with the next browser tool result.
func withNotice(a adapter, notice func(ctx context.Context) string) adapter {
	appendNotice := func(ctx context.Context, out string) string {
		msg := notice(ctx)
		if msg == "" {
			return out
		}
		if out == "" {
			return "Note: " + msg
		}
		return out + "\n\nNote: " + msg
	}
	if run := a.run; run != nil {
		a.run = func(ctx context.Context, input json.RawMessage) (string, error) {
			out, err := run(ctx, input)
			if err != nil {
				return out, err
			}
			return appendNotice(ctx, out), nil
		}
	}
	if run := a.runImages; run != nil {
		a.runImages = func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			out, imgs, err := run(ctx, input)
			if err != nil {
				return out, imgs, err
			}
			return appendNotice(ctx, out), imgs, nil
		}
	}
	return a
}

// --- per-site permissions ----------------------------------------------------

// sitePolicy builds the browser site policy from the live rules: every
// gocodeInChromeDomain(<host>) allow / deny rule. In a session an action on a
// site with no allow rule asks (RequireAllowed).
func (d *Dispatcher) sitePolicy() browser.SitePolicy {
	return browser.NewSitePolicy(SiteRulesFrom(d.SnapshotRules()), true)
}

// SiteRulesFrom extracts the gocodeInChromeDomain rule contents of a rule set.
// A rule with no host (bare tool name, "" or "*") names no site and is
// ignored (a site rule pattern requires a host).
func SiteRulesFrom(rules toolreg.RuleSet) browser.SiteRules {
	var out browser.SiteRules
	for _, r := range rules {
		if r.ToolName != browser.SiteRuleTool || r.Content == "" || r.Content == "*" {
			continue
		}
		switch r.Behavior {
		case toolreg.RuleAllow:
			out.Allow = append(out.Allow, r.Content)
		case toolreg.RuleDeny:
			out.Deny = append(out.Deny, r.Content)
		}
	}
	return out
}

// Messages of the in-chrome permission check.
const (
	siteNonWebMessage     = "Can't interact with browser-internal or unparseable URLs. Navigate to a web page first."
	siteUnreadableMessage = "Can't run this gocode in Chrome action: its input couldn't be safely checked."
)

func siteDeniedMessage(host string) string {
	return "gocode in Chrome is denied on " + host + "."
}

// siteAskMessage is the reason shown (and, with no interactive user, the
// denial) when a site needs approval.
func siteAskMessage(host string) string {
	return fmt.Sprintf("gocode in Chrome requires permission to act on %s. To allow it without asking, add \"%s(%s)\" to permissions.allow in your settings.", host, browser.SiteRuleTool, host)
}

// siteAskCall is the call a site prompt is about: the browser tool, keyed under
// the gocodeInChromeDomain rule for the host, so "don't ask again" saves
// gocodeInChromeDomain(<host>).
func siteAskCall(tool protocol.Tool, host string) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) {
	pc := toolreg.Call{Tool: tool, Rule: browser.SiteRuleTool, RuleContent: host}
	desc := toolreg.ToolDescriptor{Name: tool, RulePrefix: browser.SiteRuleTool}
	dec := toolreg.Decision{Behavior: toolreg.BehaviorAsk, ReasonType: toolreg.ReasonSafetyCheck, Reason: siteAskMessage(host)}
	return pc, desc, dec
}

// navigationTargets returns the URLs a call navigates to (navigate, a
// tabs_create url, a browser_batch's navigate / tabs_create items), skipping
// back/forward: their destination is only known once there, so the extension
// checks the page they land on (and goes back again when it is refused; a
// site that needs approval comes back as "needs approval" and is prompted for
// like any other). bad is set when an input that names a navigation cannot be
// read.
func navigationTargets(tool protocol.Tool, input json.RawMessage) (urls []string, bad bool) {
	type navIn struct {
		URL *string `json:"url"`
	}
	add := func(u string) {
		if !browser.IsHistoryNavigation(u) {
			urls = append(urls, u)
		}
	}
	switch tool {
	case protocol.ToolBrowserNavigate, protocol.ToolBrowserTabsCreate:
		var in navIn
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, true
		}
		if in.URL != nil && (*in.URL != "" || tool == protocol.ToolBrowserNavigate) {
			add(*in.URL)
		}
	case protocol.ToolBrowserBatch:
		var in struct {
			Actions []json.RawMessage `json:"actions"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, true
		}
		for _, raw := range in.Actions {
			var item struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return nil, true
			}
			switch item.Name {
			case "navigate", "tabs_create", "tabs_create_mcp":
				var ni struct {
					URL any `json:"url"`
				}
				if len(item.Input) > 0 && json.Unmarshal(item.Input, &ni) != nil {
					return nil, true
				}
				switch u := ni.URL.(type) {
				case string:
					if u != "" || item.Name == "navigate" {
						add(u)
					}
				case nil:
					if item.Name == "navigate" {
						return nil, true
					}
				default:
					return nil, true
				}
			}
		}
	}
	return urls, false
}

// navigationHosts returns the web hosts of a call's navigation targets.
func navigationHosts(tool protocol.Tool, input json.RawMessage) []string {
	urls, _ := navigationTargets(tool, input)
	var hosts []string
	for _, u := range urls {
		if h, ok := browser.WebHost(u); ok {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// refineSites applies the site rules to a call that navigates:
//
//   - an unreadable input, a non-web URL, or a denied host denies (in every
//     mode — a deny rule wins over everything);
//   - when every target host is allowed, a plain "not granted yet" ask becomes
//     allow (the site rule is the grant);
//   - a target host with no rule asks — even where the mode or a whole-tool
//     allow rule would have allowed the call (the in-chrome tools ignore
//     whole-tool allow rules and are excluded from bypass / auto allow) —
//     offering gocodeInChromeDomain(<host>) when there is one host.
func (d *Dispatcher) refineSites(tool protocol.Tool) func(json.RawMessage, toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) {
	return func(input json.RawMessage, pc toolreg.Call, desc toolreg.ToolDescriptor, decision toolreg.Decision) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) {
		deny := func(rt string, msg string) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) {
			return pc, desc, toolreg.Decision{Behavior: toolreg.BehaviorDeny, ReasonType: rt, Reason: msg}
		}
		urls, bad := navigationTargets(tool, input)
		if bad {
			return deny(toolreg.ReasonSafetyCheck, siteUnreadableMessage)
		}
		if len(urls) == 0 {
			return pc, desc, decision
		}
		pol := d.sitePolicy()
		var unknown []string
		seen := map[string]bool{}
		for _, u := range urls {
			host, ok := browser.WebHost(u)
			if !ok {
				return deny(toolreg.ReasonSafetyCheck, siteNonWebMessage)
			}
			switch pol.Check(host) {
			case browser.SiteDenied:
				return deny(toolreg.ReasonRule, siteDeniedMessage(host))
			case browser.SiteUnknown:
				if !seen[host] {
					seen[host] = true
					unknown = append(unknown, host)
				}
			}
		}
		if decision.Behavior == toolreg.BehaviorDeny {
			return pc, desc, decision
		}
		if len(unknown) == 0 {
			if decision.Behavior == toolreg.BehaviorAsk && askSatisfiableByAllowRule(decision) {
				return pc, desc, toolreg.Decision{Behavior: toolreg.BehaviorAllow, ReasonType: toolreg.ReasonRule, Reason: string(tool) + " is allowed by a " + browser.SiteRuleTool + " rule."}
			}
			return pc, desc, decision
		}
		// A specific (content) allow rule the user wrote for this tool keeps
		// its allow; an ask rule / plan hold / safety ask keeps its own prompt.
		if decision.Behavior == toolreg.BehaviorAllow && decision.ReasonType == toolreg.ReasonRule &&
			decision.Rule != nil && decision.Rule.Content != "" && decision.Rule.Content != "*" {
			return pc, desc, decision
		}
		if decision.Behavior == toolreg.BehaviorAsk && !askSatisfiableByAllowRule(decision) {
			return pc, desc, decision
		}
		if d.CurrentMode() == toolreg.ModeDontAsk {
			return deny(toolreg.ReasonMode, siteAskMessage(strings.Join(unknown, ", ")))
		}
		if len(unknown) == 1 {
			return siteAskCall(tool, unknown[0])
		}
		return pc, desc, toolreg.Decision{Behavior: toolreg.BehaviorAsk, ReasonType: toolreg.ReasonSafetyCheck, Reason: siteAskMessage(strings.Join(unknown, ", "))}
	}
}

// approveSite returns the ApproveFunc a browser call uses when the extension
// refused a page as "needs approval": a denied site stays refused, an allowed
// one (granted meanwhile) passes, otherwise the user is asked through the
// normal permission prompt (offering gocodeInChromeDomain(<site>)).
func (d *Dispatcher) approveSite(tool protocol.Tool) browser.ApproveFunc {
	return func(ctx context.Context, site string) error {
		switch d.sitePolicy().Check(site) {
		case browser.SiteDenied:
			return errors.New(siteDeniedMessage(site))
		case browser.SiteAllowed:
			return nil
		}
		if d.CurrentMode() == toolreg.ModeDontAsk {
			return errors.New(siteAskMessage(site))
		}
		pc, desc, dec := siteAskCall(tool, site)
		input, _ := json.Marshal(map[string]string{"site": site})
		out := d.resolveAsk(ctx, pc, input, desc, dec)
		if !out.allowed {
			return errors.New(out.denyMessage)
		}
		if out.input != nil {
			return errors.New(errChangedInput)
		}
		return nil
	}
}

// siteChecked wires a browser adapter into the site rules: navigation targets
// are checked before the call (refine) and allowed for the call itself; a
// "needs approval" refusal from the extension is prompted for and the call
// retried once.
func (d *Dispatcher) siteChecked(tool protocol.Tool, a adapter) adapter {
	a.refine = d.refineSites(tool)
	approve := d.approveSite(tool)
	withHosts := func(ctx context.Context, input json.RawMessage) context.Context {
		return browser.WithApprovedSites(ctx, navigationHosts(tool, input)...)
	}
	if run := a.run; run != nil {
		a.run = func(ctx context.Context, input json.RawMessage) (string, error) {
			ctx = withHosts(ctx, input)
			out, err := run(ctx, input)
			if na, ok := browser.AsNeedsApproval(err); ok && tool != protocol.ToolBrowserBatch {
				if aerr := approve(ctx, na.Site); aerr != nil {
					return "", aerr
				}
				return run(browser.WithApprovedSites(ctx, na.Site), input)
			}
			return out, err
		}
	}
	if run := a.runImages; run != nil {
		a.runImages = func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			ctx = withHosts(ctx, input)
			out, imgs, err := run(ctx, input)
			if na, ok := browser.AsNeedsApproval(err); ok && tool != protocol.ToolBrowserBatch {
				if aerr := approve(ctx, na.Site); aerr != nil {
					return "", nil, aerr
				}
				return run(browser.WithApprovedSites(ctx, na.Site), input)
			}
			return out, imgs, err
		}
	}
	return a
}

// screenshotPathChecked runs a screenshot's save_to path through the normal
// Write permission check (deny/ask/allow rules, the path guard, the session's
// file-write checks, a prompt when asked) before the capture runs.
func (d *Dispatcher) screenshotPathChecked(a adapter) adapter {
	run := a.runImages
	if run == nil {
		return a
	}
	a.runImages = func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
		var in struct {
			Action string `json:"action"`
			SaveTo string `json:"save_to"`
		}
		_ = json.Unmarshal(input, &in)
		// computer saves only what it captures (screenshot / zoom); the
		// screenshot tool has no action.
		captures := in.Action == "" || in.Action == "screenshot" || in.Action == "zoom"
		if in.SaveTo != "" && captures {
			if err := d.authorizeWrite(ctx, in.SaveTo); err != nil {
				return "", nil, err
			}
		}
		return run(ctx, input)
	}
	return a
}

// authorizeWrite decides a write of path exactly as a Write tool call would.
func (d *Dispatcher) authorizeWrite(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("save_to must be an absolute path")
	}
	desc, _ := d.Registry.Lookup(protocol.ToolWrite)
	pc := toolreg.Call{Tool: protocol.ToolWrite, Rule: desc.Rule(), RuleContent: path, Path: path, Op: pathguard.OpWrite}
	decision := d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), nil)
	switch decision.Behavior {
	case toolreg.BehaviorAllow:
		return nil
	case toolreg.BehaviorAsk:
		input, _ := json.Marshal(map[string]string{"file_path": path})
		out := d.resolveAsk(ctx, pc, input, desc, decision)
		if !out.allowed {
			return errors.New(out.denyMessage)
		}
		if out.input != nil {
			return errors.New(errChangedInput)
		}
		return nil
	default:
		if decision.Reason != "" {
			return errors.New(decision.Reason)
		}
		return errors.New("Permission to write " + path + " has been denied.")
	}
}

// BrowserComputerAdapter adapts browser.ComputerTool. ACTION (gated like Bash):
// it drives mouse/keyboard input on the user's logged-in sites via CDP. The rule
// content is the action so a computer(<action>) rule can scope it. screenshot/
// zoom actions emit an image, so this uses runImages.
func BrowserComputerAdapter(t browser.ComputerTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.ComputerInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.Action, "", "", nil
		},
		runImages: func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			var in browser.ComputerInput
			if err := unmarshal(input, &in); err != nil {
				return "", nil, err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", nil, err
			}
			return res.Text, toAgentImages(res.Images), nil
		},
		description: toolDescriptions[protocol.ToolBrowserComputer],
		inputSchema: toolInputSchemas[protocol.ToolBrowserComputer],
	}
}

// BrowserFindAdapter adapts browser.FindTool. READ (leans auto-allow like Read):
// it only locates elements. No rule content.
func BrowserFindAdapter(t browser.FindTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.FindInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.FindInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserFind],
		inputSchema: toolInputSchemas[protocol.ToolBrowserFind],
	}
}

// BrowserFormInputAdapter adapts browser.FormInputTool. ACTION (gated like
// Bash): it changes form state. The rule content is the target ref.
func BrowserFormInputAdapter(t browser.FormInputTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.FormInputInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.Ref, "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.FormInputInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserFormInput],
		inputSchema: toolInputSchemas[protocol.ToolBrowserFormInput],
	}
}

// BrowserBatchAdapter adapts browser.BatchTool. ACTION (gated like Bash): a
// batch can contain clicks/typing. It uses runImages because screenshot/zoom
// sub-commands return interleaved images.
func BrowserBatchAdapter(t browser.BatchTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.BatchInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		runImages: func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			var in browser.BatchInput
			if err := unmarshal(input, &in); err != nil {
				return "", nil, err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", nil, err
			}
			return res.Text, toAgentImages(res.Images), nil
		},
		description: toolDescriptions[protocol.ToolBrowserBatch],
		inputSchema: toolInputSchemas[protocol.ToolBrowserBatch],
	}
}

// toAgentImages converts browser image blocks to the dispatcher's image type.
func toAgentImages(imgs []browser.Image) []agent.ToolResultImage {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]agent.ToolResultImage, len(imgs))
	for i, im := range imgs {
		out[i] = agent.ToolResultImage{Base64: im.Base64, MediaType: im.MediaType}
	}
	return out
}

// BrowserNavigateAdapter adapts browser.NavigateTool. ACTION (gated like Bash);
// the rule content is the target URL so a navigate(<url-glob>) rule can scope it.
func BrowserNavigateAdapter(t browser.NavigateTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.NavigateInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.URL, "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.NavigateInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserNavigate],
		inputSchema: toolInputSchemas[protocol.ToolBrowserNavigate],
	}
}

// BrowserReadPageAdapter adapts browser.ReadPageTool for the given tool name
// (read_page or get_page_text). READ (leans auto-allow like Read); no rule
// content.
func BrowserReadPageAdapter(name protocol.Tool, t browser.ReadPageTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.ReadPageInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.ReadPageInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[name],
		inputSchema: toolInputSchemas[name],
	}
}

// BrowserTabsContextAdapter adapts browser.TabsContextTool. READ (leans
// auto-allow like Read); no input, no rule content.
func BrowserTabsContextAdapter(t browser.TabsContextTool) adapter {
	return adapter{
		permission: func(json.RawMessage) (string, string, pathguard.Operation, error) {
			return "", "", "", nil
		},
		run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			res, err := t.Execute(ctx, struct{}{})
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserTabsContext],
		inputSchema: toolInputSchemas[protocol.ToolBrowserTabsContext],
	}
}

// BrowserTabsCreateAdapter adapts browser.TabsCreateTool. ACTION (gated like
// Bash); the rule content is the target URL.
func BrowserTabsCreateAdapter(t browser.TabsCreateTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.TabsCreateInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.URL, "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.TabsCreateInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserTabsCreate],
		inputSchema: toolInputSchemas[protocol.ToolBrowserTabsCreate],
	}
}

// BrowserTabsCloseAdapter adapts browser.TabsCloseTool. ACTION (gated like
// Bash).
func BrowserTabsCloseAdapter(t browser.TabsCloseTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.TabsCloseInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in browser.TabsCloseInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return res.Text, nil
		},
		description: toolDescriptions[protocol.ToolBrowserTabsClose],
		inputSchema: toolInputSchemas[protocol.ToolBrowserTabsClose],
	}
}

// BrowserScreenshotAdapter adapts browser.ScreenshotTool. READ (leans auto-allow
// like Read). It uses runImages so the decoded PNG flows through the structured-
// image delivery path.
func BrowserScreenshotAdapter(t browser.ScreenshotTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in browser.ScreenshotInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		runImages: func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error) {
			var in browser.ScreenshotInput
			if err := unmarshal(input, &in); err != nil {
				return "", nil, err
			}
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", nil, err
			}
			if len(res.Images) == 0 {
				return res.Text, nil, nil
			}
			images := make([]agent.ToolResultImage, len(res.Images))
			for i, im := range res.Images {
				images[i] = agent.ToolResultImage{Base64: im.Base64, MediaType: im.MediaType}
			}
			return res.Text, images, nil
		},
		description: toolDescriptions[protocol.ToolBrowserScreenshot],
		inputSchema: toolInputSchemas[protocol.ToolBrowserScreenshot],
	}
}
