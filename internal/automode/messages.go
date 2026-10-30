package automode

import "strconv"

// Tool-result messages for a classifier denial, as the model sees them.

// blockPrefix opens every classifier block message.
const blockPrefix = "Permission for this action was denied by the gocode auto mode classifier. Reason: "

// workAroundGuidance tells the model which workarounds are acceptable.
const workAroundGuidance = "IMPORTANT: You *may* attempt to accomplish this action using other tools that might naturally be used to accomplish this goal, e.g. using head instead of cat. But you *should not* attempt to work around this denial in malicious ways, e.g. do not use your ability to run tests to execute non-test actions. You should only try to work around this restriction in reasonable ways that do not attempt to bypass the intent behind this denial. "

// blockGuidance is the guidance appended to a block message.
const blockGuidance = workAroundGuidance + "If you believe this capability is essential to complete the user's request, first try a safer method. Get as much of the rest of the task done as you can, then STOP and explain to the user what you were trying to do and why you need this permission. Let the user decide how to proceed."

// readOnlyNote reminds the model that read-only operations still work.
const readOnlyNote = "Note: reading files, searching code, and other read-only operations do not require the classifier and can still be used."

// retryLater suggests moving on and retrying later.
const retryLater = "If it keeps failing, continue with other tasks that don't require this action and come back to it later."

// BlockMessage is the tool result for a classifier block.
func BlockMessage(reason string) string {
	return blockPrefix + reason + ". If you have other tasks that don't depend on this action, continue working on those. " + blockGuidance
}

// UnavailableMessage is the tool result when the classifier gave no verdict
// and nobody can be asked (including an unparseable or refused answer).
func UnavailableMessage(tool, model string, res Result) string {
	if model == "" {
		model = "The auto mode classifier"
	}
	if res.Err == nil && res.Reason != "" && res.Reason != "Classifier unavailable - blocking for safety" {
		return res.Reason + ". This is not a judgment that the action is unsafe. This is usually transient — wait briefly and try this action again as-is; don't rewrite it. " + retryLater + " " + readOnlyNote
	}
	cause := ""
	if res.Reason == "Classifier unavailable (timed out) - blocking for safety" {
		cause = " (timed out)"
	}
	return model + " is temporarily unavailable" + cause + ", so auto mode cannot determine the safety of " + tool + " right now. Wait a moment and then try this action again. " + retryLater + " " + readOnlyNote
}

// Denial limits: after this many consecutive blocks, or this many in the
// session, the next block falls back to asking the user.
const (
	MaxConsecutiveDenials = 3
	MaxTotalDenials       = 20
)

// DenialLimitDisclosure is the note shown when the limit sends a block to the
// user instead.
func DenialLimitDisclosure(consecutive, total int, reason string) string {
	var head string
	if total >= MaxTotalDenials {
		head = strconv.Itoa(total) + " actions were blocked this session. Please review the transcript before continuing."
	} else {
		head = strconv.Itoa(consecutive) + " consecutive actions were blocked. Please review the transcript before continuing."
	}
	return head + "\n\nLatest blocked action: " + reason
}
