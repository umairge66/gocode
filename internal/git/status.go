package git

import (
	"strings"
)

// Branch is the resolved checked-out branch. Detached is true when HEAD is not
// on a branch (symbolic-ref exits non-zero with empty output).
type Branch struct {
	Name     string
	Detached bool
}

// ParseBranch interprets the result of CurrentBranchArgs. A zero exit with
// non-empty output yields the branch name; anything else is detached HEAD.
func ParseBranch(code int, stdout []byte) Branch {
	name := strings.TrimSpace(string(stdout))
	if code != 0 || name == "" {
		return Branch{Detached: true}
	}
	return Branch{Name: name}
}
