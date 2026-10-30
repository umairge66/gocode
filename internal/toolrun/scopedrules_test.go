package toolrun

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/exec"
)

// fakeBash replaces the Bash adapter's run with a recorder so no shell runs;
// the permission side (rule content = the command) is the real adapter's.
func fakeBash(d *Dispatcher) *[]string {
	var mu sync.Mutex
	var ran []string
	real := BashAdapter(exec.BashTool{})
	real.run = func(_ context.Context, input json.RawMessage) (string, error) {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		mu.Lock()
		ran = append(ran, in.Command)
		mu.Unlock()
		return "ran", nil
	}
	d.Register(protocol.ToolBash, real)
	return &ran
}

// A skill's allowed-tools layered as a temporary scope auto-approves matching
// Bash commands for the scope only: a chained command is still asked about,
// the rule never reaches the shared store, and once the layer is removed the
// same command prompts again.
func TestScopedAllowRuleIsTemporaryAndNeverPersisted(t *testing.T) {
	store := NewSharedRules(nil)
	d := New(toolreg.ModeDefault, nil, WithSharedRules(store))
	p := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	d.SetPermissionPrompter(p)
	ran := fakeBash(d)
	ctx := context.Background()

	remove := d.ScopedRules().Push(toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "make:*", Source: "command"}})

	if res, _ := d.Dispatch(ctx, bashCall(t, "make build")); res.IsError {
		t.Fatalf("make build under the skill scope: %q", res.Content)
	}
	if p.count() != 0 {
		t.Fatalf("make build prompted %d times; the scoped allow rule should cover it", p.count())
	}
	if res, _ := d.Dispatch(ctx, bashCall(t, "make build; rm -rf x")); !res.IsError {
		t.Fatalf("chained command ran under Bash(make:*): %q", res.Content)
	}
	if p.count() != 1 {
		t.Fatalf("chained command should be asked about (prompts=%d)", p.count())
	}
	if len(*ran) != 1 || (*ran)[0] != "make build" {
		t.Fatalf("ran = %v", *ran)
	}
	// The temporary rule is never in the shared (persistable) rules.
	if r := store.Snapshot().Match(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "make build"}, toolreg.RuleAllow); r != nil {
		t.Fatalf("scoped rule leaked into the shared store: %+v", r)
	}
	if len(d.PersistentRules()) != 0 {
		t.Fatalf("PersistentRules = %v", d.PersistentRules())
	}

	// Scope ends: the rule is gone and the next call prompts again.
	remove()
	remove() // idempotent
	if res, _ := d.Dispatch(ctx, bashCall(t, "make build")); !res.IsError {
		t.Fatalf("make build ran after the scope ended: %q", res.Content)
	}
	if p.count() != 2 {
		t.Fatalf("after the scope ended make build should prompt (prompts=%d)", p.count())
	}
	if d.ScopedRules().Len() != 0 {
		t.Fatalf("layers left: %d", d.ScopedRules().Len())
	}
}

// A scoped deny rule (disallowed-tools) blocks the tool during the scope only,
// wins over an allow rule, and a subagent dispatcher inheriting the parent's
// layers sees the deny but not the allow.
func TestScopedDenyRuleAndSubagentInheritance(t *testing.T) {
	store := NewSharedRules(toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "make:*"}})
	parent := New(toolreg.ModeBypassPermissions, nil, WithSharedRules(store))
	fakeBash(parent)
	ctx := context.Background()

	remove := parent.ScopedRules().Push(toolreg.RuleSet{
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "make:*"},
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "ls:*"},
	})
	if res, _ := parent.Dispatch(ctx, bashCall(t, "make build")); !res.IsError {
		t.Fatalf("disallowed tool ran during the scope (even in bypass): %q", res.Content)
	}

	child := New(toolreg.ModeDefault, nil, WithSharedRules(store), WithInheritedScopedRules(parent.ScopedRules()))
	call := func(cmd string) toolreg.Call {
		return toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}
	}
	if child.SnapshotRules().Match(call("make build"), toolreg.RuleDeny) == nil {
		t.Fatal("subagent should inherit the skill's deny rule")
	}
	if child.SnapshotRules().Match(call("ls -la"), toolreg.RuleAllow) != nil {
		t.Fatal("subagent must not inherit the skill's allow rule")
	}

	remove()
	if res, _ := parent.Dispatch(ctx, bashCall(t, "make build")); res.IsError {
		t.Fatalf("tool still blocked after the scope ended: %q", res.Content)
	}
	if child.SnapshotRules().Match(call("make build"), toolreg.RuleDeny) != nil {
		t.Fatal("inherited deny outlived the scope")
	}
}

// A forked skill's scope passes to the agents the fork spawns — and theirs —
// unchanged (allow AND deny), while a parent's own layers pass deny-only and a
// deny-only view is never upgraded: a descendant never gets an allow rule its
// ancestor fork does not have, and deny rules keep winning.
func TestScopesForChildNeverWidens(t *testing.T) {
	store := NewSharedRules(nil)
	call := func(cmd string) toolreg.Call {
		return toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}
	}
	main := New(toolreg.ModeDefault, nil, WithSharedRules(store))
	main.ScopedRules().Push(toolreg.RuleSet{
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "git:*"},
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "curl:*"},
	})

	// The fork: the skill allows make and npm, but denies npm publish.
	fork := NewScopedRules()
	fork.Push(toolreg.RuleSet{
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "make:*"},
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "npm:*"},
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "npm publish:*"},
	})
	forkD := New(toolreg.ModeDefault, nil, WithSharedRules(store),
		WithInheritedScopes(append(main.ScopesForChild(), InheritedScope{Rules: fork})...))
	// An inline skill the fork itself invokes: its allow stays with the fork.
	forkD.ScopedRules().Push(toolreg.RuleSet{
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "go:*"},
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "rm:*"},
	})

	child := New(toolreg.ModeDefault, nil, WithSharedRules(store), WithInheritedScopes(forkD.ScopesForChild()...))
	grandchild := New(toolreg.ModeDefault, nil, WithSharedRules(store), WithInheritedScopes(child.ScopesForChild()...))

	decide := func(d *Dispatcher, cmd string) toolreg.Behavior {
		pc := call(cmd)
		pc.Rule = "Bash"
		return d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), nil).Behavior
	}
	for name, d := range map[string]*Dispatcher{"child": child, "grandchild": grandchild} {
		if got := decide(d, "make build"); got != toolreg.BehaviorAllow {
			t.Errorf("%s: make build = %v, want allow (fork's allow inherited)", name, got)
		}
		if got := decide(d, "npm publish"); got != toolreg.BehaviorDeny {
			t.Errorf("%s: npm publish = %v, want deny (deny wins over the fork's npm:* allow)", name, got)
		}
		if got := decide(d, "curl x"); got != toolreg.BehaviorDeny {
			t.Errorf("%s: curl = %v, want deny (main inline deny)", name, got)
		}
		if got := decide(d, "rm -rf x"); got != toolreg.BehaviorDeny {
			t.Errorf("%s: rm = %v, want deny (fork's inline deny)", name, got)
		}
		// Never wider than the fork: main's inline allow and the fork's own
		// inline allow do not reach descendants.
		for _, cmd := range []string{"git push", "go test"} {
			if d.SnapshotRules().Match(call(cmd), toolreg.RuleAllow) != nil {
				t.Errorf("%s: inherited an allow rule for %q", name, cmd)
			}
		}
		// Every allow rule a descendant holds is one the fork holds.
		for _, r := range d.SnapshotRules() {
			if r.Behavior == toolreg.RuleAllow && forkD.SnapshotRules().Match(call(strings.TrimSuffix(r.Content, ":*")+" x"), toolreg.RuleAllow) == nil {
				t.Errorf("%s: allow rule %+v not held by the fork", name, r)
			}
		}
	}
	// The fork itself reads all of its own scopes in full.
	if got := decide(forkD, "go test"); got != toolreg.BehaviorAllow {
		t.Errorf("fork: go test = %v, want allow", got)
	}
}
