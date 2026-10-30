package settingsload

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func ctxWithOS(r OSPolicyReadout) *Context {
	ctx := newCtx()
	ctx.OSPolicyReader = func() OSPolicyReadout { return r }
	return ctx
}

// First-wins (the default): the MDM tier is the policy verbatim; the managed
// file contributes nothing, not even restrictive values.
func TestComposePolicyFirstWins(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"model":"file","disableAllHooks":true}`))
	ctx := ctxWithOS(OSPolicyReadout{HKLM: strp(`{"model":"hklm","permissions":{"deny":["Bash"]}}`)})

	p := ComposePolicy(fsys, ctx)
	if p.Merged || p.Behavior != settingspolicy.FirstWins {
		t.Fatalf("behavior: %v merged=%v", p.Behavior, p.Merged)
	}
	if !reflect.DeepEqual(p.TierSources, []string{"hklm", "file"}) {
		t.Fatalf("sources: %v", p.TierSources)
	}
	if p.Settings["model"] != "hklm" || p.Settings["disableAllHooks"] != nil {
		t.Fatalf("first-wins must use the top tier only: %v", p.Settings)
	}

	// A macOS plist is the MDM tier the same way.
	plist := `{"model":"plist"}`
	ctx = ctxWithOS(OSPolicyReadout{Plists: []PlistDocument{{Label: "/Library/Managed Preferences/x.plist", JSON: &plist}}})
	p = ComposePolicy(fsys, ctx)
	if p.Settings["model"] != "plist" || p.TierSources[0] != "plist" {
		t.Fatalf("plist tier: %+v", p)
	}
}

// managedSourcesBehavior "merge": lower tiers merge beneath the top one with
// secrets / non-inheritable keys stripped and restrictive keys clamped.
func TestComposePolicyMergeRestrictive(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{
		"disableAllHooks": true,
		"skipDangerousModePermissionPrompt": true,
		"maxEffortLevel": "low",
		"apiKeyHelper": "/bin/leak",
		"permissions": {"defaultMode": "bypassPermissions", "deny": ["WebFetch"], "allow": ["Bash(ls:*)"]},
		"availableModels": ["haiku"],
		"fileOnly": 1
	}`))
	ctx := ctxWithOS(OSPolicyReadout{HKLM: strp(`{
		"managedSourcesBehavior": "merge",
		"disableAllHooks": false,
		"maxEffortLevel": "high",
		"permissions": {"deny": ["Bash(rm:*)"]},
		"availableModels": ["opus"]
	}`)})

	p := ComposePolicy(fsys, ctx)
	if !p.Merged || p.Behavior != settingspolicy.MergeBehavior {
		t.Fatalf("expected a merge: %+v", p)
	}
	s := p.Settings
	if s["disableAllHooks"] != true {
		t.Errorf("restrictive true from a lower tier must win: %v", s["disableAllHooks"])
	}
	if _, has := s["skipDangerousModePermissionPrompt"]; has {
		t.Errorf("a permissive value only a lower tier sets must be dropped: %v", s)
	}
	if s["maxEffortLevel"] != "low" {
		t.Errorf("lowest effort cap wins: %v", s["maxEffortLevel"])
	}
	if _, has := s["apiKeyHelper"]; has {
		t.Errorf("credential helpers are not inherited from a lower tier")
	}
	if m, _ := settingspolicy.GetPath(s, []string{"permissions", "defaultMode"}); m != nil {
		t.Errorf("permissions.defaultMode is non-inheritable: %v", m)
	}
	deny, _ := settingspolicy.GetPath(s, []string{"permissions", "deny"})
	if !reflect.DeepEqual(deny, []any{"Bash(rm:*)", "WebFetch"}) {
		t.Errorf("deny rules union across merged tiers: %v", deny)
	}
	if !reflect.DeepEqual(s["availableModels"], []any{"opus"}) {
		t.Errorf("availableModels is replaced by the top tier: %v", s["availableModels"])
	}
	if s["fileOnly"] != 1.0 {
		t.Errorf("plain lower-tier keys merge in: %v", s)
	}
	if _, has := s["managedSourcesBehavior"]; has {
		t.Errorf("control key must not leak into the policy")
	}
}

// The HKCU key is user-writable: admin-only keys are stripped, and it never
// applies beside (or merges with) an admin tier.
func TestComposePolicyHKCU(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	hkcu := `{"managedMcpServers":{"x":{}},"prependPlugins":["p"],"isolation":{"required":false},
		"enabledPlugins":{"good@m":false,"evil@m":true,"core@builtin":false},
		"permissions":{"deny":["Bash"]}}`
	p := ComposePolicy(fsys, ctxWithOS(OSPolicyReadout{HKCU: strp(hkcu)}))
	if !p.FromHKCU {
		t.Fatalf("HKCU should be the policy: %+v", p)
	}
	for _, k := range []string{"managedMcpServers", "prependPlugins", "isolation"} {
		if _, has := p.Settings[k]; has {
			t.Errorf("admin-only %q must be stripped from HKCU", k)
		}
	}
	if !reflect.DeepEqual(p.Settings["enabledPlugins"], map[string]any{"good@m": false}) {
		t.Errorf("only disabling entries survive: %v", p.Settings["enabledPlugins"])
	}
	warned := 0
	for _, e := range p.Errors {
		if e.Severity == "warning" && strings.Contains(e.Message, "administrator-controlled") {
			warned++
		}
	}
	if warned != 4 {
		t.Errorf("want 4 stripping warnings, got %d: %v", warned, p.Errors)
	}

	// HKLM present: HKCU is not applied at all.
	p = ComposePolicy(fsys, ctxWithOS(OSPolicyReadout{HKLM: strp(`{"model":"m"}`), HKCU: strp(`{"permissions":{"allow":["Bash"]}}`)}))
	if p.FromHKCU || p.Settings["permissions"] != nil {
		t.Fatalf("HKCU must not merge beside an admin tier: %v", p.Settings)
	}
	// A deployed managed file suppresses HKCU too.
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"model":"file"}`))
	p = ComposePolicy(fsys, ctxWithOS(OSPolicyReadout{HKCU: strp(`{"permissions":{"allow":["Bash"]}}`)}))
	if p.FromHKCU || p.Settings["model"] != "file" || p.Settings["permissions"] != nil {
		t.Fatalf("managed file outranks HKCU: %v", p.Settings)
	}
}

// An unreadable / unparsable admin document fails closed: no policy from it,
// a fatal error, HKCU still suppressed, and the source reports unreadable so
// key presence is unknowable.
func TestComposePolicyUnparsableAdminFailsClosed(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"permissions": {"deny": [`))
	ctx := ctxWithOS(OSPolicyReadout{HKCU: strp(`{"permissions":{"allow":["Bash"]}}`)})

	res := LoadSource(fsys, ctx, settingspolicy.SourcePolicy)
	if res.LoadState != settingspolicy.LoadUnreadable || res.Settings != nil {
		t.Fatalf("want fail-closed unreadable, got %+v", res)
	}
	fatal := false
	for _, e := range res.Errors {
		fatal = fatal || e.Severity == "fatal"
	}
	if !fatal {
		t.Fatalf("missing fatal diagnostic: %v", res.Errors)
	}
	st := NewStore(fsys, ctx, nil)
	if got, _ := st.RawKeyPresence("permissions"); got != "unknowable" {
		t.Fatalf("presence = %q", got)
	}

	// A broken drop-in beside a good base: the good content still applies,
	// and the error surfaces.
	fsys = fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"model":"m"}`))
	fsys.WriteFile("/etc/gocode/managed-settings.d/50-bad.json", []byte(`[1,2]`))
	p := ComposePolicy(fsys, newCtx())
	if p.Settings["model"] != "m" || p.LoadState != settingspolicy.LoadDidNotLoad || len(p.Errors) == 0 {
		t.Fatalf("broken drop-in: %+v", p)
	}
}

// Invalid values surface as validation warnings labelled with their document.
func TestComposePolicyValidationWarnings(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.d/10.json", []byte(`{"disableAllHooks":"yes"}`))
	p := ComposePolicy(fsys, ctxWithOS(OSPolicyReadout{HKLM: strp(`{"bogusKey":1,"managedSourcesBehavior":"merge"}`)}))
	joined := strings.Join(p.Warnings, "\n")
	if !strings.Contains(joined, "hklm: Unrecognized field: bogusKey") ||
		!strings.Contains(joined, filepath.FromSlash("/etc/gocode/managed-settings.d/10.json")+": disableAllHooks") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

// The admin env blocks union (highest tier wins per variable) unless the
// opt-out is set.
func TestComposePolicyEnvUnion(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"env":{"A":"file","B":"file","GOCODE_BEDROCK_BASE_URL":"x"},"awsAuthRefresh":"helper"}`))
	ctx := ctxWithOS(OSPolicyReadout{HKLM: strp(`{"env":{"a":"hklm"}}`)})
	p := ComposePolicy(fsys, ctx)
	// A lower tier's differently-cased spelling keeps the higher tier's value
	//; a helper-bound variable of a lower tier that
	// configures that helper is dropped.
	want := map[string]any{"a": "hklm", "A": "hklm", "B": "file"}
	if !reflect.DeepEqual(p.Settings["env"], want) {
		t.Fatalf("env union = %v; want %v", p.Settings["env"], want)
	}
	ctx.Env = func(k string) string {
		if k == disableAdminEnvUnionVar {
			return "1"
		}
		return ""
	}
	p = ComposePolicy(fsys, ctx)
	if !reflect.DeepEqual(p.Settings["env"], map[string]any{"a": "hklm"}) {
		t.Fatalf("opted-out env = %v", p.Settings["env"])
	}
}

// Every policy consumer reads the composed source through EffectiveSources.
func TestEffectiveSourcesSeeRegistryPolicy(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	ctx := ctxWithOS(OSPolicyReadout{HKLM: strp(`{"allowManagedHooksOnly":true,"enforceAvailableModels":true,"availableModels":["opus"]}`)})
	var policy settingspolicy.LoadResult
	for _, or := range EffectiveSources(fsys, ctx, CanonicalSourceOrder) {
		if or.Source == settingspolicy.SourcePolicy {
			policy = or.Result
		}
	}
	if policy.LoadState != settingspolicy.LoadLoaded || policy.Settings["allowManagedHooksOnly"] != true ||
		policy.Settings["enforceAvailableModels"] != true {
		t.Fatalf("policy source = %+v", policy)
	}
	if all := LoadAll(fsys, ctx); all[settingspolicy.SourcePolicy].Settings["allowManagedHooksOnly"] != true {
		t.Fatalf("LoadAll policy = %+v", all[settingspolicy.SourcePolicy])
	}
	// SkillShellExecutionDisabled reads the same composition.
	ctx = ctxWithOS(OSPolicyReadout{HKLM: strp(`{"disableSkillShellExecution":true}`)})
	if !SkillShellExecutionDisabled(fsys, ctx) {
		t.Fatal("registry disableSkillShellExecution must apply")
	}
}
