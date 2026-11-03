package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/skills"
	"github.com/umairge66/gocode/internal/subagent"
)

// fakeModel replies with a SubagentHandback call carrying report, then ends.
type fakeModel struct {
	mu      sync.Mutex
	report  string
	calls   int
	systems []string
	tools   [][]string
	extra   *agent.ToolUse // optional tool call before the hand-back
}

func (f *fakeModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.systems = append(f.systems, req.System)
	var names []string
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	f.tools = append(f.tools, names)
	switch {
	case f.extra != nil && f.calls == 1:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock(f.extra.ID, f.extra.Name, f.extra.Input)), StopReason: "tool_use"}, nil
	case f.calls == 1 || (f.extra != nil && f.calls == 2):
		in, _ := json.Marshal(map[string]string{"message": f.report})
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("hb", "SubagentHandback", in)), StopReason: "tool_use"}, nil
	default:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: "end_turn"}, nil
	}
}

type fakeDispatcher struct{ calls []string }

func (d *fakeDispatcher) Dispatch(_ context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	d.calls = append(d.calls, call.Name)
	return agent.ToolResult{Content: "ok"}, nil
}

type harness struct {
	tool       *AgentTool
	model      *fakeModel
	models     []string
	allowed    [][]string
	dispatcher *fakeDispatcher
}

func newHarness(defs *agentsdef.Registry) *harness {
	h := &harness{model: &fakeModel{report: "the report"}, dispatcher: &fakeDispatcher{}}
	h.tool = NewAgentTool(subagent.NewTaskRegistry(), defs,
		func(model string) agent.ModelCaller { h.models = append(h.models, model); return h.model },
		func(allowed []string) agent.ToolDispatcher {
			h.allowed = append(h.allowed, allowed)
			return h.dispatcher
		},
	)
	h.tool.ToolNames = []string{"Read", "Write", "Edit", "Bash", "Agent", "Skill"}
	h.tool.ToolSchemas = func(allowed []string) []agent.ToolSchema {
		var out []agent.ToolSchema
		for _, n := range allowed {
			out = append(out, agent.ToolSchema{Name: n})
		}
		return out
	}
	h.tool.ParentModel = "parent-model"
	return h
}

func customDefs(t *testing.T) *agentsdef.Registry {
	t.Helper()
	md := "---\nname: reviewer\ndescription: Reviews code for bugs\ntools: Read, Bash\nmodel: haiku\n---\nYou are a meticulous code reviewer.\n"
	pr := agentsdef.ParseDefinition([]byte(md), "/p/.gocode/agents/reviewer.md")
	d, msg := agentsdef.BuildFileDescriptor(pr, "/p/.gocode/agents/reviewer.md", "/p/.gocode/agents", agentsdef.SourceProject)
	if msg != "" {
		t.Fatalf("build descriptor: %s", msg)
	}
	reg := agentsdef.NewRegistry()
	reg.Add(d)
	return reg
}

func call(t *testing.T, tool *AgentTool, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return tool.Execute(context.Background(), raw)
}

func TestAgentBuiltinGeneralPurpose(t *testing.T) {
	h := newHarness(nil)
	out, err := call(t, h.tool, map[string]any{"description": "find it", "prompt": "Find the bug"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[Subagent hand-back]") || !strings.Contains(out, "  the report") {
		t.Fatalf("missing framed report: %q", out)
	}
	if !strings.Contains(out, "agentId: task_1") || !strings.Contains(out, "<usage>tool_uses: 0") {
		t.Fatalf("missing agentId/usage trailer: %q", out)
	}
	if h.models[0] != "parent-model" {
		t.Fatalf("model = %q, want inherited parent-model", h.models[0])
	}
	// general-purpose gets every tool; Agent is kept (depth 1 < cap 3).
	if got := strings.Join(h.allowed[0], ","); got != "Read,Write,Edit,Bash,Agent,Skill" {
		t.Fatalf("allowed = %s", got)
	}
	if !strings.Contains(h.model.systems[0], "SubagentHandback({message: <your full report>})") {
		t.Fatalf("system prompt lacks hand-back contract: %q", h.model.systems[0])
	}
	task, ok := h.tool.Tasks.Get("task_1")
	if !ok || task.Status != subagent.StatusCompleted || task.AgentType != "general-purpose" || task.Depth != 1 {
		t.Fatalf("task = %+v", task)
	}
}

func TestAgentExploreRestrictedNoTrailer(t *testing.T) {
	h := newHarness(nil)
	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "Explore"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "agentId:") {
		t.Fatalf("Explore result should omit the trailer: %q", out)
	}
	if got := strings.Join(h.allowed[0], ","); got != "Read,Bash,Skill" {
		t.Fatalf("Explore allowed = %s", got)
	}
}

func TestAgentCustomType(t *testing.T) {
	h := newHarness(customDefs(t))
	h.model.extra = &agent.ToolUse{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)}
	out, err := call(t, h.tool, map[string]any{"description": "review", "prompt": "Review x.go", "subagent_type": "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.allowed[0], ","); got != "Read,Bash" {
		t.Fatalf("custom allowed = %s", got)
	}
	if h.models[0] != "haiku" {
		t.Fatalf("custom model = %q", h.models[0])
	}
	if !strings.HasPrefix(h.model.systems[0], "You are a meticulous code reviewer.") {
		t.Fatalf("custom system prompt not used: %q", h.model.systems[0])
	}
	if got := strings.Join(h.model.tools[0], ","); got != "Read,Bash,SubagentHandback" {
		t.Fatalf("advertised tools = %s", got)
	}
	if len(h.dispatcher.calls) != 1 || h.dispatcher.calls[0] != "Read" {
		t.Fatalf("dispatcher calls = %v", h.dispatcher.calls)
	}
	if !strings.Contains(out, "tool_uses: 1") || !strings.Contains(out, "  the report") {
		t.Fatalf("out = %q", out)
	}
	// Explicit model override wins.
	if _, err := call(t, h.tool, map[string]any{"description": "r", "prompt": "p", "subagent_type": "reviewer", "model": "opus"}); err != nil {
		t.Fatal(err)
	}
	if h.models[1] != "opus" {
		t.Fatalf("override model = %q", h.models[1])
	}
}

// TestAgentSystemPromptAssembly checks the Agent-tool path assembles the full
// child system prompt: persona (the default for a built-in), a tool
// section reflecting the allowed tools, the injected MemoryFor seam, and the
// hand-back contract terminating the prompt. A nil seam omits memory.
func TestAgentSystemPromptAssembly(t *testing.T) {
	h := newHarness(nil)
	h.tool.MemoryFor = func(a subagent.AgentType) string {
		return "Persistent memory for " + a.Name + ": prefer tabs."
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatal(err)
	}
	sys := h.model.systems[0]
	if !strings.HasPrefix(sys, "You are the general-purpose subagent") {
		t.Fatalf("default general-purpose persona missing:\n%s", sys)
	}
	if !strings.Contains(sys, "## Tools") || !strings.Contains(sys, "Read, Write, Edit, Bash, Agent, Skill") {
		t.Fatalf("tool guidance does not reflect allowed tools:\n%s", sys)
	}
	if !strings.Contains(sys, "Persistent memory for general-purpose: prefer tabs.") {
		t.Fatalf("injected MemoryFor seam missing:\n%s", sys)
	}
	if !strings.HasSuffix(sys, "plain text you write at the end is not delivered.") {
		t.Fatalf("prompt does not end with the hand-back contract:\n%s", sys)
	}

	// Nil MemoryFor: no memory section, no crash, contract still terminates.
	h2 := newHarness(nil)
	if _, err := call(t, h2.tool, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h2.model.systems[0], "Persistent memory") {
		t.Fatalf("memory should be absent when MemoryFor is nil:\n%s", h2.model.systems[0])
	}
	if !strings.HasSuffix(h2.model.systems[0], "plain text you write at the end is not delivered.") {
		t.Fatalf("nil-memory prompt must still end with the contract:\n%s", h2.model.systems[0])
	}
}

func TestAgentUnknownType(t *testing.T) {
	h := newHarness(customDefs(t))
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "nope"})
	if !errors.Is(err, ErrUnknownAgentType) {
		t.Fatalf("err = %v", err)
	}
	want := "Agent type 'nope' not found. Available agents: general-purpose, Explore, Plan, reviewer"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "fork"}); err == nil {
		t.Fatal("fork should be refused")
	}
}

func TestAgentDepthCap(t *testing.T) {
	h := newHarness(nil)
	child := h.tool.ForChild(3, "task_9", "general-purpose")
	_, err := call(t, child, map[string]any{"description": "d", "prompt": "p"})
	if !errors.Is(err, ErrNestingLimit) || !strings.Contains(err.Error(), "Subagent nesting limit reached (depth 3 of 3)") {
		t.Fatalf("err = %v", err)
	}
	// At depth 2 the spawn succeeds but the grandchild loses the Agent tool.
	child2 := h.tool.ForChild(2, "task_8", "general-purpose")
	if _, err := call(t, child2, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range h.allowed[0] {
		if n == "Agent" {
			t.Fatalf("Agent should be dropped at the depth cap: %v", h.allowed[0])
		}
	}
	task, _ := h.tool.Tasks.Get("task_1")
	if task.Depth != 3 || task.ParentID != "task_8" {
		t.Fatalf("task = %+v", task)
	}
}

func TestAgentCallerForbidsAgent(t *testing.T) {
	h := newHarness(nil)
	child := h.tool.ForChild(1, "task_1", "Explore")
	if _, err := call(t, child, map[string]any{"description": "d", "prompt": "p"}); err == nil || !strings.Contains(err.Error(), "cannot launch other agents") {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentNoReportDelivered(t *testing.T) {
	h := newHarness(nil)
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return endingModel{} }
	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"})
	if err != nil {
		t.Fatal(err)
	}
	// The agent ended without a hand-back but produced a final message ("bye");
	// we now fall back to it (framed) so the caller still gets the agent's work.
	if !strings.Contains(out, "did not call SubagentHandback") || !strings.Contains(out, "bye") {
		t.Fatalf("out = %q", out)
	}
}

type endingModel struct{}

func (endingModel) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("bye")), StopReason: "end_turn"}, nil
}

func TestAgentBackground(t *testing.T) {
	h := newHarness(nil)
	done := make(chan string, 1)
	h.tool.OnBackgroundDone = func(task *subagent.Task, result string) { done <- task.ID + "|" + result }
	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "bg work"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Async agent launched successfully") || !strings.Contains(out, "agentId: task_1 ") {
		t.Fatalf("out = %q", out)
	}
	select {
	case r := <-done:
		if !strings.HasPrefix(r, "task_1|") || !strings.Contains(r, "the report") {
			t.Fatalf("notification = %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background agent never completed")
	}
	// Without a notifier, run_in_background runs in the foreground.
	h2 := newHarness(nil)
	out, err = call(t, h2.tool, map[string]any{"description": "d", "prompt": "p", "run_in_background": true})
	if err != nil || !strings.Contains(out, "the report") {
		t.Fatalf("foreground fallback: %q %v", out, err)
	}
}

// blockingResumeModel blocks its first turn until cancelled (so a Stop can
// interrupt a background spawn), then hands back on resume and ends.
type blockingResumeModel struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	once    sync.Once
}

func (m *blockingResumeModel) CallModel(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	switch n {
	case 1:
		m.once.Do(func() { close(m.entered) })
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	case 2:
		in, _ := json.Marshal(map[string]string{"message": "resumed report"})
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("hb", "SubagentHandback", in)), StopReason: "tool_use"}, nil
	default:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: "end_turn"}, nil
	}
}

// TestAgentStopResumeClose exercises the host-facing interactive controls on
// *AgentTool: a background spawn is Stopped (→ resumable), Resumed through the
// same conversation (firing OnSubagentUpdate), discarded on completion, and
// Close/Resume on absent sessions error.
func TestAgentStopResumeClose(t *testing.T) {
	h := newHarness(nil)
	bm := &blockingResumeModel{entered: make(chan struct{})}
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return bm }
	bgDone := make(chan *subagent.Task, 4)
	h.tool.OnBackgroundDone = func(task *subagent.Task, _ string) { bgDone <- task }
	upd := make(chan string, 4)
	h.tool.OnSubagentUpdate = func(_ *subagent.Task, result string) { upd <- result }

	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "bg work"})
	if err != nil || !strings.Contains(out, "Async agent launched") {
		t.Fatalf("background launch: out=%q err=%v", out, err)
	}
	select {
	case <-bm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never started")
	}
	ids := h.tool.Sessions().ActiveIDs()
	if len(ids) != 1 {
		t.Fatalf("active sessions = %v, want one", ids)
	}
	id := ids[0]

	if err := h.tool.StopSubagent(id); err != nil {
		t.Fatalf("StopSubagent: %v", err)
	}
	select {
	case tk := <-bgDone:
		if tk.Status != subagent.StatusStopped {
			t.Fatalf("background task status after Stop = %q, want stopped", tk.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped turn never settled")
	}
	if !h.tool.Sessions().Active(id) {
		t.Fatal("stopped subagent should remain resumable")
	}

	if err := h.tool.ResumeSubagent(context.Background(), id, "finish up"); err != nil {
		t.Fatalf("ResumeSubagent: %v", err)
	}
	select {
	case r := <-upd:
		if !strings.Contains(r, "resumed report") {
			t.Fatalf("OnSubagentUpdate result = %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume notifier never fired")
	}
	if h.tool.Sessions().Active(id) {
		t.Fatal("session should be discarded after resume completion")
	}
	if err := h.tool.ResumeSubagent(context.Background(), id, "again"); err == nil {
		t.Fatal("Resume after completion should error")
	}
	if err := h.tool.CloseSubagent("ghost"); err == nil {
		t.Fatal("Close on unknown id should error")
	}
}

func TestAgentDescriptionListsTypes(t *testing.T) {
	h := newHarness(customDefs(t))
	desc := h.tool.Description()
	for _, want := range []string{
		"Launch a new agent to handle complex, multi-step tasks.",
		"Available agent types for the Agent tool:",
		"- general-purpose: General-purpose agent",
		"- Explore: Read-only search agent",
		"(Tools: All tools except Agent, Artifact",
		"- reviewer: Reviews code for bugs (Tools: Read, Bash)",
		"If omitted, the general-purpose agent is used.",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q", want)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(h.tool.InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	for _, k := range []string{"description", "prompt", "subagent_type", "model", "run_in_background"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing %s", k)
		}
	}
	h.tool.ListingInReminder = true
	if !strings.Contains(h.tool.Description(), "Available agent types are listed in <system-reminder>") {
		t.Error("reminder variant missing mTr note")
	}
}

// ---- Skill tool ----

func skillFixture(t *testing.T) (*SkillTool, *fsfacade.MemFS) {
	t.Helper()
	fsys := fsfacade.NewMemFS()
	root := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(root+"/deploy/SKILL.md", []byte("---\ndescription: Deploy the app\n---\nRun ${GOCODE_SKILL_DIR}/deploy.sh $0 to $1.\nAll args: $ARGUMENTS\n"))
	fsys.WriteFile(root+"/notes/SKILL.md", []byte("---\ndescription: Take notes\n---\nWrite notes in ${CLAUDE_SKILL_DIR}.\n"))
	fsys.WriteFile(root+"/secret/SKILL.md", []byte("---\ndescription: User only\ndisable-model-invocation: true\n---\nDo the secret thing.\n"))
	reg := skills.NewRegistry()
	reg.AddAll(skills.DiscoverScope(fsys, root, skills.ScopePersonal, ""))
	return NewSkillTool(reg, fsys), fsys
}

func skillCall(t *testing.T, s *SkillTool, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return s.Execute(context.Background(), raw)
}

func TestSkillLoadsAndSubstitutes(t *testing.T) {
	s, _ := skillFixture(t)
	out, err := skillCall(t, s, map[string]any{"skill": "/deploy", "args": `prod "us east"`})
	if err != nil {
		t.Fatal(err)
	}
	dir := "/home/u/.config/gocode/skills/deploy"
	want := "Launching skill: deploy\n\nBase directory for this skill: " + dir + "\n\nRun " + dir + `/deploy.sh prod to us east.` + "\nAll args: prod \"us east\"\n"
	if out != want {
		t.Fatalf("out =\n%q\nwant\n%q", out, want)
	}
}

func TestSkillAppendsArgumentsWhenNoPlaceholder(t *testing.T) {
	s, _ := skillFixture(t)
	inv, err := s.Invoke(context.Background(), "notes", "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inv.Content, "Write notes in /home/u/.config/gocode/skills/notes.") {
		t.Fatalf("CLAUDE_SKILL_DIR alias not substituted: %q", inv.Content)
	}
	if !strings.HasSuffix(inv.Content, "\n\nARGUMENTS: hello world") {
		t.Fatalf("missing ARGUMENTS append: %q", inv.Content)
	}
	if inv.ToolResult() != "Launching skill: notes" {
		t.Fatalf("tool result = %q", inv.ToolResult())
	}
}

func TestSkillUnknown(t *testing.T) {
	s, _ := skillFixture(t)
	_, err := skillCall(t, s, map[string]any{"skill": "deplyo"})
	if !errors.Is(err, ErrUnknownSkill) || err.Error() != "Unknown skill: deplyo. Did you mean deploy?" {
		t.Fatalf("err = %v", err)
	}
	_, err = skillCall(t, s, map[string]any{"skill": "zzzzzzzz"})
	if err == nil || err.Error() != "Unknown skill: zzzzzzzz" {
		t.Fatalf("err = %v", err)
	}
	if _, err := skillCall(t, s, map[string]any{"skill": "  "}); err == nil || !strings.HasPrefix(err.Error(), "Invalid skill format") {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillDisableModelInvocation(t *testing.T) {
	s, _ := skillFixture(t)
	_, err := skillCall(t, s, map[string]any{"skill": "secret"})
	if !errors.Is(err, ErrModelInvocationDisabled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "Skill secret cannot be used with Skill tool due to disable-model-invocation. Ask the user to run /secret themselves") {
		t.Fatalf("err = %q", err)
	}
}

// TestSkillInvokeAsUser: the person may run a disable-model-invocation skill,
// but not a user-invocable: false one.
func TestSkillInvokeAsUser(t *testing.T) {
	s, fsys := skillFixture(t)
	inv, err := s.InvokeAsUser(context.Background(), "secret", "")
	if err != nil || !strings.Contains(inv.Content, "Do the secret thing.") || inv.Forked {
		t.Fatalf("InvokeAsUser(secret) = %+v, %v", inv, err)
	}
	root := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(root+"/auto/SKILL.md", []byte("---\ndescription: Model only\nuser-invocable: false\n---\nx\n"))
	s.Registry.AddAll(skills.DiscoverScope(fsys, root, skills.ScopePersonal, ""))
	if _, err := s.InvokeAsUser(context.Background(), "auto", ""); !errors.Is(err, ErrNotUserInvocable) {
		t.Fatalf("err = %v, want ErrNotUserInvocable", err)
	}
	if _, err := s.Invoke(context.Background(), "auto", ""); err != nil {
		t.Fatalf("the model may still invoke it: %v", err)
	}
}

func TestSkillDescriptionListsSkills(t *testing.T) {
	s, _ := skillFixture(t)
	desc := s.Description()
	if !strings.HasPrefix(desc, "Invoke a skill.") {
		t.Fatalf("desc = %q", desc)
	}
	for _, want := range []string{"The following skills are available for use with the Skill tool:", "- deploy: Deploy the app (user)", "- notes: Take notes (user)"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q", want)
		}
	}
	if strings.Contains(desc, "secret") {
		t.Error("disable-model-invocation skill must not be listed")
	}
	var schema map[string]any
	_ = json.Unmarshal(s.InputSchema(), &schema)
	if req := schema["required"].([]any); len(req) != 1 || req[0] != "skill" {
		t.Fatalf("required = %v", req)
	}
}

func TestSkillForkRunsSubagent(t *testing.T) {
	s, fsys := skillFixture(t)
	root := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(root+"/audit/SKILL.md", []byte("---\ndescription: Audit\ncontext: fork\nagent: Explore\n---\nAudit everything.\n"))
	s.Registry.AddAll(skills.DiscoverScope(fsys, root, skills.ScopePersonal, ""))
	h := newHarness(nil)
	s.Agent = h.tool
	out, err := skillCall(t, s, map[string]any{"skill": "audit"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Skill \"audit\" completed (forked execution).\n\nResult:\n") || !strings.Contains(out, "the report") {
		t.Fatalf("out = %q", out)
	}
	task, _ := h.tool.Tasks.Get("task_1")
	if task.AgentType != "Explore" || !strings.Contains(task.Prompt, "Audit everything.") {
		t.Fatalf("task = %+v", task)
	}
}

func TestSubstituteArgumentsEscapes(t *testing.T) {
	got, err := SubstituteArguments(`cost \$1 then $1 and $ARGUMENTS[5] and $2x`, "a b", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cost $1 then b and $ARGUMENTS[5] and $2x"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Filled values are not re-expanded and are shell-mark escaped.
	got, _ = SubstituteArguments("x $ARGUMENTS", "$1 !`rm`", true, escapeShellMarks)
	if want := "x $1 \\! `rm`"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
