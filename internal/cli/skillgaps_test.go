package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// --- model checks (auto mode / allowlist) and numeric effort --------------

// runSkillModel applies an inline activation to a fresh caller under g and
// returns the active model, the caller's model/effort during the scope, and
// after it ended.
func runSkillModel(t *testing.T, g skillModelGate, current string, a agenttool.SkillActivation) (active, during, effort, after string) {
	t.Helper()
	c := &skillTurnCaller{model: current, effort: "medium"}
	var undo []func()
	active = applySkillModel(c, g, a, func(f func()) { undo = append(undo, f) })
	during, effort = c.CurrentModel(), c.CurrentEffort()
	for i := len(undo) - 1; i >= 0; i-- {
		undo[i]()
	}
	if c.CurrentEffort() != "medium" {
		t.Errorf("effort not restored: %q", c.CurrentEffort())
	}
	return active, during, effort, c.CurrentModel()
}

func TestSkillModelChecks(t *testing.T) {
	mode := func(m toolreg.Mode) func() toolreg.Mode { return func() toolreg.Mode { return m } }
	list := func(set bool, e ...string) func() *modelPolicy {
		return func() *modelPolicy {
			return &modelPolicy{provider: providerAnthropic, availableSet: set, available: e}
		}
	}
	cases := []struct {
		name           string
		g              skillModelGate
		current        string
		skillModel     string
		wantActive     string
		wantDuring     string
		wantEffortHigh bool
	}{
		{name: "default mode switches", g: skillModelGate{provider: providerAnthropic, mode: mode(toolreg.ModeDefault)},
			current: "user-model", skillModel: "claude-opus-4-5", wantActive: "claude-opus-4-5", wantDuring: "claude-opus-4-5"},
		{name: "auto mode skips a model without auto-mode support (G_t)", g: skillModelGate{provider: providerAnthropic, mode: mode(toolreg.ModeAuto)},
			current: "user-model", skillModel: "claude-opus-4-5", wantActive: "", wantDuring: "user-model"},
		{name: "auto mode keeps a supported model", g: skillModelGate{provider: providerAnthropic, mode: mode(toolreg.ModeAuto)},
			current: "user-model", skillModel: "opus", wantActive: "claude-opus-5-5", wantDuring: "claude-opus-5-5"},
		{name: "outside availableModels keeps the session model (Kfn)", g: skillModelGate{provider: providerAnthropic, policy: list(true, "sonnet")},
			current: "user-model", skillModel: "opus", wantActive: "user-model", wantDuring: "user-model"},
		{name: "family alias entry allows the family", g: skillModelGate{provider: providerAnthropic, policy: list(true, "opus")},
			current: "user-model", skillModel: "opus", wantActive: "claude-opus-5-5", wantDuring: "claude-opus-5-5"},
		{name: "id entry allows its dated variant", g: skillModelGate{provider: providerAnthropic, policy: list(true, "claude-opus-4-5")},
			current: "user-model", skillModel: "claude-opus-4-5-20251101", wantActive: "claude-opus-4-5-20251101", wantDuring: "claude-opus-4-5-20251101"},
		{name: "empty allowlist allows nothing", g: skillModelGate{provider: providerAnthropic, policy: list(true)},
			current: "user-model", skillModel: "opus", wantActive: "user-model", wantDuring: "user-model"},
		{name: "the default model is always allowed (Nb)", g: skillModelGate{provider: providerAnthropic, policy: list(true), defaultModel: "claude-opus-5-5"},
			current: "user-model", skillModel: "opus", wantActive: "claude-opus-5-5", wantDuring: "claude-opus-5-5"},
		{name: "[1m] session keeps the suffix", g: skillModelGate{provider: providerAnthropic},
			current: "claude-opus-4-6[1m]", skillModel: "claude-sonnet-4-6", wantActive: "claude-sonnet-4-6[1m]", wantDuring: "claude-sonnet-4-6[1m]"},
		{name: "integer effort runs high", g: skillModelGate{provider: providerAnthropic},
			current: "user-model", wantEffortHigh: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := agenttool.SkillActivation{Skill: "s", Model: tc.skillModel}
			if tc.wantEffortHigh {
				a.Effort = "2048"
			}
			active, during, effort, after := runSkillModel(t, tc.g, tc.current, a)
			if active != tc.wantActive {
				t.Errorf("active = %q, want %q", active, tc.wantActive)
			}
			if tc.wantDuring != "" && during != tc.wantDuring {
				t.Errorf("model during the scope = %q, want %q", during, tc.wantDuring)
			}
			if after != tc.current {
				t.Errorf("model after the scope = %q, want %q", after, tc.current)
			}
			if tc.wantEffortHigh && effort != "high" {
				t.Errorf("effort during the scope = %q, want high", effort)
			}
		})
	}
}

// The allowlist is read from the effective settings: an inline skill whose
// model is outside availableModels runs on the session model; a forked one
// falls back (AgentTool.ModelAllowed is wired to the same list).
func TestSkillModelAllowlistFromSettings(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, `{"availableModels":["sonnet"]}`)
	writeProjectSkill(t, cwd, "zz-gaps-opus", "---\ndescription: Needs opus\nmodel: opus\neffort: 64\n---\nThink.\n")
	caller := &skillTurnCaller{model: "user-model", effort: "medium", skill: "zz-gaps-opus"}
	s := &printSession{
		Model:     "user-model",
		Mode:      toolreg.ModeDefault,
		Provider:  providerAnthropic,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	if _, err := rt.conv.Submit(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(caller.seenM) != 2 || caller.seenM[1] != "user-model" {
		t.Errorf("models per round = %v, want the session model kept", caller.seenM)
	}
	if len(caller.seenE) != 2 || caller.seenE[1] != "high" {
		t.Errorf("efforts per round = %v, want the integer effort at high", caller.seenE)
	}
	allowed := rt.agentTool.ModelAllowed
	if allowed == nil || allowed("opus") || !allowed("sonnet") || !allowed("user-model") {
		t.Errorf("fork allowlist wiring wrong")
	}
}

// --- caller's frozen deny rules -------------------------------------------

// A skill tool snapshots the INVOKING agent's command-scoped deny rules: the
// main conversation's own inline-skill denies, and in a fork the fork's denies
// plus everything it inherited — never allow rules, never settings rules.
func TestSkillCallerDenyRules(t *testing.T) {
	main := &skillTurnCaller{model: "user-model", effort: "medium"}
	rt, _ := newSkillScopeSession(t, main)
	if got := rt.skillTool.CallerDenyRules(); len(got) != 0 {
		t.Fatalf("no active skill: caller denies = %v", got)
	}
	rt.dispatcher.ScopedRules().Push(compileSkillRules([]string{"Bash(git:*)"}, []string{"Write", "Bash(rm:*)"}))
	if got := rt.skillTool.CallerDenyRules(); !reflect.DeepEqual(got, []string{"Write", "Bash(rm:*)"}) {
		t.Fatalf("main caller denies = %v", got)
	}
	fd := rt.scopedChildDispatcher(rt.agentTool, []string{"*"}, agenttool.ChildRuleScope{
		Origin: "/deploy", AllowRules: []string{"Bash(make:*)"}, DenyRules: []string{"Bash(npm publish:*)"},
	}).(*toolrun.Dispatcher)
	got := fd.SkillTool.(*agenttool.SkillTool).CallerDenyRules()
	for _, want := range []string{"Write", "Bash(rm:*)", "Bash(npm publish:*)"} {
		if !slices.Contains(got, want) {
			t.Errorf("fork caller denies %v missing %q", got, want)
		}
	}
	for _, r := range got {
		if strings.Contains(r, "make") || strings.Contains(r, "git") {
			t.Errorf("an allow rule was frozen as a deny: %v", got)
		}
	}
}

// --- nesting depth ---------------------------------------------------------

// The Skill tool a subagent carries forks at the subagent's depth + 1: the
// chain main → fork (1) → its child (2) / its nested fork (2) → … carries the
// right depth at every level.
func TestSubagentSkillForkDepthChain(t *testing.T) {
	main := &skillTurnCaller{model: "user-model", effort: "medium"}
	rt, _ := newSkillScopeSession(t, main)
	if d := rt.skillTool.Agent.Depth; d != 0 {
		t.Fatalf("main skill tool depth = %d", d)
	}
	sc := agenttool.ChildRuleScope{Origin: "/x"}
	fork1 := rt.scopedChildDispatcher(rt.agentTool, []string{"*"}, sc).(*toolrun.Dispatcher)
	if d := fork1.SkillTool.(*agenttool.SkillTool).Agent.Depth; d != 1 {
		t.Fatalf("fork at depth 1 forks from depth %d", d)
	}
	fork2 := fork1.SkillTool.(*agenttool.SkillTool).Agent.NewScopedDispatcher([]string{"*"}, sc).(*toolrun.Dispatcher)
	if d := fork2.SkillTool.(*agenttool.SkillTool).Agent.Depth; d != 2 {
		t.Fatalf("nested fork forks from depth %d, want 2", d)
	}
	child := fork1.AgentTool.(*agenttool.AgentTool).NewDispatcher([]string{"*"}).(*toolrun.Dispatcher)
	if d := child.SkillTool.(*agenttool.SkillTool).Agent.Depth; d != 2 {
		t.Fatalf("fork's child forks from depth %d, want 2", d)
	}
	if d := child.AgentTool.(*agenttool.AgentTool).Depth; d != 2 {
		t.Fatalf("fork's child spawns from depth %d, want 2", d)
	}
}

// nestingCaller drives a skill that forks itself again at every level: each
// agent's first call invokes the skill; once it sees a tool result it records
// it and ends its turn.
type nestingCaller struct {
	mu      sync.Mutex
	skill   string
	results []string
}

func (c *nestingCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	raw, _ := json.Marshal(req.Messages[len(req.Messages)-1])
	if strings.Contains(string(raw), `"tool_result"`) {
		c.mu.Lock()
		c.results = append(c.results, string(raw))
		c.mu.Unlock()
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
	}
	in, _ := json.Marshal(map[string]string{"skill": c.skill})
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("tu_nest", "Skill", in)), StopReason: agent.StopToolUse}, nil
}

// Nested skill forks hit the depth cap: forks run at depths 1..cap and the
// fork at the cap is refused, with the Agent tool's message, when it forks
// again. GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH moves the cap.
func TestNestedSkillForksHitTheDepthLimit(t *testing.T) {
	for _, cap := range []int{3, 2} {
		t.Run(fmt.Sprintf("cap %d", cap), func(t *testing.T) {
			env, _, _, cwd := sessionEnv(t, "")
			if cap != 3 {
				get := env.Getenv
				env.Getenv = func(k string) string {
					if k == "GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH" {
						return fmt.Sprint(cap)
					}
					return get(k)
				}
			}
			writeProjectSkill(t, cwd, "zz-nest", "---\ndescription: Recurse\ncontext: fork\nbackground: false\n---\nInvoke the zz-nest skill again.\n")
			caller := &nestingCaller{skill: "zz-nest"}
			s := &printSession{
				Model:     "user-model",
				Mode:      toolreg.ModeDefault,
				Provider:  providerAnthropic,
				newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
			}
			rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
			defer rt.Close("other")
			if _, err := rt.conv.Submit(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			var depths []int
			for _, tk := range rt.tasks.List() {
				depths = append(depths, tk.Depth)
			}
			slices.Sort(depths)
			want := make([]int, cap)
			for i := range want {
				want[i] = i + 1
			}
			if !reflect.DeepEqual(depths, want) {
				t.Fatalf("fork depths = %v, want %v", depths, want)
			}
			msg := fmt.Sprintf("Subagent nesting limit reached (depth %d of %d). Complete this task directly using your tools instead of spawning another agent. If the user explicitly requested deeper nesting, ask them to raise GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH.", cap, cap)
			found := false
			caller.mu.Lock()
			for _, r := range caller.results {
				if strings.Contains(r, msg) {
					found = true
				}
			}
			caller.mu.Unlock()
			if !found {
				t.Fatalf("no tool result carried the nesting-limit refusal; results = %v", caller.results)
			}
		})
	}
}
