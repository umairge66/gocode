package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

func TestTodoWriteSelfAllowsInEveryMode(t *testing.T) {
	d := newDecider()
	call := Call{Tool: protocol.ToolTodoWrite, Rule: "TodoWrite"}
	for _, m := range []Mode{ModeDefault, ModePlan, ModeAcceptEdits, ModeAuto, ModeDontAsk, ModeBypassPermissions} {
		if dec := d.Decide(call, m, nil, nil); dec.Behavior != BehaviorAllow {
			t.Errorf("%s: TodoWrite = %s (%s), want allow", m, dec.Behavior, dec.Reason)
		}
	}
	for _, m := range []Mode{ModeDefault, ModePlan, ModeBypassPermissions} {
		if dec := d.Decide(call, m, RuleSet{{Behavior: RuleDeny, ToolName: "TodoWrite"}}, nil); dec.Behavior != BehaviorDeny {
			t.Errorf("%s: deny rule gave %s, want deny", m, dec.Behavior)
		}
	}
	for _, m := range []Mode{ModeDefault, ModePlan} {
		if dec := d.Decide(call, m, RuleSet{{Behavior: RuleAsk, ToolName: "TodoWrite"}}, nil); dec.Behavior != BehaviorAsk || dec.ReasonType != ReasonRule {
			t.Errorf("%s: ask rule gave %s/%s, want an ask from the rule", m, dec.Behavior, dec.ReasonType)
		}
	}
}

func TestSessionCatalogToolsSelfAllowInDefault(t *testing.T) {
	d := newDecider()
	for _, tool := range []protocol.Tool{protocol.ToolTodoWrite, protocol.ToolSearch, protocol.ToolListAgents, protocol.ToolSkill, protocol.ToolSendMessage, protocol.ToolAgent} {
		if dec := d.Decide(Call{Tool: tool, Rule: string(tool)}, ModeDefault, nil, nil); dec.Behavior != BehaviorAllow {
			t.Errorf("%s in default mode = %s (%s), want allow", tool, dec.Behavior, dec.Reason)
		}
	}
}

func TestAskUserQuestionAsksTheUserDirectly(t *testing.T) {
	d := newDecider()
	call := Call{Tool: protocol.ToolAskUserQuestion, Rule: "AskUserQuestion"}
	for _, m := range []Mode{ModeDefault, ModePlan, ModeAcceptEdits, ModeAuto} {
		dec := d.Decide(call, m, nil, nil)
		if dec.Behavior != BehaviorAsk || dec.ReasonType != ReasonUserInteraction || dec.Classify {
			t.Errorf("%s: AskUserQuestion = %+v, want a user-interaction ask", m, dec)
		}
	}
	if dec := d.Decide(call, ModeDontAsk, nil, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("dontAsk: AskUserQuestion = %s, want deny", dec.Behavior)
	}
	if dec := d.Decide(call, ModeDefault, RuleSet{{Behavior: RuleDeny, ToolName: "AskUserQuestion"}}, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("deny rule: AskUserQuestion = %s, want deny", dec.Behavior)
	}
}

func TestPlanFloorStillHoldsUndecidedWrites(t *testing.T) {
	d := newDecider()
	for _, tool := range []protocol.Tool{protocol.ToolWrite, protocol.ToolBash, protocol.ToolAmendAgent} {
		dec := d.Decide(Call{Tool: tool, Rule: string(tool), RuleContent: "x"}, ModePlan, nil, nil)
		if dec.Behavior != BehaviorAsk || dec.Mode != ModePlan {
			t.Errorf("%s in plan = %+v, want held by the plan floor", tool, dec)
		}
	}
}
