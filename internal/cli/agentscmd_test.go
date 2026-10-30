package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func agentMD(name, desc, model string) string {
	s := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if model != "" {
		s += "model: " + model + "\n"
	}
	return s + "---\nPrompt body.\n"
}

func TestAgentsListsDefinitionsWithPrecedence(t *testing.T) {
	f := newSubcmdFixture(t)
	userDir := filepath.Join(f.home, ".config", "gocode", "agents")
	projDir := filepath.Join(f.cwd, ".gocode", "agents")
	subcmdWriteFile(t, filepath.Join(userDir, "reviewer.md"), agentMD("reviewer", "User reviewer", "haiku"))
	subcmdWriteFile(t, filepath.Join(userDir, "writer.md"), agentMD("writer", "Writes docs", ""))
	subcmdWriteFile(t, filepath.Join(projDir, "reviewer.md"), agentMD("reviewer", "Project reviewer", "sonnet"))

	code, out, errOut := f.run(t, "agents", "--definitions")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	for _, want := range []string{"active agents", "Project agents:\n  reviewer · sonnet", "User agents:\n  writer · inherit", "Built-in agents:", "Shadowed", "reviewer (User, " + filepath.Join(userDir, "reviewer.md") + ") — overridden by project"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	code, out, _ = f.run(t, "agents", "--definitions", "--json")
	if code != 0 {
		t.Fatalf("json exit %d", code)
	}
	var rows []agentListEntry
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var activeReviewer, shadowed bool
	for _, r := range rows {
		if r.Name == "reviewer" && r.Active && r.Source == "projectSettings" && r.Description == "Project reviewer" {
			activeReviewer = true
		}
		if r.Name == "reviewer" && !r.Active && r.OverriddenBy == "projectSettings" {
			shadowed = true
		}
	}
	if !activeReviewer || !shadowed {
		t.Fatalf("rows = %+v", rows)
	}

	// --setting-sources user leaves project agents out.
	code, out, _ = f.run(t, "agents", "--definitions", "--setting-sources", "user")
	if code != 0 || strings.Contains(out, "Project agents") || !strings.Contains(out, "reviewer · haiku") {
		t.Fatalf("user-only exit %d:\n%s", code, out)
	}
	code, _, errOut = f.run(t, "agents", "--definitions", "--setting-sources", "bogus")
	if code != 1 || !strings.Contains(errOut, "Invalid setting source: bogus") {
		t.Fatalf("bad source exit %d err %q", code, errOut)
	}
	code, _, errOut = f.run(t, "agents", "--frobnicate")
	if code != 1 || !strings.Contains(errOut, "unknown option '--frobnicate'") {
		t.Fatalf("unknown opt exit %d err %q", code, errOut)
	}
}

// Plain `gocode agents` lists background sessions (the
// --bg hint says "gocode agents   list sessions"); --json gives them as JSON.
func TestAgentsListsBackgroundSessions(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, _ := f.run(t, "agents")
	if code != 0 || !strings.Contains(out, "No background sessions") {
		t.Fatalf("agents exit %d:\n%s", code, out)
	}
	code, out, _ = f.run(t, "agents", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("agents --json exit %d: %q", code, out)
	}
}
