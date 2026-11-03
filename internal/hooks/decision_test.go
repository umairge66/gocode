package hooks

import "testing"

func TestClassifyExitCodes(t *testing.T) {
	// Exit 2 => blocking deny, stderr is the feedback.
	d := Classify(2, "", "not allowed", DecisionOptions{})
	if d.Behavior != BehaviorDeny || d.Reason != "not allowed" {
		t.Errorf("exit2 = %+v, want deny with reason", d)
	}
	// Exit 2 with no stderr => a placeholder reason.
	d = Classify(2, "", "  ", DecisionOptions{})
	if d.Behavior != BehaviorDeny || d.Reason != "it gave no reason" {
		t.Errorf("exit2 empty = %+v", d)
	}
	// Other non-zero => non-blocking error, action proceeds.
	d = Classify(1, "", "boom", DecisionOptions{})
	if d.Behavior != BehaviorNone || !d.Continue || d.ErrKind != ErrExit || d.Err != "boom" {
		t.Errorf("exit1 = %+v, want non-blocking ErrExit", d)
	}
	// Exit 0 with no/plain-text stdout => no opinion.
	d = Classify(0, "ok, looks fine\n", "", DecisionOptions{})
	if d.Behavior != BehaviorNone || !d.Continue {
		t.Errorf("exit0 plaintext = %+v, want none/continue", d)
	}
	// Exit 0 with broken JSON => malformed non-blocking error.
	d = Classify(0, "{not json", "", DecisionOptions{})
	if d.ErrKind != ErrMalformed || d.Behavior != BehaviorNone {
		t.Errorf("exit0 brokenjson = %+v, want malformed", d)
	}
}

func TestClassifyJSONDecision(t *testing.T) {
	// Top-level decision:"block".
	d := Classify(0, `{"decision":"block","reason":"nope"}`, "", DecisionOptions{})
	if d.Behavior != BehaviorDeny || d.Reason != "nope" {
		t.Errorf("block = %+v", d)
	}
	// decision:"approve".
	d = Classify(0, `{"decision":"approve"}`, "", DecisionOptions{})
	if d.Behavior != BehaviorAllow {
		t.Errorf("approve = %+v", d)
	}
	// continue:false => stop.
	d = Classify(0, `{"continue":false,"stopReason":"halt"}`, "", DecisionOptions{})
	if d.Behavior != BehaviorDeny || d.Continue || d.StopReason != "halt" {
		t.Errorf("continue-false = %+v", d)
	}
	// continue:true alone => no opinion.
	d = Classify(0, `{"continue":true}`, "", DecisionOptions{})
	if d.Behavior != BehaviorNone || !d.Continue {
		t.Errorf("continue-true = %+v", d)
	}
}

func TestInterpretPermissionDecision(t *testing.T) {
	deny := Classify(0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked tool"}}`, "", DecisionOptions{})
	if deny.Behavior != BehaviorDeny || deny.Reason != "blocked tool" {
		t.Errorf("perm deny = %+v", deny)
	}
	ask := Classify(0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, "", DecisionOptions{})
	if ask.Behavior != BehaviorAsk {
		t.Errorf("perm ask = %+v", ask)
	}
	allow := Classify(0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, "", DecisionOptions{})
	if allow.Behavior != BehaviorAllow {
		t.Errorf("perm allow = %+v", allow)
	}
}

func TestInterpretDeferFoldsToAskForPersonal(t *testing.T) {
	stdout := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer","updatedInput":{"x":1}}}`
	// Project hook: the call is deferred, and its updatedInput dropped.
	if d := Classify(0, stdout, "", DecisionOptions{Personal: false}); d.Behavior != BehaviorDefer || d.UpdatedInput != nil {
		t.Errorf("project defer = %+v, want defer without updatedInput", d)
	}
	// Personal hook: defer -> ask.
	if d := Classify(0, stdout, "", DecisionOptions{Personal: true}); d.Behavior != BehaviorAsk {
		t.Errorf("personal defer = %+v, want ask", d)
	}
}

func TestInterpretSideChannels(t *testing.T) {
	d := Classify(0, `{"suppressOutput":true,"systemMessage":"warn","hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"extra"}}`, "", DecisionOptions{})
	if !d.SuppressOutput || d.SystemMessage != "warn" || d.AdditionalContext != "extra" {
		t.Errorf("side channels = %+v", d)
	}
	if d.Behavior != BehaviorNone {
		t.Errorf("no permission fields => none, got %v", d.Behavior)
	}
}

func TestParseStdoutPlainText(t *testing.T) {
	out, err := ParseStdout("just some log output")
	if err != nil || out != nil {
		t.Errorf("plain text = (%v,%v), want (nil,nil)", out, err)
	}
	out, err = ParseStdout(`  {"decision":"approve"}  `)
	if err != nil || out == nil || out.Decision != "approve" {
		t.Errorf("json = (%+v,%v)", out, err)
	}
}
