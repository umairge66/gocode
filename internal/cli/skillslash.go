package cli

import (
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

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
