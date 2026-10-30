package skills

import (
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

func TestGuardHookOutsideHooksDropsHooksAndAllowedTools(t *testing.T) {
	bad := "---\nname: s\nallowed-tools: Bash\nPreToolUse:\n  - matcher: Bash\n    hooks:\n      - type: command\n        command: check\nhooks:\n  PostToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: x\n---\nbody\n"
	pr := ParseSkillMD([]byte(bad), "/s/SKILL.md")
	if pr.Frontmatter.Hooks != nil || pr.Frontmatter.AllowedTools != nil {
		t.Fatalf("misplaced guard must drop hooks and allowed-tools: %+v", pr.Frontmatter)
	}

	// Nested one level down (e.g. under a stray key) is caught too.
	nested := "---\nname: s\nallowed-tools: Read\nextra:\n  PermissionRequest:\n    - matcher: \"*\"\n---\nbody\n"
	if pr := ParseSkillMD([]byte(nested), "/s/SKILL.md"); pr.Frontmatter.AllowedTools != nil {
		t.Fatalf("nested guard not detected: %+v", pr.Frontmatter)
	}

	// Under metadata (unscanned), or an empty list, is fine.
	ok := "---\nname: s\nallowed-tools: Read\nPreToolUse: []\nmetadata:\n  PreToolUse:\n    - matcher: Bash\n---\nbody\n"
	if pr := ParseSkillMD([]byte(ok), "/s/SKILL.md"); len(pr.Frontmatter.AllowedTools) != 1 {
		t.Fatalf("false positive: %+v", pr.Frontmatter)
	}

	good := "---\nname: s\nallowed-tools: Read\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: x\n---\nbody\n"
	pr = ParseSkillMD([]byte(good), "/s/SKILL.md")
	if pr.Frontmatter.Hooks == nil || len(pr.Frontmatter.AllowedTools) != 1 {
		t.Fatalf("hooks under `hooks` must load: %+v", pr.Frontmatter)
	}
}

func TestManagedSkillsRootWinsOverProject(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/etc/gocode/.gocode/skills/deploy")
	fs.WriteFile("/etc/gocode/.gocode/skills/deploy/SKILL.md", skillFile("managed deploy"))
	fs.MkdirAll("/proj/.gocode/skills/deploy")
	fs.WriteFile("/proj/.gocode/skills/deploy/SKILL.md", skillFile("project deploy"))

	reg := Load(fs, LoadConfig{Cwd: "/proj", ManagedRoot: "/etc/gocode/.gocode/skills"})
	d, ok := reg.Lookup("deploy")
	if !ok || d.Scope != ScopeManaged || d.Description != "managed deploy" {
		t.Fatalf("got %v %q %v, want managed", d.Scope, d.Description, ok)
	}
	if d, _ := Load(fs, LoadConfig{Cwd: "/proj"}).Lookup("deploy"); d.Scope != ScopeProject {
		t.Fatalf("without ManagedRoot: %v", d.Scope)
	}
}
