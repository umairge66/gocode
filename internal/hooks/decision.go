package hooks

import (
	"encoding/json"
	"strings"
)

// PermissionDecision is the per-tool permission a PreToolUse (and a few other)
// hook may return in hookSpecificOutput.permissionDecision.
type PermissionDecision string

const (
	PermAllow PermissionDecision = "allow"
	PermDeny  PermissionDecision = "deny"
	PermAsk   PermissionDecision = "ask"
	PermDefer PermissionDecision = "defer"
)

// HookSpecificOutput is the event-tagged portion of a hook's JSON output, a
// discriminated union keyed by hookEventName. The fields modelled here are the
// ones that affect dispatch: PreToolUse's permission fields plus the
// near-universal additionalContext.
type HookSpecificOutput struct {
	HookEventName            string             `json:"hookEventName,omitempty"`
	PermissionDecision       PermissionDecision `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string             `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput lets a PreToolUse hook rewrite the tool input before it runs.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	// AdditionalContext is injected into the model's context (UserPromptSubmit,
	// SessionStart, PostToolUse, ...).
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookOutput is the JSON document a hook may write to stdout (the top-level
// hook output schema).
//
// Pointers are used for the tri-state booleans so an absent field is
// distinguishable from an explicit false (only an explicit continue:false
// stops the agent).
type HookOutput struct {
	Continue           *bool               `json:"continue,omitempty"`
	SuppressOutput     *bool               `json:"suppressOutput,omitempty"`
	StopReason         string              `json:"stopReason,omitempty"`
	Decision           string              `json:"decision,omitempty"` // "approve" | "block"
	SystemMessage      string              `json:"systemMessage,omitempty"`
	TerminalSequence   string              `json:"terminalSequence,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	HookSpecificOutput *HookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// Behavior is the dispatch-level outcome the agent acts on after interpreting a
// hook's exit code and output.
type Behavior string

const (
	// BehaviorAllow proceeds (tool runs / prompt submits / agent stops).
	BehaviorAllow Behavior = "allow"
	// BehaviorDeny blocks the action and feeds Reason back to the model.
	BehaviorDeny Behavior = "deny"
	// BehaviorAsk escalates to an interactive permission prompt.
	BehaviorAsk Behavior = "ask"
	// BehaviorNone means the hook expressed no opinion; the action proceeds.
	BehaviorNone Behavior = "none"
	// BehaviorDefer (PreToolUse) holds the tool call for later: the turn
	// ends without running it, and a resumed session asks the hooks again.
	BehaviorDefer Behavior = "defer"
)

// ErrorKind classifies a hook that did not produce a usable decision
// ("spawn_failed"/"timeout"/"exit"/"malformed"/"wrap_failed").
type ErrorKind string

const (
	ErrSpawnFailed ErrorKind = "spawn_failed"
	ErrTimeout     ErrorKind = "timeout"
	ErrExit        ErrorKind = "exit"      // non-zero, non-2 exit
	ErrMalformed   ErrorKind = "malformed" // exit 0 but unreadable JSON
)

// Decision is the interpreted result of running a hook: the dispatch behaviour,
// any feedback, and the side-channel fields (added context, rewritten input,
// system message). For a non-blocking failure Err is set and Behavior is
// BehaviorNone.
type Decision struct {
	Behavior Behavior

	// Continue is false when the hook asked the agent to stop
	// (output.continue === false). It defaults to true.
	Continue bool

	// Reason is the feedback text shown to the model for a deny/stop.
	Reason string

	// StopReason is the message surfaced when Continue is false.
	StopReason string

	// SystemMessage is a warning surfaced to the user (not the model).
	SystemMessage string

	// AdditionalContext is injected into the model's context.
	AdditionalContext string

	// UpdatedInput, when non-nil, replaces a PreToolUse tool input.
	UpdatedInput json.RawMessage

	// SuppressOutput hides the hook's stdout from the transcript.
	SuppressOutput bool

	// ErrKind / Err describe a non-blocking failure. ErrKind is empty on success.
	ErrKind ErrorKind
	Err     string

	// Failed marks a hook that gave no answer at all: it timed out, could not
	// be started, was killed by a signal, or (http) its request could not be
	// sent. The hook's on_failure decides whether that blocks (see
	// CommandHook.FailureBlocks); the runner itself leaves Behavior none.
	Failed bool
}

// DecisionOptions carries context that affects interpretation.
type DecisionOptions struct {
	// Personal is true when the hook came from personal (user) settings rather
	// than project settings. A "defer" permission decision folds to "ask" for
	// personal hooks.
	Personal bool
}

// ParseStdout interprets a hook's stdout. A document that (after trimming) does
// not start with "{" is treated as plain text, not a decision ("Hook output
// does not start with {, treating as plain text"). The returned
// HookOutput is nil in that case. A non-empty string that starts with "{" but
// fails to parse returns an error.
func ParseStdout(stdout string) (*HookOutput, error) {
	trimmed := strings.TrimSpace(stdout)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, nil // plain text, no decision
	}
	var out HookOutput
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Interpret maps a parsed HookOutput onto a Decision:
//
//   - A permissionDecision of "deny", a decision of "block", or continue:false
//     blocks; the feedback text is permissionDecisionReason ?? reason ??
//     stopReason.
//
//   - permissionDecision "ask" escalates to a prompt; "allow" and decision
//     "approve" allow; "defer" defers the call (BehaviorDefer, its
//     updatedInput dropped), except that for personal hooks "defer" folds to
//     "ask".
//
//   - additionalContext, systemMessage, updatedInput and suppressOutput are
//     carried through regardless of the behaviour.
func Interpret(out *HookOutput, opts DecisionOptions) Decision {
	d := Decision{Behavior: BehaviorNone, Continue: true}
	if out == nil {
		return d
	}

	if out.SuppressOutput != nil {
		d.SuppressOutput = *out.SuppressOutput
	}
	d.SystemMessage = out.SystemMessage
	d.StopReason = out.StopReason

	var perm PermissionDecision
	if hso := out.HookSpecificOutput; hso != nil {
		perm = hso.PermissionDecision
		d.AdditionalContext = hso.AdditionalContext
		d.UpdatedInput = hso.UpdatedInput
	}

	// Personal hooks cannot defer: a deferred decision becomes an ask.
	if perm == PermDefer && opts.Personal {
		perm = PermAsk
	}

	// Feedback precedence: permissionDecisionReason ?? reason ?? stopReason.
	reason := out.Reason
	if hso := out.HookSpecificOutput; hso != nil && hso.PermissionDecisionReason != "" {
		reason = hso.PermissionDecisionReason
	}
	if reason == "" {
		reason = out.StopReason
	}

	switch {
	case perm == PermDeny || out.Decision == "block" || (out.Continue != nil && !*out.Continue):
		d.Behavior = BehaviorDeny
		d.Reason = reason
		if out.Continue != nil && !*out.Continue {
			d.Continue = false
		}
	case perm == PermAsk:
		d.Behavior = BehaviorAsk
		d.Reason = reason
	case perm == PermAllow || out.Decision == "approve":
		d.Behavior = BehaviorAllow
	case perm == PermDefer:
		// A deferred call does not run now, so a rewrite of its input has
		// nothing to apply to; the resumed call is asked about afresh.
		d.Behavior = BehaviorDefer
		d.Reason = reason
		d.UpdatedInput = nil
	}
	return d
}

// BodyDecision interprets a success-path hook body as a decision document,
// exactly as a command hook's exit-0 stdout is interpreted. It is shared by the
// http and mcp_tool hooks, whose response body / tool text goes through the
// same reader and output mapper that command stdout uses. A body that is not
// a JSON object is treated as plain text (no decision); a body that starts with "{" but does not parse is a
// malformed non-blocking error.
func BodyDecision(body string, opts DecisionOptions) Decision {
	out, err := ParseStdout(body)
	if err != nil {
		return Decision{Behavior: BehaviorNone, Continue: true, ErrKind: ErrMalformed, Err: err.Error()}
	}
	return Interpret(out, opts)
}

// Classify turns a finished hook execution (exit code, captured stdout/stderr)
// into a Decision using these exit-code semantics:
//
//   - exit 2            -> blocking deny; stderr is the feedback.
//
//   - exit !=0 (and !=2) -> non-blocking error (kind "exit"): the action
//     proceeds and the failure is surfaced to the user.
//
//   - exit 0            -> parse stdout as JSON and Interpret it; non-JSON
//     ("plain text") stdout is no decision; unparseable JSON is a malformed
//     non-blocking error.
func Classify(exitCode int, stdout, stderr string, opts DecisionOptions) Decision {
	switch {
	case exitCode == 2:
		reason := strings.TrimSpace(stderr)
		if reason == "" {
			reason = "it gave no reason"
		}
		return Decision{Behavior: BehaviorDeny, Continue: true, Reason: reason}
	case exitCode != 0:
		return Decision{
			Behavior: BehaviorNone,
			Continue: true,
			ErrKind:  ErrExit,
			Err:      strings.TrimSpace(stderr),
		}
	default:
		out, err := ParseStdout(stdout)
		if err != nil {
			return Decision{
				Behavior: BehaviorNone,
				Continue: true,
				ErrKind:  ErrMalformed,
				Err:      err.Error(),
			}
		}
		return Interpret(out, opts)
	}
}
