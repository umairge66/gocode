package graphify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runTool(t *testing.T, tool Tool, input string) string {
	t.Helper()
	in, err := ParseToolInput(json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", input, err)
	}
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("%s: %v", input, err)
	}
	return out
}

// The tool builds the graph in memory, answers the four operations, writes
// nothing to the tree, and sees edits without being told about them.
func TestToolAnswersAndFollowsEdits(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":    "module example.com/app\n",
		"main.go":   "package main\n\nimport \"example.com/app/auth\"\n\nfunc main() { auth.Login(\"u\") }\n",
		"auth/a.go": "package auth\n\nfunc Login(user string) bool { return check(user) }\n\nfunc check(user string) bool { return user != \"\" }\n",
		"secret.go": "package main\n\nfunc hidden() {}\n",
	})
	tool := Tool{Root: root, Exclude: func(abs string) bool { return filepath.Base(abs) == "secret.go" }}

	if out := runTool(t, tool, `{"operation":"overview"}`); !strings.Contains(out, "2 code files") || !strings.Contains(out, "Login()") {
		t.Errorf("overview:\n%s", out)
	}
	if out := runTool(t, tool, `{"operation":"explain","name":"check"}`); !strings.Contains(out, "auth/a.go:L5") || !strings.Contains(out, "← calls (1)") {
		t.Errorf("explain:\n%s", out)
	}
	if out := runTool(t, tool, `{"operation":"path","from":"main","to":"check"}`); !strings.Contains(out, "2 hops") {
		t.Errorf("path:\n%s", out)
	}
	if out := runTool(t, tool, `{"operation":"query","question":"who calls login","depth":1}`); !strings.Contains(out, "main() --calls--> Login()") {
		t.Errorf("query:\n%s", out)
	}
	// A file the session may not read is not in the graph.
	if out := runTool(t, tool, `{"operation":"explain","name":"hidden"}`); !strings.Contains(out, "No node matches") {
		t.Errorf("excluded file leaked:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, OutDirName)); !os.IsNotExist(err) {
		t.Errorf("the tool wrote %s (err %v)", OutDirName, err)
	}

	// An edit shows up on the next call after the recheck interval.
	liveMu.Lock()
	lg := liveGraphs[root]
	liveMu.Unlock()
	edited := "package auth\n\nfunc Login(user string) bool { return check(user) && audit(user) }\n\nfunc check(user string) bool { return user != \"\" }\n\nfunc audit(user string) bool { return true }\n"
	if err := os.WriteFile(filepath.Join(root, "auth", "a.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	lg.mu.Lock()
	lg.checked = time.Time{}
	lg.mu.Unlock()
	if out := runTool(t, tool, `{"operation":"explain","name":"audit"}`); !strings.Contains(out, "← calls (1)") {
		t.Errorf("edit not picked up:\n%s", out)
	}
	// A deleted file leaves the graph.
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	lg.mu.Lock()
	lg.checked = time.Time{}
	lg.mu.Unlock()
	if out := runTool(t, tool, `{"operation":"explain","name":"main"}`); strings.Contains(out, "main.go") {
		t.Errorf("deleted file still in the graph:\n%s", out)
	}
}

func TestToolInputValidation(t *testing.T) {
	for _, bad := range []string{
		`{"operation":"build"}`, `{"operation":"query"}`, `{"operation":"explain","name":" "}`,
		`{"operation":"path","from":"a"}`, `{"operation":"overview","depth":9}`, `{"operation":"overview","extra":1}`, `not json`,
	} {
		if _, err := ParseToolInput(json.RawMessage(bad)); err == nil || !strings.HasPrefix(err.Error(), "Invalid input") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(ToolInputSchema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if got := ToolUseSummary(json.RawMessage(`{"operation":"path","from":"a","to":"b"}`)); got != "a → b" {
		t.Errorf("summary = %q", got)
	}
}

func TestToolEmptyTree(t *testing.T) {
	out := runTool(t, Tool{Root: t.TempDir()}, `{"operation":"overview"}`)
	if !strings.Contains(out, "no graph") {
		t.Errorf("empty tree:\n%s", out)
	}
}
