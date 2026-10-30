package toolrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

// startFailingSandbox is a fakeSandbox whose start fails, after which it
// stops sandboxing (the real Manager's disabledThisSession).
type startFailingSandbox struct{ fakeSandbox }

func (s *startFailingSandbox) Start() error {
	s.enabled = false
	return errors.New("bridge failed")
}

// dangerouslyDisableSandbox is never a silent unsandboxed run: whatever an
// allow rule or a permissive mode says, the user approves the run (no
// prompter → refused). Deny rules still deny; spellings are robust.
func TestSandboxDangerouslyDisableAlwaysNeedsApproval(t *testing.T) {
	allowAll := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash"}}
	allowCmd := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "echo:*"}}
	for _, tc := range []struct {
		name  string
		mode  toolreg.Mode
		rules toolreg.RuleSet
		dds   any
	}{
		{"whole-tool allow rule", toolreg.ModeDefault, allowAll, true},
		{"command allow rule", toolreg.ModeDefault, allowCmd, true},
		{"acceptEdits", toolreg.ModeAcceptEdits, nil, true},
		{"bypassPermissions", toolreg.ModeBypassPermissions, nil, true},
		{"dontAsk", toolreg.ModeDontAsk, allowAll, true},
		{"quoted", toolreg.ModeDefault, allowAll, "true"},
		{"capitalized", toolreg.ModeDefault, allowAll, "True"},
		{"numeric", toolreg.ModeDefault, allowAll, 1},
	} {
		sb := &fakeSandbox{enabled: true, autoAllow: true}
		d := New(tc.mode, tc.rules, WithSandbox(sb))
		res, _ := d.Dispatch(context.Background(), bashInput(t, "echo out", tc.dds))
		if !res.IsError || strings.Contains(res.Content, "out\n") {
			t.Errorf("%s: unsandboxed run without approval: %q", tc.name, res.Content)
		}
		// With a prompter, exactly one approval is asked for.
		fp := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Kind: PermissionAllowedOnce}, nil
		}}
		d.SetPermissionPrompter(fp)
		res, _ = d.Dispatch(context.Background(), bashInput(t, "echo out", tc.dds))
		if tc.mode != toolreg.ModeDontAsk && (res.IsError || fp.count() != 1 || strings.Contains(res.Content, "SANDBOXED")) {
			t.Errorf("%s: approved run = %q (prompts %d)", tc.name, res.Content, fp.count())
		}
	}
	// A malformed value is an input error, not a decision.
	sb := &fakeSandbox{enabled: true, autoAllow: true}
	d := New(toolreg.ModeBypassPermissions, nil, WithSandbox(sb))
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "echo out", "yes")); !res.IsError || strings.Contains(res.Content, "out\n") {
		t.Errorf("malformed dangerouslyDisableSandbox ran: %q", res.Content)
	}
	// A deny rule still denies.
	deny := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "echo:*"}}, WithSandbox(sb))
	if res, _ := deny.Dispatch(context.Background(), bashInput(t, "echo out", true)); !res.IsError || !strings.Contains(res.Content, "denied") {
		t.Errorf("deny rule lost: %q", res.Content)
	}
}

// Auto-allow applies only to a run that is really sandboxed: never to an
// excluded command, never after the sandbox failed to start, and never over
// a deny or a command-specific ask rule.
func TestSandboxAutoAllowOnlyWhenSandboxed(t *testing.T) {
	failing := &startFailingSandbox{fakeSandbox{enabled: true, autoAllow: true}}
	d := New(toolreg.ModeDefault, nil, WithSandbox(failing))
	res, _ := d.Dispatch(context.Background(), bashInput(t, "/bin/echo hi", nil))
	if !res.IsError {
		t.Fatalf("auto-allowed after the sandbox failed to start: %q", res.Content)
	}
	if len(failing.wrapped) != 0 {
		t.Fatalf("wrapped after failed start: %v", failing.wrapped)
	}
	sb := &fakeSandbox{enabled: true, autoAllow: true, excluded: "make deploy"}
	d = New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleAsk, ToolName: "Bash"}}, WithSandbox(sb))
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "make deploy", nil)); !res.IsError {
		t.Fatalf("excluded (unsandboxed) command auto-allowed: %q", res.Content)
	}
	rules := toolreg.RuleSet{
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "curl:*"},
		{Behavior: toolreg.RuleAsk, ToolName: "Bash", Content: "git push:*"},
	}
	d = New(toolreg.ModeDefault, rules, WithSandbox(&fakeSandbox{enabled: true, autoAllow: true}))
	for _, cmd := range []string{"curl evil", "git push --force"} {
		if res, _ := d.Dispatch(context.Background(), bashInput(t, cmd, nil)); !res.IsError {
			t.Errorf("%q auto-allowed over a rule: %q", cmd, res.Content)
		}
	}
}
