package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// Tests for the auto-mode hand-off of un-approved `!`-blocks to the model and
// for the disableSkillShellExecution placeholder, through the live-session
// shell seam (configureSession). All offline.

func askRule(content string) toolreg.Rule {
	return toolreg.Rule{Behavior: toolreg.RuleAsk, ToolName: "Bash", Content: content}
}

// modelSaw reports whether the last model request contains s (s must need no
// JSON escaping: no quotes, newlines, backslashes or &<>).
func (c *gateCaller) modelSaw(t *testing.T, s string) bool {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := json.Marshal(c.last)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), s)
}

const handOffText = "[run this first, exactly as written, and use its output: `make build`]"

// Auto mode + ask rule: the block is not run and not prompted for; the model
// gets the "run this first" instruction in its place.
func TestShellHandOff_AutoModeAskRuleHandsToModel(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, toolreg.RuleSet{askRule("make:*")})
	d.SetPermissionPrompter(failPrompter(t))
	f := newGateFixture(t, d, gateCmd("st: !`echo ok`\nbuild: !`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := f.runner.commands(); len(got) != 1 || got[0] != "echo ok" {
		t.Fatalf("ran = %v; only the auto-allowed block may run", got)
	}
	if f.model.count() != 1 || !f.model.modelSaw(t, handOffText) {
		t.Fatalf("model calls = %d; hand-off instruction missing from prompt", f.model.count())
	}
}

// -p (no prompter) in auto mode hands off too instead of failing.
func TestShellHandOff_AutoModePrintHandsOff(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, toolreg.RuleSet{askRule("make:*")})
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(f.runner.commands()) != 0 || !f.model.modelSaw(t, handOffText) {
		t.Fatalf("ran=%v; want hand-off and no run", f.runner.commands())
	}
}

// SECURITY: a deny rule in auto mode is never handed off — the command fails,
// nothing runs, and the model is not called.
func TestShellHandOff_DenyRuleNeverHandedOff(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, toolreg.RuleSet{denyRule("make:*"), askRule("make:*")})
	d.SetPermissionPrompter(failPrompter(t))
	f := newGateFixture(t, d, gateCmd("!`echo ok` !`make build`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil || !strings.Contains(err.Error(), "Shell command permission check failed") {
		t.Fatalf("err = %v, want the permission failure", err)
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 0 {
		t.Fatalf("ran=%v model=%d; a denied block must abort everything", f.runner.commands(), f.model.count())
	}
}

// SECURITY: the gate itself reports a deny as a denial, not a hand-off, and an
// allowed-tools rule cannot turn a denied block into a hand-off or a run.
func TestShellHandOff_GateDenyIsNotHandOff(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, toolreg.RuleSet{denyRule("rm:*")})
	err := d.AuthorizeShellCommand(context.Background(), toolrun.ShellCommandCheck{
		Command:    "rm -rf x",
		ExtraAllow: toolreg.RuleSet{allowRule("rm:*")},
	})
	if err == nil || err == toolrun.ErrShellHandOff {
		t.Fatalf("err = %v, want a denial", err)
	}
}

// Without the Bash tool on offer the hand-off is unavailable: the ask is
// handled as before (-p: denial with guidance).
func TestShellHandOff_NeedsBashTool(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, toolreg.RuleSet{askRule("make:*")})
	d.Restrict([]string{"Read"})
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil || !strings.Contains(err.Error(), "haven't granted it yet") {
		t.Fatalf("err = %v, want the -p denial", err)
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 0 {
		t.Fatal("block ran or model called")
	}
}

// Outside auto mode there is no hand-off: -p default mode still fails.
func TestShellHandOff_OnlyInAutoMode(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{askRule("make:*")})
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err == nil {
		t.Fatal("default -p should deny, not hand off")
	}
	if f.model.count() != 0 {
		t.Fatal("model called")
	}
}

// newPolicyFixture is newGateFixture with disableSkillShellExecution forced.
func newPolicyFixture(t *testing.T, d *toolrun.Dispatcher, disabled bool, cmds ...*commands.Command) *gateFixture {
	t.Helper()
	model := &gateCaller{}
	conv := conversation.New(model, d)
	reg := commands.NewRegistry()
	for _, c := range cmds {
		c.Type = "prompt"
		reg.Add(c)
	}
	conv.Commands = reg
	runner := &gateRunner{}
	configureSession(conv, &printSession{shellRunner: runner.run, skillShellDisabled: func() bool { return disabled }}, nil, nil, nil, d)
	return &gateFixture{conv: conv, d: d, model: model, runner: runner}
}

// disableSkillShellExecution: a project/plugin command's blocks become the
// placeholder; nothing is checked (no prompt, deny rules irrelevant) or run.
func TestShellPolicy_DisabledReplacesBlocks(t *testing.T) {
	for _, src := range []commands.Source{commands.SourceProject, commands.SourceUser, commands.SourcePlugin} {
		d := toolrun.New(toolreg.ModeBypassPermissions, toolreg.RuleSet{denyRule("rm:*")})
		d.SetPermissionPrompter(failPrompter(t))
		cmd := gateCmd("a !`echo hi` b !`rm -rf x`")
		cmd.Source = src
		f := newPolicyFixture(t, d, true, cmd)
		if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
			t.Fatalf("%s: Submit: %v", src, err)
		}
		if len(f.runner.commands()) != 0 {
			t.Fatalf("%s: ran %v under disableSkillShellExecution", src, f.runner.commands())
		}
		if !f.model.modelSaw(t, "a "+commands.ShellDisabledPlaceholder+" b "+commands.ShellDisabledPlaceholder) {
			t.Fatalf("%s: placeholder missing from the prompt", src)
		}
	}
}

// With the setting off, blocks run as usual; a builtin-source command is not
// covered by the setting at all.
func TestShellPolicy_OffOrUncoveredRuns(t *testing.T) {
	d := toolrun.New(toolreg.ModeBypassPermissions, nil)
	f := newPolicyFixture(t, d, false, gateCmd("!`echo hi`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.commands()) != 1 {
		t.Fatalf("setting off: ran %v", f.runner.commands())
	}

	b := gateCmd("!`echo hi`")
	b.Source = commands.SourceBuiltin
	fb := newPolicyFixture(t, toolrun.New(toolreg.ModeBypassPermissions, nil), true, b)
	if _, err := fb.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatal(err)
	}
	if len(fb.runner.commands()) != 1 {
		t.Fatalf("builtin source: ran %v", fb.runner.commands())
	}
}
