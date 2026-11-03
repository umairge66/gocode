package hookhost

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Result is the merged outcome of every hook run for one event. A nil *Result
// (or Ran == 0) means no hook matched.
type Result struct {
	// Decision is the merged decision (see the package doc for the rules).
	hooks.Decision
	// Ran is the number of hooks executed synchronously.
	Ran int
	// Errors are the hook failures (spawn failure, timeout, non-2 exit,
	// malformed JSON), one entry per failed hook, for surfacing to the user
	// — including a failure that the hook's onFailure turned into a block.
	Errors []string
	// PlainOutputs are the trimmed plain-text (non-JSON) stdouts of hooks
	// that exited 0, in configuration order.
	PlainOutputs []string
}

// Blocked reports whether the merged decision denies the action.
func (r *Result) Blocked() bool { return r != nil && r.Behavior == hooks.BehaviorDeny }

// rank orders behaviours by strictness: deny > defer > ask > allow.
func rank(b hooks.Behavior) int {
	switch b {
	case hooks.BehaviorDeny:
		return 4
	case hooks.BehaviorDefer:
		return 3
	case hooks.BehaviorAsk:
		return 2
	case hooks.BehaviorAllow:
		return 1
	}
	return 0
}

// timeoutFor returns the event default timeout.
func (h *Host) timeoutFor(ev hooks.Event) time.Duration {
	if ev == hooks.EventUserPromptSubmit {
		return UserPromptSubmitTimeout
	}
	return h.defaultTimeout
}

// basePayload fills the common payload fields.
func (h *Host) basePayload(ev hooks.Event) hooks.Payload {
	s := h.Session()
	return hooks.Payload{
		HookEventName:  ev,
		SessionID:      s.SessionID,
		TranscriptPath: s.TranscriptPath,
		Cwd:            s.Cwd,
		PermissionMode: s.PermissionMode,
	}
}

// matchInput builds the per-tool matcher context for query (a tool name).
func (h *Host) matchInput(query string) hooks.MatchInput {
	in := hooks.MatchInput{ToolName: query, GroupResolver: h.groupResolver}
	if h.toolInfo != nil {
		tn := h.toolInfo(query)
		in.FamilyNames, in.LegacyNames, in.Aliases, in.MCPCliOwned = tn.Family, tn.Legacy, tn.Aliases, tn.MCPCliOwned
	}
	return in
}

// selectHooks returns the hooks registered for ev that match query, with
// duplicate hooks removed. hasQuery=false selects every group (events without a
// match query ignore matchers).
func (h *Host) selectHooks(ev hooks.Event, query string, hasQuery bool) []registered {
	if h.disabled {
		return nil
	}
	var mi hooks.MatchInput
	if hasQuery {
		mi = h.matchInput(query)
		mi.Event = ev
	}
	var out []registered
	seen := map[string]bool{}
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	for _, r := range h.byEvent[ev] {
		if hasQuery && !r.matcher.MatchFull(mi) {
			continue
		}
		key := hookIdentity(r.hook)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// hookIdentity is the dedupe/once key for a hook, covering every type's
// distinguishing fields.
func hookIdentity(hk hooks.CommandHook) string {
	switch hk.NormalizeType() {
	case hooks.HookTypeHTTP:
		return "http\x00" + hk.URL
	case hooks.HookTypeMCPTool:
		return "mcp_tool\x00" + hk.Server + "\x00" + hk.Tool
	case hooks.HookTypeMCPPrompt:
		return "mcp_prompt\x00" + hk.Server + "\x00" + hk.PromptName
	case hooks.HookTypePrompt:
		return "prompt\x00" + hk.Prompt
	default:
		return "command\x00" + hk.Shell + "\x00" + hk.Command + "\x00" + strings.Join(hk.Args, "\x00")
	}
}

// onceKey identifies a hook for the per-session "once" flag.
func onceKey(ev hooks.Event, r registered) string {
	return string(ev) + "\x00" + string(r.source) + "\x00" + hookIdentity(r.hook)
}

// toolEvents are the events whose input carries a tool name, so the only events
// an "if" guard can be evaluated for.
var toolEvents = map[hooks.Event]bool{
	hooks.EventPreToolUse:         true,
	hooks.EventPostToolUse:        true,
	hooks.EventPostToolUseFailure: true,
	hooks.EventPermissionDenied:   true,
}

// guardAllows evaluates a hook's "if" guard for the event/query. It returns
// false when the hook should be skipped (guard did not match, or the guard is
// on a non-tool event and so cannot be evaluated).
func (h *Host) guardAllows(ev hooks.Event, r registered, payload hooks.Payload, query string) bool {
	if r.hook.If == "" {
		return true
	}
	gc := hooks.GuardContext{
		IsToolEvent:    toolEvents[ev],
		ToolInput:      payload.ToolInput,
		Canon:          h.canon,
		ContentMatcher: h.contentMatcher,
	}
	if gc.IsToolEvent {
		gc.Names = append([]string{query}, aliasNames(h.toolInfo, query)...)
	}
	run, _ := hooks.EvalIf(r.hook.If, gc)
	return run
}

func aliasNames(info func(string) ToolNames, tool string) []string {
	if info == nil {
		return nil
	}
	tn := info(tool)
	out := append([]string{}, tn.Family...)
	out = append(out, tn.Legacy...)
	out = append(out, tn.Aliases...)
	return out
}

// Run executes every hook registered for payload.HookEventName whose matcher
// selects query (hasQuery=false ignores matchers), concurrently, and merges
// the results. It returns nil when no hook matched. The common payload fields
// are filled in from the session when empty.
func (h *Host) Run(ctx context.Context, payload hooks.Payload, query string, hasQuery bool) *Result {
	ev := payload.HookEventName
	sel := h.selectHooks(ev, query, hasQuery)
	if len(sel) == 0 {
		return nil
	}
	base := h.basePayload(ev)
	if payload.SessionID == "" {
		payload.SessionID = base.SessionID
	}
	if payload.TranscriptPath == "" {
		payload.TranscriptPath = base.TranscriptPath
	}
	if payload.Cwd == "" {
		payload.Cwd = base.Cwd
	}
	if payload.PermissionMode == "" {
		payload.PermissionMode = base.PermissionMode
	}

	// Skip hooks whose "if" guard does not match, then drop "once" hooks that
	// already ran this session (guard first, so a skipped hook keeps its once).
	var run []registered
	h.onceMu.Lock()
	for _, r := range sel {
		if !h.guardAllows(ev, r, payload, query) {
			continue
		}
		if r.hook.Once {
			k := onceKey(ev, r)
			if h.onceRan[k] {
				continue
			}
			h.onceRan[k] = true
		}
		run = append(run, r)
	}
	h.onceMu.Unlock()

	s := h.Session()
	baseEnv := h.baseEnv
	if h.sanitizeEnv {
		baseEnv = hooks.SanitizeEnv(baseEnv)
	}
	projEnv := []string{}
	if pd := h.projectDir(s); pd != "" {
		projEnv = append(projEnv, "GOCODE_PROJECT_DIR="+pd, "gocode_PROJECT_DIR="+pd, "CLAUDE_PROJECT_DIR="+pd)
	}
	additions := hooks.EnvAdditions{SessionID: s.SessionID, Attended: h.attended, PID: os.Getpid()}.Env()

	type out struct {
		d   hooks.Decision
		res hooks.RunResult
		err error
		ok  bool
	}
	results := make([]out, len(run))
	var wg sync.WaitGroup
	for i, r := range run {
		env := make([]string, 0, len(baseEnv)+len(projEnv)+len(additions)+len(r.env))
		env = append(env, baseEnv...)
		env = append(env, projEnv...)
		env = append(env, additions...)
		env = append(env, r.env...)
		req := RunRequest{
			Event:   ev,
			Hook:    r.hook,
			Payload: payload,
			Options: hooks.DecisionOptions{Personal: r.source == settingspolicy.SourceUser},
			Env:     env,
			Dir:     s.Cwd,
			Timeout: h.timeoutFor(ev),
		}
		if r.hook.TimeoutSeconds > 0 && time.Duration(r.hook.TimeoutSeconds)*time.Second > maxHookTimeout {
			req.Hook.TimeoutSeconds = int(maxHookTimeout / time.Second)
		}
		if r.hook.IsAsync() {
			// Detached: the event does not wait on it. A plain async hook's
			// result is discarded; an async_rewake hook's late decision is
			// delivered to the rewake sink when it finishes.
			rew := r.hook.AsyncRewake && h.rewakeSink != nil
			reg := r
			h.async.Add(1)
			go func() {
				defer h.async.Done()
				d, _, _ := h.runOne(context.WithoutCancel(ctx), req)
				if rew {
					h.rewakeSink.Rewake(hooks.RewakeResult{
						Event:    ev,
						Decision: d,
						Message:  reg.hook.RewakeMessage,
						Summary:  reg.hook.RewakeSummary,
					})
				}
			}()
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, res, err := h.runOne(ctx, req)
			results[i] = out{d: d, res: res, err: err, ok: true}
		}(i)
	}
	wg.Wait()

	merged := &Result{Decision: hooks.Decision{Behavior: hooks.BehaviorNone, Continue: true}}
	var reasons, contexts, sysmsgs []string
	for i, o := range results {
		if !o.ok {
			continue
		}
		merged.Ran++
		if o.err != nil {
			merged.Errors = append(merged.Errors, hookLabel(ev, run[i])+": "+o.err.Error())
			continue
		}
		d := o.d
		if d.ErrKind != "" {
			msg := hookLabel(ev, run[i]) + ": " + string(d.ErrKind)
			if d.Err != "" {
				msg += ": " + d.Err
			}
			merged.Errors = append(merged.Errors, msg)
			if merged.ErrKind == "" {
				merged.ErrKind, merged.Err = d.ErrKind, d.Err
			}
		}
		// A hook that gave no answer (timed out, could not start, crashed)
		// blocks the action under its onFailure "block" (the default), so a
		// broken guard never silently lets the action through.
		if run[i].hook.FailureBlocks(d) && hooks.BlocksOnFailure(ev) {
			d.Behavior = hooks.BehaviorDeny
			d.Reason = failureBlockReason(ev, run[i], d)
		}
		if o.res.ExitCode == 0 && d.ErrKind == "" {
			if t := strings.TrimSpace(o.res.Stdout); t != "" && !strings.HasPrefix(t, "{") {
				merged.PlainOutputs = append(merged.PlainOutputs, t)
			}
		}
		if rank(d.Behavior) > rank(merged.Behavior) {
			merged.Behavior = d.Behavior
			reasons = reasons[:0]
		}
		if d.Behavior == merged.Behavior && d.Reason != "" && (d.Behavior == hooks.BehaviorDeny || d.Behavior == hooks.BehaviorAsk || d.Behavior == hooks.BehaviorDefer) {
			reasons = append(reasons, d.Reason)
		}
		if !d.Continue {
			merged.Continue = false
			if merged.StopReason == "" {
				merged.StopReason = d.StopReason
			}
		}
		if d.AdditionalContext != "" {
			contexts = append(contexts, d.AdditionalContext)
		}
		if d.SystemMessage != "" {
			sysmsgs = append(sysmsgs, d.SystemMessage)
		}
		if d.UpdatedInput != nil {
			merged.UpdatedInput = d.UpdatedInput // later wins
		}
		merged.SuppressOutput = merged.SuppressOutput || d.SuppressOutput
	}
	merged.Reason = strings.Join(reasons, "\n")
	merged.AdditionalContext = strings.Join(contexts, "\n")
	merged.SystemMessage = strings.Join(sysmsgs, "\n")
	if merged.Ran == 0 {
		return nil
	}
	return merged
}

// runOne executes one hook, dispatching by its type. Command hooks go through
// the injected CommandRunner (ExecRunner by default); http/mcp/prompt hooks go
// through the hooks-package runners built from the injected seams, each with
// the event's default timeout. The returned RunResult is meaningful (exit code,
// stdout) only for command hooks; for the others it reports success/failure
// through the Decision's ErrKind, which the merge logic already handles.
func (h *Host) runOne(ctx context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error) {
	switch req.Hook.NormalizeType() {
	case hooks.HookTypeHTTP:
		hr := &hooks.HTTPRunner{Doer: h.httpRunner.Doer, EnvLookup: h.httpRunner.EnvLookup, DefaultTimeout: req.Timeout}
		d, res, err := hr.Run(ctx, req.Hook, req.Payload, req.Options)
		return d, hooks.RunResult{ExitCode: res.StatusCode, Stdout: res.Body, TimedOut: res.Aborted, Duration: res.Duration}, err
	case hooks.HookTypeMCPTool, hooks.HookTypeMCPPrompt:
		mr := &hooks.MCPRunner{Invoker: h.mcpRunner.Invoker, DefaultTimeout: req.Timeout}
		d, res, err := mr.Run(ctx, req.Hook, req.Payload, req.Options)
		return d, hooks.RunResult{Stdout: res.Content}, err
	case hooks.HookTypePrompt:
		pr := &hooks.PromptRunner{Prompter: h.promptRunner.Prompter, DefaultTimeout: req.Timeout}
		d, _, err := pr.Run(ctx, req.Hook, req.Payload, req.Options)
		return d, hooks.RunResult{}, err
	default:
		return h.runner.RunHook(ctx, req)
	}
}

func hookLabel(ev hooks.Event, r registered) string {
	l := string(ev) + " hook"
	if r.plugin != nil {
		l += " (plugin " + r.plugin.ID() + ")"
	}
	what := r.hook.Command
	switch r.hook.NormalizeType() {
	case hooks.HookTypeHTTP:
		what = r.hook.URL
	case hooks.HookTypeMCPTool:
		what = r.hook.Server + "/" + r.hook.Tool
	case hooks.HookTypeMCPPrompt:
		what = r.hook.Server + "/" + r.hook.PromptName
	case hooks.HookTypePrompt:
		what = "prompt"
	}
	return l + " `" + what + "`"
}

// failureBlockReason is the block message of a hook that failed to answer
// under onFailure "block".
func failureBlockReason(ev hooks.Event, r registered, d hooks.Decision) string {
	why := string(d.ErrKind)
	if d.Err != "" {
		why += ": " + d.Err
	}
	return hookLabel(ev, r) + " failed (" + why + `) and its onFailure is "block" (the default), so the action was blocked. ` +
		`Fix the hook, or set "onFailure": "continue" on it to let the action proceed when it fails.`
}
