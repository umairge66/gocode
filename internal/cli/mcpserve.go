package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcpserver"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// This file wires the `gocode mcp serve` invocation: gocode running AS an MCP
// server that exposes ITS OWN tool catalog to other MCP clients. The responder
// machinery (handlers, server→client correlation, stdio framing) lives in
// internal/mcpserver; this file is the host side that the server package was
// built to expect — it assembles the served tool catalog from the same toolrun
// dispatcher the agent loop uses and supplies the ToolInvoker seam that routes
// an inbound tools/call back through that dispatcher.
//
// Option surface: -d/--debug, --verbose, --transport <stdio> and
// --result-format <raw|rendered>. The handler defaults transport and
// result-format, checks that the cwd exists, and serves stdio. The served catalog is gocode's own enabled tools, each
// advertised with its inputSchema; results map text -> {type:"text"} and
// image(base64) -> {type:"image", data, mimeType} (see toolResultToMCP).
//
// Limitations:
//   - --result-format {raw,rendered} is accepted but currently maps both values
//     to the dispatcher's model-facing rendered text (+image) content, because
//     the toolrun seam returns that rendered form, not the tool's raw structured
//     output.
//   - Permission asks: a served call that needs an "ask" asks the person behind
//     the client through elicitation/create when the client declared that
//     capability, and refuses with how-to-allow guidance otherwise (see
//     mcpserve_permission.go).

// mcpServeOptions is the option surface of `gocode mcp serve`.
type mcpServeOptions struct {
	debug        bool
	verbose      bool
	transport    string // "stdio" (the only transport)
	resultFormat string // "raw" (default) | "rendered"
}

// parseMcpServeOptions reads the serve flags out of the args that follow
// `mcp serve`. Unknown flags are ignored (tolerating them keeps the surface
// forgiving); the returned error is a user-facing validation message.
func parseMcpServeOptions(args []string) (mcpServeOptions, string) {
	opts := mcpServeOptions{transport: "stdio", resultFormat: "raw"}

	// Narrow to the tokens after `serve`; everything before it (e.g. "mcp") is
	// not an option. A "--" terminator ends option parsing.
	serve := indexOf(args, "serve")
	scope := args
	if serve != -1 {
		scope = args[serve+1:]
	}
	if term := indexOf(scope, "--"); term != -1 {
		scope = scope[:term]
	}

	takeValue := func(i int, inline string, hasInline bool) (string, int) {
		if hasInline {
			return inline, i
		}
		if i+1 < len(scope) {
			return scope[i+1], i + 1
		}
		return "", i
	}

	for i := 0; i < len(scope); i++ {
		arg := scope[i]
		name, inline, hasInline := arg, "", false
		if eq := strings.Index(arg, "="); eq >= 0 && strings.HasPrefix(arg, "--") {
			name, inline, hasInline = arg[:eq], arg[eq+1:], true
		}
		switch name {
		case "-d", "--debug":
			opts.debug = true
		case "--verbose":
			opts.verbose = true
		case "--transport":
			opts.transport, i = takeValue(i, inline, hasInline)
		case "--result-format":
			opts.resultFormat, i = takeValue(i, inline, hasInline)
		default:
			// Ignore unrecognized tokens (positionals, other flags).
		}
	}

	// Transport choice.
	switch opts.transport {
	case "", "stdio":
		opts.transport = "stdio"
	default:
		return opts, fmt.Sprintf("Error: --transport must be 'stdio', got '%s'", opts.transport)
	}

	// Result-format choice.
	switch opts.resultFormat {
	case "", "raw":
		opts.resultFormat = "raw"
	case "rendered":
	default:
		return opts, fmt.Sprintf("Error: --result-format must be 'raw' or 'rendered', got '%s'", opts.resultFormat)
	}

	return opts, ""
}

// runMcpServe is the `gocode mcp serve` entrypoint. It builds gocode's own tool
// catalog from a toolrun dispatcher, registers each tool on an mcpserver.Server,
// installs the ToolInvoker that routes tools/call through that dispatcher, and
// serves the MCP protocol over stdio (default) until the client closes the
// stream or the process is interrupted.
//
// Stdout is the JSON-RPC channel on the stdio transport, so NOTHING but protocol
// frames is written to env.Stdout here; every diagnostic goes to env.Stderr.
// The serve path runs as a non-interactive session with no banner on stdout.
func runMcpServe(env *Env, defaults *DispatchDefaults, rest []string) int {
	opts, verr := parseMcpServeOptions(rest)
	if verr != "" {
		fmt.Fprintln(env.Stderr, verr)
		return 1
	}

	// The working directory must still exist ("Directory … does not exist").
	// This is cheap and keeps a tool's relative-path resolution sane.
	if env.Cwd != nil {
		if cwd, err := env.Cwd(); err != nil {
			fmt.Fprintf(env.Stderr, "Error: Directory %s does not exist\n", cwd)
			return 1
		}
	}

	// stdin is the inbound protocol stream; without it there is nothing to serve.
	if env.Stdin == nil {
		fmt.Fprintln(env.Stderr, "Error: mcp serve requires a stdin stream for the stdio transport.")
		return 1
	}

	// The Bash tool served here runs in the OS sandbox like a session's.
	sb := newSessionSandbox(env, nil, false)
	defer sb.Close()
	_ = sb.Start()
	if code, stop := sandboxStartupCheck(env, sb); stop {
		return code
	}
	// mcp serve never prompts: an untrusted folder's risky settings stay
	// ignored (trust.go), said once on stderr (stdout is the protocol).
	if rep := assessWorkspaceTrust(env); rep.needsPrompt() {
		fmt.Fprintln(env.Stderr, "mcp serve: "+rep.untrustedNotice())
	}
	srv, built := buildMcpServeServer(env, defaults, toolrun.WithSandbox(sb))
	if opts.debug {
		fmt.Fprintf(env.Stderr, "mcp serve: serving %d tool(s) over stdio (result-format=%s)\n", built, opts.resultFormat)
	}

	// Diagnostics that cannot be answered in band (framing/decoding errors,
	// unmatched responses) go to stderr, never stdout.
	srv.OnError(func(err error) {
		fmt.Fprintf(env.Stderr, "mcp serve: %v\n", err)
	})

	// Serve until the client closes its end (EOF) or the process is interrupted,
	// then close the server gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := srv.ServeStdio(ctx, env.Stdin, env.Stdout); err != nil {
		if ctx.Err() != nil {
			// Interrupted: a clean shutdown, not a failure.
			return 0
		}
		fmt.Fprintf(env.Stderr, "mcp serve: %v\n", err)
		return 1
	}
	return 0
}

// buildMcpServeServer assembles the MCP server for `mcp serve`: a toolrun
// dispatcher (the SAME one the agent loop uses, minus any model caller — tool
// execution is on demand and starting the server needs no network), its
// advertised tool catalog registered as tools/list descriptors, and the
// ToolInvoker that routes a tools/call back through the dispatcher. It returns
// the server and the number of tools registered.
//
// The served catalog comes from (*toolrun.Dispatcher).Schemas() — the single
// source of truth for what gocode offers, already filtered by the permission
// engine's enablement (toolreg). This reuses the existing schema-advertising
// path rather than hand-rebuilding schemas.
func buildMcpServeServer(env *Env, defaults *DispatchDefaults, opts ...toolrun.Option) (*mcpserver.Server, int) {
	// No model caller: the dispatcher's Schemas()/Dispatch() work offline, and a
	// tool that needs the model (e.g. WebFetch's summarizer) simply has none —
	// starting the server must not require network/model access.
	// disableBypassPermissionsMode and the root check hold here too; a
	// refused bypass serves in default mode (bypassmode.go).
	mode, notices := mcpServeMode(env, defaults)
	// Rules as in a normal session: settings deny/ask/allow plus the
	// --allowedTools / --disallowedTools "cliArg" rules (session.go).
	managedOnly := managedPermissionRulesOnly(env)
	var allowed, disallowed []string
	if defaults != nil {
		allowed, disallowed = defaults.AllowedTools, defaults.DisallowedTools
	}
	cliRules, cliNotices := cliPermissionRules(allowed, disallowed, managedOnly)
	notices = append(notices, cliNotices...)
	for _, n := range notices {
		fmt.Fprintln(env.Stderr, "mcp serve: "+n)
	}
	rules := toolrun.NewSharedRules(append(loadPermissionRules(env), cliRules...))
	// An "ask" goes to the person behind the client via elicitation/create when
	// the client declared it, and is refused with guidance otherwise
	// (mcpserve_permission.go).
	opts = append([]toolrun.Option{
		toolrun.WithSharedRules(rules),
		toolrun.WithPermissionPrompter(mcpServePrompter{managedOnly: managedOnly}),
	}, opts...)
	disp := newSessionDispatcher(mode, env, nil, opts...)

	inv := dispatcherInvoker{disp: disp}
	srv := mcpserver.NewServer(mcpserver.DefaultServerInfo, mcpserver.WithToolInvoker(inv))

	n := 0
	for _, sc := range disp.Schemas() {
		tool := mcp.Tool{
			Name:        sc.Name,
			Description: sc.Description,
			InputSchema: mcp.ToolSchema{Type: "object"},
		}
		if len(sc.InputSchema) > 0 {
			var parsed mcp.ToolSchema
			if err := json.Unmarshal(sc.InputSchema, &parsed); err == nil {
				tool.InputSchema = parsed
			}
		}
		// nil handler -> the server falls back to the ToolInvoker seam.
		srv.RegisterToolFull(tool, nil)
		n++
	}
	return srv, n
}

// mcpServeMode resolves the permission mode `mcp serve` runs its calls in:
// --permission-mode / --dangerously-skip-permissions, else the settings
// defaultMode, as in a session (run.go). bypassPermissions is honoured only
// when a flag asked for it — never from settings alone — and still passes
// through disableBypassPermissionsMode and the root refusal (bypassmode.go);
// a refused bypass serves in default mode. It returns the mode and any
// notices for stderr.
func mcpServeMode(env *Env, defaults *DispatchDefaults) (toolreg.Mode, []string) {
	var notices []string
	mode := modeFromDefaults(defaults)
	if defaults == nil || strings.TrimSpace(defaults.PermissionMode) == "" {
		if m := toolreg.Mode(loadPermissionModeSetting(env)); m != "" && m != toolreg.ModeBypassPermissions {
			mode = modeFromDefaults(&DispatchDefaults{PermissionMode: string(m)})
		}
	}
	mode, notice, refusal := gateBypassMode(env, defaults, mode)
	if notice != "" {
		notices = append(notices, notice)
	}
	if refusal != "" && mode == toolreg.ModeBypassPermissions {
		notices = append(notices, refusal)
		mode = toolreg.ModeDefault
	}
	return mode, notices
}

// dispatcherInvoker adapts a *toolrun.Dispatcher to the mcpserver.ToolInvoker
// seam: an inbound tools/call is run through the dispatcher's normal pathway
// (the same one the agent loop uses, gated by the permission engine), and its
// agent.ToolResult is shaped into an mcp.CallToolResult.
//
// The content mapping lives here so internal/mcpserver depends on neither
// toolrun nor agent.
type dispatcherInvoker struct {
	disp *toolrun.Dispatcher
}

// InvokeTool runs the named tool and maps the outcome onto an MCP CallToolResult.
//
// Error contract (the tools/call handler):
// a tool-level failure is reported IN BAND — the dispatcher returns
// agent.ToolResult{IsError:true} with a NIL Go error, which maps to
// CallToolResult{IsError:true}. A non-nil Go error is reserved for the
// loop-fatal case (e.g. a PreToolUse hook that failed outright) where no result
// can be produced; it is returned to the server, which renders it as the same
// in-band isError text result (never a transport-level JSON-RPC error).
func (a dispatcherInvoker) InvokeTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallToolResult, error) {
	res, err := a.disp.Dispatch(ctx, agent.ToolUse{Name: name, Input: args})
	if err != nil {
		return mcp.CallToolResult{}, err
	}
	return toolResultToMCP(res), nil
}

// toolResultToMCP maps internal/toolrun's agent.ToolResult onto an MCP
// CallToolResult: the text content becomes a text block, each image becomes an
// {type:"image",data,mimeType} block, and IsError carries through.
func toolResultToMCP(res agent.ToolResult) mcp.CallToolResult {
	out := mcp.CallToolResult{
		IsError: res.IsError,
		Content: []mcp.ContentBlock{},
	}
	// A text block is emitted whenever there is text, or when there is no image
	// content at all (so an empty result still carries one text block rather
	// than an empty content array).
	if res.Content != "" || len(res.Images) == 0 {
		out.Content = append(out.Content, mcp.ContentBlock{Type: "text", Text: res.Content})
	}
	for _, img := range res.Images {
		out.Content = append(out.Content, mcp.ContentBlock{
			Type:     "image",
			Data:     img.Base64,
			MimeType: img.MediaType,
		})
	}
	return out
}
