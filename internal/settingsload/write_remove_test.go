package settingsload

import (
	"os"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/settingspolicy"
)

// TestRemovePermissionRule asserts a rule is removed from its list, emptied
// lists and permissions objects are dropped, other keys survive, and a
// missing rule changes nothing.
func TestRemovePermissionRule(t *testing.T) {
	ctx, _ := localCtx(t)
	path, _ := ctx.PathForSource(settingspolicy.SourceLocal)
	for _, r := range []string{"Bash(ls:*)", "Read"} {
		if _, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionDeny, "WebFetch"); err != nil {
		t.Fatal(err)
	}

	removed, err := RemovePermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Bash(ls:*)")
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if got := allowList(t, readLocal(t, ctx)); len(got) != 1 || got[0] != "Read" {
		t.Fatalf("allow after remove = %q", got)
	}
	removed, err = RemovePermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Missing")
	if err != nil || removed {
		t.Fatalf("missing rule: removed=%v err=%v", removed, err)
	}
	for _, step := range []struct{ behavior, rule string }{{PermissionAllow, "Read"}, {PermissionDeny, "WebFetch"}} {
		if _, err := RemovePermissionRule(ctx, settingspolicy.SourceLocal, step.behavior, step.rule); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "permissions") {
		t.Fatalf("emptied permissions object kept: %s", data)
	}
	if _, err := RemovePermissionRule(ctx, settingspolicy.SourceLocal, "bogus", "x"); err == nil {
		t.Fatal("an unknown behavior was accepted")
	}
}
