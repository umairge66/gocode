package cli

import (
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/toolreg"
)

// policyTestCtx is a hermetic settings context whose OS-managed policy comes
// from the given readout (the Context test reader seam).
func policyTestCtx(r settingsload.OSPolicyReadout) *settingsload.Context {
	return &settingsload.Context{
		HomeDir:                "/home/u",
		Cwd:                    "/proj",
		ManagedBaseDirOverride: "/etc/gocode",
		Env:                    func(string) string { return "" },
		OSPolicyReader:         func() settingsload.OSPolicyReadout { return r },
	}
}

func strPtr(s string) *string { return &s }

// A deny rule delivered only through the registry policy key (an MDM tier)
// blocks the tool through the live Decider, even against a user allow rule
// and in bypassPermissions mode.
func TestRegistryPolicyDenyBlocksTool(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/home/u/.config/gocode/settings.json", []byte(`{"permissions":{"allow":["Bash(rm:*)"]}}`))
	ctx := policyTestCtx(settingsload.OSPolicyReadout{
		HKLM: strPtr(`{"permissions":{"deny":["Bash(rm:*)"]}}`),
	})

	rules := permissionRulesFor(fsys, ctx)
	d := &toolreg.Decider{Registry: toolreg.DefaultRegistry()}
	got := d.Decide(toolreg.Call{Tool: protocol.ToolBash, RuleContent: "rm -rf /tmp/x"}, toolreg.ModeBypassPermissions, rules, nil)
	if got.Behavior != toolreg.BehaviorDeny || got.Rule == nil || got.Rule.Source != "policySettings" {
		t.Fatalf("registry deny must block: %+v", got)
	}

	// Without the registry tier the user allow rule applies.
	ctx = policyTestCtx(settingsload.OSPolicyReadout{})
	got = d.Decide(toolreg.Call{Tool: protocol.ToolBash, RuleContent: "rm -rf /tmp/x"}, toolreg.ModeDefault, permissionRulesFor(fsys, ctx), nil)
	if got.Behavior != toolreg.BehaviorAllow {
		t.Fatalf("control: user allow should apply without policy: %+v", got)
	}
}

// allowManagedPermissionRulesOnly from the registry tier drops every
// non-policy rule (allow, ask and deny alike).
func TestRegistryPolicyManagedRulesOnly(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/home/u/.config/gocode/settings.json", []byte(`{"permissions":{"allow":["Bash"],"deny":["WebFetch"]}}`))
	ctx := policyTestCtx(settingsload.OSPolicyReadout{
		HKLM: strPtr(`{"allowManagedPermissionRulesOnly":true,"permissions":{"ask":["Read"]}}`),
	})
	rules := permissionRulesFor(fsys, ctx)
	for _, r := range rules {
		if r.Source != "policySettings" {
			t.Fatalf("non-policy rule kept under allowManagedPermissionRulesOnly: %+v", r)
		}
	}
	if len(rules) != 1 {
		t.Fatalf("want only the policy rule, got %+v", rules)
	}
	d := &toolreg.Decider{Registry: toolreg.DefaultRegistry()}
	if got := d.Decide(toolreg.Call{Tool: protocol.ToolBash, RuleContent: "ls"}, toolreg.ModeDefault, rules, nil); got.Behavior == toolreg.BehaviorAllow {
		t.Fatalf("user allow must not apply: %+v", got)
	}
}
