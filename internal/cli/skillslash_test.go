package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// TestSessionSkillSlashCommand: in a built session, `/name args` runs a
// user-invocable skill through the Skill tool, even a model-hidden one.
func TestSessionSkillSlashCommand(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "ship", "SKILL.md"), "---\ndescription: ship it\ndisable-model-invocation: true\n---\nShip to $ARGUMENTS now.\n")

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	if _, err := rt.conv.Submit(context.Background(), "/ship staging"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var first string
	for _, b := range rt.conv.History[0].Content {
		first += b.Text
	}
	if !strings.Contains(first, "<command-name>/ship</command-name>") || !strings.Contains(first, "Ship to staging now.") {
		t.Fatalf("user turn = %q", first)
	}
}

// TestSessionSkillListing_DeniedAndConditional: a Skill(<name>) deny rule
// keeps that skill out of the listing, and a `paths:` skill joins it once the
// agent reads a matching file.
func TestSessionSkillListing_DeniedAndConditional(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, `{"permissions":{"deny":["Skill(nope)"]}}`)
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "nope", "SKILL.md"), "---\ndescription: denied skill\n---\nb\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "ok", "SKILL.md"), "---\ndescription: fine skill\n---\nb\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "gostyle", "SKILL.md"), "---\ndescription: go style\npaths: \"*.go\"\n---\nb\n")

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	l := rt.conv.Skills.Listing()
	if !strings.Contains(l, "- ok: fine skill") || strings.Contains(l, "nope") || strings.Contains(l, "gostyle") {
		t.Fatalf("listing = %q", l)
	}
	in, _ := json.Marshal(map[string]string{"file_path": filepath.Join(cwd, "main.go")})
	rt.conv.Hooks.PostToolUse(context.Background(), agent.ToolUse{Name: "Read", Input: in}, agent.ToolResult{})
	if l := rt.conv.Skills.Listing(); !strings.Contains(l, "- gostyle: go style") {
		t.Fatalf("conditional skill not listed after the read: %q", l)
	}
}

// A subagent's file touches activate `paths:` skills too: the registry is
// the session's, shared with the main agent.
func TestSubagentFileTouchActivatesConditionalSkill(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "gostyle", "SKILL.md"), "---\ndescription: go style\npaths: \"*.go\"\n---\nb\n")

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	if l := rt.conv.Skills.Listing(); strings.Contains(l, "gostyle") {
		t.Fatalf("listing = %q", l)
	}
	hooks := rt.agentTool.ChildHooks(subagent.AgentType{Name: "general-purpose"}, "")
	if hooks == nil {
		t.Fatal("no subagent hooks")
	}
	in, _ := json.Marshal(map[string]string{"file_path": filepath.Join("pkg", "main.go")})
	hooks.PostToolUse(context.Background(), agent.ToolUse{Name: "Edit", Input: in}, agent.ToolResult{})
	if l := rt.conv.Skills.Listing(); !strings.Contains(l, "- gostyle: go style") {
		t.Fatalf("conditional skill not listed after the subagent's edit: %q", l)
	}
}
