package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// managedOnlyEnv is a session env whose admin policy (managed-settings.json)
// sets allowManagedPermissionRulesOnly with the given policy permissions.
func managedOnlyEnv(t *testing.T, user, policyPerms string) (*Env, *strings.Builder, string) {
	t.Helper()
	env, _, errb, cwd := sessionEnv(t, user)
	dir := t.TempDir()
	doc := `{"allowManagedPermissionRulesOnly":true` + policyPerms + `}`
	if err := os.WriteFile(filepath.Join(dir, "managed-settings.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	env.ManagedSettingsDir = dir
	return env, errb, cwd
}

func bashUse(cmd string) agent.ToolUse {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return agent.ToolUse{ID: "tu", Name: string(protocol.ToolBash), Input: in}
}

func TestCLIPermissionRules(t *testing.T) {
	rules, notes := cliPermissionRules([]string{"Bash(git log:*), Read", "Edit"}, []string{"Bash(rm:*)"}, false)
	var allow, deny []string
	for _, r := range rules {
		if r.Source != "cliArg" {
			t.Fatalf("source = %q", r.Source)
		}
		if r.Behavior == toolreg.RuleAllow {
			allow = append(allow, r.ToolName+"("+r.Content+")")
		} else {
			deny = append(deny, r.ToolName+"("+r.Content+")")
		}
	}
	if strings.Join(allow, ";") != "Bash(git log:*);Read();Edit()" || strings.Join(deny, ";") != "Bash(rm:*)" || len(notes) != 0 {
		t.Fatalf("allow=%v deny=%v notes=%v", allow, deny, notes)
	}
	// allowManagedPermissionRulesOnly: allows dropped with a notice, denies
	// kept.
	rules, notes = cliPermissionRules([]string{"Bash", "Read"}, []string{"WebFetch"}, true)
	if len(rules) != 1 || rules[0].Behavior != toolreg.RuleDeny || rules[0].ToolName != "WebFetch" {
		t.Fatalf("managed-only rules = %+v", rules)
	}
	want := "Ignoring --allowedTools Bash, Read: permission rules are restricted to managed settings (allowManagedPermissionRulesOnly)."
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes = %v", notes)
	}
}

func TestParseAllowedToolsFlags(t *testing.T) {
	d, rest := parseDispatchDefaults([]string{"--allowedTools", "Bash(git:*)", "Read", "--disallowed-tools=WebFetch", "-p", "hi"})
	if d == nil || strings.Join(d.AllowedTools, "|") != "Bash(git:*)|Read" || strings.Join(d.DisallowedTools, "|") != "WebFetch" {
		t.Fatalf("defaults = %+v", d)
	}
	if strings.Join(rest, " ") != "-p hi" {
		t.Fatalf("rest = %v", rest)
	}
}

// End to end through the live Decider: under allowManagedPermissionRulesOnly
// the user's settings allow, the --allowedTools allow and a persisted local
// approval are all ignored, while the policy's rules and the
// --disallowedTools deny still apply.
func TestManagedRulesOnlySessionE2E(t *testing.T) {
	env, _, cwd := managedOnlyEnv(t, `{"permissions":{"allow":["Bash(/bin/echo:*)"]}}`, `,"permissions":{"allow":["Bash(pwd)"]}`)
	// A "don't ask again" approval persisted earlier in the local settings.
	if err := os.MkdirAll(filepath.Join(cwd, ".gocode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".gocode", "settings.local.json"), []byte(`{"permissions":{"allow":["Bash(/bin/ls:*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	caller := &skillTurnCaller{model: "m", effort: "medium"}
	s := &printSession{
		Model: "m", Mode: toolreg.ModeDefault, Provider: providerAnthropic,
		AllowedTools: []string{"Bash(printf:*)"}, DisallowedTools: []string{"Bash(touch:*)"},
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	if !rt.dispatcher.ManagedRulesOnly {
		t.Fatalf("dispatcher not in managed-only mode")
	}
	if notes := strings.Join(rt.notices, "\n"); !strings.Contains(notes, "Ignoring --allowedTools Bash(printf:*): permission rules are restricted to managed settings (allowManagedPermissionRulesOnly).") {
		t.Fatalf("missing the --allowedTools notice: %q", notes)
	}
	ctx := context.Background()
	for _, cmd := range []string{"/bin/echo hi", "/bin/ls", "printf -v v x"} {
		res, err := rt.dispatcher.Dispatch(ctx, bashUse(cmd))
		if err != nil || !res.IsError {
			t.Errorf("%q must not be pre-approved under managed-only: %v %q", cmd, err, res.Content)
		}
	}
	if res, err := rt.dispatcher.Dispatch(ctx, bashUse("pwd")); err != nil || res.IsError {
		t.Errorf("the policy allow rule must apply: %v %q", err, res.Content)
	}
	// The --disallowedTools deny holds even in bypassPermissions.
	rt.dispatcher.SetMode(toolreg.ModeBypassPermissions)
	target := filepath.Join(t.TempDir(), "made")
	res, err := rt.dispatcher.Dispatch(ctx, bashUse("touch "+target))
	if err != nil || !res.IsError {
		t.Fatalf("cliArg deny must block: %v %q", err, res.Content)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("denied command ran")
	}
	// A subagent's dispatcher inherits the mode.
	child := rt.childDispatcherFrom(nil, rt.agentTool, 1, nil)
	if !child.ManagedRulesOnly {
		t.Fatalf("child dispatcher lost managed-only")
	}
}

// Control: without the policy the same session honours the user, local and
// --allowedTools allows.
func TestManagedRulesOffSessionE2E(t *testing.T) {
	env, _, _, _ := sessionEnv(t, `{"permissions":{"allow":["Bash(/bin/echo:*)"]}}`)
	env.ManagedSettingsDir = t.TempDir()
	caller := &skillTurnCaller{model: "m", effort: "medium"}
	s := &printSession{
		Model: "m", Mode: toolreg.ModeDefault, Provider: providerAnthropic,
		AllowedTools: []string{"Bash(printf:*)"},
		newCaller:    func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	for _, cmd := range []string{"/bin/echo hi", "printf -v v x"} {
		if res, err := rt.dispatcher.Dispatch(context.Background(), bashUse(cmd)); err != nil || res.IsError {
			t.Errorf("%q should be allowed: %v %q", cmd, err, res.Content)
		}
	}
}

// A non-managed skill's allowed-tools are not applied as temporary allows
// (with the stderr line in -p and the /status entry); its disallowed-tools
// still apply.
func TestManagedRulesOnlySkillAllowedTools(t *testing.T) {
	env, errb, cwd := managedOnlyEnv(t, "", "")
	writeProjectSkill(t, cwd, "zz-grant", "---\ndescription: d\nallowed-tools: Bash(/bin/echo:*)\ndisallowed-tools: Bash(pwd)\n---\nBody.\n")
	caller := &skillTurnCaller{model: "m", effort: "medium"}
	s := &printSession{
		Model: "m", Mode: toolreg.ModeDefault, Provider: providerAnthropic,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	scope := rt.skillTool.Scope
	if _, err := scope.ActivateInline(context.Background(), agenttool.SkillActivation{
		Skill: "zz-grant", AllowedTools: []string{"Bash(/bin/echo:*)"}, DisallowedTools: []string{"Bash(pwd)"},
	}); err != nil {
		t.Fatal(err)
	}
	if res, _ := rt.dispatcher.Dispatch(context.Background(), bashUse("/bin/echo hi")); !res.IsError {
		t.Errorf("skill allowed-tools must not pre-approve under managed-only: %q", res.Content)
	}
	rt.dispatcher.SetMode(toolreg.ModeBypassPermissions)
	if res, _ := rt.dispatcher.Dispatch(context.Background(), bashUse("pwd")); !res.IsError {
		t.Errorf("skill disallowed-tools must still deny: %q", res.Content)
	}
	if got := s.grants.Withheld(); len(got) != 1 || got[0] != "/zz-grant (project)" {
		t.Fatalf("withheld = %v", got)
	}
	if !strings.Contains(errb.String(), "allowed-tools from /zz-grant (project) ignored: your organization restricts permission rules to managed settings (allowManagedPermissionRulesOnly). Ask an admin to add the rules it needs to managed settings.") {
		t.Errorf("stderr = %q", errb.String())
	}
	status := cmdInfo{conv: rt.conv, env: env, print: s}.statusText()
	if !strings.Contains(status, "Permission rules: Managed settings only (allowManagedPermissionRulesOnly)") ||
		!strings.Contains(status, "allowed-tools ignored this session: /zz-grant (project)") {
		t.Errorf("status = %s", status)
	}
}

// A custom command's `!`-block allowed-tools: ignored for a user/project
// command under managed-only, kept for a built-in one.
func TestManagedRulesOnlyCommandShellGate(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil, toolrun.WithManagedRulesOnly(true))
	s := &printSession{grants: &allowedToolsGate{}}
	gate := shellPermissionGate(d, s)
	inv := commands.ShellInvocation{Name: "deploy", Source: commands.SourceProject, AllowedTools: []string{"Bash(/bin/echo:*)"}}
	if err := gate(commands.WithShellInvocation(context.Background(), inv), "/bin/echo hi"); err == nil {
		t.Fatalf("project command allowed-tools must not apply under managed-only")
	}
	inv.Source = commands.SourceBuiltin
	if err := gate(commands.WithShellInvocation(context.Background(), inv), "/bin/echo hi"); err != nil {
		t.Fatalf("built-in command allowed-tools keep applying: %v", err)
	}
	if got := s.grants.Withheld(); len(got) != 1 || got[0] != "/deploy (project)" {
		t.Fatalf("withheld = %v", got)
	}
}

// The prompter withholds "don't ask again" and never persists under
// managed-only.
func TestPrompterNoAlwaysAllow(t *testing.T) {
	persisted := 0
	p := newDialogPrompter(func(string) error { persisted++; return nil })
	var opened *ui.Dialog
	p.bind(func(m tea.Msg) {
		if o, ok := m.(ui.OpenDialogMsg); ok {
			opened = o.Dialog
			go p.submit([]string{ui.PermAllowAlways})
		}
	})
	req := toolrun.PermissionRequest{Tool: protocol.ToolBash, ToolName: "Bash", Suggested: []string{"Bash(echo:*)"}, NoAlwaysAllow: true}
	resp, err := p.Prompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opened.Options {
		if o.Value == ui.PermAllowAlways {
			t.Fatalf("don't-ask-again offered under managed-only")
		}
	}
	if resp.Kind != toolrun.PermissionAllowedOnce || resp.Rule != nil || persisted != 0 {
		t.Fatalf("resp = %+v persisted = %d", resp, persisted)
	}
}
