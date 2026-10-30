package toolrun

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

// Under allowManagedPermissionRulesOnly a "don't ask again" answer is neither
// offered nor kept: the prompt carries NoAlwaysAllow (no suggested rule), the
// call runs once, the rule never reaches the live rules, and the next call
// prompts again.
func TestManagedRulesOnlyNoDontAskAgain(t *testing.T) {
	shared := NewSharedRules(nil)
	d := New(toolreg.ModeDefault, nil, WithSharedRules(shared), WithManagedRulesOnly(true))
	rule := toolreg.Rule{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "/bin/echo:*", Source: "localSettings"}
	p := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedAlways, Rule: &rule}, nil
	}}
	d.SetPermissionPrompter(p)

	for i := 0; i < 2; i++ {
		res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo hi"))
		if err != nil || res.IsError {
			t.Fatalf("call %d: %v %q", i, err, res.Content)
		}
	}
	if p.count() != 2 {
		t.Fatalf("prompts = %d, want 2 (no standing approval under managed-only)", p.count())
	}
	if got := shared.Snapshot(); len(got) != 0 {
		t.Fatalf("a don't-ask-again rule reached the live rules: %+v", got)
	}
	req := p.requests[0]
	if !req.NoAlwaysAllow || len(req.Suggested) != 0 {
		t.Fatalf("request must withhold don't-ask-again: %+v", req)
	}

	// Control: without the policy the same answer adds the rule.
	shared2 := NewSharedRules(nil)
	d2 := New(toolreg.ModeDefault, nil, WithSharedRules(shared2))
	p2 := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedAlways, Rule: &rule}, nil
	}}
	d2.SetPermissionPrompter(p2)
	if _, err := d2.Dispatch(context.Background(), bashCall(t, "/bin/echo hi")); err != nil {
		t.Fatal(err)
	}
	if len(shared2.Snapshot()) != 1 || p2.requests[0].NoAlwaysAllow {
		t.Fatalf("control: rule should be added and offered: %+v", shared2.Snapshot())
	}
}

// Without a prompter (-p) the denial does not suggest an allow rule a
// managed-only policy would ignore.
func TestManagedRulesOnlyDenyMessage(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithManagedRulesOnly(true))
	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo hi"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(res.Content, "Add an allow rule") || !strings.Contains(res.Content, "allowManagedPermissionRulesOnly") {
		t.Fatalf("deny message = %q", res.Content)
	}
}
