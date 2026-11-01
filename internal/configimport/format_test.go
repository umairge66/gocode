package configimport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"hello":      "hello",
		"my command": "my_command",
		"a@b.c":      "a_b_c",
		"---weird":   "___weird", // 3 hyphens → 3 underscores (leading-hyphen rule no longer applies)
		"con":        "_con",     // reserved device name
		"":           "_",
		"keep-dash":  "keep-dash",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmDash(t *testing.T) {
	if got := emDash("a---b"); got != "a—b" {
		t.Errorf("emDash = %q", got)
	}
	if got := emDash("a--b"); got != "a--b" {
		t.Errorf("emDash two hyphens should be unchanged, got %q", got)
	}
}

func TestParseTOML(t *testing.T) {
	in := `
# comment
approval_policy = "on-request"
count = 3
flag = true

[mcp_servers.one]
command = "node"
args = ["a", "b"]

[[skills.config]]
path = "skills/foo"

[[skills.config]]
path = "skills/bar"
`
	m, err := parseTOML(in)
	if err != nil {
		t.Fatal(err)
	}
	if m["approval_policy"] != "on-request" {
		t.Errorf("approval_policy = %v", m["approval_policy"])
	}
	if m["count"] != int64(3) {
		t.Errorf("count = %v (%T)", m["count"], m["count"])
	}
	if m["flag"] != true {
		t.Errorf("flag = %v", m["flag"])
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	one, _ := servers["one"].(map[string]any)
	if one["command"] != "node" {
		t.Errorf("command = %v", one["command"])
	}
	if !reflect.DeepEqual(toStringSlice(one["args"]), []string{"a", "b"}) {
		t.Errorf("args = %v", one["args"])
	}
	skills, _ := m["skills"].(map[string]any)
	cfg, _ := skills["config"].([]any)
	if len(cfg) != 2 {
		t.Fatalf("skills.config len = %d", len(cfg))
	}
	first, _ := cfg[0].(map[string]any)
	if first["path"] != "skills/foo" {
		t.Errorf("first path = %v", first["path"])
	}
}

func TestParseTOMLMultiline(t *testing.T) {
	in := "prompt = \"\"\"line1\nline2\"\"\"\n"
	m, err := parseTOML(in)
	if err != nil {
		t.Fatal(err)
	}
	if m["prompt"] != "line1\nline2" {
		t.Errorf("prompt = %q", m["prompt"])
	}
}

func TestParseJSONC(t *testing.T) {
	in := `{
  // a comment
  "a": 1, /* block */
  "b": "x",
}`
	var out map[string]any
	if err := parseJSONC(in, &out); err != nil {
		t.Fatal(err)
	}
	if out["b"] != "x" {
		t.Errorf("b = %v", out["b"])
	}
}

func TestFrontmatterRoundTripOrder(t *testing.T) {
	fm := splitFrontmatter("---\nname: x\ndescription: y\nglobs: a\n---\nbody\n")
	if !fm.hadFrontmatter {
		t.Fatal("expected frontmatter")
	}
	if !reflect.DeepEqual(fm.order, []string{"name", "description", "globs"}) {
		t.Errorf("order = %v", fm.order)
	}
	if fm.content != "body\n" {
		t.Errorf("content = %q", fm.content)
	}
}

func TestGeminiTranslateRejectsNestedBrace(t *testing.T) {
	tr := translateGeminiPrompt("do !{echo {nested}}")
	if tr.untranslatable == "" {
		t.Errorf("expected untranslatable for nested brace")
	}
}

func TestGeminiTranslatePathEscape(t *testing.T) {
	tr := translateGeminiPrompt("read @{my file.txt}")
	if tr.untranslatable != "" {
		t.Fatalf("unexpected untranslatable: %q", tr.untranslatable)
	}
	if tr.body != `read @my\ file.txt` {
		t.Errorf("body = %q", tr.body)
	}
}

func TestValidateArgPlaceholdersSafe(t *testing.T) {
	if r := validateArgPlaceholders("Just text with $ARGUMENTS here.", nil); r != "" {
		t.Errorf("expected safe, got %q", r)
	}
}

func TestContainedJoin(t *testing.T) {
	if got := containedJoin(filepath.FromSlash("/base"), "sub/x"); got != filepath.FromSlash("/base/sub/x") {
		t.Errorf("containedJoin inside = %q", got)
	}
	if got := containedJoin(filepath.FromSlash("/base"), "../escape"); got != "" {
		t.Errorf("containedJoin escape = %q, want empty", got)
	}
}

func TestCodexApprovalMode(t *testing.T) {
	cases := map[string]string{
		"suggest": "default", "on-request": "default", "untrusted": "default",
		"auto-edit": "acceptEdits", "full-auto": "auto", "on-failure": "auto",
		"never": "auto", "weird": "auto",
	}
	for policy, want := range cases {
		if got, _ := codexApprovalMode(policy); got != want {
			t.Errorf("codexApprovalMode(%q) = %q, want %q", policy, got, want)
		}
	}
}
