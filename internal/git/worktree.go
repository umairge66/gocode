package git

// --- worktree op argv builders ---------------------------------------------

// WorktreeAddArgs builds `git worktree add`. The isolation flow always passes
// --no-track; -b creates a brand-new branch, -B resets/creates. base is the
// commit the new worktree is checked out at.
// When sparse paths are configured, noCheckout prepends --no-checkout.
func WorktreeAddArgs(branch, path, base string, isBranchNew, noCheckout bool) []string {
	args := []string{"worktree", "add"}
	if noCheckout {
		args = append(args, "--no-checkout")
	}
	branchFlag := "-B"
	if isBranchNew {
		branchFlag = "-b"
	}
	args = append(args, "--no-track", branchFlag, branch, path, base+"^{commit}")
	return args
}

// WorktreeRemoveArgs builds `git worktree remove --force [--force] <path>`. The
// isolation flow passes --force twice when forceTwice is set (to remove a
// worktree with a dirty or locked state).
func WorktreeRemoveArgs(path string, forceTwice bool) []string {
	args := []string{"worktree", "remove", "--force"}
	if forceTwice {
		args = append(args, "--force")
	}
	return append(args, path)
}

// WorktreeLockArgs builds `git worktree lock --reason <reason> <path>`.
func WorktreeLockArgs(path, reason string) []string {
	return []string{"worktree", "lock", "--reason", reason, path}
}
