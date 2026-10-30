package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// skillTurnCaller is a fake session model caller with the live model/effort
// surface of provider.ModelCaller. Round 1 invokes the skill; round 2 runs
// probe (the rest of the turn, under the skill's scope) and ends the turn.
type skillTurnCaller struct {
	mu     sync.Mutex
	model  string
	effort string
	round  int
	skill  string
	probe  func()
	seenM  []string
	seenE  []string
}

func (c *skillTurnCaller) CurrentModel() string { c.mu.Lock(); defer c.mu.Unlock(); return c.model }
func (c *skillTurnCaller) SetModel(m string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m != "" {
		c.model = m
	}
}
func (c *skillTurnCaller) CurrentEffort() string { c.mu.Lock(); defer c.mu.Unlock(); return c.effort }
func (c *skillTurnCaller) SetEffort(l string)    { c.mu.Lock(); defer c.mu.Unlock(); c.effort = l }

func (c *skillTurnCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.round++
	r := c.round
	c.seenM = append(c.seenM, c.model)
	c.seenE = append(c.seenE, c.effort)
	skill, probe := c.skill, c.probe
	c.mu.Unlock()
	if r%2 == 1 && skill != "" {
		in, _ := json.Marshal(map[string]string{"skill": skill})
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("tu_skill", "Skill", in)), StopReason: agent.StopToolUse}, nil
	}
	if probe != nil {
		probe()
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

func writeProjectSkill(t *testing.T, cwd, name, md string) {
	t.Helper()
	dir := filepath.Join(cwd, ".gocode", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An inline skill's allowed-tools / disallowed-tools / model / effort apply
// for the rest of the turn it was invoked in and are fully undone when the
// turn ends: Bash(make:*) approves `make build` (never a chained command),
// Write is denied, the model and effort switch — and afterwards the next turn
// asks again, Write is no longer denied, the user's model/effort are back,
// and nothing was added to the shared rule store or written to settings.
func TestInlineSkillScopeLastsForTheTurnOnly(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeProjectSkill(t, cwd, "zz-scope-build", "---\ndescription: Build things\nallowed-tools: Bash(make:*)\ndisallowed-tools: Write\nmodel: opus\neffort: high\n---\nRun make.\n")
	writeProjectSkill(t, cwd, "zz-scope-inherit", "---\ndescription: Keep the model\nmodel: inherit\n---\nKeep going.\n")

	caller := &skillTurnCaller{model: "user-model", effort: "medium", skill: "zz-scope-build"}
	s := &printSession{
		Model:     "user-model",
		Mode:      toolreg.ModeDefault,
		Provider:  providerAnthropic,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")

	decide := func(tool protocol.Tool, content string) toolreg.Behavior {
		d := rt.dispatcher
		desc, _ := d.Registry.Lookup(tool)
		pc := toolreg.Call{Tool: tool, Rule: desc.Rule(), RuleContent: content}
		return d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), nil).Behavior
	}
	type verdicts struct{ make, chained, write toolreg.Behavior }
	var during verdicts
	caller.probe = func() {
		during = verdicts{
			make:    decide(protocol.ToolBash, "make build"),
			chained: decide(protocol.ToolBash, "make build; rm -rf x"),
			write:   decide(protocol.ToolWrite, filepath.Join(cwd, "f.txt")),
		}
	}
	if _, err := rt.conv.Submit(context.Background(), "build it"); err != nil {
		t.Fatal(err)
	}

	if during.make != toolreg.BehaviorAllow {
		t.Errorf("during the skill turn `make build` = %v, want allow", during.make)
	}
	if during.chained == toolreg.BehaviorAllow {
		t.Errorf("Bash(make:*) approved a chained command")
	}
	if during.write != toolreg.BehaviorDeny {
		t.Errorf("during the skill turn Write = %v, want deny (disallowed-tools)", during.write)
	}
	// Round 1 ran on the user's model; round 2 (after the skill) on the skill's.
	if len(caller.seenM) != 2 || caller.seenM[0] != "user-model" || caller.seenM[1] != "claude-opus-5-5" {
		t.Errorf("models per round = %v", caller.seenM)
	}
	if len(caller.seenE) != 2 || caller.seenE[1] != "high" {
		t.Errorf("efforts per round = %v", caller.seenE)
	}

	// After the turn: everything is restored.
	if caller.CurrentModel() != "user-model" || caller.CurrentEffort() != "medium" {
		t.Errorf("after the turn model/effort = %q/%q, want user-model/medium", caller.CurrentModel(), caller.CurrentEffort())
	}
	if n := rt.dispatcher.ScopedRules().Len(); n != 0 {
		t.Errorf("%d temporary rule layers left after the turn", n)
	}
	if got := decide(protocol.ToolBash, "make build"); got != toolreg.BehaviorAsk {
		t.Errorf("next turn `make build` = %v, want ask (rule must not outlive the turn)", got)
	}
	if got := decide(protocol.ToolWrite, filepath.Join(cwd, "f.txt")); got == toolreg.BehaviorDeny {
		t.Errorf("Write still denied after the skill's turn")
	}
	for _, r := range rt.ruleStore.Snapshot() {
		if r.Content == "make:*" || r.ToolName == "Write" {
			t.Errorf("skill rule leaked into the shared store: %+v", r)
		}
	}
	for _, p := range []string{filepath.Join(cwd, ".gocode", "settings.local.json"), filepath.Join(cwd, ".gocode", "settings.json")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("settings written: %s", p)
		}
	}

	// `model: inherit` leaves the model unchanged.
	caller.mu.Lock()
	caller.skill, caller.probe, caller.seenM, caller.round = "zz-scope-inherit", nil, nil, 0
	caller.mu.Unlock()
	if _, err := rt.conv.Submit(context.Background(), "keep going"); err != nil {
		t.Fatal(err)
	}
	if len(caller.seenM) != 2 || caller.seenM[1] != "user-model" || caller.CurrentModel() != "user-model" {
		t.Errorf("model: inherit changed the model: %v / %q", caller.seenM, caller.CurrentModel())
	}
}
