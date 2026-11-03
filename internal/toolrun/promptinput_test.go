package toolrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// changingPrompter allows every prompt with input in place of the call's.
func changingPrompter(t *testing.T, input any) *fakePrompter {
	b := mustJSON(t, input)
	return &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedOnce, UpdatedInput: b}, nil
	}}
}

// An allow whose answer changed the input runs the changed input. Its own
// check would ask again; the answer is taken for it, so there is one prompt.
func TestPromptUpdatedInputRuns(t *testing.T) {
	dir := t.TempDir()
	asked, changed := filepath.Join(dir, "asked.txt"), filepath.Join(dir, "changed.txt")
	d := New(toolreg.ModeDefault, nil)
	p := changingPrompter(t, map[string]any{"file_path": changed, "content": "host"})
	d.SetPermissionPrompter(p)

	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": asked, "content": "model"})
	if res.IsError {
		t.Fatalf("changed input refused: %+v", res)
	}
	if p.count() != 1 {
		t.Fatalf("prompts = %d, want 1", p.count())
	}
	if got, err := os.ReadFile(changed); err != nil || string(got) != "host" {
		t.Fatalf("changed file = %q %v, want the host's content", got, err)
	}
	if _, err := os.Stat(asked); err == nil {
		t.Fatal("the original input ran too")
	}
}

// A changed input that a deny rule covers is refused, though the prompt
// allowed it.
func TestPromptUpdatedInputIntoDeniedPathIsDenied(t *testing.T) {
	allowed, secret := t.TempDir(), t.TempDir()
	target := filepath.Join(secret, "creds.txt")
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Write", Content: "//" + strings.TrimPrefix(filepath.ToSlash(secret), "/") + "/**"}}
	d := New(toolreg.ModeDefault, rules)
	d.SetPermissionPrompter(changingPrompter(t, map[string]any{"file_path": target, "content": "x"}))

	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": filepath.Join(allowed, "ok.txt"), "content": "x"})
	if !res.IsError || !res.PermissionDenied {
		t.Fatalf("changed input into a denied path ran: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the denied file was written")
	}
}

// A changed input that does not parse for the tool is an input error.
func TestPromptUpdatedInputInvalid(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(changingPrompter(t, map[string]any{"file_path": 7}))
	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": filepath.Join(t.TempDir(), "a.txt"), "content": "x"})
	if !res.IsError || !strings.Contains(res.Content, "changed by the permission prompt") {
		t.Fatalf("invalid changed input = %+v", res)
	}
}
