package toolrun

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file is the interactive permission-request seam: it turns a toolreg
// "ask" decision into a real prompt to the user (in the interactive host) and
// honours the answer, instead of refusing the call outright.
//
// # Flow (canUseTool / the permission prompt)
//
// When the permission engine returns behavior:"ask", the REPL opens the
// permission dialog ("Do you want to …?") offering these options:
//
//	Yes                                         -> allow this one call
//	Yes, and don't ask again …                  -> allow + persist a settings rule
//	Yes, allow all edits during this session    -> switch to acceptEdits (edit tools)
//	No, and tell gocode what to do differently   -> deny, with optional feedback
//
// A "Yes, and don't ask again" persists a rule string (see
// settingsload.AddPermissionRule) to the local project settings
// (.gocode/settings.local.json, permissions.allow) AND adds it to the live
// session rule set so the same call is not asked again. A "No" returns the
// rejection message as an is_error tool_result (with the user's
// feedback appended when they typed any); without feedback it also ends the
// main agent's turn (see Dispatcher.rejected).
//
// When there is no interactive user (-p / headless), no prompter is set and an
// "ask" stays a denial, but carries a message telling the user how
// to allow the tool (an allow rule / a non-default permission mode). There is
// NO telemetry here and no network permission-prompt delegation is reproduced.

// PermissionResponseKind is the outcome the user chose at a permission prompt.
type PermissionResponseKind int

const (
	// PermissionDenied: the user refused the call ("No, and tell …"). Any typed
	// feedback is carried in PermissionResponse.Feedback.
	PermissionDenied PermissionResponseKind = iota
	// PermissionAllowedOnce: allow this one call ("Yes").
	PermissionAllowedOnce
	// PermissionAllowedAlways: allow and persist a rule ("Yes, and don't ask
	// again"). PermissionResponse.Rule is added to the live rule set.
	PermissionAllowedAlways
	// PermissionAllowedEditsSession: allow and switch to acceptEdits mode for the
	// rest of the session ("Yes, allow all edits during this session"). Only
	// valid for an edit tool.
	PermissionAllowedEditsSession
)

// PermissionRequest is the context handed to a PermissionPrompter: what the
// model wants to do and the rule(s) that would grant it.
type PermissionRequest struct {
	// Tool is the tool kind (the built-in name, or the full mcp__server__tool
	// name for an external tool).
	Tool protocol.Tool
	// ToolName is the model-facing tool name (== string(Tool)).
	ToolName string
	// Input is the JSON input that will run — the model's, or a PreToolUse
	// hook's rewrite of it — for the prompt's per-tool detail rendering (Bash
	// command, Edit/Write path + diff, WebFetch domain, …).
	Input json.RawMessage
	// RuleContent is the permission rule content for this call: the Bash
	// command, the file path, "domain:<host>" for WebFetch, "" for a whole-tool
	// rule.
	RuleContent string
	// Reason is the decider's human-readable reason for asking.
	Reason string
	// Suggested lists the rule string(s) a "don't ask again" would persist,
	// most specific first (e.g. ["WebFetch(domain:example.com)"]).
	Suggested []string
	// IsEdit reports whether this is a file-editing tool, so the prompter may
	// offer "allow all edits during this session".
	IsEdit bool
	// Origin, when set, names what is requesting the call when it is not the
	// model — e.g. "/deploy" for a `!`-block of the /deploy slash command — so
	// the prompt can attribute it. "" for an ordinary model tool call.
	Origin string
	// NoAlwaysAllow: the policy sets allowManagedPermissionRulesOnly, so the
	// prompt must not offer "Yes, and don't ask again".
	NoAlwaysAllow bool
	// AlwaysAllowLabel, when set, is the "don't ask again" option's label
	// (e.g. "Yes, allow reading from /x/ during this session").
	AlwaysAllowLabel string
	// SessionOnly: the "don't ask again" rule applies to this session only and
	// must not be written to a settings file.
	SessionOnly bool
	// Notice, when set, is a line explaining why the prompt is shown (an
	// outside read, or the auto mode classifier handing the call over).
	Notice string
}

// PermissionResponse is the user's answer at a permission prompt.
type PermissionResponse struct {
	Kind PermissionResponseKind
	// Rule, for PermissionAllowedAlways, is the compiled rule to add to the live
	// session rule set (the prompter also persists its string to settings).
	Rule *toolreg.Rule
	// Feedback, for PermissionDenied, is the user's optional "tell gocode what to
	// do differently" text, appended to the rejection message the model sees.
	Feedback string
	// UpdatedInput, for an allow, is the input the call runs with in place of
	// the one asked about (a stream-json host's or permission prompt tool's
	// updatedInput); nil keeps the input. The dispatcher re-derives the
	// permission fields from it and checks it again, as it does a PreToolUse
	// hook's rewrite: a deny still refuses the call, while an ask is answered
	// by this allow rather than prompted again.
	UpdatedInput json.RawMessage
}

// PermissionPrompter presents an "ask" decision to the user and returns their
// answer. It is injected by the interactive host (internal/cli drives the ui
// permission dialog); when nil, an "ask" stays a denial. Prompt blocks until the
// user answers; a dismissed prompt (esc / ctrl+c) or a cancelled context is
// reported as an error, which the dispatcher turns into a denial.
type PermissionPrompter interface {
	Prompt(ctx context.Context, req PermissionRequest) (PermissionResponse, error)
}

// PromptSurface is optionally implemented by a PermissionPrompter whose ability
// to ask depends on the call's context — `gocode mcp serve`, where only a
// connected client that declared the MCP elicitation capability can put a
// question to its user. When PromptUnavailable reports true, the ask is
// resolved exactly as with no prompter (the headless denial), and a non-empty
// hint is appended to the generic how-to-allow denial. suggested is the rule
// string(s) that would allow the call, most specific first.
type PromptSurface interface {
	PromptUnavailable(ctx context.Context, suggested []string) (unavailable bool, hint string)
}

// WithPermissionPrompter injects the interactive permission prompter.
func WithPermissionPrompter(p PermissionPrompter) Option {
	return func(d *Dispatcher) { d.Prompter = p }
}

// SetPermissionPrompter injects (or clears) the prompter after construction.
// It mirrors SetAsker: the host builds the ui-driven prompter after the
// Dispatcher (the Bubble Tea program exists later) and hands it here.
func (d *Dispatcher) SetPermissionPrompter(p PermissionPrompter) {
	d.Prompter = p
}

// SetModeSwitch installs the callback invoked when the user chooses "allow all
// edits during this session" at a prompt: the dispatcher flips its own Mode to
// acceptEdits and calls this so the host can mirror the change onto the
// conversation + UI footer (the same effect as /permission-mode acceptEdits).
func (d *Dispatcher) SetModeSwitch(f func(toolreg.Mode)) {
	d.OnModeSwitch = f
}

// --- synchronized Mode / Rules access ---------------------------------------
//
// Concurrency-safe (read-only) tool calls are dispatched on parallel goroutines
// (agent.Loop.dispatchTurn), so every read of Mode / Rules on the hot path and
// every mutation from a prompt answer goes through stateMu. promptMu serializes
// the prompts themselves so two dialogs are never shown at once.

// CurrentMode returns the dispatcher's current permission mode under the lock.
func (d *Dispatcher) CurrentMode() toolreg.Mode {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.Mode
}

// SnapshotRules returns a copy of the rule set the Decider consults, safe to hand
// to it while another goroutine may be appending an allow-always rule: the live
// rules (shared store or per-dispatcher Rules) followed by any active temporary
// layers (skill allowed-tools / disallowed-tools, see scopedrules.go). The
// temporary layers are never part of the live rules; use PersistentRules to
// seed another dispatcher or to read what may be persisted.
func (d *Dispatcher) SnapshotRules() toolreg.RuleSet {
	base := d.PersistentRules()
	if extra := d.scopedSnapshot(); len(extra) > 0 {
		out := make(toolreg.RuleSet, 0, len(base)+len(extra))
		out = append(out, base...)
		return append(out, extra...)
	}
	return base
}

// SetMode replaces the permission mode under the lock. Entering plan mode
// records the mode it was entered from; a real change is reported to the
// plan-mode tracker and the mode listeners (see AddModeListener).
func (d *Dispatcher) SetMode(m toolreg.Mode) {
	d.stateMu.Lock()
	old := d.Mode
	d.Mode = m
	if m == toolreg.ModePlan && old != toolreg.ModePlan {
		d.prePlanMode = old
	}
	listeners := d.modeListeners
	tracker := d.PlanTracker
	d.stateMu.Unlock()
	if old == m {
		return
	}
	tracker.ModeChanged(old == toolreg.ModePlan, m == toolreg.ModePlan)
	for _, f := range listeners {
		f(old, m)
	}
}

// addAllowRule appends an allow rule to the live rule set under the lock. The
// slice header is replaced so a concurrent SnapshotRules reader sees either the
// old or the new set, never a torn one.
func (d *Dispatcher) addAllowRule(r toolreg.Rule) {
	if d.shared != nil {
		d.shared.Add(r)
		return
	}
	d.stateMu.Lock()
	next := make(toolreg.RuleSet, len(d.Rules), len(d.Rules)+1)
	copy(next, d.Rules)
	d.Rules = append(next, r)
	d.stateMu.Unlock()
}

// --- the ask → prompt resolution --------------------------------------------

// askOutcome is the resolution of an "ask" decision.
type askOutcome struct {
	// allowed is true when the call may proceed.
	allowed bool
	// denyMessage is the is_error tool_result body when !allowed.
	denyMessage string
	// input, when allowed, is the changed input the answer carried
	// (PermissionResponse.UpdatedInput); nil when the call runs as asked.
	input json.RawMessage
}

// resolveAsk turns a behavior:"ask" decision into a concrete allow/deny by
// prompting the user (when a prompter is set) and honouring the answer.
// Prompts are serialized so two dialogs are never shown at once.
func (d *Dispatcher) resolveAsk(ctx context.Context, pc toolreg.Call, input json.RawMessage, desc toolreg.ToolDescriptor, decision toolreg.Decision) askOutcome {
	return d.resolveAskFrom(ctx, pc, input, desc, decision, "")
}

// resolveAskFrom is resolveAsk with an Origin attribution carried on the
// PermissionRequest (see PermissionRequest.Origin).
func (d *Dispatcher) resolveAskFrom(ctx context.Context, pc toolreg.Call, input json.RawMessage, desc toolreg.ToolDescriptor, decision toolreg.Decision, origin string) askOutcome {
	prompter := d.Prompter
	unavailableHint := ""
	if ps, ok := prompter.(PromptSurface); ok {
		if unavailable, hint := ps.PromptUnavailable(ctx, suggestedRules(pc, desc)); unavailable {
			prompter, unavailableHint = nil, hint
		}
	}
	if decision.Classify {
		// Auto mode: the safety classifier decides (autoclassify.go); it
		// hands the call to the user only when it has no verdict or the
		// denial limit is reached, and never allows without a verdict.
		out, next, done := d.classifyAsk(ctx, pc, input, decision, prompter != nil)
		if done {
			return out
		}
		decision = next
	}
	if prompter == nil {
		// No interactive user (-p / headless). A plan-mode hold has no one to
		// approve it, so keep the safe denial with a plan-specific message rather
		// than the generic how-to-allow text — a non-interactive run must never
		// silently execute a write it only held for approval.
		if decision.ReasonType == toolreg.ReasonMode && decision.Mode == toolreg.ModePlan {
			return askOutcome{allowed: false, denyMessage: planModeDenyMessage(pc.Tool)}
		}
		// A safety-check ask (a settings file, a sensitive file, …) is not
		// something an allow rule or a permission mode grants, so the denial is
		// the ask message itself, without the how-to-allow guidance.
		if decision.ReasonType == toolreg.ReasonSafetyCheck && decision.Reason != "" {
			return askOutcome{allowed: false, denyMessage: decision.Reason}
		}
		// Otherwise keep the denial, but tell the user how to allow it.
		msg := noPrompterDenyMessage(pc, desc, decision, d.ManagedRulesOnly)
		if unavailableHint != "" {
			msg += " " + unavailableHint
		}
		return askOutcome{allowed: false, denyMessage: msg}
	}

	// A tool whose call is itself a question to the user (AskUserQuestion)
	// presents its own dialog when it runs; that dialog is the ask, so no
	// permission prompt is shown in front of it.
	if desc.RequiresUserInteraction && decision.ReasonType == toolreg.ReasonUserInteraction {
		return askOutcome{allowed: true}
	}

	// Serialize: only one permission dialog at a time.
	d.promptMu.Lock()
	defer d.promptMu.Unlock()

	// The rule may have been granted by a concurrent prompt while we waited for
	// the lock; re-check the live rules before asking again. Only a plain
	// "not granted yet" ask can be satisfied that way: a safety-check ask, a
	// plan-mode hold and an ask rule each outrank an allow rule in the decider
	//, so a matching
	// allow rule must not skip their prompt.
	if askSatisfiableByAllowRule(decision) {
		rules := d.SnapshotRules()
		if r := d.Decider.MatchRule(rules, pc, toolreg.RuleAllow); r != nil {
			return askOutcome{allowed: true}
		}
	}

	req := PermissionRequest{
		Tool:        pc.Tool,
		ToolName:    string(pc.Tool),
		Input:       input,
		RuleContent: pc.RuleContent,
		Reason:      decision.Reason,
		Suggested:   suggestedRules(pc, desc),
		IsEdit:      desc.EditTool,
		Origin:      origin,
		// allowManagedPermissionRulesOnly: no "don't ask again".
		NoAlwaysAllow: d.ManagedRulesOnly || decision.ReasonType == ReasonSandboxOverride,
	}
	switch decision.ReasonType {
	case toolreg.ReasonWorkingDir:
		// A read outside the working directories: "Yes, allow reading from
		// <dir>/ during this session" adds a session Read(<dir>/**) rule — it
		// is not saved to any settings file.
		req.Notice = decision.Reason
		req.Suggested, req.AlwaysAllowLabel = outsideReadSuggestion(pc)
		req.SessionOnly = true
	case ReasonClassifier:
		req.Notice = decision.Reason
	}
	if d.ManagedRulesOnly || decision.ReasonType == ReasonSandboxOverride {
		req.Suggested = nil
	}

	if d.OnPermissionPrompt != nil {
		d.OnPermissionPrompt(ctx, req)
	}
	resp, err := prompter.Prompt(ctx, req)
	if err != nil {
		// Cancelled turn or a prompter failure: a denial with the rejection
		// message. It is not the user's answer, so it does not end the turn
		// (an interrupted turn is already ending).
		return d.rejected(ctx, "", false)
	}

	allowed := askOutcome{allowed: true, input: changedInput(input, resp.UpdatedInput)}
	switch resp.Kind {
	case PermissionAllowedOnce:
		return allowed
	case PermissionAllowedAlways:
		// Under allowManagedPermissionRulesOnly only managed rules count: a
		// "don't ask again" answer allows this one call and is never added to
		// the live rules.
		if resp.Rule != nil && !d.ManagedRulesOnly {
			d.addAllowRule(*resp.Rule)
		}
		return allowed
	case PermissionAllowedEditsSession:
		d.SetMode(toolreg.ModeAcceptEdits)
		if d.OnModeSwitch != nil {
			d.OnModeSwitch(toolreg.ModeAcceptEdits)
		}
		return allowed
	default: // PermissionDenied
		return d.rejected(ctx, resp.Feedback, true)
	}
}

// changedInput is updated when it is a JSON document that differs from
// input, else nil.
func changedInput(input, updated json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(updated)) == 0 || !json.Valid(updated) || jsonSameValue(input, updated) {
		return nil
	}
	return updated
}

// jsonSameValue reports whether two JSON documents hold the same value (an
// empty document counts as {}).
func jsonSameValue(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) ([]byte, bool) {
		if len(bytes.TrimSpace(r)) == 0 {
			r = json.RawMessage(`{}`)
		}
		var v any
		if json.Unmarshal(r, &v) != nil {
			return nil, false
		}
		out, _ := json.Marshal(v)
		return out, true
	}
	x, okx := norm(a)
	y, oky := norm(b)
	return okx && oky && bytes.Equal(x, y)
}

// errChangedInput is the refusal for a changed input on a check whose input
// gocode synthesized (a browser site, a save_to path, a `!` command): there
// is no call to run it with.
const errChangedInput = "The permission answer changed the input (updatedInput), which cannot be applied to this check; it was not allowed."

// rejected is the outcome of a permission prompt the user rejected.
//
// In the main agent a rejection without feedback ends the turn (abort=true):
// the loop answers the remaining calls of the round with the same rejection,
// adds the "[Request interrupted by user for tool use]" marker and returns to
// the user without calling the model again — the model is told to stop and
// wait. A rejection WITH feedback does not end the turn: the feedback rides in
// the tool result and the model goes on with it.
//
// A subagent's rejection never ends any turn: the subagent is told the call
// was denied and to try another approach or report the limitation (with the
// user's words appended when they typed any).
func (d *Dispatcher) rejected(ctx context.Context, feedback string, abort bool) askOutcome {
	if d.Subagent {
		return askOutcome{allowed: false, denyMessage: subagentRejectionMessage(feedback)}
	}
	if abort && strings.TrimSpace(feedback) == "" {
		agent.AbortTurn(ctx, agent.ErrToolUseRejected)
	}
	return askOutcome{allowed: false, denyMessage: rejectionMessage(feedback)}
}

// suggestedRules computes the rule string(s) a "don't ask again" would persist,
// most specific first. For a tool with rule content it is "Rule(content)"; for
// a whole-tool rule it is the bare rule key. An MCP tool additionally offers the
// server-level rule ("mcp__server").
func suggestedRules(pc toolreg.Call, desc toolreg.ToolDescriptor) []string {
	key := desc.Rule()
	if key == "" {
		key = string(pc.Tool)
	}
	// MCP: the specific "mcp__server__tool" rule plus the whole-server rule.
	if strings.HasPrefix(key, mcpRuleNamePrefix) {
		out := []string{key}
		if server := mcpServerOf(key); server != "" {
			out = append(out, mcpRuleNamePrefix+server)
		}
		return dedupeStrings(out)
	}
	// File / path tools: the gitignore-style, root-anchored "//<path>" rule
	// content (an absolute path becomes "//abs/path"), e.g. Edit(//path).
	if pc.Path != "" {
		return dedupeStrings([]string{key + "(" + pathRuleContent(pc.Path) + ")"})
	}
	// Bash: offer a command-PREFIX rule first, then the exact command. The prefix
	// (e.g. Bash(ls:*)) lets "don't ask again" for `ls -la /a` also cover `ls /b`
	// in any other folder — so approving a command once stops it re-prompting per
	// path. It is saved in the Bash tool's colon grammar "<cmd>:*", which matches
	// at a word boundary (`ls`, `ls -la`, not `lsblk`) and — like every Bash allow
	// rule — never approves a chained/substituted/redirected command (toolreg
	// bashPrefixRuleMatches). A command with shell metacharacters yields no safe
	// prefix → exact only.
	if pc.Tool == protocol.ToolBash && pc.RuleContent != "" {
		out := make([]string, 0, 2)
		if pfx := bashCommandPrefix(pc.RuleContent); pfx != "" {
			out = append(out, key+"("+pfx+":*)")
		}
		out = append(out, key+"("+pc.RuleContent+")")
		return dedupeStrings(out)
	}
	// Other content-bearing tools: WebFetch "domain:<host>", etc. (exact content).
	if pc.RuleContent != "" {
		return dedupeStrings([]string{key + "(" + pc.RuleContent + ")"})
	}
	// Whole-tool rule.
	return dedupeStrings([]string{key})
}

// bashCommandPrefix returns the leading command token of a Bash command (e.g.
// "ls" from "ls -la /a"), for building a prefix allow rule. It returns "" when
// the first token carries shell metacharacters or an env assignment, so a
// command like `FOO=bar x` or `a | b` yields no broad prefix (exact rule only).
func bashCommandPrefix(cmd string) string {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return ""
	}
	// A prefix rule "<first>*" glob-matches anything starting with <first>, so if
	// the command chains or redirects (|, &, ;, >, <, $, `, (, newline) a prefix
	// would also allow the chained/redirected part — unsafe. Only a simple single
	// command (no shell operators anywhere) gets a broad prefix.
	if strings.ContainsAny(s, "|&;<>$`()\n\r") {
		return ""
	}
	first := s
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		first = s[:i]
	}
	if first == "" || strings.ContainsAny(first, "=*\"'\\") {
		return ""
	}
	return first
}

// pathRuleContent renders a filesystem path as the root-anchored rule
// content: an absolute "/a/b" becomes "//a/b".
func pathRuleContent(p string) string {
	if strings.HasPrefix(p, "/") {
		return "//" + strings.TrimPrefix(p, "/")
	}
	return p
}

const mcpRuleNamePrefix = "mcp__"

// mcpServerOf extracts the server segment of an "mcp__server__tool" name.
func mcpServerOf(name string) string {
	rest := strings.TrimPrefix(name, mcpRuleNamePrefix)
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// askSatisfiableByAllowRule reports whether an "ask" decision is the kind an
// allow rule granted meanwhile would have prevented: not a safety check, not a
// plan-mode hold and not an ask rule.
func askSatisfiableByAllowRule(decision toolreg.Decision) bool {
	if decision.SafetyAsk {
		return false
	}
	switch decision.ReasonType {
	case toolreg.ReasonSafetyCheck, toolreg.ReasonRule, ReasonSandboxOverride:
		// A run outside the sandbox is approved per run (sandbox.go).
		return false
	case toolreg.ReasonMode:
		return decision.Mode != toolreg.ModePlan
	}
	return true
}
