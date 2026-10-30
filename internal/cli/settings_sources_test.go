package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// settingsTestEnv builds a hermetic Env whose HOME and cwd are fresh temp
// directories. The returned home/cwd let a test drop settings files where the
// user (<HOME>/.config/gocode/settings.json) and project
// (<cwd>/.gocode/settings.json) sources resolve.
func settingsTestEnv(t *testing.T) (env *Env, home, cwd string) {
	t.Helper()
	env, _, _ = newTestEnv()
	home, cwd = t.TempDir(), t.TempDir()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	return env, home, cwd
}

func writeSettingsFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func userSettingsPath(home string) string {
	return filepath.Join(home, ".config", "gocode", "settings.json")
}

// TestLoadPermissionRules_UserDenyBlocksTool asserts a deny rule in the USER
// settings file is loaded (it used to be silently ignored because only the
// flag/policy sources were consulted) and blocks the tool even in bypass mode.
func TestLoadPermissionRules_UserDenyBlocksTool(t *testing.T) {
	env, home, _ := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home), `{"permissions":{"deny":["Bash"]}}`)

	rules := loadPermissionRules(env)
	if len(rules) != 1 || rules[0].Behavior != toolreg.RuleDeny || rules[0].ToolName != "Bash" {
		t.Fatalf("rules = %+v, want one deny Bash rule from user settings", rules)
	}

	d := toolrun.New(toolreg.ModeBypassPermissions, rules)
	input, _ := json.Marshal(map[string]any{"command": "echo should-not-run"})
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t1", Name: "Bash", Input: input})
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Bash ran despite user deny rule: %+v", res)
	}
	if strings.Contains(res.Content, "should-not-run") {
		t.Fatalf("Bash output leaked; tool was executed: %q", res.Content)
	}
}

// TestLoadPermissionRules_ProjectSettings asserts a rule in the project
// .gocode/settings.json is loaded too.
func TestLoadPermissionRules_ProjectSettings(t *testing.T) {
	env, _, cwd := settingsTestEnv(t)
	writeSettingsFile(t, filepath.Join(cwd, ".gocode", "settings.json"),
		`{"permissions":{"allow":["Glob"],"ask":["Write"]}}`)

	rules := loadPermissionRules(env)
	var sawAllow, sawAsk bool
	for _, r := range rules {
		if r.Behavior == toolreg.RuleAllow && r.ToolName == "Glob" {
			sawAllow = true
		}
		if r.Behavior == toolreg.RuleAsk && r.ToolName == "Write" {
			sawAsk = true
		}
	}
	if !sawAllow || !sawAsk {
		t.Fatalf("rules = %+v, want allow Glob and ask Write from project settings", rules)
	}
}

// TestLoadPermissionRules_NoSettings asserts an empty HOME/cwd yields no rules.
func TestLoadPermissionRules_NoSettings(t *testing.T) {
	env, _, _ := settingsTestEnv(t)
	if rules := loadPermissionRules(env); len(rules) != 0 {
		t.Fatalf("rules = %+v, want none", rules)
	}
}

// TestResolveInteractiveSettings_UserSettings asserts theme and statusLine from
// the USER settings file are applied, and that the project file overrides the
// user theme (higher precedence).
func TestResolveInteractiveSettings_UserSettings(t *testing.T) {
	env, home, cwd := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home),
		`{"theme":"light","statusLine":{"type":"command","command":"my-status.sh"}}`)

	theme, sl := resolveInteractiveSettings(env)
	if theme != "light" {
		t.Fatalf("theme = %q, want %q", theme, "light")
	}
	if sl == nil || sl.Command != "my-status.sh" {
		t.Fatalf("statusLine = %+v, want command my-status.sh", sl)
	}

	writeSettingsFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"theme":"dark"}`)
	theme, sl = resolveInteractiveSettings(env)
	if theme != "dark" {
		t.Fatalf("theme with project override = %q, want %q", theme, "dark")
	}
	if sl == nil || sl.Command != "my-status.sh" {
		t.Fatalf("statusLine lost after project override: %+v", sl)
	}
}
