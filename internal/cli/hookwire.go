package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file carries the adapters that connect the hook host's non-command
// seams (internal/hookhost Options) to their real host dependencies: the MCP
// host, an HTTP client, the session model caller, and the permission-rule /
// tool-registry matching the `if` guard needs. None of it introduces any
// phone-home; the http hook posts only to the hook's own configured URL and the
// prompt hook calls only the provider the session already uses.

// --- MCP hook invoker ------------------------------------------------------

// mcpHookInvoker adapts the session MCP host to the hooks.MCPInvoker seam. The
// host is read lazily through get because the hook host is constructed (step 2
// of buildSession) before the MCP host exists (step 3); hooks only ever run
// during turns, by which point r.mcp is set.
//
// CallTool maps a server+tool to the model-facing MCP name and routes through
// mcphost.Host.Call, which returns (text, isError, err). GetPrompt has no
// mcphost equivalent (the MCP host lists and calls tools only, not prompts),
// so it reports a non-blocking error; an mcp_prompt hook therefore degrades
// gracefully, the same way an mcp hook with no client is skipped.
type mcpHookInvoker struct {
	get func() *mcphost.Host
}

func (m mcpHookInvoker) host() *mcphost.Host {
	if m.get == nil {
		return nil
	}
	return m.get()
}

func (m mcpHookInvoker) CallTool(ctx context.Context, server, tool string, input json.RawMessage) (hooks.MCPToolResult, error) {
	h := m.host()
	if h == nil {
		return hooks.MCPToolResult{}, errors.New("hookhost: no MCP host for mcp_tool hook")
	}
	text, isErr, err := h.Call(ctx, mcphost.ToolName(server, tool), input)
	if err != nil {
		return hooks.MCPToolResult{}, err
	}
	return hooks.MCPToolResult{Content: text, IsError: isErr}, nil
}

func (m mcpHookInvoker) GetPrompt(ctx context.Context, server, prompt string, input json.RawMessage) (hooks.MCPToolResult, error) {
	return hooks.MCPToolResult{}, errors.New("hookhost: MCP prompt hooks are not supported (mcphost exposes no GetPrompt)")
}

// --- HTTP hook client ------------------------------------------------------

// defaultHookHTTPClient is the HTTP client http hooks POST through when the
// session does not inject one. It carries a bounded timeout so a slow endpoint
// cannot wedge a turn; the per-hook timeout still applies on top via context.
// It posts ONLY to the hook's user-configured URL — there is no telemetry.
func defaultHookHTTPClient() hooks.HTTPDoer {
	return &http.Client{Timeout: 30 * time.Second}
}

// --- prompt hook model caller ----------------------------------------------

// hookPromptSystem instructs the model to judge a hook's condition and answer
// with the {"ok","reason","impossible"} verdict document.
const hookPromptSystem = `You are evaluating a Gocode hook condition for the current event. ` +
	`Decide whether the condition is satisfied. Reply with ONLY a single JSON object ` +
	`of the form {"ok": boolean, "reason": string, "impossible": boolean}. ` +
	`Set "ok" true when the condition holds (the action may proceed), false to block it. ` +
	`Set "impossible" true only when the condition can never be satisfied. ` +
	`"reason" is a short human-readable explanation. Output no other text.`

// modelCallerPrompter adapts the session model caller to the hooks.Prompter
// seam, so a prompt hook asks the same model the session uses (honouring /model
// and /provider). It performs one non-streaming model call and parses the
// verdict JSON out of the reply. A nil caller makes prompt hooks degrade to a
// non-blocking error (as the hook layer already handles).
type modelCallerPrompter struct {
	caller agent.ModelCaller
}

func (p modelCallerPrompter) Judge(ctx context.Context, prompt string, payload hooks.Payload) (hooks.PromptJudgment, error) {
	if p.caller == nil {
		return hooks.PromptJudgment{}, errors.New("hookhost: no model caller for prompt hook")
	}
	payloadJSON, _ := json.Marshal(payload)
	user := "Condition to evaluate:\n" + prompt + "\n\nEvent payload (JSON):\n" + string(payloadJSON)
	resp, err := p.caller.CallModel(ctx, agent.ModelRequest{
		System:    hookPromptSystem,
		Messages:  []agent.Message{agent.UserMessage(agent.TextBlock(user))},
		MaxTokens: 512,
	})
	if err != nil {
		return hooks.PromptJudgment{}, err
	}
	return parsePromptJudgment(firstText(resp.Message))
}

// firstText joins the text blocks of an assistant message.
func firstText(m agent.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == agent.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// parsePromptJudgment extracts the {"ok","reason","impossible"} object from the
// model's reply, tolerating surrounding prose by scanning for the first/last
// brace. A reply with no parseable object is a non-blocking allow.
func parsePromptJudgment(text string) (hooks.PromptJudgment, error) {
	raw := extractJSONObject(text)
	if raw == "" {
		return hooks.PromptJudgment{OK: true, Reason: "prompt hook returned no JSON verdict; allowing"}, nil
	}
	var doc struct {
		OK         bool   `json:"ok"`
		Reason     string `json:"reason"`
		Impossible bool   `json:"impossible"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return hooks.PromptJudgment{OK: true, Reason: "prompt hook verdict was unparseable; allowing"}, nil
	}
	return hooks.PromptJudgment{OK: doc.OK, Reason: doc.Reason, Impossible: doc.Impossible}, nil
}

// extractJSONObject returns the substring from the first "{" to the last "}",
// or "" when there is no brace pair.
func extractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

// --- if-guard matcher context ---------------------------------------------

// ifGuardSentinel is a reserved tool name used only to drive the generic
// settings-rule glob/domain matcher for an `if`-guard content match (the guard
// seam carries the rule content and the tool input, but not the tool kind).
const ifGuardSentinel = "__ifguard__"

// hookContentMatcher decides whether an `if`-guard rule's content matches a tool
// call's input, reusing the permission-rule matcher (toolreg): the
// relevant content field is extracted from the tool input and the rule content
// is matched against it with the same "*" glob (and WebFetch "domain:" matcher)
// the settings rules use. So `Bash(ls*)` matches a Bash call whose command
// starts with "ls", `Read(/etc/*)` a read under /etc, and so on.
//
// Approximation: the guard seam does not carry the tool kind, so the
// ruleContent field is found heuristically (extractRuleContentField).
// For the built-in tools this is exact (Bash->command, file tools->file_path,
// Grep->pattern, Web->url/domain); an unknown shape yields no content, so a
// content rule then does not match (the conservative default).
func hookContentMatcher(ruleContent string, input json.RawMessage) bool {
	content := extractRuleContentField(ruleContent, input)
	rs := toolreg.RuleSet{{
		Behavior: toolreg.RuleAllow,
		ToolName: ifGuardSentinel,
		Content:  ruleContent,
	}}
	call := toolreg.Call{Tool: protocol.Tool(ifGuardSentinel), RuleContent: content}
	return rs.Match(call, toolreg.RuleAllow) != nil
}

// extractRuleContentField pulls the string a settings rule is matched against
// out of a tool input object. It tries the fields the built-in tools key their
// rule content on, in order. For a "domain:"-shaped rule it turns a "url" field
// into the "domain:<host>" content the WebFetch domain matcher expects.
func extractRuleContentField(ruleContent string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	// WebFetch domain rules match against "domain:<hostname>".
	if strings.HasPrefix(ruleContent, "domain:") {
		if u, ok := stringField(m, "url"); ok {
			if parsed, err := url.Parse(u); err == nil && parsed.Hostname() != "" {
				return "domain:" + parsed.Hostname()
			}
		}
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url", "prompt"} {
		if v, ok := stringField(m, key); ok {
			return v
		}
	}
	return ""
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// --- tool-name canonicalisation + alias context ----------------------------

// canonToolName canonicalises a tool name for the `if`-guard name comparison:
// every character outside [A-Za-z0-9_-] maps to "_" (mcphost.NormalizeName).
// It is CASE-PRESERVING (rule tool-name matching is case-sensitive), so for
// the built-in tools (whose names are already literal) it is effectively
// identity; it only
// bites on a hand-written rule carrying unsafe characters.
func canonToolName(name string) string {
	return mcphost.NormalizeName(name)
}

// hookToolInfo returns a tool's family / legacy / alias names and whether it is
// a CLI-owned MCP tool, feeding both the hook matcher (MatchFull) and the
// `if`-guard name set:
//
//   - an MCP tool ("mcp__srv__tool") carries its server-level family name
//     ("mcp__srv") and its full name, so a server-scoped matcher selects it;
//     MCPCliOwned is false because the MCP tools are user/project
//     servers, never hosted CLI-owned connectors.
//   - Edit and its legacy name MultiEdit alias each other (the one legacy
//     pair; internal/protocol ToolMultiEdit).
//
// Any other tool has no alias names, so matching degrades to exact + regex on
// the primary name, which is correct for every built-in.
func hookToolInfo(name string) hookhost.ToolNames {
	if strings.HasPrefix(name, mcphost.ToolPrefix) {
		var tn hookhost.ToolNames
		rest := strings.TrimPrefix(name, mcphost.ToolPrefix)
		if i := strings.Index(rest, "__"); i > 0 {
			tn.Family = []string{mcphost.ToolPrefix + rest[:i], name}
		} else {
			tn.Family = []string{name}
		}
		return tn
	}
	switch name {
	case string(protocol.ToolEdit):
		return hookhost.ToolNames{Legacy: []string{string(protocol.ToolMultiEdit)}}
	case string(protocol.ToolMultiEdit):
		return hookhost.ToolNames{Legacy: []string{string(protocol.ToolEdit)}}
	}
	return hookhost.ToolNames{}
}
