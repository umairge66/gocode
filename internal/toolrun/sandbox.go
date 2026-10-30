package toolrun

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/exec"
)

// BashSandbox is the session's OS sandbox as the Bash tool and its permission
// check see it (internal/sandbox.Manager satisfies it).
type BashSandbox interface {
	exec.Sandbox
	// IsSandboxingEnabled reports whether commands run sandboxed this session.
	IsSandboxingEnabled() bool
	// AutoAllowEnabled is sandbox.autoAllowBashIfSandboxed where supported.
	AutoAllowEnabled() bool
}

// WithSandbox runs the dispatcher's Bash commands through the session's OS
// sandbox (nil: unconfined).
func WithSandbox(sb BashSandbox) Option {
	return func(d *Dispatcher) { d.Sandbox = sb }
}

// sandboxAutoAllowReason is the decision reason of a sandboxed command that
// skipped the prompt.
const sandboxAutoAllowReason = "Auto-allowed with sandbox (autoAllowBashIfSandboxed enabled)"

// ReasonSandboxOverride is the decisionReason type of the prompt for a
// command asking to leave the sandbox (reason "dangerouslyDisableSandbox").
const ReasonSandboxOverride = "sandboxOverride"

// bashAdapter builds the Bash adapter carrying the dispatcher's background
// task manager, working directory and sandbox.
func (d *Dispatcher) bashAdapter() adapter {
	t := exec.BashTool{BackgroundTasks: d.BackgroundTasks, Dir: d.WorkDir, Owner: d.taskOwner(), UgrepPath: d.ugrepShadow}
	if d.Sandbox != nil {
		t.Sandbox = d.Sandbox
	}
	a := BashAdapter(t)
	a.refine = d.refineSandboxedBash
	if d.GrepViaBash() {
		a.description = bashDescriptionGrepViaBash
	}
	return a
}

// refineSandboxedBash applies the sandbox to the Bash permission decision:
//
//   - a command that will run sandboxed skips the prompt when
//     autoAllowBashIfSandboxed is on (default): an "ask" from the default
//     flow or a whole-tool Bash ask rule becomes "allow", as does the dontAsk
//     mode's refusal. Deny rules and command-specific ask rules still apply,
//     and plan mode keeps holding the call;
//   - a command whose dangerouslyDisableSandbox actually takes it out of the
//     sandbox is confirmed ("Run outside of the sandbox") unless a rule or a
//     permissive mode already decided it.
//
// Nothing is auto-allowed when the sandbox is unavailable: ShouldSandbox is
// false then.
func (d *Dispatcher) refineSandboxedBash(input json.RawMessage, pc toolreg.Call, desc toolreg.ToolDescriptor, decision toolreg.Decision) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision) {
	sb := d.Sandbox
	if sb == nil || pc.Tool != protocol.ToolBash {
		return pc, desc, decision
	}
	var in exec.BashInput
	if err := unmarshal(input, &in); err != nil {
		return pc, desc, decision
	}
	if decision.Behavior == toolreg.BehaviorDeny && decision.ReasonType == toolreg.ReasonRule {
		return pc, desc, decision
	}
	dds := bool(in.DangerouslyDisableSandbox)
	sandboxed := sb.ShouldSandbox(in.Command, dds)
	if dds && !sandboxed && sb.ShouldSandbox(in.Command, false) {
		// Leaving the sandbox always needs the user's explicit approval of
		// this run: no allow rule, permission mode (acceptEdits,
		// bypassPermissions, dontAsk) or hook turns it into a silent
		// unsandboxed run. A deny or ask stays as it is.
		if decision.Behavior == toolreg.BehaviorAllow {
			decision = toolreg.Decision{
				Behavior:   toolreg.BehaviorAsk,
				ReasonType: ReasonSandboxOverride,
				Reason:     "Run outside of the sandbox",
			}
		}
		return pc, desc, decision
	}
	if !sandboxed || !sb.AutoAllowEnabled() || d.CurrentMode() == toolreg.ModePlan {
		return pc, desc, decision
	}
	// Settle the sandbox's start before auto-allowing: a start failure
	// disables sandboxing for the session, and the command would then run
	// unconfined on an allow that was only granted because it was sandboxed.
	if st, ok := sb.(interface{ Start() error }); ok {
		if err := st.Start(); err != nil || !sb.ShouldSandbox(in.Command, dds) {
			return pc, desc, decision
		}
	}
	switch decision.Behavior {
	case toolreg.BehaviorAsk:
		switch decision.ReasonType {
		case toolreg.ReasonRule:
			if decision.Rule != nil && decision.Rule.Content != "" && decision.Rule.Content != "*" {
				return pc, desc, decision // a command-specific ask rule still asks
			}
		case toolreg.ReasonSafetyCheck, toolreg.ReasonHook:
			return pc, desc, decision
		}
	case toolreg.BehaviorDeny:
		if decision.ReasonType != toolreg.ReasonMode {
			return pc, desc, decision
		}
	default:
		return pc, desc, decision
	}
	return pc, desc, toolreg.Decision{
		Behavior:   toolreg.BehaviorAllow,
		ReasonType: toolreg.ReasonOther,
		Reason:     sandboxAutoAllowReason,
	}
}
