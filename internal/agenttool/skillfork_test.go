package agenttool

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// While a background fork of a skill is live, invoking the skill again does
// not start a second background fork: the second invocation runs in the
// foreground and returns its result (the fork runs synchronously). Once the
// first fork has finished, a new background fork
// starts normally. A different skill is unaffected.
func TestSkillForkLiveDuplicateRunsInForeground(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "bgjob", "---\ndescription: Long job\ncontext: fork\n---\nDo the long job.\n")
	addSkill(t, s, fsys, "other", "---\ndescription: Other job\ncontext: fork\n---\nDo the other job.\n")
	h := newHarness(nil)
	release := make(chan struct{})
	var mu sync.Mutex
	n := 0
	h.tool.NewModelCaller = func(string) agent.ModelCaller {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return &gatedModel{inner: &fakeModel{report: "first"}, gate: release}
		}
		return &fakeModel{report: "again"}
	}
	done := make(chan string, 4)
	h.tool.OnBackgroundDone = func(task *subagent.Task, result string) { done <- task.ID }
	s.Agent = h.tool

	out, err := skillCall(t, s, map[string]any{"skill": "bgjob"})
	if err != nil || !strings.Contains(out, "running in the background") {
		t.Fatalf("first launch: %q %v", out, err)
	}
	if tk, _ := h.tool.Tasks.Get("task_1"); tk == nil || tk.ForkedSkillName != "bgjob" {
		t.Fatalf("background fork task = %+v, want ForkedSkillName bgjob", tk)
	}

	// Second invocation while the first is live: foreground, completed result.
	out, err = skillCall(t, s, map[string]any{"skill": "bgjob"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Skill \"bgjob\" completed (forked execution).") || !strings.Contains(out, "again") {
		t.Fatalf("duplicate launch = %q, want the foreground completed result", out)
	}
	if tk, _ := h.tool.Tasks.Get("task_2"); tk == nil || tk.Background || tk.ForkedSkillName != "" {
		t.Fatalf("fallback task = %+v, want a foreground task not keyed as a background fork", tk)
	}

	// Another skill still launches in the background.
	out, err = skillCall(t, s, map[string]any{"skill": "other"})
	if err != nil || !strings.Contains(out, "running in the background") {
		t.Fatalf("other skill: %q %v", out, err)
	}

	// After the first fork ends, a new background fork of the skill starts.
	close(release)
	deadline := time.After(5 * time.Second)
	for got := 0; got < 2; {
		select {
		case <-done:
			got++
		case <-deadline:
			t.Fatal("background forks never finished")
		}
	}
	out, err = skillCall(t, s, map[string]any{"skill": "bgjob"})
	if err != nil || !strings.Contains(out, "running in the background") {
		t.Fatalf("relaunch after the first ended: %q %v", out, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relaunched fork never finished")
	}
}

// A fork whose scoping record would not validate is not launched in the
// background: it runs in the foreground instead. The record freezes the
// CALLER's command-scoped deny rules, so an
// over-long rule active on the invoking agent forces the foreground.
func TestSkillForkUnpersistableScopingRunsInForeground(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "bigdeny", "---\ndescription: Big deny\ncontext: fork\n---\nGo.\n")
	long := "Bash(" + strings.Repeat("x", 1025) + ")"
	s.CallerDenyRules = func() []string { return []string{long} }
	h := newHarness(nil)
	h.tool.OnBackgroundDone = func(*subagent.Task, string) { t.Error("an unpersistable fork launched in the background") }
	s.Agent = h.tool
	out, err := skillCall(t, s, map[string]any{"skill": "bigdeny"})
	if err != nil || !strings.HasPrefix(out, "Skill \"bigdeny\" completed (forked execution).") {
		t.Fatalf("out = %q %v", out, err)
	}
}

// The skill's OWN disallowed-tools are not part of the frozen record (they
// ride on the fork's dispatcher): an over-long one does not stop a background
// launch, and the caller's rules are snapshotted at invocation.
func TestSkillForkOwnDeniesDoNotBlockBackground(t *testing.T) {
	s, fsys := skillFixture(t)
	long := strings.Repeat("x", 1025)
	addSkill(t, s, fsys, "owndeny", "---\ndescription: Own deny\ncontext: fork\ndisallowed-tools: Bash("+long+")\n---\nGo.\n")
	calls := 0
	s.CallerDenyRules = func() []string { calls++; return []string{"Write", "Bash(rm:*)"} }
	h := newHarness(nil)
	done := make(chan struct{}, 1)
	h.tool.OnBackgroundDone = func(*subagent.Task, string) { done <- struct{}{} }
	s.Agent = h.tool
	out, err := skillCall(t, s, map[string]any{"skill": "owndeny"})
	if err != nil || !strings.Contains(out, "running in the background") {
		t.Fatalf("out = %q %v, want a background launch", out, err)
	}
	if calls != 1 {
		t.Fatalf("caller deny rules read %d times, want once at invocation", calls)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background fork never finished")
	}
}

func TestForkScopingPersistable(t *testing.T) {
	ok := func(fs forkedSkill) bool { return forkScopingPersistable(&fs) }
	if !ok(forkedSkill{Name: "deploy", Effort: "high", Scope: ChildRuleScope{DenyRules: []string{"Write"}}}) {
		t.Error("a plain fork must be persistable")
	}
	if !ok(forkedSkill{Name: "deploy", Effort: "1000"}) || ok(forkedSkill{Name: "deploy", Effort: "1001"}) || ok(forkedSkill{Name: "deploy", Effort: "turbo"}) {
		t.Error("effort: a named level or an integer 1-1000")
	}
	if ok(forkedSkill{Name: ""}) || ok(forkedSkill{Name: strings.Repeat("n", 257)}) || ok(forkedSkill{Name: "a\nb"}) {
		t.Error("skill name: 1-256 chars without CR/LF")
	}
	if ok(forkedSkill{Name: "d", FrozenDenies: make([]string, 1001)}) || ok(forkedSkill{Name: "d", FrozenDenies: []string{strings.Repeat("r", 1025)}}) {
		t.Error("more than 1000 frozen deny rules, or one over 1024 chars, must not be persistable")
	}
	if !ok(forkedSkill{Name: "d", FrozenDenies: make([]string, 1000)}) {
		t.Error("1000 frozen deny rules are persistable")
	}
	if !ok(forkedSkill{Name: "d", Scope: ChildRuleScope{DenyRules: make([]string, 1001)}}) {
		t.Error("the skill's own deny rules are not part of the frozen record")
	}
	if !forkScopingPersistable(nil) {
		t.Error("a plain Agent spawn carries no scoping")
	}
}
