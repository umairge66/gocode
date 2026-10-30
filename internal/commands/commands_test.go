package commands

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInput(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantArgs string
	}{
		{"/foo bar baz", true, "foo", "bar baz"},
		{"/clear", true, "clear", ""},
		{"/ns:foo", true, "ns:foo", ""},
		{"/project:git:commit fix the bug", true, "project:git:commit", "fix the bug"},
		{"hello world", false, "", ""},
		{"/", false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		got, ok := ParseInput(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseInput(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != c.wantName || got.Args != c.wantArgs {
			t.Errorf("ParseInput(%q) = {%q,%q}, want {%q,%q}", c.in, got.Name, got.Args, c.wantName, c.wantArgs)
		}
	}
}

func TestIsSlashCommand(t *testing.T) {
	for in, want := range map[string]bool{
		"/foo":     true,
		"/foo bar": true,
		"/":        false,
		" /foo":    false,
		"foo":      false,
	} {
		if got := IsSlashCommand(in); got != want {
			t.Errorf("IsSlashCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExpandArgumentsAndPositional(t *testing.T) {
	tmpl := "all=[$ARGUMENTS] first=[$ARGUMENTS[0]] one=[$1] two=[$2]"
	got, err := Expand(tmpl, "alpha beta gamma", ExpandOptions{Append: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "all=[alpha beta gamma] first=[alpha] one=[beta] two=[gamma]"
	if got != want {
		t.Fatalf("Expand positional:\n got %q\nwant %q", got, want)
	}
}

func TestExpandAppendsWhenNoPlaceholder(t *testing.T) {
	got, err := Expand("Do the thing.", "with care", ExpandOptions{Append: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "Do the thing.\n\nARGUMENTS: with care"
	if got != want {
		t.Fatalf("Expand append: got %q want %q", got, want)
	}
	// With Append disabled, nothing is appended.
	got, _ = Expand("Do the thing.", "with care", ExpandOptions{Append: false})
	if got != "Do the thing." {
		t.Fatalf("Expand no-append: got %q", got)
	}
}

func TestExpandOutOfRangeLeftLiteral(t *testing.T) {
	got, err := Expand("x=[$5] y=[$ARGUMENTS[9]]", "only", ExpandOptions{Append: false})
	if err != nil {
		t.Fatal(err)
	}
	if got != "x=[$5] y=[$ARGUMENTS[9]]" {
		t.Fatalf("out-of-range: got %q", got)
	}
}

func TestExpandEscapedPlaceholderIsLiteral(t *testing.T) {
	got, err := Expand(`literal \$ARGUMENTS and real $ARGUMENTS`, "ARGS", ExpandOptions{Append: false})
	if err != nil {
		t.Fatal(err)
	}
	if got != "literal $ARGUMENTS and real ARGS" {
		t.Fatalf("escaped: got %q", got)
	}
}

func TestExpandNamedPlaceholders(t *testing.T) {
	// First declared name -> token[0], second -> token[1].
	got, err := Expand("pr #$number by $author", "42 alice", ExpandOptions{
		ArgNames: []string{"number", "author"},
		Append:   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "pr #42 by alice" {
		t.Fatalf("named: got %q", got)
	}

	// An unfilled named placeholder substitutes to empty (not left literal).
	got2, err := Expand("[$number][$author]", "42", ExpandOptions{
		ArgNames: []string{"number", "author"},
		Append:   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "[42][]" {
		t.Fatalf("unfilled named: got %q want %q", got2, "[42][]")
	}
}

func TestExpandShellEscapeTransform(t *testing.T) {
	// Substituted value is run through ShellEscape so it cannot inject into a
	// !-block: a leading '!' becomes '\!'.
	got, err := Expand("run $ARGUMENTS", "!rm -rf /", ExpandOptions{
		Append:    false,
		Transform: ShellEscape,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != `run \!rm -rf /` {
		t.Fatalf("shell-escape: got %q", got)
	}
}

func TestTokenizeArgsQuoting(t *testing.T) {
	got := tokenizeArgs(`one "two three" 'four five' six`)
	want := []string{"one", "two three", "four five", "six"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize: got %#v want %#v", got, want)
	}
}

func TestExtractShellBlocks(t *testing.T) {
	body := "Context:\n!`git status`\nmore\n```!\ngit log --oneline\n```\n"
	blocks := ExtractShellBlocks(body)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d: %#v", len(blocks), blocks)
	}
	cmds := map[string]bool{}
	for _, b := range blocks {
		cmds[b.Command] = true
	}
	if !cmds["git status"] || !cmds["git log --oneline"] {
		t.Fatalf("unexpected commands: %#v", cmds)
	}
}

func writeCmd(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirMarkdownCommand(t *testing.T) {
	root := t.TempDir()
	writeCmd(t, filepath.Join(root, "git", "commit.md"), `---
description: Create a commit
argument-hint: "[message]"
allowed-tools: Bash, Read
model: sonnet
disable-model-invocation: true
---
Please create a git commit with message: $ARGUMENTS
`)
	cmds, err := LoadDir(root, SourceProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(cmds))
	}
	c := cmds[0]
	if c.Name != "project:git:commit" {
		t.Errorf("name = %q, want project:git:commit", c.Name)
	}
	if c.Description != "Create a commit" || !c.HasUserSpecifiedDescription {
		t.Errorf("description = %q (user=%v)", c.Description, c.HasUserSpecifiedDescription)
	}
	if c.ArgumentHint != "[message]" {
		t.Errorf("argument-hint = %q", c.ArgumentHint)
	}
	if !reflect.DeepEqual(c.AllowedTools, []string{"Bash", "Read"}) {
		t.Errorf("allowed-tools = %#v", c.AllowedTools)
	}
	if c.Model != "sonnet" {
		t.Errorf("model = %q", c.Model)
	}
	if !c.DisableModelInvocation {
		t.Errorf("disable-model-invocation not set")
	}
	if c.Source != SourceProject {
		t.Errorf("source = %q", c.Source)
	}
	if !strings.Contains(c.Body, "$ARGUMENTS") {
		t.Errorf("body missing template: %q", c.Body)
	}

	// The body expands with the typed arguments.
	out, err := Expand(c.Body, "fix the parser", ExpandOptions{ArgNames: c.ArgNames, Append: true, Transform: ShellEscape})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "message: fix the parser") {
		t.Errorf("expanded body = %q", out)
	}
}

func TestLoadDirNoFrontmatterDescriptionFromBody(t *testing.T) {
	root := t.TempDir()
	writeCmd(t, filepath.Join(root, "greet.md"), "# Say hello\n\nGreet the user warmly.\n")
	cmds, err := LoadDir(root, SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want 1, got %d", len(cmds))
	}
	c := cmds[0]
	if c.Name != "user:greet" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Description != "Say hello" || c.HasUserSpecifiedDescription {
		t.Errorf("derived description = %q (user=%v)", c.Description, c.HasUserSpecifiedDescription)
	}
	if !c.UserInvocable || c.Hidden {
		t.Errorf("defaults: userInvocable=%v hidden=%v", c.UserInvocable, c.Hidden)
	}
	if c.Shell != "bash" {
		t.Errorf("default shell = %q", c.Shell)
	}
}

func TestRegistryPrecedenceProjectShadowsUser(t *testing.T) {
	userRoot := t.TempDir()
	projRoot := t.TempDir()
	writeCmd(t, filepath.Join(userRoot, "deploy.md"), "---\ndescription: user deploy\n---\nuser version\n")
	writeCmd(t, filepath.Join(projRoot, "deploy.md"), "---\ndescription: project deploy\n---\nproject version\n")

	userCmds, err := LoadDir(userRoot, SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	projCmds, err := LoadDir(projRoot, SourceProject)
	if err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	// Add user first, then project, to prove precedence is by source, not order.
	for _, c := range userCmds {
		reg.Add(c)
	}
	for _, c := range projCmds {
		reg.Add(c)
	}

	// Both are registered (distinct namespaced names).
	if len(reg.All()) != 2 {
		t.Fatalf("want 2 registered, got %d", len(reg.All()))
	}

	// A user typing the bare name "deploy" resolves to the project command.
	got := reg.Resolve("deploy")
	if got == nil {
		t.Fatal("Resolve(deploy) = nil")
	}
	if got.Source != SourceProject {
		t.Errorf("Resolve(deploy).Source = %q, want project", got.Source)
	}

	// The fully-qualified user name still resolves to the user command.
	gotUser := reg.Resolve("user:deploy")
	if gotUser == nil || gotUser.Source != SourceUser {
		t.Errorf("Resolve(user:deploy) = %+v, want user command", gotUser)
	}
}

func TestRegistryDedup(t *testing.T) {
	reg := NewRegistry()
	c := &Command{Type: "prompt", Name: "project:foo", Source: SourceProject}
	reg.Add(c)
	reg.Add(c) // same key, ignored
	if len(reg.All()) != 1 {
		t.Fatalf("dedup failed: %d", len(reg.All()))
	}
}
