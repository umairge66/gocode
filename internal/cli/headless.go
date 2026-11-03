package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/provider"
)

// Print-mode output formats (`gocode -p`):
//
//   - text (default): the final answer on stdout.
//   - json: one result record on stdout when the run ends (with --verbose,
//     a JSON array of every record instead).
//   - stream-json (needs --verbose): newline-delimited records as they happen:
//     system/init, each assistant turn, each tool-result user turn, optional
//     partial stream_event records, system records (compact_boundary,
//     api_retry, task_started, task_notification), and a result record per
//     turn.
//
// --input-format stream-json (with --output-format stream-json) reads user
// messages and control requests as newline-delimited JSON on stdin, while
// turns run (headless_stream.go): one turn per message, messages that arrive
// during a turn delivered to it between tool rounds, and permission asks sent
// to the host as can_use_tool requests (headless_permission.go).
// --json-schema makes the model return its answer through a StructuredOutput
// tool whose input is validated against the schema; the validated object is
// the result's structured_output.

// printOutputOptions are the print-mode output flags.
type printOutputOptions struct {
	outputFormat string
	inputFormat  string
	verbose      bool
	// partial is --include-partial-messages; partialEnv the
	// GOCODE_CODE_INCLUDE_PARTIAL_MESSAGES opt-in, which (unlike the flag)
	// is dropped silently where it does not apply.
	partial    bool
	partialEnv bool
	jsonSchema string
	schemaSet  bool
	// permissionPromptTool is --permission-prompt-tool: "stdio" (the
	// stream-json host) or an MCP tool that decides permission asks.
	permissionPromptTool string
}

// parsePrintOutputFlags pulls the output flags out of the print-mode
// arguments. It returns the remaining arguments, or an error message for a
// value outside an option's choices.
func parsePrintOutputFlags(rest []string) (printOutputOptions, []string, string) {
	var o printOutputOptions
	var out []string
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		key, val, hasVal := x, "", false
		if c := strings.Index(x, "="); c > 0 && strings.HasPrefix(x, "--") {
			key, val, hasVal = x[:c], x[c+1:], true
		}
		value := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(rest) {
				i++
				return rest[i], true
			}
			return "", false
		}
		switch key {
		case "--output-format":
			v, ok := value()
			if !ok {
				return o, nil, fmt.Sprintf("error: option '%s' argument missing", headless.OutputFormatSpec)
			}
			if !headless.ValidOutputFormat(v) {
				return o, nil, headless.InvalidChoice(headless.OutputFormatSpec, v, headless.OutputFormats)
			}
			o.outputFormat = v
		case "--input-format":
			v, ok := value()
			if !ok {
				return o, nil, fmt.Sprintf("error: option '%s' argument missing", headless.InputFormatSpec)
			}
			if !headless.ValidInputFormat(v) {
				return o, nil, headless.InvalidChoice(headless.InputFormatSpec, v, headless.InputFormats)
			}
			o.inputFormat = v
		case "--json-schema":
			v, ok := value()
			if !ok {
				return o, nil, "error: option '--json-schema <schema>' argument missing"
			}
			o.jsonSchema, o.schemaSet = v, true
		case "--permission-prompt-tool":
			v, ok := value()
			if !ok {
				return o, nil, "error: option '--permission-prompt-tool <tool>' argument missing"
			}
			o.permissionPromptTool = strings.TrimSpace(v)
		case "--verbose":
			if hasVal {
				out = append(out, x)
				continue
			}
			o.verbose = true
		case "--include-partial-messages":
			if hasVal {
				out = append(out, x)
				continue
			}
			o.partial = true
		default:
			out = append(out, x)
		}
	}
	return o, out, ""
}

// format returns the effective output format.
func (o printOutputOptions) format() string {
	if o.outputFormat == "" {
		return headless.FormatText
	}
	return o.outputFormat
}

// validate applies the startup checks that do not depend on the prompt.
func (o *printOutputOptions) validate(env *Env) string {
	if o.inputFormat == headless.FormatStreamJSON && o.format() != headless.FormatStreamJSON {
		return headless.ErrInputStreamNeedsOutputStream
	}
	if msg := validatePermissionPromptTool(o.permissionPromptTool, o.inputFormat); msg != "" {
		return msg
	}
	o.partialEnv = envutil.EnvTruthy(envGetenv(env)("GOCODE_CODE_INCLUDE_PARTIAL_MESSAGES"))
	if (o.partial || o.partialEnv) && o.format() != headless.FormatStreamJSON {
		if o.partial {
			return headless.ErrPartialNeedsStreamJSON
		}
		o.partialEnv = false
	}
	return ""
}

// includePartial reports whether partial stream_event records are emitted.
func (o printOutputOptions) includePartial() bool {
	return o.format() == headless.FormatStreamJSON && (o.partial || o.partialEnv)
}

// validateSessionFlags checks --session-id and its interplay with
// --continue/--resume/--fork-session ("" when acceptable).
func validateSessionFlags(env *Env, d *DispatchDefaults) string {
	if d == nil || strings.TrimSpace(d.SessionID) == "" {
		return ""
	}
	if (d.Continue || d.Resume) && !d.ForkSession {
		return headless.ErrSessionIDNeedsFork
	}
	id, ok := ids.ValidateUUID(strings.TrimSpace(d.SessionID))
	if !ok {
		return headless.ErrInvalidSessionID
	}
	if d.ForkSession && d.Resume && strings.TrimSpace(d.ResumeValue) == id {
		return ""
	}
	paths := resolveSessionPaths(env)
	if base := sessionConfigHome(env, paths); base != "" && transcriptExists(base)(paths.cwd, id) {
		return headless.ErrSessionIDInUse(id)
	}
	return ""
}

// headlessRun drives the json / stream-json output of one print-mode session.
type headlessRun struct {
	env  *Env
	s    *printSession
	rt   *sessionRuntime
	opts printOutputOptions

	stream  *headless.Writer
	mu      sync.Mutex
	records []any // json --verbose: every record, printed as one array

	// priceNoticeShown: the unpriced-model notice was printed.
	priceNoticeShown bool

	// in is the stream-json input (nil for a single prompt).
	in *streamInput
	// initCommands are the slash commands initialize reports, taken when
	// the session is wired.
	initCommands []map[string]any
	// ctxTokens is the context size the latest model response measured
	// (get_context_usage during a turn).
	ctxTokens atomic.Int64

	// per-turn observations
	lastStop string
	denials  []headless.PermissionDenial
	partial  *partialStreamer
	schema   map[string]any
	so       *structuredOutput
}

// emit writes a record now (stream-json) or keeps it for the final array
// (json --verbose); plain json only prints the result.
func (h *headlessRun) emit(v any) {
	switch {
	case h.stream != nil:
		_ = h.stream.Emit(v)
	case h.opts.verbose:
		h.mu.Lock()
		h.records = append(h.records, v)
		h.mu.Unlock()
	}
}

// runHeadlessPrint runs the json / stream-json print session: prompts come
// from next (one turn each, until it reports no more).
func runHeadlessPrint(env *Env, s *printSession, o printOutputOptions, schema map[string]any, next func() (string, bool, error)) int {
	return runHeadless(env, s, o, schema, nil, func(context.Context) ([]agent.ContentBlock, bool, error) {
		prompt, ok, err := next()
		if !ok || err != nil {
			return nil, ok, err
		}
		return []agent.ContentBlock{agent.TextBlock(prompt)}, true, nil
	})
}

// runHeadlessStream runs a print session driven by stream-json input on r.
func runHeadlessStream(env *Env, s *printSession, o printOutputOptions, schema map[string]any, r io.Reader) int {
	in := newStreamInput()
	return runHeadless(env, s, o, schema, in, in.next)
}

// runHeadless is the json / stream-json print session: prompts come from
// next, one turn each, until it reports no more. in, when set, is the
// stream-json input the session reads from (control requests, mid-turn
// messages, host permission prompts); it starts reading once the session
// is built.
func runHeadless(env *Env, s *printSession, o printOutputOptions, schema map[string]any, in *streamInput, next func(context.Context) ([]agent.ContentBlock, bool, error)) int {
	return withPrintSession(env, s, false, func(ctx context.Context, env *Env, rt *sessionRuntime) int {
		h := &headlessRun{env: env, s: s, rt: rt, opts: o, schema: schema, in: in}
		if o.format() == headless.FormatStreamJSON {
			h.stream = headless.NewWriter(env.Stdout)
		}
		h.wire()
		h.emit(h.systemInit())
		if in != nil {
			in.h = h
			in.start(env.Stdin)
		}
		var last *headless.Result
		for {
			blocks, ok, err := next(ctx)
			if err != nil {
				fmt.Fprintln(env.Stderr, err.Error())
				return 1
			}
			if !ok {
				break
			}
			r := h.turnWith(ctx, func(ctx context.Context) (conversation.Result, error) {
				return h.rt.conv.SubmitContent(ctx, blocks)
			})
			last = &r
			h.emit(r)
			if ctx.Err() != nil {
				break
			}
		}
		// Background commands still running after the last turn are awaited;
		// each completion notification runs one more turn, whose result
		// becomes the final one.
		if last != nil && !last.IsError && ctx.Err() == nil {
			rt.awaitBackgroundShells(ctx, env, func(ctx context.Context) bool {
				ran := false
				r := h.turnWith(ctx, func(ctx context.Context) (conversation.Result, error) {
					res, ok, err := rt.conv.ResumeWithQueuedContext(ctx)
					ran = ok
					return res, err
				})
				if !ran {
					return true
				}
				last = &r
				h.emit(r)
				return !r.IsError
			})
		}
		if o.format() == headless.FormatJSON {
			if last == nil {
				fmt.Fprintln(env.Stderr, headless.ErrNoResult)
				return 1
			}
			var b []byte
			if o.verbose {
				b, _ = headless.Marshal(h.records)
			} else {
				b, _ = headless.Marshal(last)
			}
			fmt.Fprintln(env.Stdout, string(b))
		}
		if o.format() == headless.FormatText && last != nil {
			printTextResult(env, last)
		}
		if last != nil && last.IsError {
			return 1
		}
		return 0
	})
}

// printTextResult prints a result the way the text format does: the answer on
// stdout, an error on stderr.
func printTextResult(env *Env, r *headless.Result) {
	switch {
	case r.Subtype == headless.SubtypeSuccess && !r.IsError:
		if r.Result != nil {
			fmt.Fprintln(env.Stdout, *r.Result)
		}
	case r.Subtype == headless.SubtypeSuccess:
		if r.Result != nil {
			fmt.Fprintln(env.Stderr, "gocode -p: "+*r.Result)
		}
	default:
		for _, e := range r.Errors {
			fmt.Fprintln(env.Stderr, "Error: "+e)
		}
	}
}

// wire installs the observers on the session: message records, partial
// events, permission denials and the StructuredOutput tool.
func (h *headlessRun) wire() {
	conv := h.rt.conv
	conv.OnMessage = func(ev agent.MessageEvent) {
		switch ev.Message.Role {
		case agent.RoleAssistant:
			h.mu.Lock()
			h.lastStop = ev.StopReason
			h.mu.Unlock()
			if u := ev.Usage; u.InputTokens+u.CachedInputTokens+u.CacheWriteTokens > 0 {
				h.ctxTokens.Store(int64(u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens + u.OutputTokens))
			}
			if h.partial != nil {
				h.partial.finish(ev)
			}
			h.emit(headless.NewAssistantMessage(h.rt.currentSessionID(), h.modelName(), "", ev.Message, ev.StopReason, ev.Usage))
		case agent.RoleUser:
			if headless.HasToolResult(ev.Message) {
				h.emit(headless.NewUserMessage(h.rt.currentSessionID(), ev.Message))
			}
		}
	}
	if h.opts.includePartial() {
		h.partial = &partialStreamer{h: h}
		conv.OnDelta = h.partial.text
		conv.OnThinkingDelta = h.partial.thinking
		conv.OnToolUseStart = h.partial.toolStart
		conv.OnToolInputDelta = h.partial.toolInput
	}
	h.wireEvents()
	h.wirePrompter()
	if h.rt.dispatcher != nil {
		h.rt.dispatcher.OnPermissionDenied = func(call agent.ToolUse) {
			input := call.Input
			if len(input) == 0 || !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			h.mu.Lock()
			h.denials = append(h.denials, headless.PermissionDenial{ToolName: call.Name, ToolUseID: call.ID, ToolInput: input})
			h.mu.Unlock()
		}
	}
	if h.schema != nil {
		h.so = newStructuredOutput(h.schema, conv.Tools, structuredOutputRetries(h.env))
		conv.Tools = h.so
		raw, _ := json.Marshal(h.schema)
		conv.ToolSchemas = append(conv.ToolSchemas, agent.ToolSchema{
			Name: headless.StructuredOutputTool, Description: headless.StructuredOutputDescription, InputSchema: raw,
		})
		conv.TurnDone = h.so.done
	}
}

// modelName is the model the session is using.
func (h *headlessRun) modelName() string {
	if h.rt.conv != nil && h.rt.conv.ModelID != "" {
		return h.rt.conv.ModelID
	}
	return h.s.Model
}

// systemInit builds the system/init record.
func (h *headlessRun) systemInit() headless.SystemInit {
	conv := h.rt.conv
	cwd := resolveSessionPaths(h.env).cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	init := headless.NewSystemInit(h.rt.currentSessionID(), cwd, h.modelName(), string(h.s.Mode))
	for _, ts := range conv.ToolSchemas {
		init.Tools = append(init.Tools, ts.Name)
	}
	if h.rt.mcp != nil {
		for _, st := range h.rt.mcp.Status() {
			init.MCPServers = append(init.MCPServers, headless.MCPServerStatus{Name: st.Name, Status: string(st.State)})
		}
	}
	for _, sg := range commandSuggestions(conv) {
		init.SlashCommands = append(init.SlashCommands, sg.Name)
	}
	if h.s.KeySource != "" {
		init.APIKeySource = h.s.KeySource
	}
	init.Version = updateCurrentVersion()
	if h.in != nil {
		init.Capabilities = append(init.Capabilities, headless.ControlCapabilities...)
	}
	return init
}

// turn runs one prompt and builds its result record.
func (h *headlessRun) turn(ctx context.Context, prompt string) headless.Result {
	return h.turnWith(ctx, func(ctx context.Context) (conversation.Result, error) {
		return h.rt.conv.Submit(ctx, prompt)
	})
}

// turnWith runs one turn through submit and builds its result record.
func (h *headlessRun) turnWith(ctx context.Context, submit func(context.Context) (conversation.Result, error)) headless.Result {
	conv := h.rt.conv
	started := time.Now()
	if h.in != nil {
		// The turn runs under its own context, which an interrupt cancels.
		ctx = h.in.beginTurn(ctx)
	}
	h.mu.Lock()
	h.lastStop, h.denials = "", nil
	h.mu.Unlock()
	if h.so != nil {
		h.so.reset()
	}
	usageBefore := headless.UsageSnapshot(conv.Cost)

	h.rt.syncCompactionWindow()
	preTokens, preMsgs := compactionSnapshot(conv.History)
	res, err := submit(ctx)
	h.notices(res.Notices)
	turns := res.TurnCount
	// A turn that ended without calling StructuredOutput is reminded once.
	if err == nil && res.Err == nil && h.so != nil && res.Kind == conversation.ResultModel &&
		res.Reason == agent.ReasonCompleted && !h.so.provided() && !h.so.exhausted() {
		res2, err2 := conv.Submit(ctx, headless.StructuredOutputEnforce)
		h.notices(res2.Notices)
		turns += res2.TurnCount
		res, err = res2, err2
	}
	if h.in != nil && h.in.endTurn() && res.Reason == agent.ReasonAborted {
		// An interrupt request ended the turn; the session goes on.
		res.Err = errInterrupted
	}

	r := headless.NewResult(h.rt.currentSessionID())
	r.NumTurns = turns
	h.mu.Lock()
	if h.lastStop != "" {
		stop := h.lastStop
		r.StopReason = &stop
	}
	if len(h.denials) > 0 {
		r.PermissionDenials = append([]headless.PermissionDenial(nil), h.denials...)
	}
	h.mu.Unlock()

	switch {
	case err != nil:
		r.SetError(headless.SubtypeErrorDuringExecution, err.Error())
	case res.HookBlocked:
		r.SetError(headless.SubtypeErrorDuringExecution, renderLocalEffect(res.Effect))
	case res.Kind == conversation.ResultExit:
		r.SetSuccess("")
	case res.Kind == conversation.ResultLocal:
		r.SetSuccess(printLocalResultText(h.env, h.s, h.rt, res, preTokens, preMsgs))
	case res.Reason == agent.ReasonMaxTurns:
		r.SetError(headless.SubtypeErrorMaxTurns, fmt.Sprintf("Reached maximum number of turns (%d)", conv.MaxTurns))
	case res.Reason == agent.ReasonMaxBudget:
		r.SetError(headless.SubtypeErrorMaxBudgetUSD, stopErrorText(res.Err, "Reached maximum budget"))
	case res.Reason == agent.ReasonAborted:
		msg := "aborted"
		if res.Err != nil {
			msg = res.Err.Error()
		}
		r.SetError(headless.SubtypeErrorDuringExecution, msg)
	case res.Reason == agent.ReasonRepeatedToolCalls:
		// The repeated-tool-call guard ended the run: an error naming the
		// tool and the count.
		r.SetError(headless.SubtypeErrorDuringExecution, repeatedCallsReason(res.Err))
	case res.Err != nil:
		// A failed model request still ends the turn normally: the error text
		// is the result, flagged as an error.
		r.SetSuccess(res.Err.Error())
		r.IsError = true
		var apiErr *provider.APIError
		if errors.As(res.Err, &apiErr) && apiErr.Status > 0 {
			st := apiErr.Status
			r.APIErrorStatus = &st
		}
	case h.so != nil && !h.so.provided():
		r.SetError(headless.SubtypeErrorMaxStructuredOutput, headless.StructuredOutputRetriesExhausted(h.so.attemptCount(), h.so.lastError()))
	default:
		text := res.AssistantText
		if h.so != nil {
			r.StructuredOutput = h.so.value()
			if strings.TrimSpace(text) == "" {
				if b, err := headless.Marshal(r.StructuredOutput); err == nil {
					text = string(b)
				}
			}
		}
		r.SetSuccess(text)
	}
	if res.Kind == conversation.ResultModel || err != nil {
		r.TerminalReason = headless.TerminalReason(res.Reason)
	}
	r.Usage = headless.UsageDelta(conv.Cost, usageBefore)
	if conv.Cost != nil {
		r.TotalCostUSD = conv.Cost.TotalCostUSD()
		r.CostUnknown = conv.Cost.HasUnknownModelCost()
		r.DurationAPIMS = conv.Cost.TotalAPIDuration().Milliseconds()
		if r.CostUnknown && !h.priceNoticeShown {
			h.priceNoticeShown = true
			h.notices([]string{cost.UnknownPriceNotice(conv.Cost.UnknownCostModels())})
		}
		h.budgetNotices(conv)
	}
	r.ModelUsage = headless.ModelUsageFrom(conv.Cost, func(model string) (int, int) {
		caps := provider.CapabilitiesFor(h.s.Provider, model)
		return caps.ContextWindow, caps.MaxOutputTokens
	})
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}

func (h *headlessRun) notices(ns []string) {
	for _, n := range ns {
		fmt.Fprintln(h.env.Stderr, "gocode -p: "+n)
	}
}

// partialStreamer turns the model's streamed deltas into stream_event
// records framed like the Messages API stream: message_start, then for each
// content block (thinking, text, tool_use) content_block_start, its
// content_block_delta events (thinking_delta, text_delta, input_json_delta)
// and content_block_stop, then message_delta and message_stop once the
// response is complete. A block ends when a delta of another block arrives;
// indices count the response's blocks from 0. Providers that do not stream
// tool calls or thinking only produce text blocks.
type partialStreamer struct {
	h *headlessRun

	mu      sync.Mutex
	started bool
	// index is the open block's index (-1 before the first); kind its type
	// ("" when none is open), toolID its tool_use id.
	index  int
	kind   string
	toolID string
}

func (p *partialStreamer) send(event any) {
	p.h.emit(headless.NewStreamEvent(p.h.rt.currentSessionID(), event))
}

// begin opens the message (under p.mu).
func (p *partialStreamer) begin() {
	if p.started {
		return
	}
	p.started, p.index, p.kind = true, -1, ""
	p.send(map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + ids.RandomUUID(), "type": "message", "role": "assistant", "model": p.h.liveModel(),
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}})
}

// open closes the open block and starts a new one (under p.mu).
func (p *partialStreamer) open(kind string, block map[string]any) {
	p.closeBlock()
	p.index++
	p.kind = kind
	p.send(map[string]any{"type": "content_block_start", "index": p.index, "content_block": block})
}

// closeBlock ends the open block, if any (under p.mu).
func (p *partialStreamer) closeBlock() {
	if p.kind == "" {
		return
	}
	p.send(map[string]any{"type": "content_block_stop", "index": p.index})
	p.kind, p.toolID = "", ""
}

func (p *partialStreamer) text(delta string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.begin()
	if p.kind != agent.BlockText {
		p.open(agent.BlockText, map[string]any{"type": "text", "text": ""})
	}
	p.send(map[string]any{"type": "content_block_delta", "index": p.index, "delta": map[string]any{"type": "text_delta", "text": delta}})
}

func (p *partialStreamer) thinking(delta string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.begin()
	if p.kind != agent.BlockThinking {
		p.open(agent.BlockThinking, map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	p.send(map[string]any{"type": "content_block_delta", "index": p.index, "delta": map[string]any{"type": "thinking_delta", "thinking": delta}})
}

func (p *partialStreamer) toolStart(id, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.begin()
	p.open(agent.BlockToolUse, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
	p.toolID = id
}

func (p *partialStreamer) toolInput(id, partial string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.kind != agent.BlockToolUse || p.toolID != id {
		return
	}
	p.send(map[string]any{"type": "content_block_delta", "index": p.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": partial}})
}

func (p *partialStreamer) finish(ev agent.MessageEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return
	}
	p.closeBlock()
	p.started = false
	var stop any
	if ev.StopReason != "" {
		stop = ev.StopReason
	}
	p.send(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": ev.Usage.OutputTokens}})
	p.send(map[string]any{"type": "message_stop"})
}

// structuredOutputRetries is the number of failed StructuredOutput attempts
// allowed (GOCODE_CODE_MAX_STRUCTURED_OUTPUT_RETRIES, default 5).
func structuredOutputRetries(env *Env) int {
	v := strings.TrimSpace(envGetenv(env)("GOCODE_CODE_MAX_STRUCTURED_OUTPUT_RETRIES"))
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
		return n
	}
	return headless.DefaultStructuredOutputRetries
}

// structuredOutput is the StructuredOutput tool: it intercepts calls to it
// ahead of the session's dispatcher, validates the input against the schema,
// and ends the turn once a valid object was provided.
type structuredOutput struct {
	schema map[string]any
	inner  agent.ToolDispatcher
	max    int

	mu       sync.Mutex
	val      any
	ok       bool
	attempts int
	lastErr  string
}

var (
	_ agent.ToolDispatcher        = (*structuredOutput)(nil)
	_ agent.ConcurrencyClassifier = (*structuredOutput)(nil)
)

func newStructuredOutput(schema map[string]any, inner agent.ToolDispatcher, max int) *structuredOutput {
	return &structuredOutput{schema: schema, inner: inner, max: max}
}

func (s *structuredOutput) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	if call.Name != headless.StructuredOutputTool {
		if s.inner == nil {
			return agent.ToolResult{Content: "Error: No such tool available: " + call.Name, IsError: true}, nil
		}
		return s.inner.Dispatch(ctx, call)
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(call.Input)))
	dec.UseNumber()
	if len(call.Input) == 0 || dec.Decode(&v) != nil {
		v = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if detail := headless.ValidateSchema(s.schema, v); detail != "" {
		s.attempts++
		s.lastErr = headless.StructuredOutputMismatch(detail)
		return agent.ToolResult{Content: "<tool_use_error>" + s.lastErr + "</tool_use_error>", IsError: true}, nil
	}
	s.val, s.ok = v, true
	return agent.ToolResult{Content: headless.StructuredOutputOK}, nil
}

func (s *structuredOutput) IsConcurrencySafe(call agent.ToolUse) bool {
	if call.Name == headless.StructuredOutputTool {
		return true
	}
	if c, ok := s.inner.(agent.ConcurrencyClassifier); ok {
		return c.IsConcurrencySafe(call)
	}
	return false
}

// done ends the turn once a valid object was provided, or once the attempts
// are used up.
func (s *structuredOutput) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok || s.attempts >= s.max
}

func (s *structuredOutput) reset() {
	s.mu.Lock()
	s.val, s.ok, s.attempts, s.lastErr = nil, false, 0, ""
	s.mu.Unlock()
}

func (s *structuredOutput) provided() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok
}

func (s *structuredOutput) exhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts >= s.max
}

func (s *structuredOutput) value() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.val
}

func (s *structuredOutput) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *structuredOutput) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// streamInputParseError renders the error for an input line that is not
// JSON.
func streamInputParseError(line string) string {
	typ := "unknown"
	if i := strings.Index(line, `"type"`); i >= 0 {
		rest := strings.TrimLeft(line[i+len(`"type"`):], " \t")
		if strings.HasPrefix(rest, ":") {
			rest = strings.TrimLeft(rest[1:], " \t")
			if strings.HasPrefix(rest, `"`) {
				if j := strings.Index(rest[1:], `"`); j > 0 && j <= 40 {
					typ = rest[1 : 1+j]
				}
			}
		}
	}
	return fmt.Sprintf("Error parsing streaming input line (type=%s, %d chars): SyntaxError", typ, len(line))
}

// singlePrompt yields prompt once.
func singlePrompt(prompt string) func() (string, bool, error) {
	done := false
	return func() (string, bool, error) {
		if done {
			return "", false, nil
		}
		done = true
		return prompt, true, nil
	}
}
