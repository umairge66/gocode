package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// --input-format stream-json: the bidirectional protocol a host drives a
// print session with (see headless/control.go and docs/headless.md).
//
// stdin is read on its own goroutine, so the host can write while a turn
// runs:
//
//   - a user message waits in a queue. Between two tool rounds of the
//     running turn the queued messages (up to the first slash command) are
//     delivered to the model with the round's results; whatever is still
//     queued when the turn ends runs as the next turn, one result each.
//   - a control_request is answered with a control_response. interrupt
//     cancels the running turn (the session goes on); set_model,
//     set_permission_mode, mcp_status, get_context_usage and initialize
//     are answered from the session. A request that changes the session
//     (set_model) waits for the running turn to end before it applies.
//   - a control_response answers one of gocode's own requests: can_use_tool,
//     which asks the host to decide a tool call the permission rules leave
//     to the user (hostPrompter).
//
// A line that is not JSON ends the run with an error, as before.

// queuedPrompt is one user message waiting for a turn.
type queuedPrompt struct {
	blocks []agent.ContentBlock
}

// slash reports whether the message is a slash command (it never rides a
// running turn: it is expanded at a turn start).
func (q queuedPrompt) slash() bool {
	for _, b := range q.blocks {
		if b.Type == agent.BlockText {
			return commands.IsSlashCommand(b.Text)
		}
	}
	return false
}

// streamInput is the stream-json input side of a print session.
type streamInput struct {
	h *headlessRun

	mu    sync.Mutex
	queue []queuedPrompt
	// done: stdin ended; err: a line was not JSON (ends the run).
	done bool
	err  error
	wake chan struct{}
	// pending are gocode's control requests awaiting the host's answer.
	pending map[string]chan headless.InputControlResponse
	seq     int
	// cancelTurn cancels the running turn (nil between turns);
	// interrupted records that an interrupt request did.
	cancelTurn  context.CancelFunc
	interrupted bool

	// stateMu is held while a turn runs. A control request that changes the
	// session state takes it, so it waits for the turn to end (deferred).
	stateMu  sync.Mutex
	deferMu  sync.Mutex
	deferred []func()
}

func newStreamInput() *streamInput {
	return &streamInput{wake: make(chan struct{}, 1), pending: map[string]chan headless.InputControlResponse{}}
}

// start reads r until EOF on a new goroutine.
func (s *streamInput) start(r io.Reader) {
	if r == nil {
		s.finish(nil)
		return
	}
	go s.read(r)
}

func (s *streamInput) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec headless.InputRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			s.finish(errors.New(streamInputParseError(line)))
			return
		}
		switch rec.Type {
		case "user":
			if rec.Message == nil || rec.Message.Role != agent.RoleUser {
				continue
			}
			if blocks := userContentBlocks(rec.Message.Content); len(blocks) > 0 {
				s.push(queuedPrompt{blocks: blocks})
			}
		case headless.TypeControlRequest:
			s.control(rec.RequestID, rec.Request)
		case headless.TypeControlResponse:
			if rec.Response != nil {
				s.answer(*rec.Response)
			}
		}
		// Other record types (control_cancel_request for a request gocode
		// already answered, keep_alive, …) are ignored.
	}
	s.finish(nil)
}

// push queues a user message and wakes the turn loop.
func (s *streamInput) push(q queuedPrompt) {
	s.mu.Lock()
	s.queue = append(s.queue, q)
	s.mu.Unlock()
	s.signal()
}

func (s *streamInput) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// finish records the end of stdin (err: a line that was not JSON) and fails
// every request still waiting for an answer.
func (s *streamInput) finish(err error) {
	s.mu.Lock()
	s.done = true
	if s.err == nil {
		s.err = err
	}
	pending := s.pending
	s.pending = map[string]chan headless.InputControlResponse{}
	s.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	s.signal()
}

// next waits for the next queued user message. It reports false once stdin
// ended with nothing left queued, and the parse error that ended it.
func (s *streamInput) next(ctx context.Context) ([]agent.ContentBlock, bool, error) {
	for {
		s.mu.Lock()
		switch {
		case len(s.queue) > 0:
			q := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return q.blocks, true, nil
		case s.err != nil:
			err := s.err
			s.mu.Unlock()
			return nil, false, err
		case s.done:
			s.mu.Unlock()
			return nil, false, nil
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, false, nil
		}
	}
}

// HasPending / TakePending: queued user messages are turns of their own,
// not context for the next one (conversation.ContextFeed).
func (s *streamInput) HasPending() bool      { return false }
func (s *streamInput) TakePending() []string { return nil }

// TakeMidTurnBlocks hands the running turn the queued user messages up to
// the first slash command (conversation.MidTurnBlockFeed).
func (s *streamInput) TakeMidTurnBlocks() []agent.ContentBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agent.ContentBlock
	n := 0
	for _, q := range s.queue {
		if q.slash() {
			break
		}
		out = append(out, q.blocks...)
		n++
	}
	s.queue = s.queue[n:]
	return out
}

// beginTurn runs a turn under ctx: it returns the turn's context, which an
// interrupt cancels, and holds the session state until endTurn.
func (s *streamInput) beginTurn(ctx context.Context) context.Context {
	s.stateMu.Lock()
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancelTurn, s.interrupted = cancel, false
	s.mu.Unlock()
	return ctx
}

// endTurn ends the turn begun last, runs the control requests that waited
// for it and reports whether an interrupt cancelled it.
func (s *streamInput) endTurn() bool {
	s.mu.Lock()
	cancel, interrupted := s.cancelTurn, s.interrupted
	s.cancelTurn, s.interrupted = nil, false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.stateMu.Unlock()
	s.runDeferred()
	return interrupted
}

// whenIdle runs f under the session state: now when no turn runs, else once
// the running turn ends.
func (s *streamInput) whenIdle(f func()) {
	s.deferMu.Lock()
	s.deferred = append(s.deferred, f)
	s.deferMu.Unlock()
	s.runDeferred()
}

// runDeferred runs the waiting requests when the state is free. Whoever
// releases the state last re-checks, so none is left behind.
func (s *streamInput) runDeferred() {
	for {
		if !s.stateMu.TryLock() {
			return
		}
		s.deferMu.Lock()
		fs := s.deferred
		s.deferred = nil
		s.deferMu.Unlock()
		for _, f := range fs {
			f()
		}
		s.stateMu.Unlock()
		s.deferMu.Lock()
		more := len(s.deferred) > 0
		s.deferMu.Unlock()
		if !more {
			return
		}
	}
}

// idle runs f now if no turn runs and reports whether it did.
func (s *streamInput) idle(f func()) bool {
	if !s.stateMu.TryLock() {
		return false
	}
	defer s.stateMu.Unlock()
	f()
	return true
}

// respond writes a control_response.
func (s *streamInput) respond(r headless.ControlResponse) {
	if s.h != nil && s.h.stream != nil {
		_ = s.h.stream.Emit(r)
	}
}

// request sends one of gocode's control requests and waits for the host's
// answer. A cancelled ctx withdraws it (control_cancel_request); stdin
// ending fails it.
func (s *streamInput) request(ctx context.Context, body any) (headless.InputControlResponse, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return headless.InputControlResponse{}, errors.New("stream-json input closed")
	}
	s.seq++
	id := fmt.Sprintf("gocode_req_%d", s.seq)
	ch := make(chan headless.InputControlResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if s.h == nil || s.h.stream == nil {
		return headless.InputControlResponse{}, errors.New("no stream-json output")
	}
	_ = s.h.stream.Emit(headless.NewControlRequest(id, body))
	select {
	case r, ok := <-ch:
		if !ok {
			return headless.InputControlResponse{}, errors.New("stream-json input closed")
		}
		if r.Subtype == headless.ControlError {
			return r, fmt.Errorf("host refused the request: %s", r.Error)
		}
		return r, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		_ = s.h.stream.Emit(headless.NewControlCancelRequest(id))
		return headless.InputControlResponse{}, ctx.Err()
	}
}

// answer routes a host control_response to the request waiting for it.
func (s *streamInput) answer(r headless.InputControlResponse) {
	s.mu.Lock()
	ch, ok := s.pending[r.RequestID]
	delete(s.pending, r.RequestID)
	s.mu.Unlock()
	if ok {
		ch <- r
	}
}

// control answers one host control_request.
func (s *streamInput) control(id string, raw json.RawMessage) {
	var req struct {
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
		Mode    string `json:"mode"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &req) != nil {
		s.respond(headless.ControlFail(id, "invalid control request"))
		return
	}
	h := s.h
	switch req.Subtype {
	case headless.ControlInterrupt:
		s.mu.Lock()
		if s.cancelTurn != nil {
			s.interrupted = true
			s.cancelTurn()
		}
		s.mu.Unlock()
		s.respond(headless.ControlOK(id, nil))
	case headless.ControlInitialize:
		s.respond(headless.ControlOK(id, h.initializeResponse()))
	case headless.ControlSetModel:
		s.whenIdle(func() {
			model, refusal := h.setModel(req.Model)
			if refusal != "" {
				s.respond(headless.ControlFail(id, refusal))
				return
			}
			s.respond(headless.ControlOK(id, map[string]any{"model": model}))
		})
	case headless.ControlSetPermissionMode:
		mode, msg := h.permissionModeChoice(req.Mode)
		if msg != "" {
			s.respond(headless.ControlFail(id, msg))
			return
		}
		// The dispatcher takes the mode at once (the running turn's next
		// tool call sees it); the conversation follows between turns.
		if h.rt.dispatcher != nil {
			h.rt.dispatcher.SetMode(mode)
		}
		s.whenIdle(func() { h.rt.conv.SetPermissionMode(mode) })
		s.respond(headless.ControlOK(id, map[string]any{"mode": string(mode)}))
	case headless.ControlMCPStatus:
		s.respond(headless.ControlOK(id, map[string]any{"mcpServers": h.mcpStatus()}))
	case headless.ControlGetContextUsage:
		s.respond(headless.ControlOK(id, h.contextUsage(s)))
	default:
		s.respond(headless.ControlFail(id, "Unsupported control request subtype: "+req.Subtype))
	}
}

// userContentBlocks extracts a stream-json user message's content: a
// string, or the text and image blocks of a block list.
func userContentBlocks(raw json.RawMessage) []agent.ContentBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []agent.ContentBlock{agent.TextBlock(strings.TrimSpace(s))}
	}
	var blocks []struct {
		Type   string             `json:"type"`
		Text   string             `json:"text"`
		Source *agent.ImageSource `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []agent.ContentBlock
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case agent.BlockText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case agent.BlockImage:
			if b.Source != nil && (b.Source.Data != "" || b.Source.URL != "") {
				src := *b.Source
				out = append(out, agent.ContentBlock{Type: agent.BlockImage, Source: &src})
			}
		}
	}
	if text := strings.TrimSpace(strings.Join(texts, "\n")); text != "" {
		out = append([]agent.ContentBlock{agent.TextBlock(text)}, out...)
	}
	return out
}

// --- control request handlers ----------------------------------------------

// initializeResponse answers initialize: the session's commands, output
// style, model and control capabilities. Hooks and other options an SDK
// may pass are not taken over the protocol (they come from settings).
func (h *headlessRun) initializeResponse() map[string]any {
	h.mu.Lock()
	cmds := h.initCommands
	h.mu.Unlock()
	if cmds == nil {
		cmds = []map[string]any{}
	}
	return map[string]any{
		"commands":                cmds,
		"output_style":            "default",
		"available_output_styles": []string{"default"},
		"models":                  h.initModels(),
		"account":                 h.initAccount(),
		"capabilities":            headless.ControlCapabilities,
		"pid":                     os.Getpid(),
	}
}

// setModel switches the model (set_model; "" or "default" restores the
// session's starting model). It runs between turns.
func (h *headlessRun) setModel(model string) (string, string) {
	model = strings.TrimSpace(model)
	if model == "" || model == "default" {
		model = h.s.Model
	}
	if model == "" {
		return "", "set_model: no model given"
	}
	return h.rt.conv.SetModel(model)
}

// permissionModeChoice validates a set_permission_mode target: one of the
// permission modes; bypassPermissions only in a session started with it (or
// --allow-dangerously-skip-permissions) and not disabled by settings; auto
// only while auto mode is available.
func (h *headlessRun) permissionModeChoice(name string) (toolreg.Mode, string) {
	mode := toolreg.Mode(normalizePermissionModeName(strings.TrimSpace(name)))
	switch mode {
	case toolreg.ModeDefault, toolreg.ModeAcceptEdits, toolreg.ModePlan, toolreg.ModeDontAsk:
		return mode, ""
	case toolreg.ModeBypassPermissions:
		if !h.s.bypassAllowed {
			return "", "Cannot set permission mode to bypassPermissions: the session was not started with --dangerously-skip-permissions or --allow-dangerously-skip-permissions"
		}
		if bypassDisabledBySettings(h.env) {
			return "", "Cannot set permission mode to bypassPermissions: " + msgBypassDisabledNotice
		}
		return mode, ""
	case toolreg.ModeAuto:
		if reason := autoModeUnavailableReason(h.env); reason != "" {
			return "", "Cannot set permission mode to auto: " + reason
		}
		return mode, ""
	}
	return "", fmt.Sprintf("Invalid permission mode %q", name)
}

// mcpStatus is mcp_status's server list (McpServerStatus: name, status,
// serverInfo, error).
func (h *headlessRun) mcpStatus() []map[string]any {
	out := []map[string]any{}
	if h.rt.mcp == nil {
		return out
	}
	for _, st := range h.rt.mcp.Status() {
		e := map[string]any{"name": st.Name, "status": mcpStatusName(st.State)}
		if st.ServerInfo.Name != "" {
			e["serverInfo"] = map[string]any{"name": st.ServerInfo.Name, "version": st.ServerInfo.Version}
		}
		if st.Error != "" {
			e["error"] = st.Error
		}
		out = append(out, e)
	}
	return out
}

// mcpStatusName maps a server state to the protocol's connection status
// (connected, failed, needs-auth, pending).
func mcpStatusName(s mcphost.State) string {
	switch s {
	case mcphost.StateConnected:
		return "connected"
	case mcphost.StateNeedsAuth:
		return "needs-auth"
	case mcphost.StatePending, mcphost.StateReconnecting:
		return "pending"
	}
	return "failed"
}

// contextUsage answers get_context_usage. Between turns it measures the
// session (conversation.ContextTokens) and breaks it down by category
// (contextBreakdown); during one it reports the size the latest model
// response measured, as a single Messages category.
func (h *headlessRun) contextUsage(s *streamInput) map[string]any {
	tokens := int(h.ctxTokens.Load())
	model := ""
	autoCompact, threshold := false, 0
	var categories, memoryFiles, mcpTools []map[string]any
	s.idle(func() {
		tokens = h.rt.conv.ContextTokens()
		model = h.modelName()
		if c := h.rt.conv.Compactor; c != nil {
			autoCompact = c.Planner.Config.Enabled && !h.rt.autoCompactOff.Load()
			threshold = compaction.Threshold(c.Planner.Window.EffectiveWindow(), c.Planner.Config)
		}
		categories, memoryFiles, mcpTools = h.contextBreakdown(tokens, contextWindowFor(h.s.Provider, model))
	})
	if model == "" {
		model = h.liveModel()
	}
	max := contextWindowFor(h.s.Provider, model)
	if categories == nil {
		categories = []map[string]any{category("messages", "Messages", tokens, "used")}
	}
	if memoryFiles == nil {
		memoryFiles = []map[string]any{}
	}
	if mcpTools == nil {
		mcpTools = []map[string]any{}
	}
	pct := 0.0
	if max > 0 {
		pct = float64(tokens) * 100 / float64(max)
	}
	out := map[string]any{
		"categories":           categories,
		"totalTokens":          tokens,
		"maxTokens":            max,
		"rawMaxTokens":         max,
		"percentage":           pct,
		"model":                model,
		"isAutoCompactEnabled": autoCompact,
		"memoryFiles":          memoryFiles,
		"mcpTools":             mcpTools,
		"agents":               []any{},
		"gridRows":             []any{},
	}
	if autoCompact && threshold > 0 {
		out["autoCompactThreshold"] = threshold
	}
	return out
}

// liveModel is the model the live caller targets (safe during a turn).
func (h *headlessRun) liveModel() string {
	if pc, ok := h.rt.conv.Model.(*provider.ModelCaller); ok {
		if m := pc.CurrentModel(); m != "" {
			return m
		}
	}
	return h.s.Model
}
