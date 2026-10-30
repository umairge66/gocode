package agenttool

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/skills"
	"github.com/umairge66/gocode/internal/subagent"
)

// addSkill writes a personal skill and (re)discovers the personal scope.
func addSkill(t *testing.T, s *SkillTool, fsys *fsfacade.MemFS, name, md string) {
	t.Helper()
	root := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(root+"/"+name+"/SKILL.md", []byte(md))
	s.Registry.AddAll(skills.DiscoverScope(fsys, root, skills.ScopePersonal, ""))
}

// recordingScope is a fake SkillScope.
type recordingScope struct {
	mu   sync.Mutex
	acts []SkillActivation
}

func (r *recordingScope) ActivateInline(_ context.Context, a SkillActivation) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, a)
	if a.Model != "" {
		return "resolved-" + a.Model, nil
	}
	return "", nil
}

// recordingHooks is a fake SkillHooksRegistrar.
type recordingHooks struct {
	skill, root string
	raw         map[string]any
}

func (r *recordingHooks) AddSkillHooks(skill, root string, raw map[string]any, _ bool) (int, error) {
	r.skill, r.root, r.raw = skill, root, raw
	return 1, nil
}

// A forked skill passes its model, effort and allowed/disallowed tools to the
// subagent spawn; `background: false` keeps the caller waiting.
func TestSkillForkPassesModelEffortAndScope(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "build", "---\ndescription: Build\ncontext: fork\nbackground: false\nmodel: opus\neffort: high\nallowed-tools: Bash(make:*)\ndisallowed-tools: Write, Bash(rm:*)\n---\nBuild it.\n")
	h := newHarness(nil)
	em := &effortModel{fakeModel: &fakeModel{report: "built"}}
	h.tool.NewModelCaller = func(m string) agent.ModelCaller { h.models = append(h.models, m); return em }
	var scopes []ChildRuleScope
	var scopedAllowed [][]string
	h.tool.NewScopedDispatcher = func(allowed []string, sc ChildRuleScope) agent.ToolDispatcher {
		scopes = append(scopes, sc)
		scopedAllowed = append(scopedAllowed, allowed)
		return h.dispatcher
	}
	// A background notifier exists, but background:false runs in the foreground.
	h.tool.OnBackgroundDone = func(*subagent.Task, string) { t.Error("background:false must not launch in the background") }
	s.Agent = h.tool

	inv, err := s.Invoke(context.Background(), "build", "")
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Forked || inv.Background || !strings.Contains(inv.Result, "built") {
		t.Fatalf("inv = %+v", inv)
	}
	if len(h.models) != 1 || h.models[0] != "opus" {
		t.Fatalf("subagent model = %v, want [opus]", h.models)
	}
	if !reflect.DeepEqual(em.set, []string{"high"}) {
		t.Fatalf("subagent effort = %v, want [high]", em.set)
	}
	if len(scopes) != 1 {
		t.Fatalf("scoped dispatcher calls = %d", len(scopes))
	}
	if !reflect.DeepEqual(scopes[0].AllowRules, []string{"Bash(make:*)"}) || !reflect.DeepEqual(scopes[0].DenyRules, []string{"Write", "Bash(rm:*)"}) || scopes[0].Origin != "/build" {
		t.Fatalf("scope = %+v", scopes[0])
	}
	for _, n := range scopedAllowed[0] {
		if n == "Write" {
			t.Fatalf("disallowed Write still in the fork's tools: %v", scopedAllowed[0])
		}
	}
	if !strings.Contains(strings.Join(scopedAllowed[0], ","), "Bash") {
		t.Fatalf("Bash(rm:*) must not remove Bash from the tools: %v", scopedAllowed[0])
	}
	task, _ := h.tool.Tasks.Get("task_1")
	if task.Description != "/build" {
		t.Fatalf("task description = %q", task.Description)
	}
}

// `model: inherit` on a forked skill leaves the model resolution unchanged.
func TestSkillForkModelInherit(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "inh", "---\ndescription: Inherit\ncontext: fork\nbackground: false\nmodel: inherit\n---\nGo.\n")
	h := newHarness(nil)
	s.Agent = h.tool
	if _, err := s.Invoke(context.Background(), "inh", ""); err != nil {
		t.Fatal(err)
	}
	if len(h.models) != 1 || h.models[0] != "parent-model" {
		t.Fatalf("models = %v, want [parent-model]", h.models)
	}
}

// A forked skill without `background` runs as a background agent (the
// default): the call returns at once and the outcome arrives later as a task
// notification.
func TestSkillForkBackgroundDefault(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "bgjob", "---\ndescription: Long job\ncontext: fork\n---\nDo the long job.\n")
	h := newHarness(nil)
	release := make(chan struct{})
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return &gatedModel{inner: h.model, gate: release} }
	done := make(chan string, 1)
	h.tool.OnBackgroundDone = func(task *subagent.Task, result string) { done <- task.ID + "|" + result }
	s.Agent = h.tool

	out, err := skillCall(t, s, map[string]any{"skill": "bgjob"})
	if err != nil {
		t.Fatal(err)
	}
	want := "Skill \"bgjob\" launched (forked execution, running in the background).\n\nRunning in the background as @task_1"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}
	select {
	case r := <-done:
		t.Fatalf("notified before the subagent finished: %q", r)
	default:
	}
	close(release)
	select {
	case r := <-done:
		if !strings.HasPrefix(r, "task_1|") || !strings.Contains(r, "the report") {
			t.Fatalf("notification = %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background skill never reported")
	}

	// With no background path (-p), the same skill runs in the foreground.
	s2, fsys2 := skillFixture(t)
	addSkill(t, s2, fsys2, "bgjob", "---\ndescription: Long job\ncontext: fork\n---\nDo the long job.\n")
	h2 := newHarness(nil)
	s2.Agent = h2.tool
	out, err = skillCall(t, s2, map[string]any{"skill": "bgjob"})
	if err != nil || !strings.HasPrefix(out, "Skill \"bgjob\" completed (forked execution).") {
		t.Fatalf("foreground fallback: %q %v", out, err)
	}
}

// gatedModel blocks every call after the first until gate closes, so the
// background launch is observed before the subagent finishes.
type gatedModel struct {
	inner agent.ModelCaller
	gate  chan struct{}
	n     int
	mu    sync.Mutex
}

func (g *gatedModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return agent.ModelResponse{}, ctx.Err()
	}
	return g.inner.CallModel(ctx, req)
}

// An inline skill activates its allowed/disallowed tools, model and effort
// through the Scope; `model: inherit` is passed as no model change; a skill
// with no overrides activates nothing; frontmatter hooks are registered.
func TestSkillInlineActivatesScopeAndHooks(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "fast", "---\ndescription: Fast\nallowed-tools: Bash(make:*)\ndisallowed-tools: Write\nmodel: haiku\neffort: low\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: echo ${GOCODE_SKILL_DIR}\n---\nBe fast.\n")
	addSkill(t, s, fsys, "inherits", "---\ndescription: Inherits\nmodel: inherit\neffort: high\n---\nKeep model.\n")
	sc := &recordingScope{}
	hk := &recordingHooks{}
	s.Scope, s.Hooks = sc, hk

	inv, err := s.Invoke(context.Background(), "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Forked || inv.ActiveModel != "resolved-haiku" || !strings.Contains(inv.Content, "Be fast.") {
		t.Fatalf("inv = %+v", inv)
	}
	want := SkillActivation{Skill: "fast", AllowedTools: []string{"Bash(make:*)"}, DisallowedTools: []string{"Write"}, Model: "haiku", Effort: "low"}
	if len(sc.acts) != 1 || !reflect.DeepEqual(sc.acts[0], want) {
		t.Fatalf("activation = %+v\nwant %+v", sc.acts, want)
	}
	if hk.skill != "fast" || hk.root != "/home/u/.config/gocode/skills/fast" || hk.raw["PreToolUse"] == nil {
		t.Fatalf("hooks registration = %+v", hk)
	}

	if _, err := s.Invoke(context.Background(), "inherits", ""); err != nil {
		t.Fatal(err)
	}
	if got := sc.acts[1]; got.Model != "" || got.Effort != "high" {
		t.Fatalf("inherit activation = %+v (model must be empty)", got)
	}

	if _, err := s.Invoke(context.Background(), "notes", ""); err != nil {
		t.Fatal(err)
	}
	if len(sc.acts) != 2 {
		t.Fatalf("a skill without overrides activated a scope: %+v", sc.acts)
	}
}
