package commands

import (
	"strings"
	"testing"
)

func TestSanitizeNpmResolution(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://registry.npmjs.org/p/-/p-1.0.0.tgz", "https://registry.npmjs.org/p/-/p-1.0.0.tgz", true},
		{"https://user:tok@registry.example/p.tgz?sig=1#x", "https://registry.example/p.tgz", true},
		{"http://r.example/p.tgz?x", "http://r.example/p.tgz", true},
		{"file:///tmp/p.tgz", "", false},
		{"git+https://x/y", "", false},
		{"not a url", "", false},
		{`https:\\evil.example\p.tgz`, "", false},
		{`https://evil.example\@good.example/p.tgz`, "", false},
	}
	for _, c := range cases {
		got, ok := SanitizeNpmResolution(NpmResolution{PackageName: "p", TarballURL: c.in})
		if ok != c.wantOK || (ok && got.TarballURL != c.want) {
			t.Errorf("%q → (%q,%v), want (%q,%v)", c.in, got.TarballURL, ok, c.want, c.wantOK)
		}
		if ok && got.PackageName != "p" {
			t.Errorf("package name lost: %+v", got)
		}
	}
}

func TestBuildPluginCommandKeepsNpmResolutionSanitized(t *testing.T) {
	fm, body := ParseFrontmatter("---\ndescription: d\n---\nbody")
	c := BuildPluginCommand("/p/commands/x.md", "/p/commands", fm, body, PluginCommandOptions{
		NpmResolution: &NpmResolution{PackageName: "pkg", TarballURL: "https://u:p@reg.example/pkg.tgz"},
	})
	if c.NpmResolution == nil || c.NpmResolution.TarballURL != "https://reg.example/pkg.tgz" {
		t.Fatalf("npm resolution = %+v", c.NpmResolution)
	}
	c = BuildPluginCommand("/p/commands/x.md", "/p/commands", fm, body, PluginCommandOptions{
		NpmResolution: &NpmResolution{PackageName: "pkg", TarballURL: "ftp://reg.example/pkg.tgz"},
	})
	if c.NpmResolution != nil {
		t.Fatalf("a non-http resolution must be dropped, got %+v", c.NpmResolution)
	}
}

const validSkillHooks = `---
description: skill
allowed-tools: Read, Bash(git:*)
hooks:
  PreToolUse:
    - matcher: Bash
      hooks:
        - type: command
          command: ./check.sh
  PostToolUse:
    - matcher: Write
      hooks:
        - type: command
          command: ./after.sh
---
Do it.`

func TestPluginSkillValidHooksKept(t *testing.T) {
	fm, body := ParseFrontmatter(validSkillHooks)
	c := BuildCommand("/p/skills/s/SKILL.md", "/p/skills", SourcePlugin, fm, body)
	if c.Hooks == nil || c.Hooks["PreToolUse"] == nil || c.Hooks["PostToolUse"] == nil {
		t.Fatalf("hooks = %#v", c.Hooks)
	}
	if len(c.AllowedTools) != 2 {
		t.Fatalf("allowed-tools = %v", c.AllowedTools)
	}
	if c.ProgressMessage != "loading" {
		t.Errorf("skill progress = %q", c.ProgressMessage)
	}
}

func TestPluginSkillGuardHooksDroppedWhenHooksRestricted(t *testing.T) {
	fm, body := ParseFrontmatter(validSkillHooks)
	c := BuildPluginCommand("/p/skills/s/SKILL.md", "/p/skills", fm, body, PluginCommandOptions{HooksRestricted: true})
	if c.Hooks != nil || len(c.AllowedTools) != 0 {
		t.Fatalf("S3: guard hooks off → no hooks/allowed-tools, got hooks=%v tools=%v", c.Hooks, c.AllowedTools)
	}
}

func TestPluginSkillBrokenGuardUnloadable(t *testing.T) {
	src := `---
allowed-tools: Read
hooks:
  PreToolUse:
    - matcher: Bash
      hooks:
        - type: command
---
x`
	fm, body := ParseFrontmatter(src)
	c := BuildCommand("/p/skills/s/SKILL.md", "/p/skills", SourcePlugin, fm, body)
	if c.Hooks != nil || len(c.AllowedTools) != 0 {
		t.Fatalf("broken guard: hooks=%v tools=%v", c.Hooks, c.AllowedTools)
	}
	joined := strings.Join(c.Warnings, "\n")
	if !strings.Contains(joined, "hooks.PreToolUse.0.hooks.0: Invalid command hook (command: Required)") ||
		!strings.Contains(joined, "the skill loads with no hooks and no allowed-tools") {
		t.Fatalf("warnings = %q", joined)
	}
}

func TestPluginSkillBrokenNonGuardStrippedWithNote(t *testing.T) {
	src := `---
allowed-tools: Read
hooks:
  PostToolUse:
    - matcher: Write
      hooks:
        - type: nope
        - type: command
          command: ok.sh
  NotAnEvent:
    - hooks: []
---
x`
	fm, body := ParseFrontmatter(src)
	c := BuildCommand("/p/skills/s/SKILL.md", "/p/skills", SourcePlugin, fm, body)
	if c.Hooks == nil || len(c.AllowedTools) != 1 {
		t.Fatalf("non-guard problems keep the skill: hooks=%v tools=%v warnings=%v", c.Hooks, c.AllowedTools, c.Warnings)
	}
	groups := c.Hooks["PostToolUse"].([]any)
	entries := groups[0].(map[string]any)["hooks"].([]any)
	if len(entries) != 1 {
		t.Fatalf("bad entry not stripped: %v", entries)
	}
	if _, kept := c.Hooks["NotAnEvent"]; kept {
		t.Fatal("unknown event kept")
	}
	joined := strings.Join(c.Warnings, "\n")
	for _, want := range []string{
		`hooks.PostToolUse.0.hooks.0: Unknown hook type "nope"; entry ignored`,
		"hooks.NotAnEvent: unknown hook event; entry ignored",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing note %q in %q", want, joined)
		}
	}
}

func TestPluginGuardOutsideHooksDropsAllowedTools(t *testing.T) {
	src := `---
allowed-tools: Read
PreToolUse:
  - matcher: Bash
    hooks:
      - type: command
        command: x
---
x`
	fm, body := ParseFrontmatter(src)
	c := BuildCommand("/p/commands/c.md", "/p/commands", SourcePlugin, fm, body)
	if len(c.AllowedTools) != 0 {
		t.Fatalf("allowed-tools kept: %v", c.AllowedTools)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), `outside "hooks"`) {
		t.Fatalf("warnings = %v", c.Warnings)
	}
	// metadata is never scanned.
	src2 := "---\nallowed-tools: Read\nmetadata:\n  PreToolUse: [x]\n---\nx"
	fm, body = ParseFrontmatter(src2)
	if c := BuildCommand("/p/commands/c.md", "/p/commands", SourcePlugin, fm, body); len(c.AllowedTools) != 1 {
		t.Fatalf("metadata must not be scanned: %v", c.Warnings)
	}
}

func TestPluginCommandHooksMalformed(t *testing.T) {
	// A non-skill command whose hooks hold a guard event: unloadable.
	src := "---\nallowed-tools: Read\nhooks:\n  PreToolUse:\n    - hooks: [{type: command, command: x}]\n---\nx"
	fm, body := ParseFrontmatter(src)
	c := BuildCommand("/p/commands/c.md", "/p/commands", SourcePlugin, fm, body)
	if len(c.AllowedTools) != 0 || c.Hooks != nil {
		t.Fatalf("command with guard hooks: tools=%v hooks=%v", c.AllowedTools, c.Hooks)
	}
	// A plain string hooks value is malformed too.
	fm, body = ParseFrontmatter("---\nallowed-tools: Read\nhooks: nope\n---\nx")
	if c := BuildCommand("/p/commands/c.md", "/p/commands", SourcePlugin, fm, body); len(c.AllowedTools) != 0 {
		t.Fatalf("string hooks must be unloadable")
	}
	// Non-guard, well-formed command hooks are ignored (commands carry none)
	// but do not drop allowed-tools.
	fm, body = ParseFrontmatter("---\nallowed-tools: Read\nhooks:\n  Stop:\n    - hooks: [{type: command, command: x}]\n---\nx")
	if c := BuildCommand("/p/commands/c.md", "/p/commands", SourcePlugin, fm, body); len(c.AllowedTools) != 1 || c.Hooks != nil {
		t.Fatalf("plain command hooks: tools=%v hooks=%v", c.AllowedTools, c.Hooks)
	}
}

func TestNonPluginSourcesUnaffected(t *testing.T) {
	src := "---\nallowed-tools: Read\nPreToolUse: [x]\n---\nx"
	fm, body := ParseFrontmatter(src)
	if c := BuildCommand("/u/commands/c.md", "/u/commands", SourceUser, fm, body); len(c.AllowedTools) != 1 {
		t.Fatalf("user command must keep allowed-tools")
	}
}

func TestValidatePluginHooksSingleMatcher(t *testing.T) {
	v := ValidatePluginHooks(map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "x"}}})
	if v.Loadable() || !strings.Contains(v.UnloadableGuards[0], "received a single matcher") {
		t.Fatalf("v = %+v", v)
	}
}
