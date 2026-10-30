package skills

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

func TestParseSkillMD(t *testing.T) {
	src := "---\n" +
		"name: Code Review\n" +
		"description: Review the current diff for bugs\n" +
		"allowed-tools: Read, Grep, Bash\n" +
		"context: fork\n" +
		"agent: reviewer\n" +
		"background: true\n" +
		"user-invocable: false\n" +
		"metadata:\n" +
		"  catalog: internal\n" +
		"---\n" +
		"# Code Review\n\nBody goes here.\n"

	pr := ParseSkillMD([]byte(src), "/x/code-review/SKILL.md")
	if pr.ParseError != "" {
		t.Fatalf("unexpected parse error: %s", pr.ParseError)
	}
	fm := pr.Frontmatter
	if fm.Name != "Code Review" {
		t.Errorf("name = %q", fm.Name)
	}
	if fm.Description != "Review the current diff for bugs" {
		t.Errorf("description = %q", fm.Description)
	}
	if got := strings.Join(fm.AllowedTools, ","); got != "Read,Grep,Bash" {
		t.Errorf("allowed-tools = %q", got)
	}
	if fm.Context != "fork" || fm.Agent != "reviewer" || !fm.Background {
		t.Errorf("fork fields: context=%q agent=%q bg=%v", fm.Context, fm.Agent, fm.Background)
	}
	if !fm.userInvocableSet || fm.UserInvocable {
		t.Errorf("user-invocable not parsed as explicit false: set=%v val=%v", fm.userInvocableSet, fm.UserInvocable)
	}
	if !strings.Contains(pr.Content, "Body goes here.") {
		t.Errorf("body not parsed: %q", pr.Content)
	}

	d := BuildDescriptor(pr, "code-review", ScopeProject, "", "/x/code-review/SKILL.md")
	if !d.RunsInSubagent() {
		t.Errorf("context: fork should run in subagent")
	}
	if d.BaseDir != "/x/code-review" {
		t.Errorf("baseDir = %q", d.BaseDir)
	}
	if !d.HasUserSpecifiedDescription {
		t.Errorf("expected user-specified description")
	}
}

func TestParseSkillMD_NoFrontmatter(t *testing.T) {
	pr := ParseSkillMD([]byte("# Just a body\n\nNo frontmatter.\n"), "/x/foo/SKILL.md")
	if pr.ParseError != "" {
		t.Fatalf("unexpected error: %s", pr.ParseError)
	}
	if pr.Frontmatter.Name != "" {
		t.Errorf("expected empty frontmatter, got name=%q", pr.Frontmatter.Name)
	}
	if !strings.Contains(pr.Content, "Just a body") {
		t.Errorf("content = %q", pr.Content)
	}
	// Description falls back to first body heading.
	d := BuildDescriptor(pr, "foo", ScopePersonal, "", "/x/foo/SKILL.md")
	if d.Description != "Just a body" {
		t.Errorf("fallback description = %q", d.Description)
	}
	if d.HasUserSpecifiedDescription {
		t.Errorf("should not be user-specified")
	}
}

func TestParseSkillMD_OversizedFrontmatterRejected(t *testing.T) {
	big := strings.Repeat("k: v\n", maxFrontmatterBytes/5+10)
	src := "---\n" + big + "---\nbody\n"
	pr := ParseSkillMD([]byte(src), "/x/big/SKILL.md")
	if pr.ParseError == "" {
		t.Fatalf("expected rejection of oversized frontmatter")
	}
	if !strings.Contains(pr.Content, "body") {
		t.Errorf("body should still be returned: %q", pr.Content)
	}
}

func TestNormalizeAndReservedName(t *testing.T) {
	// Synced skills get the gocode-skills: namespace via settingspolicy.
	d := BuildDescriptor(ParseSkillMD([]byte("---\ndescription: x\n---\nb"), "/s/mine/SKILL.md"),
		"mine", ScopeSynced, "", "/s/mine/SKILL.md")
	if d.Name != "gocode-skills:mine" {
		t.Errorf("synced name = %q, want gocode-skills:mine", d.Name)
	}
	if d.UnqualifiedName != "mine" {
		t.Errorf("unqualified = %q", d.UnqualifiedName)
	}

	// Plugin skills are namespaced plugin:skill.
	p := BuildDescriptor(ParseSkillMD([]byte("---\ndescription: y\n---\nb"), "/p/x/SKILL.md"),
		"seo-audit", ScopePlugin, "searchfit-seo", "/p/x/SKILL.md")
	if p.Name != "searchfit-seo:seo-audit" {
		t.Errorf("plugin name = %q", p.Name)
	}
}

func skillFile(desc string) []byte {
	return []byte("---\ndescription: " + desc + "\n---\n# body\n")
}

func TestDiscoveryAndPrecedence(t *testing.T) {
	fs := fsfacade.NewMemFS()
	// Personal scope: two skills.
	fs.MkdirAll("/home/u/.config/gocode/skills/alpha")
	fs.WriteFile("/home/u/.config/gocode/skills/alpha/SKILL.md", skillFile("personal alpha"))
	fs.MkdirAll("/home/u/.config/gocode/skills/solo")
	fs.WriteFile("/home/u/.config/gocode/skills/solo/SKILL.md", skillFile("personal solo"))
	// Project scope: alpha collides, beta is new.
	fs.MkdirAll("/proj/.gocode/skills/alpha")
	fs.WriteFile("/proj/.gocode/skills/alpha/SKILL.md", skillFile("project alpha"))
	fs.MkdirAll("/proj/.gocode/skills/beta")
	fs.WriteFile("/proj/.gocode/skills/beta/SKILL.md", skillFile("project beta"))
	// A dir with no SKILL.md is skipped.
	fs.MkdirAll("/proj/.gocode/skills/empty")

	reg := Load(fs, LoadConfig{UserDir: "/home/u/.config/gocode", Home: "/home/u", Cwd: "/proj"})

	// alpha should resolve to the project copy (project > personal).
	a, ok := reg.Lookup("alpha")
	if !ok {
		t.Fatal("alpha not found")
	}
	if a.Scope != ScopeProject || a.Description != "project alpha" {
		t.Errorf("alpha resolved to %v %q, want project", a.Scope, a.Description)
	}
	// solo and beta present.
	if _, ok := reg.Lookup("solo"); !ok {
		t.Error("solo missing")
	}
	if _, ok := reg.Lookup("beta"); !ok {
		t.Error("beta missing")
	}
	// empty skipped: exactly 3 resolved.
	if n := len(reg.List()); n != 3 {
		t.Errorf("resolved %d skills, want 3", n)
	}
	// A conflict was recorded for the losing personal alpha.
	found := false
	for _, c := range reg.Conflicts {
		if c.Name == "alpha" && c.Rejected == ScopePersonal && c.WonBy == ScopeProject {
			found = true
		}
	}
	if !found {
		t.Errorf("expected personal-alpha conflict, got %+v", reg.Conflicts)
	}
}

// TestClaudeSkillsDiscovery verifies the gocode<->Claude interop addition:
// skills installed by Claude Code under <cwd>/.claude/skills and
// <home>/.claude/skills are discovered alongside gocode's own skills.
func TestClaudeSkillsDiscovery(t *testing.T) {
	fs := fsfacade.NewMemFS()
	// Claude Code project + user skills.
	fs.MkdirAll("/proj/.claude/skills/cproj")
	fs.WriteFile("/proj/.claude/skills/cproj/SKILL.md", skillFile("claude project skill"))
	fs.MkdirAll("/home/u/.claude/skills/cuser")
	fs.WriteFile("/home/u/.claude/skills/cuser/SKILL.md", skillFile("claude user skill"))

	reg := Load(fs, LoadConfig{UserDir: "/home/u/.config/gocode", Home: "/home/u", Cwd: "/proj"})

	cp, ok := reg.Lookup("cproj")
	if !ok {
		t.Fatal("claude project skill not found")
	}
	if cp.Scope != ScopeClaudeProject || cp.Description != "claude project skill" {
		t.Errorf("cproj resolved to %v %q, want claude-project", cp.Scope, cp.Description)
	}
	cu, ok := reg.Lookup("cuser")
	if !ok {
		t.Fatal("claude user skill not found")
	}
	if cu.Scope != ScopeClaudePersonal || cu.Description != "claude user skill" {
		t.Errorf("cuser resolved to %v %q, want claude-personal", cu.Scope, cu.Description)
	}
}

// TestGocodeTakesPrecedenceOverClaude verifies that on a name collision the
// gocode-native .gocode/skills entry wins over the Claude Code .claude/skills
// entry, at both the project and user tiers.
func TestGocodeTakesPrecedenceOverClaude(t *testing.T) {
	fs := fsfacade.NewMemFS()
	// Project tier collision: .gocode must win over .claude.
	fs.MkdirAll("/proj/.gocode/skills/shared")
	fs.WriteFile("/proj/.gocode/skills/shared/SKILL.md", skillFile("gocode project shared"))
	fs.MkdirAll("/proj/.claude/skills/shared")
	fs.WriteFile("/proj/.claude/skills/shared/SKILL.md", skillFile("claude project shared"))
	// User tier collision: .gocode must win over .claude.
	fs.MkdirAll("/home/u/.config/gocode/skills/mine")
	fs.WriteFile("/home/u/.config/gocode/skills/mine/SKILL.md", skillFile("gocode user mine"))
	fs.MkdirAll("/home/u/.claude/skills/mine")
	fs.WriteFile("/home/u/.claude/skills/mine/SKILL.md", skillFile("claude user mine"))

	reg := Load(fs, LoadConfig{UserDir: "/home/u/.config/gocode", Home: "/home/u", Cwd: "/proj"})

	shared, _ := reg.Lookup("shared")
	if shared.Scope != ScopeProject || shared.Description != "gocode project shared" {
		t.Errorf("shared resolved to %v %q, want gocode project", shared.Scope, shared.Description)
	}
	mine, _ := reg.Lookup("mine")
	if mine.Scope != ScopePersonal || mine.Description != "gocode user mine" {
		t.Errorf("mine resolved to %v %q, want gocode personal", mine.Scope, mine.Description)
	}
	// The losing Claude copies were recorded as conflicts, not double-registered.
	if n := len(reg.List()); n != 2 {
		t.Errorf("resolved %d skills, want 2", n)
	}
}

// TestClaudeDirsMissingTolerated verifies that absent .claude dirs cause no
// error and no change to existing .gocode discovery.
func TestClaudeDirsMissingTolerated(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/proj/.gocode/skills/only")
	fs.WriteFile("/proj/.gocode/skills/only/SKILL.md", skillFile("just gocode"))
	// No .claude dirs exist at all.

	reg := Load(fs, LoadConfig{UserDir: "/home/u/.config/gocode", Home: "/home/u", Cwd: "/proj"})
	if n := len(reg.List()); n != 1 {
		t.Errorf("resolved %d skills, want 1", n)
	}
	d, ok := reg.Lookup("only")
	if !ok || d.Scope != ScopeProject {
		t.Errorf("only skill missing or wrong scope: %v %v", ok, d.Scope)
	}
}

// TestClaudeScopeListingSuffix verifies the attribution suffixes.
func TestClaudeScopeListingSuffix(t *testing.T) {
	if got := ScopeClaudeProject.listingSuffix(); got != " (claude project)" {
		t.Errorf("claude project suffix = %q", got)
	}
	if got := ScopeClaudePersonal.listingSuffix(); got != " (claude user)" {
		t.Errorf("claude user suffix = %q", got)
	}
	if ScopeClaudeProject.String() != "claude-project" || ScopeClaudePersonal.String() != "claude-personal" {
		t.Errorf("claude scope String() mismatch")
	}
}

func TestListingFormat(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/proj/.gocode/skills/review")
	fs.WriteFile("/proj/.gocode/skills/review/SKILL.md", skillFile("Review the diff"))
	fs.MkdirAll("/proj/.gocode/skills/hidden")
	fs.WriteFile("/proj/.gocode/skills/hidden/SKILL.md",
		[]byte("---\ndescription: secret\ndisable-model-invocation: true\n---\nb"))

	reg := Load(fs, LoadConfig{Cwd: "/proj"})
	got := reg.Listing()
	want := "- review: Review the diff (project)"
	if got != want {
		t.Errorf("listing = %q, want %q", got, want)
	}
	// Full block with header.
	block := ListingHeader + "\n\n" + got
	if !strings.HasPrefix(block, "The following skills are available for use with the Skill tool:") {
		t.Errorf("header mismatch: %q", block)
	}
	// disable-model-invocation skill is absent.
	if strings.Contains(got, "hidden") {
		t.Errorf("model-disabled skill leaked into listing: %q", got)
	}
}
