package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestExtractPromptFlags: the prompt flags are pulled out (inline or with
// the next token) and everything else, including what follows "--", stays.
func TestExtractPromptFlags(t *testing.T) {
	rest, vals := extractPromptFlags([]string{"-p", "--system-prompt", "be terse", "--append-system-prompt=add me", "--agents", `{"a":{}}`, "hi", "--", "--system-prompt"})
	if want := []string{"-p", "hi", "--", "--system-prompt"}; !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest = %q, want %q", rest, want)
	}
	if vals["--system-prompt"] != "be terse" || vals["--append-system-prompt"] != "add me" || vals["--agents"] != `{"a":{}}` {
		t.Fatalf("vals = %v", vals)
	}
}

// TestApplyPromptFlags: the -file forms are read, a flag and its -file form
// conflict, and --agents must be a JSON object.
func TestApplyPromptFlags(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sys.md")
	if err := os.WriteFile(file, []byte("FROM FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &DispatchDefaults{}
	if err := applyPromptFlags(d, map[string]string{"--system-prompt-file": file, "--append-system-prompt": "tail"}); err != nil {
		t.Fatal(err)
	}
	if d.SystemPrompt != "FROM FILE\n" || d.AppendSystemPrompt != "tail" {
		t.Fatalf("defaults = %+v", d)
	}
	err := applyPromptFlags(&DispatchDefaults{}, map[string]string{"--system-prompt": "a", "--system-prompt-file": file})
	if err == nil || err.Error() != "Error: Cannot use both --system-prompt and --system-prompt-file. Please use only one." {
		t.Fatalf("conflict err = %v", err)
	}
	missing := filepath.Join(dir, "nope.md")
	err = applyPromptFlags(&DispatchDefaults{}, map[string]string{"--append-system-prompt-file": missing})
	if err == nil || err.Error() != "Error: Append system prompt file not found: "+missing {
		t.Fatalf("missing err = %v", err)
	}
	if err := applyPromptFlags(&DispatchDefaults{}, map[string]string{"--agents": "[1]"}); err == nil || !strings.HasPrefix(err.Error(), "Error: Invalid JSON provided to --agents") {
		t.Fatalf("agents err = %v", err)
	}

	f := newSubcmdFixture(t)
	code, _, errOut := f.run(t, "--agents", "{not json", "-p", "hi")
	if code != 1 || !strings.Contains(errOut, "Invalid JSON provided to --agents") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
}

// TestSessionSystemPromptFlags: --system-prompt replaces the built-in prompt
// (memory still follows), --append-system-prompt goes last, and --agents
// definitions join the agent registry.
func TestSessionSystemPromptFlags(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeTestFile(t, filepath.Join(cwd, "gocode.md"), "MEMORY-MARK\n")

	base := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, base, fakeCaller{reply: "ok"}, sessionConfig{})
	builtin := strings.SplitN(rt.conv.System, "\n", 2)[0]
	rt.Close("other")

	s := &printSession{
		Model:              "parent-model",
		Provider:           providerOpenRouter,
		SystemPrompt:       "CUSTOM PROMPT",
		AppendSystemPrompt: "APPENDED",
		AgentsJSON:         `{"reviewer":{"description":"Reviews code","prompt":"Review it.","tools":["Read","mcp__github"]},"nodesc":{"prompt":"x"}}`,
	}
	rt = buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	sys := rt.conv.System
	if !strings.HasPrefix(sys, "CUSTOM PROMPT") || strings.Contains(sys, builtin) {
		t.Fatalf("system prompt not replaced:\n%s", sys)
	}
	if !strings.Contains(sys, "MEMORY-MARK") || !strings.HasSuffix(sys, "\n\nAPPENDED") {
		t.Fatalf("memory or append missing:\n%s", sys)
	}
	d, ok := rt.conv.Agents.Lookup("reviewer")
	if !ok || d.SystemPrompt != "Review it." || !reflect.DeepEqual(d.Tools, []string{"Read", "mcp__github"}) {
		t.Fatalf("--agents reviewer = %+v, %v", d, ok)
	}
	var noted bool
	for _, n := range rt.notices {
		if strings.HasPrefix(n, "--agents: Agent 'nodesc': description is required") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("skipped --agents entry not reported: %v", rt.notices)
	}
}
