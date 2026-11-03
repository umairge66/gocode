package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// Host-answered permission prompts in print mode.
//
// Without a user to ask, print mode turns a permission "ask" into a denial.
// Two prompters let something else decide instead:
//
//   - hostPrompter (stream-json input with --permission-prompt-tool stdio):
//     sends the host a can_use_tool control
//     request and waits for its control_response.
//   - mcpPromptTool (--permission-prompt-tool mcp__server__tool): calls that
//     MCP tool with {tool_name, input, tool_use_id}; its text result is the
//     same JSON decision.
//
// Both read the decision the Agent SDK's PermissionResult shape describes:
// {"behavior":"allow","updatedInput":{…},"updatedPermissions":[…]} or
// {"behavior":"deny","message":"…","interrupt":false}.

// permissionPromptStdio is the --permission-prompt-tool value that names the
// stream-json host itself.
const permissionPromptStdio = "stdio"

// hostPrompter asks the stream-json host (can_use_tool).
type hostPrompter struct{ in *streamInput }

func (p hostPrompter) Prompt(ctx context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	resp, err := p.in.request(ctx, canUseToolRequest(ctx, req))
	if err != nil {
		return toolrun.PermissionResponse{}, err
	}
	var res headless.PermissionResult
	if len(resp.Response) == 0 || json.Unmarshal(resp.Response, &res) != nil {
		return toolrun.PermissionResponse{}, errors.New("can_use_tool: the host's answer is not a permission result")
	}
	return permissionFromResult(req, res)
}

// mcpPromptTool asks an MCP tool (--permission-prompt-tool).
type mcpPromptTool struct {
	tool string
	call func(ctx context.Context, name string, input json.RawMessage) (string, bool, error)
}

func (p mcpPromptTool) Prompt(ctx context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	input, _ := json.Marshal(map[string]any{
		"tool_name":   req.ToolName,
		"input":       toolInputOrEmpty(req.Input),
		"tool_use_id": toolrun.ToolUseIDFrom(ctx),
	})
	text, isErr, err := p.call(ctx, p.tool, input)
	if err != nil {
		return toolrun.PermissionResponse{}, fmt.Errorf("permission prompt tool %s: %w", p.tool, err)
	}
	if isErr {
		return toolrun.PermissionResponse{}, fmt.Errorf("permission prompt tool %s failed: %s", p.tool, text)
	}
	var res headless.PermissionResult
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &res) != nil {
		return toolrun.PermissionResponse{}, fmt.Errorf("permission prompt tool %s returned no permission result: %s", p.tool, text)
	}
	return permissionFromResult(req, res)
}

// canUseToolRequest is the can_use_tool request body for req.
func canUseToolRequest(ctx context.Context, req toolrun.PermissionRequest) headless.CanUseToolRequest {
	body := headless.CanUseToolRequest{
		Subtype:               headless.ControlCanUseTool,
		ToolName:              req.ToolName,
		Input:                 toolInputOrEmpty(req.Input),
		PermissionSuggestions: permissionSuggestions(req),
		ToolUseID:             toolrun.ToolUseIDFrom(ctx),
		DecisionReason:        req.Reason,
	}
	if req.SessionOnly && req.Tool == "Read" && strings.HasPrefix(req.RuleContent, "/") {
		body.BlockedPath = req.RuleContent
	}
	return body
}

// permissionSuggestions renders the request's "don't ask again" rules (and,
// for an edit, the switch to acceptEdits) as PermissionUpdate entries.
func permissionSuggestions(req toolrun.PermissionRequest) []headless.PermissionUpdate {
	out := []headless.PermissionUpdate{}
	if !req.NoAlwaysAllow {
		dest := "localSettings"
		if req.SessionOnly {
			dest = "session"
		}
		for _, s := range req.Suggested {
			r, ok := parsePermissionRule(s, toolreg.RuleAllow, "session")
			if !ok {
				continue
			}
			out = append(out, headless.PermissionUpdate{Type: "addRules", Behavior: "allow", Destination: dest,
				Rules: []headless.PermissionRule{{ToolName: r.ToolName, RuleContent: r.Content}}})
		}
	}
	if req.IsEdit {
		out = append(out, headless.PermissionUpdate{Type: "setMode", Mode: string(toolreg.ModeAcceptEdits), Destination: "session"})
	}
	return out
}

// permissionFromResult maps a host decision onto the dispatcher's answer.
//
//   - allow: the call runs. An addRules update (behavior allow) becomes a
//     rule for the rest of the session; a setMode acceptEdits on an edit
//     switches the session to acceptEdits. Rules are not written to settings
//     files whatever their destination. An updatedInput that differs from
//     the call's input is the input the call runs with; the dispatcher
//     checks it again (a deny rule still refuses it).
//   - deny: refused with the message; interrupt (or no message at all)
//     also ends the turn, as a rejection without feedback does.
func permissionFromResult(req toolrun.PermissionRequest, res headless.PermissionResult) (toolrun.PermissionResponse, error) {
	switch res.Behavior {
	case "allow":
		var updated json.RawMessage
		if len(res.UpdatedInput) > 0 && !jsonEqual(res.UpdatedInput, toolInputOrEmpty(req.Input)) {
			var obj map[string]any
			if json.Unmarshal(res.UpdatedInput, &obj) != nil || obj == nil {
				return toolrun.PermissionResponse{}, errors.New("permission result: updatedInput is not an object")
			}
			updated = res.UpdatedInput
		}
		for _, u := range res.UpdatedPermissions {
			switch u.Type {
			case "setMode":
				if toolreg.Mode(u.Mode) == toolreg.ModeAcceptEdits && req.IsEdit {
					return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedEditsSession, UpdatedInput: updated}, nil
				}
			case "addRules":
				if req.NoAlwaysAllow || (u.Behavior != "" && u.Behavior != "allow") || len(u.Rules) == 0 {
					continue
				}
				spec := u.Rules[0].ToolName
				if c := u.Rules[0].RuleContent; c != "" {
					spec += "(" + c + ")"
				}
				if rule, ok := parsePermissionRule(spec, toolreg.RuleAllow, "session"); ok {
					return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedAlways, Rule: &rule, UpdatedInput: updated}, nil
				}
			}
		}
		return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedOnce, UpdatedInput: updated}, nil
	case "deny":
		feedback := strings.TrimSpace(res.Message)
		if res.Interrupt {
			feedback = ""
		}
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied, Feedback: feedback}, nil
	}
	return toolrun.PermissionResponse{}, fmt.Errorf("permission result: unknown behavior %q", res.Behavior)
}

// toolInputOrEmpty is a call's input, {} when it has none.
func toolInputOrEmpty(in json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(in)) == 0 || !json.Valid(in) {
		return json.RawMessage(`{}`)
	}
	return in
}

// jsonEqual reports whether two JSON documents hold the same value.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// validatePermissionPromptTool checks a --permission-prompt-tool value
// ("" when acceptable): stdio needs stream-json input; anything else must
// name an MCP tool.
func validatePermissionPromptTool(tool, inputFormat string) string {
	switch {
	case tool == "":
		return ""
	case tool == permissionPromptStdio:
		if inputFormat != headless.FormatStreamJSON {
			return "Error: --permission-prompt-tool stdio requires --input-format=stream-json."
		}
		return ""
	case !strings.HasPrefix(tool, "mcp__"):
		return fmt.Sprintf("Error: --permission-prompt-tool %q must name an MCP tool (mcp__<server>__<tool>) or be \"stdio\".", tool)
	}
	return ""
}
