package git

import (
	"reflect"
	"testing"
)

func TestWorktreeAddArgs(t *testing.T) {
	// New branch, normal checkout.
	got := WorktreeAddArgs("agent/x", "/repo/wt", "deadbeef", true, false)
	want := []string{"worktree", "add", "--no-track", "-b", "agent/x", "/repo/wt", "deadbeef^{commit}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new-branch add = %v, want %v", got, want)
	}
	// Existing/reset branch (-B) with sparse --no-checkout.
	got = WorktreeAddArgs("agent/x", "/repo/wt", "deadbeef", false, true)
	want = []string{"worktree", "add", "--no-checkout", "--no-track", "-B", "agent/x", "/repo/wt", "deadbeef^{commit}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reset-branch no-checkout add = %v, want %v", got, want)
	}
}

func TestWorktreeRemoveArgs(t *testing.T) {
	if got := WorktreeRemoveArgs("/repo/wt", false); !reflect.DeepEqual(got, []string{"worktree", "remove", "--force", "/repo/wt"}) {
		t.Fatalf("remove = %v", got)
	}
	if got := WorktreeRemoveArgs("/repo/wt", true); !reflect.DeepEqual(got, []string{"worktree", "remove", "--force", "--force", "/repo/wt"}) {
		t.Fatalf("remove forceTwice = %v", got)
	}
}

func TestWorktreeLockArgs(t *testing.T) {
	if got := WorktreeLockArgs("/repo/wt", "agent session"); !reflect.DeepEqual(got, []string{"worktree", "lock", "--reason", "agent session", "/repo/wt"}) {
		t.Fatalf("WorktreeLockArgs = %v", got)
	}
}
