package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/mcpserver"
	"github.com/umairge66/gocode/internal/toolrun"
)

// Permission prompts for `gocode mcp serve`.
//
// Permission rules, the --allowedTools / --disallowedTools rules and the
// permission mode apply as in a normal session (the toolrun dispatcher's
// Decider). A call that would need a prompt is put to the person behind the
// MCP client through elicitation/create when the client declared the
// elicitation capability (form mode), offering only "Allow once" / "Deny" — no
// "always" answer for a served call. A client without elicitation gets the
// headless denial ("<tool> was not run: a permission rule says to ask a person
// …") plus guidance on how to allow the call. Hosted caller-attestation of
// safety asks is not supported. Nothing is ever allowed silently: a decline, a
// cancel, an unreadable answer or a failed request is a denial.

// mcpServePrompter is the toolrun.PermissionPrompter (and toolrun.PromptSurface)
// installed on the `mcp serve` dispatcher. The connection a call arrived on is
// taken from the call's context (mcpserver.PeerFromContext), so each call asks
// the client that made it.
type mcpServePrompter struct {
	// managedOnly: allowManagedPermissionRulesOnly is set, so neither an
	// --allowedTools rule nor a user allow rule would count; the guidance must
	// not suggest them.
	managedOnly bool
}

// clientElicitation returns the call's client connection (from ctx) and the
// elicitation capability it declared at initialize.
func clientElicitation(ctx context.Context) (*mcpserver.Conn, json.RawMessage) {
	c := mcpserver.PeerFromContext(ctx)
	if c == nil {
		return nil, nil
	}
	return c, c.ClientCapabilities().Elicitation
}

// supportsFormElicitation reports whether a client's declared elicitation
// capability covers form-mode requests. Per the MCP spec an empty object
// ({}) means form mode; a client that lists modes must include "form".
func supportsFormElicitation(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	var modes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &modes); err != nil {
		return false
	}
	_, form := modes["form"]
	_, url := modes["url"]
	return form || !url
}

// PromptUnavailable implements toolrun.PromptSurface: a call can be asked only
// when it came from a connected client that declared form elicitation.
func (p mcpServePrompter) PromptUnavailable(ctx context.Context, suggested []string) (bool, string) {
	if c, caps := clientElicitation(ctx); c != nil && supportsFormElicitation(caps) {
		return false, ""
	}
	return true, mcpServeNoPromptHint(suggested, p.managedOnly)
}

// mcpServeNoPromptHint is the guidance appended to the denial of a call that
// needed approval from a client that cannot show a prompt.
func mcpServeNoPromptHint(suggested []string, managedOnly bool) string {
	const lead = "This MCP client did not declare the elicitation capability, so `gocode mcp serve` cannot ask you to approve the call."
	if managedOnly {
		return lead + " Ask an admin to add an allow rule to managed settings, or restart `gocode mcp serve` with --permission-mode acceptEdits (for file edits)."
	}
	rule := "<Tool>"
	if len(suggested) > 0 {
		rule = suggested[0]
	}
	return fmt.Sprintf("%s To allow it, restart `gocode mcp serve --allowedTools %q`, add %q to permissions.allow in your settings, or restart it with --permission-mode acceptEdits (for file edits).", lead, rule, rule)
}

// elicitResult is the elicitation/create result.
type elicitResult struct {
	Action  string                     `json:"action"`
	Content map[string]json.RawMessage `json:"content,omitempty"`
}

// elicitDecisionField / values: the single enum field of the approval form.
const (
	elicitDecisionField = "decision"
	elicitAllowOnce     = "allow_once"
	elicitDeny          = "deny"
)

// Prompt implements toolrun.PermissionPrompter by sending elicitation/create
// to the client the call came from. Only an explicit "accept" whose decision
// is "allow_once" allows the call; anything else is a denial.
func (p mcpServePrompter) Prompt(ctx context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	peer, caps := clientElicitation(ctx)
	if peer == nil || !supportsFormElicitation(caps) {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, fmt.Errorf("mcp serve: no client to ask")
	}
	params := map[string]any{
		"message": mcpServeApprovalMessage(req),
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				elicitDecisionField: map[string]any{
					"type":        "string",
					"title":       "Allow this call?",
					"description": "Allow once runs this one call. Nothing is remembered.",
					"enum":        []string{elicitAllowOnce, elicitDeny},
					"enumNames":   []string{"Allow once", "Deny"},
				},
			},
			"required": []string{elicitDecisionField},
		},
	}
	var res elicitResult
	if err := peer.ElicitInput(ctx, params, &res); err != nil {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, err
	}
	if res.Action != "accept" {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, nil
	}
	var choice string
	if raw, ok := res.Content[elicitDecisionField]; ok {
		_ = json.Unmarshal(raw, &choice)
	}
	if choice == elicitAllowOnce {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedOnce}, nil
	}
	return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, nil
}

// mcpServeApprovalMessage is the elicitation message: the tool, the exact
// command / path / URL it would act on, and the permission engine's reason.
func mcpServeApprovalMessage(req toolrun.PermissionRequest) string {
	var b strings.Builder
	b.WriteString("gocode (mcp serve) needs your approval to run ")
	b.WriteString(req.ToolName)
	b.WriteString(".\n")
	for _, line := range mcpServeCallDetail(req) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if r := strings.TrimSpace(req.Reason); r != "" {
		b.WriteString("Reason: ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// mcpServeCallDetail renders what the call would do, exactly as requested: the
// Bash command, the file path, the URL, or (for any other tool) its input.
func mcpServeCallDetail(req toolrun.PermissionRequest) []string {
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	str := func(k string) string {
		if v, ok := in[k].(string); ok {
			return v
		}
		return ""
	}
	var out []string
	for _, f := range []struct{ key, label string }{
		{"command", "Command"},
		{"file_path", "Path"},
		{"notebook_path", "Path"},
		{"path", "Path"},
		{"url", "URL"},
	} {
		if v := str(f.key); v != "" {
			out = append(out, f.label+": "+v)
		}
	}
	if len(out) > 0 {
		return out
	}
	if req.RuleContent != "" {
		return []string{"Target: " + req.RuleContent}
	}
	if len(in) > 0 {
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make([]string, 0, len(keys))
		for _, k := range keys {
			v, _ := json.Marshal(in[k])
			ordered = append(ordered, k+"="+string(v))
		}
		return []string{"Input: " + strings.Join(ordered, ", ")}
	}
	return nil
}
