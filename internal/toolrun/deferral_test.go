package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// dispatchDeferrable dispatches call through d as an agent loop with
// AllowDefer settles a deferred round, and returns d's own result.
func dispatchDeferrable(t *testing.T, d *Dispatcher, call agent.ToolUse) agent.ToolResult {
	t.Helper()
	rec := &recordResult{inner: d}
	l := &agent.Loop{Tools: rec, AllowDefer: true}
	l.RunDeferredRound(context.Background(), nil, agent.DeferredRound{Calls: []agent.ToolUse{call}})
	if !rec.could {
		t.Fatal("the call was not deferrable")
	}
	return rec.res
}

type recordResult struct {
	inner agent.ToolDispatcher
	could bool
	res   agent.ToolResult
}

func (r *recordResult) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	r.could = agent.CanDefer(ctx)
	res, err := r.inner.Dispatch(ctx, call)
	r.res = res
	return res, err
}

// TestPreToolUseDeferHoldsTheCall: a hook's defer holds a deferrable call
// unrun; a deny rule still denies it; where the call cannot be deferred the
// defer is an ask, which print mode (no prompter) refuses.
func TestPreToolUseDeferHoldsTheCall(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("secret words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newD := func(mode toolreg.Mode, rules toolreg.RuleSet) *Dispatcher {
		d := New(mode, rules)
		d.Hook = func(context.Context, agent.ToolUse) (*hooks.Decision, error) {
			return &hooks.Decision{Behavior: hooks.BehaviorDefer, Continue: true}, nil
		}
		return d
	}
	call := agent.ToolUse{ID: "toolu_1", Name: string(protocol.ToolRead), Input: mustJSON(t, map[string]any{"file_path": file})}

	res := dispatchDeferrable(t, newD(toolreg.ModeBypassPermissions, nil), call)
	if !res.Deferred || res.Content != "" {
		t.Fatalf("deferrable: %+v", res)
	}

	deny := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: string(protocol.ToolRead)}}
	res = dispatchDeferrable(t, newD(toolreg.ModeBypassPermissions, deny), call)
	if res.Deferred || !res.IsError || strings.Contains(res.Content, "secret") {
		t.Fatalf("deny rule: %+v", res)
	}

	res, _ = newD(toolreg.ModeBypassPermissions, nil).Dispatch(context.Background(), call)
	if res.Deferred || !res.IsError || strings.Contains(res.Content, "secret") {
		t.Fatalf("not deferrable: %+v", res)
	}
}
