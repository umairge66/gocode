package builtincmds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatuslineBuiltin(t *testing.T) {
	tb := NewTable()
	eff, ok := tb.Dispatch("/statusline")
	if !ok || eff.Kind != EffectSubmitPrompt {
		t.Fatalf("dispatch = %+v %v", eff, ok)
	}
	want := `Create an Agent with subagent_type "statusline-setup" and the prompt "Configure my statusLine from my shell PS1 configuration"`
	if eff.Text != want {
		t.Fatalf("prompt = %q", eff.Text)
	}
	if strings.Join(eff.AllowedTools, "|") != "Agent|Read(~/**)|Edit(~/.config/gocode/settings.json)" {
		t.Fatalf("allowed = %v", eff.AllowedTools)
	}
	eff, _ = tb.Dispatch("/statusline  show the git branch ")
	if !strings.HasSuffix(eff.Text, `the prompt "show the git branch"`) {
		t.Fatalf("prompt with args = %q", eff.Text)
	}
}

func TestGeneratedPromptBuiltins(t *testing.T) {
	tb := NewTable()
	eff, ok := tb.Dispatch("/insights")
	if !ok || eff.Kind != EffectSubmitPrompt || eff.Generator != GeneratorInsights {
		t.Fatalf("insights = %+v", eff)
	}
	eff, ok = tb.Dispatch("/team-onboarding")
	if !ok || eff.Generator != GeneratorTeamOnboarding || eff.Effort != "low" ||
		strings.Join(eff.AllowedTools, "|") != "Edit(ONBOARDING.md)|Bash(ls *)" {
		t.Fatalf("team-onboarding = %+v", eff)
	}
}

func writeTranscript(t *testing.T, dir, name string, lines []string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestTeamOnboardingPrompt(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cwd := t.TempDir()
	writeTranscript(t, dir, "a.jsonl", []string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/review</command-name>"}}`,
		`{"type":"user","message":{"role":"user","content":"Fix the flaky login test please"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__linear__get_issue","input":{}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__linear__list","input":{}}]}}`,
		`{"type":"custom-title","customTitle":"Login test fix"}`,
		`{"type":"pr-link","prNumber":42}`,
	}, now.Add(-time.Hour))
	// The transcript format: content as a block array.
	writeTranscript(t, dir, "b.jsonl", []string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Add a CSV export"}]}}`,
	}, now.Add(-48*time.Hour))
	// Outside the 30-day window: ignored.
	writeTranscript(t, dir, "old.jsonl", []string{
		`{"type":"user","message":{"role":"user","content":"ancient history"}}`,
	}, now.Add(-40*24*time.Hour))
	os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte(`{"mcpServers":{"linear":{"type":"http","url":"https://mcp.linear.app/sse?x=1"}}}`), 0o600)

	git := func(_ context.Context, args ...string) string {
		switch strings.Join(args, " ") {
		case "config user.name":
			return "Ada"
		case "remote get-url origin":
			return "git@github.com:Acme/Widgets.git"
		}
		return ""
	}
	prompt, err := TeamOnboardingPrompt(context.Background(), TeamOnboardingOptions{TranscriptDir: dir, Cwd: cwd, Now: func() time.Time { return now }, Git: git})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, "You are helping a power user generate an onboarding guide") ||
		!strings.Contains(prompt, "## Usage data (last 30 days)") ||
		!strings.Contains(prompt, "# Welcome to [Team Name]") ||
		strings.Contains(prompt, "{{") {
		t.Fatalf("prompt not assembled:\n%s", prompt[:300])
	}
	start := strings.Index(prompt, "```json\n") + len("```json\n")
	end := strings.Index(prompt[start:], "\n```")
	var usage struct {
		GeneratedBy   string `json:"generatedBy"`
		CurrentRepo   string `json:"currentRepo"`
		SessionCount  int    `json:"sessionCount"`
		SlashCommands []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"slashCommands"`
		MCPServers []struct {
			Name      string `json:"name"`
			CallCount int    `json:"callCount"`
			URLOrigin string `json:"urlOrigin"`
		} `json:"mcpServers"`
		SessionDescriptors []sessionDescriptor `json:"sessionDescriptors"`
	}
	if err := json.Unmarshal([]byte(prompt[start:start+end]), &usage); err != nil {
		t.Fatalf("usage json: %v\n%s", err, prompt[start:start+end])
	}
	if usage.GeneratedBy != "Ada" || usage.CurrentRepo != "github.com/acme/widgets" || usage.SessionCount != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(usage.SlashCommands) != 1 || usage.SlashCommands[0].Name != "/review" || usage.SlashCommands[0].Count != 1 {
		t.Fatalf("slash = %+v", usage.SlashCommands)
	}
	if len(usage.MCPServers) != 1 || usage.MCPServers[0].CallCount != 2 || usage.MCPServers[0].URLOrigin != "https://mcp.linear.app" {
		t.Fatalf("mcp = %+v", usage.MCPServers)
	}
	if len(usage.SessionDescriptors) != 2 {
		t.Fatalf("descriptors = %+v", usage.SessionDescriptors)
	}
	a := usage.SessionDescriptors[0]
	if a.Title != "Login test fix" || len(a.PRNumbers) != 1 || a.PRNumbers[0] != 42 || a.FirstMessage != "Fix the flaky login test please" {
		t.Fatalf("descriptor a = %+v", a)
	}
	if usage.SessionDescriptors[1].FirstMessage != "Add a CSV export" {
		t.Fatalf("descriptor b = %+v", usage.SessionDescriptors[1])
	}
}

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Acme/Widgets.git":        "github.com/acme/widgets",
		"https://user@gitlab.com/g/sub/repo.git": "gitlab.com/g/sub/repo",
		"ssh://git@host:22/x/y":                  "host:22/x/y",
		"/local/path":                            "",
		"":                                       "",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
