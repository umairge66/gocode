package hooks

import (
	"context"
	"encoding/json"
	"time"
)

// MCPToolResult is one MCP tool/prompt invocation's result: the text content
// joined from the tool's content blocks and whether the tool reported an error.
type MCPToolResult struct {
	Content string
	IsError bool
}

// MCPInvoker is the seam an mcp_tool / mcp_prompt hook calls through. It is a
// read-only view of the MCP host: CallTool invokes a server's tool, GetPrompt
// requests a server's prompt. The hook layer does not own the MCP connection,
// so the host (cli) injects an implementation. A nil invoker makes every mcp
// hook a non-blocking error ("no MCP client context"): mcp_tool hooks are
// skipped when no MCP client list is available.
//
// cli would wire this to internal/mcphost: CallTool(server, tool, input) maps
// to mcphost.Host.Call(ctx, mcphost.ToolName(server, tool), input), which
// returns (text, isError, err). GetPrompt has no mcphost equivalent yet; until
// one exists the host may leave it returning an error.
type MCPInvoker interface {
	CallTool(ctx context.Context, server, tool string, input json.RawMessage) (MCPToolResult, error)
	GetPrompt(ctx context.Context, server, prompt string, input json.RawMessage) (MCPToolResult, error)
}

// MCPRunner executes mcp_tool and mcp_prompt hooks through an MCPInvoker and
// interprets the returned text as a decision document, exactly as a command
// hook's stdout is interpreted.
type MCPRunner struct {
	Invoker        MCPInvoker
	DefaultTimeout time.Duration
}

// Run invokes the hook's MCP tool/prompt and returns the interpreted Decision.
// A tool error (isError) or a transport error is a non-blocking error; a
// successful call's text is parsed as a decision document. The error return is
// always nil (hook failures are non-blocking), kept for signature symmetry.
func (r *MCPRunner) Run(ctx context.Context, hook CommandHook, payload Payload, opts DecisionOptions) (Decision, MCPToolResult, error) {
	if r.Invoker == nil {
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrSpawnFailed, Err: "no MCP client context for mcp hook",
		}, MCPToolResult{}, nil
	}

	timeout := r.DefaultTimeout
	if hook.TimeoutSeconds > 0 {
		timeout = time.Duration(hook.TimeoutSeconds) * time.Second
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var res MCPToolResult
	var err error
	if hook.NormalizeType() == HookTypeMCPPrompt {
		res, err = r.Invoker.GetPrompt(ctx, hook.Server, hook.PromptName, hook.Input)
	} else {
		res, err = r.Invoker.CallTool(ctx, hook.Server, hook.Tool, hook.Input)
	}
	if err != nil {
		kind := ErrExit
		if ctx.Err() != nil {
			kind = ErrTimeout
		}
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: kind, Err: err.Error(),
		}, res, nil
	}
	if res.IsError {
		msg := res.Content
		if msg == "" {
			msg = "MCP tool returned an error"
		}
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrExit, Err: msg,
		}, res, nil
	}
	return BodyDecision(res.Content, opts), res, nil
}
