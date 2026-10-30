package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

func newSkillScopeSession(t *testing.T, main *skillTurnCaller) (*sessionRuntime, string) {
	t.Helper()
	env, _, _, cwd := sessionEnv(t, "")
	writeProjectSkill(t, cwd, "zz-sub-build", "---\ndescription: Build things\nallowed-tools: Bash(make:*)\ndisallowed-tools: Write\nmodel: opus\neffort: high\n---\nRun make.\n")
	s := &printSession{
		Model:     main.model,
		Mode:      toolreg.ModeDefault,
		Provider:  providerAnthropic,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return main, nil },
	}
	rt := buildSession(context.Background(), env, s, main, sessionConfig{})
	t.Cleanup(func() { rt.Close("other") })
	return rt, cwd
}

func decideOn(d *toolrun.Dispatcher, tool protocol.Tool, content string) toolreg.Behavior {
	desc, _ := d.Registry.Lookup(tool)
	pc := toolreg.Call{Tool: tool, Rule: desc.Rule(), RuleContent: content}
	return d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), nil).Behavior
}

// An inline skill invoked BY A SUBAGENT switches THAT subagent's model and
// effort (not the main session's) and applies its allowed/disallowed tools to
// the subagent's dispatcher, for the rest of the subagent's run; everything is
// restored when the run ends.
func TestSubagentInlineSkillSwitchesItsOwnModel(t *testing.T) {
	main := &skillTurnCaller{model: "user-model", effort: "medium"}
	rt, cwd := newSkillScopeSession(t, main)

	d := rt.childDispatcher(rt.agentTool, 1, []string{"*"})
	st, ok := d.SkillTool.(*agenttool.SkillTool)
	if !ok {
		t.Fatalf("subagent Skill tool = %T", d.SkillTool)
	}
	child := &skillTurnCaller{model: "child-model", effort: "low"}
	scope := subagent.NewTurnScope(child)
	ctx := subagent.WithTurnScope(context.Background(), scope)

	inv, err := st.Invoke(ctx, "zz-sub-build", "")
	if err != nil {
		t.Fatal(err)
	}
	if inv.ActiveModel != "claude-opus-5-5" {
		t.Errorf("ActiveModel = %q, want claude-opus-5-5", inv.ActiveModel)
	}
	if child.CurrentModel() != "claude-opus-5-5" || child.CurrentEffort() != "high" {
		t.Errorf("subagent model/effort = %q/%q, want claude-opus-5-5/high", child.CurrentModel(), child.CurrentEffort())
	}
	if main.CurrentModel() != "user-model" || main.CurrentEffort() != "medium" {
		t.Errorf("main session switched to %q/%q", main.CurrentModel(), main.CurrentEffort())
	}
	if got := decideOn(d, protocol.ToolBash, "make build"); got != toolreg.BehaviorAllow {
		t.Errorf("subagent `make build` = %v, want allow", got)
	}
	if got := decideOn(d, protocol.ToolWrite, filepath.Join(cwd, "f")); got != toolreg.BehaviorDeny {
		t.Errorf("subagent Write = %v, want deny", got)
	}
	if got := decideOn(rt.dispatcher, protocol.ToolBash, "make build"); got == toolreg.BehaviorAllow {
		t.Error("the subagent's skill auto-approved `make build` for the main conversation")
	}

	scope.End()
	if child.CurrentModel() != "child-model" || child.CurrentEffort() != "low" {
		t.Errorf("after the run model/effort = %q/%q, want child-model/low", child.CurrentModel(), child.CurrentEffort())
	}
	if n := d.ScopedRules().Len(); n != 0 {
		t.Errorf("%d rule layers left after the run", n)
	}

	// A subagent that shares the main session's caller (a pass-through fake)
	// is never retargeted from here: that would switch the main session.
	shared := subagent.NewTurnScope(main)
	if _, err := st.Invoke(subagent.WithTurnScope(context.Background(), shared), "zz-sub-build", ""); err != nil {
		t.Fatal(err)
	}
	if main.CurrentModel() != "user-model" || main.CurrentEffort() != "medium" {
		t.Errorf("shared caller switched to %q/%q", main.CurrentModel(), main.CurrentEffort())
	}
	shared.End()
}

// A forked skill's allowed-/disallowed-tools reach the agents the fork spawns,
// and theirs, unchanged — never more: a main-conversation inline allow does not
// leak in, the fork's denied tools leave the descendants' rosters, and a skill
// forked further down cannot re-allow what an ancestor fork denies.
func TestForkedSkillRulesReachTheForksAgents(t *testing.T) {
	main := &skillTurnCaller{model: "user-model", effort: "medium"}
	rt, cwd := newSkillScopeSession(t, main)

	// An inline skill active on the main conversation: its allow must not
	// reach any subagent; its deny must.
	rt.dispatcher.ScopedRules().Push(compileSkillRules([]string{"Bash(git:*)"}, []string{"Bash(curl:*)"}))

	fd, ok := rt.scopedChildDispatcher(rt.agentTool, []string{"*"}, agenttool.ChildRuleScope{
		Origin:     "/deploy",
		AllowRules: []string{"Bash(make:*)", "Bash(npm:*)"},
		DenyRules:  []string{"Write", "Bash(npm publish:*)"},
	}).(*toolrun.Dispatcher)
	if !ok {
		t.Fatal("fork dispatcher is not a *toolrun.Dispatcher")
	}
	forkAgent, ok := fd.AgentTool.(*agenttool.AgentTool)
	if !ok {
		t.Fatalf("fork Agent tool = %T", fd.AgentTool)
	}
	if slices.Contains(forkAgent.ToolNames, "Write") {
		t.Errorf("Write (denied by the fork) still on the fork's agents' roster: %v", forkAgent.ToolNames)
	}
	child := forkAgent.NewDispatcher([]string{"*"}).(*toolrun.Dispatcher)
	grand := child.AgentTool.(*agenttool.AgentTool).NewDispatcher([]string{"*"}).(*toolrun.Dispatcher)
	// A skill forked by the fork's child, allowing Write.
	nested := child.SkillTool.(*agenttool.SkillTool).Agent.NewScopedDispatcher([]string{"*"}, agenttool.ChildRuleScope{
		Origin: "/sneaky", AllowRules: []string{"Write", "Bash(npm publish:*)"},
	}).(*toolrun.Dispatcher)

	write := filepath.Join(cwd, "f.txt")
	for name, d := range map[string]*toolrun.Dispatcher{"fork": fd, "child": child, "grandchild": grand, "nested fork": nested} {
		if got := decideOn(d, protocol.ToolBash, "make build"); got != toolreg.BehaviorAllow {
			t.Errorf("%s: make build = %v, want allow (the fork's allow)", name, got)
		}
		if got := decideOn(d, protocol.ToolBash, "make build; rm -rf /"); got == toolreg.BehaviorAllow {
			t.Errorf("%s: a chained command was approved by Bash(make:*)", name)
		}
		if got := decideOn(d, protocol.ToolWrite, write); got != toolreg.BehaviorDeny {
			t.Errorf("%s: Write = %v, want deny (the fork's deny wins)", name, got)
		}
		if got := decideOn(d, protocol.ToolBash, "npm publish"); got != toolreg.BehaviorDeny {
			t.Errorf("%s: npm publish = %v, want deny", name, got)
		}
		if got := decideOn(d, protocol.ToolBash, "curl x"); got != toolreg.BehaviorDeny {
			t.Errorf("%s: curl = %v, want deny (main inline deny)", name, got)
		}
		if got := decideOn(d, protocol.ToolBash, "git push"); got == toolreg.BehaviorAllow {
			t.Errorf("%s: git push allowed by the MAIN conversation's inline allow", name)
		}
	}
	// A sibling subagent of the main conversation gets none of the fork's rules.
	sib := rt.childDispatcher(rt.agentTool, 1, []string{"*"})
	if got := decideOn(sib, protocol.ToolBash, "make build"); got == toolreg.BehaviorAllow {
		t.Error("a non-fork subagent inherited the fork's allow rule")
	}
	if got := decideOn(sib, protocol.ToolWrite, write); got == toolreg.BehaviorDeny {
		t.Error("a non-fork subagent inherited the fork's deny rule")
	}
	// Nothing reached the shared (persistable) store.
	for _, r := range rt.ruleStore.Snapshot() {
		if r.Source == skillRuleSource {
			t.Errorf("skill rule leaked into the shared store: %+v", r)
		}
	}
}
