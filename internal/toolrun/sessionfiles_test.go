package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

func sessionFilesFixture(t *testing.T) (*pathguard.SessionFiles, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	proj := filepath.Join(root, "proj")
	cfg := filepath.Join(root, "home", ".config", "gocode")
	for _, d := range []string{proj, cfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &pathguard.SessionFiles{ConfigDir: cfg, HomeDir: filepath.Join(root, "home"), OriginalCwd: proj, Cwd: proj, Platform: "linux"}, proj
}

func writeToolUse(t *testing.T, p string) agent.ToolUse {
	return agent.ToolUse{ID: "tu_w", Name: string(protocol.ToolWrite), Input: mustJSON(t, map[string]any{"file_path": p, "content": "x"})}
}

// A safety-check ask (sensitive file) must reach the prompter even when an
// allow rule for the tool exists, and without a prompter the denial is the
// ask message itself.
func TestSafetyCheckAskNotSkippedByAllowRule(t *testing.T) {
	f, proj := sessionFilesFixture(t)
	target := filepath.Join(proj, ".bashrc")
	allow := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Write"}}

	fp := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	d := New(toolreg.ModeDefault, allow, WithSessionFiles(f))
	d.SetPermissionPrompter(fp)
	res, err := d.Dispatch(context.Background(), writeToolUse(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || fp.count() != 1 {
		t.Fatalf("want a prompt and a denial, got prompts=%d res=%+v", fp.count(), res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("sensitive file was written")
	}

	headless := New(toolreg.ModeDefault, allow, WithSessionFiles(f))
	res, err = headless.Dispatch(context.Background(), writeToolUse(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, pathguard.MsgSensitiveFile(target)) || strings.Contains(res.Content, "allow rule") {
		t.Fatalf("headless: %+v", res)
	}
}

// The agent-memory allowlist lets the write run without a prompt.
func TestAgentMemoryWriteAllowedWithoutPrompt(t *testing.T) {
	f, proj := sessionFilesFixture(t)
	dir := filepath.Join(proj, ".gocode", "agent-memory", "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "MEMORY.md")
	d := New(toolreg.ModeDefault, nil, WithSessionFiles(f))
	res, err := d.Dispatch(context.Background(), writeToolUse(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("agent memory write refused: %q", res.Content)
	}
}

func TestAskSatisfiableByAllowRule(t *testing.T) {
	cases := []struct {
		d    toolreg.Decision
		want bool
	}{
		{toolreg.Decision{ReasonType: toolreg.ReasonOther}, true},
		{toolreg.Decision{ReasonType: toolreg.ReasonMode, Mode: toolreg.ModeDefault}, true},
		{toolreg.Decision{ReasonType: toolreg.ReasonSafetyCheck}, false},
		{toolreg.Decision{ReasonType: toolreg.ReasonRule}, false},
		{toolreg.Decision{ReasonType: toolreg.ReasonMode, Mode: toolreg.ModePlan}, false},
	}
	for _, c := range cases {
		if got := askSatisfiableByAllowRule(c.d); got != c.want {
			t.Errorf("%+v: got %v", c.d, got)
		}
	}
}
