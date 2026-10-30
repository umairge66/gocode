package toolrun

import (
	"context"
	"errors"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

// Tests for the auto-mode hand-off verdict of AuthorizeShellCommand.

func bashRule(b toolreg.RuleBehavior, content string) toolreg.Rule {
	return toolreg.Rule{Behavior: b, ToolName: "Bash", Content: content}
}

func TestAuthorizeShellCommandAutoModeVerdicts(t *testing.T) {
	rules := toolreg.RuleSet{
		bashRule(toolreg.RuleAsk, "make:*"),
		bashRule(toolreg.RuleDeny, "rm:*"),
	}
	cases := []struct {
		cmd     string
		handOff bool
		allowed bool
	}{
		{"echo hi", false, true},        // auto mode auto-allows (user's choice)
		{"make build", true, false},     // ask rule: handed to the model
		{"rm -rf /tmp/x", false, false}, // deny rule: plain denial, never handed off
	}
	d := New(toolreg.ModeAuto, rules)
	for _, tc := range cases {
		err := d.AuthorizeShellCommand(context.Background(), ShellCommandCheck{Command: tc.cmd, Origin: "/x"})
		if got := errors.Is(err, ErrShellHandOff); got != tc.handOff {
			t.Errorf("%q: hand-off = %v (err %v), want %v", tc.cmd, got, err, tc.handOff)
		}
		if (err == nil) != tc.allowed {
			t.Errorf("%q: allowed = %v (err %v), want %v", tc.cmd, err == nil, err, tc.allowed)
		}
	}
}

// SECURITY: a deny rule is never handed off, even when allowed-tools or an
// ask rule also match the command.
func TestAuthorizeShellCommandDenyNeverHandedOff(t *testing.T) {
	d := New(toolreg.ModeAuto, toolreg.RuleSet{
		bashRule(toolreg.RuleAsk, "rm:*"),
		bashRule(toolreg.RuleDeny, "rm:*"),
	})
	err := d.AuthorizeShellCommand(context.Background(), ShellCommandCheck{
		Command:    "rm -rf x",
		ExtraAllow: toolreg.RuleSet{bashRule(toolreg.RuleAllow, "rm:*")},
	})
	if err == nil || errors.Is(err, ErrShellHandOff) {
		t.Fatalf("err = %v, want a denial", err)
	}
}

// The hand-off needs auto mode and the Bash tool on offer.
func TestAuthorizeShellCommandHandOffPreconditions(t *testing.T) {
	rules := toolreg.RuleSet{bashRule(toolreg.RuleAsk, "make:*")}
	chk := ShellCommandCheck{Command: "make build"}

	for _, m := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModeAcceptEdits, toolreg.ModePlan} {
		if err := New(m, rules).AuthorizeShellCommand(context.Background(), chk); err == nil || errors.Is(err, ErrShellHandOff) {
			t.Errorf("mode %s: err = %v, want a non-hand-off denial", m, err)
		}
	}

	d := New(toolreg.ModeAuto, rules)
	d.Restrict([]string{"Read"})
	if err := d.AuthorizeShellCommand(context.Background(), chk); err == nil || errors.Is(err, ErrShellHandOff) {
		t.Fatalf("Bash not offered: err = %v, want a non-hand-off denial", err)
	}
}
