package hooks

import (
	"context"
	"time"
)

// PromptJudgment is a prompt hook's verdict. The model is asked to answer a
// condition with {"ok":bool, "reason":string, "impossible"?:bool}. OK=true
// allows the action; OK=false blocks it with Reason as feedback. Impossible
// marks a condition that can never be satisfied; it is treated as "do not
// block" (allow), so a
// prompt hook cannot wedge the session, and surfaces Reason as a system message.
type PromptJudgment struct {
	OK         bool
	Reason     string
	Impossible bool
}

// Prompter is the seam a prompt hook queries. Given the hook's prompt text and
// the event payload, it returns the model's judgment. The hook layer does not
// own a model client, so the host (cli) injects one. A nil prompter makes every
// prompt hook a non-blocking error, keeping offline/test builds free of any
// model call or network egress.
type Prompter interface {
	Judge(ctx context.Context, prompt string, payload Payload) (PromptJudgment, error)
}

// PromptRunner executes prompt hooks: it asks the Prompter to judge the hook's
// prompt against the event and maps the verdict onto a Decision. For
// Stop/SubagentStop the prompt is framed as a stop-condition judge
// ("has the stopping condition been satisfied?"); the caller frames the text,
// so the mapping here is uniform: OK=true -> allow, OK=false -> deny(reason).
type PromptRunner struct {
	Prompter       Prompter
	DefaultTimeout time.Duration
}

// Run queries the prompter and returns the interpreted Decision. The error
// return is always nil (hook failures are non-blocking).
func (r *PromptRunner) Run(ctx context.Context, hook CommandHook, payload Payload, opts DecisionOptions) (Decision, PromptJudgment, error) {
	if r.Prompter == nil {
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrSpawnFailed, Err: "no model available for prompt hook",
		}, PromptJudgment{}, nil
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

	j, err := r.Prompter.Judge(ctx, hook.Prompt, payload)
	if err != nil {
		kind := ErrExit
		if ctx.Err() != nil {
			kind = ErrTimeout
		}
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: kind, Err: err.Error(),
		}, j, nil
	}

	switch {
	case j.Impossible:
		// The condition can never be met: do not block the session; surface the
		// explanation to the user.
		return Decision{Behavior: BehaviorNone, Continue: true, SystemMessage: j.Reason}, j, nil
	case j.OK:
		return Decision{Behavior: BehaviorAllow, Continue: true}, j, nil
	default:
		return Decision{Behavior: BehaviorDeny, Continue: true, Reason: j.Reason}, j, nil
	}
}
