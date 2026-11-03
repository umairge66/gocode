package cli

import (
	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/headless"
)

// A PreToolUse hook's "defer" ends the turn without running the call; the
// session keeps it pending (internal/conversation/deferred.go). In print mode
// the result reports it, and `gocode -p --resume <id>` (or -c) with no
// prompt runs it: the hooks and permissions decide it again, and the
// conversation goes on from its result.

// noPromptToResume is the -p error for a resume with no prompt and no
// deferred tool call to run.
const noPromptToResume = "gocode -p: no prompt provided, and the resumed session has no deferred tool call to run (pass a prompt argument or pipe one on stdin)."

// deferredResult fills a print result for a turn that ended on a deferred
// tool call: a success whose stop_reason is tool_deferred, carrying the
// pending call in deferred_tool_use; or, when the resumed call's tool is
// gone, an is_error success with stop_reason tool_deferred_unavailable.
func deferredResult(r *headless.Result, res conversation.Result) {
	stop := string(res.Reason)
	r.StopReason = &stop
	r.SetSuccess(res.AssistantText)
	if res.Reason == agent.ReasonToolDeferredUnavailable {
		r.IsError = true
		if res.Err != nil {
			msg := res.Err.Error()
			r.Result = &msg
		}
	}
	if res.Deferred != nil {
		r.DeferredToolUse = headless.NewDeferredToolUse(*res.Deferred)
	}
}

// deferredResumeNotice is the notice an interactive resume shows when the
// session holds a deferred tool call ("" when it holds none).
func deferredResumeNotice(conv *conversation.Conversation) string {
	call, ok := conv.DeferredToolUse()
	if !ok {
		return ""
	}
	return "A PreToolUse hook deferred this session's last " + call.Name + " call. It runs with your next message, once the hooks and permissions allow it."
}

// turnDeferred reports whether a turn ended with a tool call still pending.
func turnDeferred(res conversation.Result) bool {
	return res.Reason == agent.ReasonToolDeferred || res.Reason == agent.ReasonToolDeferredUnavailable
}
