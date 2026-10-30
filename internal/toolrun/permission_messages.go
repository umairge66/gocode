package toolrun

import (
	"strings"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Permission messages. The strings below are the user- and model-facing text
// of the permission flow, kept in one place.

// rejectDenied is the interactive reject message returned to the model when the
// user denies a tool call at the prompt and gives NO feedback.
const rejectDenied = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."

// rejectDeniedWithFeedbackPrefix precedes the user's typed reason when they deny
// with feedback; the feedback is appended verbatim after the trailing newline.
const rejectDeniedWithFeedbackPrefix = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\n"

// subagentRejectDenied is the reject message a subagent receives when the user
// denies its call without feedback. The subagent's turn is not ended, so it
// is told to work around the denial or report it.
const subagentRejectDenied = "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). Try a different approach or report the limitation to complete your task."

// subagentRejectWithFeedbackPrefix precedes the user's typed reason when they
// deny a subagent's call with feedback.
const subagentRejectWithFeedbackPrefix = "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). The user said:\n"

// rejectionMessage builds the deny tool_result body, appending the user's
// optional feedback.
func rejectionMessage(feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return rejectDenied
	}
	return rejectDeniedWithFeedbackPrefix + feedback
}

// subagentRejectionMessage is rejectionMessage for a subagent's call.
func subagentRejectionMessage(feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return subagentRejectDenied
	}
	return subagentRejectWithFeedbackPrefix + feedback
}

// planModeDenyMessage is the model-facing text when a plan-mode hold cannot
// be prompted for approval (no interactive surface, e.g. `-p` / headless). The
// call is held for approval (behavior:"ask"); with no one to approve
// it, the safe outcome is a denial that names the tool and says it needs
// approval — never a silent execution of the held write.
func planModeDenyMessage(tool protocol.Tool) string {
	return "plan mode: " + string(tool) + " needs approval. Cannot call " + string(tool) +
		" while in plan mode; it is held for approval, and there is no interactive user to approve it in this non-interactive session. Exit plan mode (or approve the plan) to run it."
}

// noPrompterDenyMessage is the model-facing text when an "ask" cannot be
// prompted (no interactive surface, e.g. `-p` / headless). The "ask" message
// itself is the terminal denial: "gocode requested permissions
// to <verb>, but you haven't granted it yet." A guidance sentence
// is appended telling the user how to allow it (an allow rule in settings or a
// less restrictive permission mode), since the raw constant does not carry it.
func noPrompterDenyMessage(pc toolreg.Call, desc toolreg.ToolDescriptor, decision toolreg.Decision, managedOnly bool) string {
	if decision.ReasonType == toolreg.ReasonWorkingDir && !managedOnly {
		// A read outside the working directories: the remedies are a Read
		// allow rule for the directory or making it a working directory.
		msg := decision.Reason
		if r, _ := outsideReadSuggestion(pc); len(r) > 0 {
			msg += " Add an allow rule to your settings (permissions.allow: [\"" + r[0] + "\"]) or add the directory as a working directory (--add-dir or permissions.additionalDirectories)."
		}
		return msg
	}
	var b strings.Builder
	b.WriteString("gocode requested permissions to ")
	switch {
	case pc.Path != "" && (pc.Op == pathguard.OpWrite || pc.Op == pathguard.OpCreate):
		b.WriteString("write to " + pc.Path)
	case pc.Path != "" && pc.Op == pathguard.OpRead:
		b.WriteString("read from " + pc.Path)
	default:
		b.WriteString("use " + string(pc.Tool))
	}
	b.WriteString(", but you haven't granted it yet.")

	// How-to-allow guidance. Under allowManagedPermissionRulesOnly
	// a user allow rule would be ignored, so it is not suggested.
	if managedOnly {
		b.WriteString(" Permission rules are restricted to managed settings (allowManagedPermissionRulesOnly): ask an admin to add the rule it needs to managed settings, or run in a permission mode that does not require approval.")
		return b.String()
	}
	if r := suggestedRules(pc, desc); len(r) > 0 {
		b.WriteString(" Add an allow rule to your settings (permissions.allow: [\"")
		b.WriteString(r[0])
		b.WriteString("\"]) or run in a permission mode that does not require approval (e.g. --permission-mode acceptEdits or --dangerously-skip-permissions).")
	} else {
		b.WriteString(" Add an allow rule to your settings or run in a less restrictive permission mode.")
	}
	return b.String()
}

// RejectedToolUseLabel is the short transcript label of a tool call the user
// rejected at its permission prompt.
const RejectedToolUseLabel = "User rejected tool use"

// RejectionFeedback reports whether a tool result is the main agent's
// rejection of a call at its permission prompt, and the feedback the user
// typed with it ("" when none).
func RejectionFeedback(result string) (feedback string, rejected bool) {
	if inner, ok := strings.CutPrefix(result, "<tool_use_error>"); ok {
		result = strings.TrimSuffix(inner, "</tool_use_error>")
	}
	if result == rejectDenied {
		return "", true
	}
	if rest, ok := strings.CutPrefix(result, rejectDeniedWithFeedbackPrefix); ok {
		return rest, true
	}
	return "", false
}
