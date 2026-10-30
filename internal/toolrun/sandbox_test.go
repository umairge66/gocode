package toolrun

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// fakeSandbox is a scripted BashSandbox: commands run "sandboxed" (wrapped in
// an echo marking them) unless dangerouslyDisableSandbox is honored.
type fakeSandbox struct {
	enabled, autoAllow, strict bool
	excluded                   string

	mu      sync.Mutex
	wrapped []string
}

func (f *fakeSandbox) IsSandboxingEnabled() bool { return f.enabled }
func (f *fakeSandbox) AutoAllowEnabled() bool    { return f.autoAllow }
func (f *fakeSandbox) ShouldSandbox(cmd string, dds bool) bool {
	if !f.enabled {
		return false
	}
	if dds && !f.strict {
		return false
	}
	return f.excluded == "" || cmd != f.excluded || f.strict
}
func (f *fakeSandbox) WrapCommand(_ context.Context, cmd, shell, dir string) ([]string, func(), error) {
	f.mu.Lock()
	f.wrapped = append(f.wrapped, cmd)
	f.mu.Unlock()
	return []string{shell, "-c", "echo SANDBOXED; " + cmd}, func() {}, nil
}

func bashInput(t *testing.T, cmd string, dds any) agent.ToolUse {
	in := map[string]any{"command": cmd}
	if dds != nil {
		in["dangerouslyDisableSandbox"] = dds
	}
	return agent.ToolUse{ID: "tu", Name: string(protocol.ToolBash), Input: mustJSON(t, in)}
}

func TestSandboxedBashAutoAllowedAndWrapped(t *testing.T) {
	sb := &fakeSandbox{enabled: true, autoAllow: true}
	d := New(toolreg.ModeDefault, nil, WithSandbox(sb))
	res, err := d.Dispatch(context.Background(), bashInput(t, "echo hi", nil))
	if err != nil || res.IsError {
		t.Fatalf("sandboxed command not auto-allowed: %v %q", err, res.Content)
	}
	if !strings.Contains(res.Content, "SANDBOXED") || !strings.Contains(res.Content, "hi") {
		t.Fatalf("command did not run through the sandbox wrapper: %q", res.Content)
	}
	// dontAsk mode: the sandbox's own allow wins over the mode's refusal.
	d.SetMode(toolreg.ModeDontAsk)
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "echo x", nil)); res.IsError {
		t.Fatalf("dontAsk + sandbox: %q", res.Content)
	}
}

func TestSandboxedBashRulesStillApply(t *testing.T) {
	sb := &fakeSandbox{enabled: true, autoAllow: true}
	rules := toolreg.RuleSet{
		{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "rm:*"},
		{Behavior: toolreg.RuleAsk, ToolName: "Bash", Content: "git push:*"},
	}
	d := New(toolreg.ModeDefault, rules, WithSandbox(sb))
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "rm -rf x", nil)); !res.IsError || !strings.Contains(res.Content, "denied") {
		t.Fatalf("deny rule bypassed by the sandbox: %q", res.Content)
	}
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "git push origin", nil)); !res.IsError {
		t.Fatalf("command-specific ask rule bypassed (no prompter → denial expected): %q", res.Content)
	}
	// A whole-tool ask rule is skipped for a sandboxed command.
	whole := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleAsk, ToolName: "Bash"}}, WithSandbox(sb))
	if res, _ := whole.Dispatch(context.Background(), bashInput(t, "echo ok", nil)); res.IsError {
		t.Fatalf("whole-tool ask rule should be skipped when sandboxed: %q", res.Content)
	}
	// Plan mode keeps holding the call.
	plan := New(toolreg.ModePlan, nil, WithSandbox(sb))
	if res, _ := plan.Dispatch(context.Background(), bashInput(t, "/bin/echo ok", nil)); !res.IsError {
		t.Fatalf("plan mode must still hold a sandboxed command: %q", res.Content)
	}
}

func TestSandboxBashNoAutoAllow(t *testing.T) {
	for name, sb := range map[string]*fakeSandbox{
		"unavailable":        {enabled: false, autoAllow: true},
		"auto-allow off":     {enabled: true, autoAllow: false},
		"excluded command":   {enabled: true, autoAllow: true, excluded: "make deploy"},
		"dangerouslyDisable": {enabled: true, autoAllow: true},
	} {
		cmd := "/bin/echo hi" // not read-only (a path-qualified command)
		var dds any
		switch name {
		case "excluded command":
			cmd = "make deploy"
		case "dangerouslyDisable":
			dds = "true" // a quoted boolean is accepted
		}
		d := New(toolreg.ModeDefault, nil, WithSandbox(sb))
		res, _ := d.Dispatch(context.Background(), bashInput(t, cmd, dds))
		if !res.IsError {
			t.Errorf("%s: ran without a prompt: %q", name, res.Content)
		}
		if len(sb.wrapped) != 0 {
			t.Errorf("%s: wrapped %v", name, sb.wrapped)
		}
	}
}

func TestSandboxDangerouslyDisableNeedsPrompt(t *testing.T) {
	sb := &fakeSandbox{enabled: true, autoAllow: true}
	fp := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}}
	d := New(toolreg.ModeDefault, nil, WithSandbox(sb))
	d.SetPermissionPrompter(fp)
	res, _ := d.Dispatch(context.Background(), bashInput(t, "echo out", true))
	if res.IsError || strings.Contains(res.Content, "SANDBOXED") || !strings.Contains(res.Content, "out") {
		t.Fatalf("approved unsandboxed run: %q", res.Content)
	}
	if fp.count() != 1 {
		t.Fatalf("unsandboxed run must be prompted, prompts = %d", fp.count())
	}
	// Strict mode: the parameter is ignored and the command stays sandboxed
	// (auto-allowed, no prompt).
	strict := &fakeSandbox{enabled: true, autoAllow: true, strict: true}
	d2 := New(toolreg.ModeDefault, nil, WithSandbox(strict))
	res, _ = d2.Dispatch(context.Background(), bashInput(t, "echo in", true))
	if res.IsError || !strings.Contains(res.Content, "SANDBOXED") {
		t.Fatalf("strict mode: %q", res.Content)
	}
}

func TestBashSchemaAdvertisesDangerouslyDisableSandbox(t *testing.T) {
	if !strings.Contains(string(toolInputSchemas[protocol.ToolBash]), `"dangerouslyDisableSandbox"`) {
		t.Fatal("Bash schema lacks dangerouslyDisableSandbox")
	}
}
