package projectmemory

import (
	"reflect"
	"strings"
	"testing"
)

// TestProjectRulesLoaded: unconditional *.md rules under .gocode/rules are
// loaded recursively in name order, after the directory's gocode.md files and
// before gocode.local.md, with their frontmatter stripped. Non-.md files and
// conditional rules stay out of the startup block.
func TestProjectRulesLoaded(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":                     "project memory",
		"/w/gocode.local.md":               "local memory",
		"/w/.gocode/rules/a.md":            "---\ndescription: style\n---\nrule A body",
		"/w/.gocode/rules/sub/b.md":        "rule B body",
		"/w/.gocode/rules/notes.txt":       "not a rule",
		"/w/.gocode/rules/go.md":           "---\npaths: \"**/*.go\"\n---\nconditional go rule",
		"/w/.gocode/rules/everywhere.md":   "---\npaths: \"**\"\n---\nstar rule is unconditional",
		"/w/.gocode/rules/sub/deeper/c.md": "rule C body",
	})
	res := Load(Config{Cwd: "/w", FS: fs})

	want := []string{
		"/w/gocode.md",
		"/w/.gocode/rules/a.md",
		"/w/.gocode/rules/everywhere.md",
		"/w/.gocode/rules/sub/b.md",
		"/w/.gocode/rules/sub/deeper/c.md",
		"/w/gocode.local.md",
	}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Fatalf("Sources = %v\nwant      %v", res.Sources, want)
	}
	if strings.Contains(res.Text, "description: style") {
		t.Errorf("frontmatter must be stripped from rule content:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "Contents of /w/.gocode/rules/a.md:\n\nrule A body") {
		t.Errorf("rule rendered without its Contents frame:\n%s", res.Text)
	}
	for _, absent := range []string{"conditional go rule", "not a rule"} {
		if strings.Contains(res.Text, absent) {
			t.Errorf("%q must not be in the startup block", absent)
		}
	}
}

// TestUserAndManagedRules: the managed rules dir (<ManagedDir>/.gocode/rules)
// and the user rules dir (<ConfigDir>/rules) follow their tier's gocode.md.
func TestUserAndManagedRules(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/etc/gocode/gocode.md":             "managed memory",
		"/etc/gocode/.gocode/rules/m.md":    "managed rule",
		"/home/u/.config/gocode/gocode.md":  "user memory",
		"/home/u/.config/gocode/rules/u.md": "user rule",
		"/w/gocode.md":                      "project memory",
	})
	res := Load(Config{Cwd: "/w", ConfigDir: "/home/u/.config/gocode", ManagedDir: "/etc/gocode", FS: fs})
	want := []string{
		"/etc/gocode/gocode.md",
		"/etc/gocode/.gocode/rules/m.md",
		"/home/u/.config/gocode/gocode.md",
		"/home/u/.config/gocode/rules/u.md",
		"/w/gocode.md",
	}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Fatalf("Sources = %v\nwant      %v", res.Sources, want)
	}
	if res.Entries[1].Kind != KindManaged || res.Entries[3].Kind != KindUser {
		t.Errorf("rule kinds = %v / %v", res.Entries[1].Kind, res.Entries[3].Kind)
	}
}

// TestRulesSymlinks: a directory cycle through a symlink is walked once; a
// project link leading out of the working directory is not followed, while a
// user-tier link is.
func TestRulesSymlinks(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/.gocode/rules/a.md":             "rule A",
		"/outside/shared.md":                "outside rule",
		"/outside/userlinked/u.md":          "user linked rule",
		"/home/u/.config/gocode/rules/x.md": "user rule",
	})
	fs.Symlink("/w/.gocode/rules", "/w/.gocode/rules/loop")
	fs.Symlink("/outside/shared.md", "/w/.gocode/rules/shared.md")
	fs.Symlink("/outside/userlinked", "/home/u/.config/gocode/rules/linked")

	res := Load(Config{Cwd: "/w", ConfigDir: "/home/u/.config/gocode", FS: fs})
	if strings.Count(res.Text, "rule A") != 1 {
		t.Errorf("cycle must load a.md once:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "outside rule") {
		t.Error("a project rule linked from outside the working directory must not load")
	}
	if !strings.Contains(res.Text, "user linked rule") {
		t.Error("a user rule linked from elsewhere must load")
	}
	if !sourcesContain(res, "/outside/userlinked/u.md") {
		t.Errorf("a linked rule loads from its resolved path; Sources = %v", res.Sources)
	}
}

// TestSplitPathPatterns: `paths:` accepts a comma string or a list, braces are
// expanded and commas inside braces do not split.
func TestSplitPathPatterns(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{"src/**/*.ts, docs/*.md", []string{"src/**/*.ts", "docs/*.md"}},
		{"*.{ts,tsx}", []string{"*.ts", "*.tsx"}},
		{[]any{"a/*.go", "b/{x,y}.md"}, []string{"a/*.go", "b/x.md", "b/y.md"}},
		{42, nil},
	}
	for _, c := range cases {
		if got := splitPathPatterns(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitPathPatterns(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestSplitMemoryFrontmatter: trailing "/**" is dropped, and a paths list that
// is empty or only "**" leaves the file unconditional.
func TestSplitMemoryFrontmatter(t *testing.T) {
	body, globs := splitMemoryFrontmatter("---\npaths:\n  - src/**\n  - \"*.go\"\n---\nbody\n")
	if body != "body\n" || !reflect.DeepEqual(globs, []string{"src", "*.go"}) {
		t.Errorf("got body %q globs %v", body, globs)
	}
	if _, g := splitMemoryFrontmatter("---\npaths: \"**\"\n---\nx"); g != nil {
		t.Errorf("all-** paths must be unconditional, got %v", g)
	}
	// The shape `gocode import cursor` writes for a scoped rule.
	if _, g := splitMemoryFrontmatter("---\npaths: [\"**/*.go\", \"web/*.ts\"]\n---\n\n# Go files\n\nUse gofmt.\n"); !reflect.DeepEqual(g, []string{"**/*.go", "web/*.ts"}) {
		t.Errorf("imported rule globs = %v", g)
	}
	if b, g := splitMemoryFrontmatter("no frontmatter"); b != "no frontmatter" || g != nil {
		t.Errorf("plain file changed: %q %v", b, g)
	}
}

// TestGlobsMatch covers the gitignore-style matching used for conditional
// rules.
func TestGlobsMatch(t *testing.T) {
	cases := []struct {
		globs []string
		rel   string
		want  bool
	}{
		{[]string{"*.go"}, "main.go", true},
		{[]string{"*.go"}, "a/b/c.go", true},
		{[]string{"*.go"}, "a/b/c.ts", false},
		{[]string{"src/**/*.ts"}, "src/a/b.ts", true},
		{[]string{"src/**/*.ts"}, "src/b.ts", true},
		{[]string{"src/**/*.ts"}, "lib/src/b.ts", false},
		{[]string{"src"}, "src/deep/file.txt", true},
		{[]string{"docs/*.md"}, "docs/a.md", true},
		{[]string{"docs/*.md"}, "docs/x/a.md", false},
		{[]string{"*.MD"}, "readme.md", true},
		{[]string{"*.go", "!*_test.go"}, "x_test.go", false},
		{[]string{"build/"}, "build/out.js", true},
		{[]string{"build/"}, "build", false},
		{[]string{"*.go"}, "../x.go", false},
	}
	for _, c := range cases {
		if got := globsMatch(c.globs, c.rel); got != c.want {
			t.Errorf("globsMatch(%v, %q) = %v, want %v", c.globs, c.rel, got, c.want)
		}
	}
}

// TestNestedConditionalRules: a conditional rule is injected once for a file
// its globs match (relative to the directory owning the rules), never for a
// file they do not match, and never again afterwards.
func TestNestedConditionalRules(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":                   "project memory",
		"/w/.gocode/rules/go.md":         "---\npaths: \"**/*.go\"\n---\nGo rule",
		"/w/.gocode/rules/plain.md":      "plain rule",
		"/home/u/cfg/rules/user-ts.md":   "---\npaths: \"*.ts\"\n---\nTS user rule",
		"/etc/gocode/.gocode/rules/m.md": "---\npaths: cmd/**\n---\nmanaged cmd rule",
	})
	cfg := Config{Cwd: "/w", ConfigDir: "/home/u/cfg", ManagedDir: "/etc/gocode", FS: fs}
	start := Load(cfg)
	n := NewNested(cfg, start.Sources)

	if got := n.ForFile("/w/README.txt"); len(got) != 0 {
		t.Fatalf("no rule matches README.txt, got %v", got)
	}
	got := n.ForFile("/w/pkg/x.go")
	if len(got) != 1 || got[0].Path != "/w/.gocode/rules/go.md" || strings.TrimSpace(got[0].Content) != "Go rule" {
		t.Fatalf("ForFile(x.go) = %+v", got)
	}
	if again := n.ForFile("/w/pkg/y.go"); len(again) != 0 {
		t.Errorf("an injected rule must not be injected twice, got %v", again)
	}
	if got := n.ForFile("/w/web/app.ts"); len(got) != 1 || got[0].Kind != KindUser {
		t.Errorf("user conditional rule should match relative to cwd, got %+v", got)
	}
	if got := n.ForFile("/w/cmd/main.c"); len(got) != 1 || got[0].Kind != KindManaged {
		t.Errorf("managed conditional rule should match cmd/**, got %+v", got)
	}
	if r := RenderNested(Entry{Path: "/w/r.md", Content: "body"}); r != "Contents of /w/r.md:\n\nbody" {
		t.Errorf("RenderNested = %q", r)
	}
}

// TestNestedDirectoryMemory: touching a file below cwd brings in the
// gocode.md, gocode.local.md and rules of each directory on the way, outermost
// first; the startup sources and the touched file itself are not repeated.
func TestNestedDirectoryMemory(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":                       "top",
		"/w/pkg/gocode.md":                   "pkg memory",
		"/w/pkg/inner/gocode.local.md":       "inner local",
		"/w/pkg/inner/.gocode/rules/r.md":    "inner rule",
		"/w/pkg/inner/.gocode/rules/only.md": "---\npaths: \"*.py\"\n---\npython only",
		"/w/pkg/.gocode/rules/rel.md":        "---\npaths: inner/*.go\n---\nanchored to pkg",
	})
	cfg := Config{Cwd: "/w", FS: fs}
	n := NewNested(cfg, Load(cfg).Sources)

	got := n.ForFile("/w/pkg/inner/f.go")
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path)
	}
	want := []string{
		"/w/pkg/gocode.md",
		"/w/pkg/.gocode/rules/rel.md",
		"/w/pkg/inner/gocode.local.md",
		"/w/pkg/inner/.gocode/rules/r.md",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ForFile paths = %v\nwant           %v", paths, want)
	}
	if got := n.ForFile("/w/pkg/gocode.md"); len(got) != 0 {
		t.Errorf("already-injected sources must not repeat, got %v", got)
	}
	if got := n.ForFile("/elsewhere/x.go"); len(got) != 0 {
		t.Errorf("a file outside cwd has no nested memory, got %v", got)
	}
}

// TestNestedForAgent: each subagent gets its own tracker, so a rule the parent
// was already shown is injected again for the agent (once), and an agent that
// omits the project memory is not treated as having the startup sources.
func TestNestedForAgent(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":           "project memory",
		"/w/.gocode/rules/go.md": "---\npaths: \"**/*.go\"\n---\nGo rule",
		"/w/pkg/gocode.md":       "pkg memory",
	})
	cfg := Config{Cwd: "/w", FS: fs}
	parent := NewNested(cfg, Load(cfg).Sources)
	if got := parent.ForFile("/w/pkg/x.go"); len(got) != 2 {
		t.Fatalf("parent ForFile = %+v", got)
	}
	child := parent.ForAgent("", true)
	got := child.ForFile("/w/pkg/x.go")
	if len(got) != 2 || got[0].Path != "/w/pkg/gocode.md" || got[1].Path != "/w/.gocode/rules/go.md" {
		t.Fatalf("child ForFile = %+v", got)
	}
	if again := child.ForFile("/w/pkg/y.go"); len(again) != 0 {
		t.Fatalf("child must dedupe per agent, got %+v", again)
	}
	// Seeded with startup memory: the top gocode.md is never re-injected.
	if got := parent.ForAgent("", true).ForFile("/w/gocode.md"); len(got) != 0 {
		t.Fatalf("startup source repeated: %+v", got)
	}
	// A child rooted elsewhere matches rules relative to its own cwd.
	if got := parent.ForAgent("/w/pkg", true).ForFile("/w/pkg/z.go"); len(got) != 1 || got[0].Path != "/w/.gocode/rules/go.md" {
		t.Fatalf("child with cwd /w/pkg = %+v", got)
	}
	var nilN *Nested
	if nilN.ForAgent("/x", true) != nil {
		t.Fatal("nil tracker must stay nil")
	}
}
