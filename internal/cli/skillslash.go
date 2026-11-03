package cli

import (
	"context"

	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// userSkillRunner runs a skill the person typed as `/name args` through the
// session's Skill tool (InvokeAsUser): the same rendering, inline scoping and
// fork as the model's Skill call. It is conversation.UserSkills.
type userSkillRunner struct{ tool *agenttool.SkillTool }

var _ conversation.UserSkillInvoker = userSkillRunner{}

// InvokeUserSkill implements conversation.UserSkillInvoker.
func (u userSkillRunner) InvokeUserSkill(ctx context.Context, name, args string) (conversation.UserSkillResult, error) {
	inv, err := u.tool.InvokeAsUser(ctx, name, args)
	if err != nil {
		return conversation.UserSkillResult{}, err
	}
	if inv.Forked {
		return conversation.UserSkillResult{Forked: true, Result: inv.ToolResult()}, nil
	}
	return conversation.UserSkillResult{Content: inv.Content}, nil
}

// skillDeniedFunc reports whether d's rules deny the Skill tool for a skill
// name (a Skill(<name>) deny rule, or a blanket Skill deny), read live so
// rules added during the session apply. It feeds skills.Registry.Denied: a
// skill the model would be refused is left out of the listing.
func skillDeniedFunc(d *toolrun.Dispatcher) func(string) bool {
	if d == nil {
		return nil
	}
	return func(name string) bool {
		call := toolreg.Call{Tool: protocol.ToolSkill, RuleContent: name}
		return d.SnapshotRules().Match(call, toolreg.RuleDeny) != nil
	}
}
