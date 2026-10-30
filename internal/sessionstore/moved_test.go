package sessionstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// A transcript that names a saved tool result under a former config home loads
// with the path pointing at the store's own projects tree; the file on disk is
// left as written.
func TestLoadRewritesPathsFromFormerHome(t *testing.T) {
	base := t.TempDir()
	former := filepath.Join(t.TempDir(), ".gocode")
	newRoot := filepath.Join(base, "gocode")
	oldPath := filepath.Join(former, "projects", "-w", "s1", "tool-results", "t1.txt")
	newPath := filepath.Join(newRoot, "projects", "-w", "s1", "tool-results", "t1.txt")

	st := New(newRoot)
	sess, err := st.Create("/w", "s1")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []agent.Message{
		agent.UserMessage(agent.TextBlock("look")),
		agent.UserMessage(agent.ToolResultBlock("call-1", "Output saved to "+oldPath+" (preview…)", false)),
	}
	if err := sess.AppendMessages(msgs); err != nil {
		t.Fatal(err)
	}

	plain, err := st.Load("/w", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain[1].Content[0].Content, oldPath) {
		t.Fatalf("without MovedFrom the path changed: %+v", plain[1])
	}

	st.MovedFrom = []string{former}
	got, err := st.Load("/w", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if c := got[1].Content[0].Content; !strings.Contains(c, newPath) || strings.Contains(c, oldPath) {
		t.Fatalf("rewritten content = %q, want path %q", c, newPath)
	}
	raw, err := os.ReadFile(filepath.Join(st.ProjectDir("/w"), "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), string(jsonEscapedPath(oldPath))) {
		t.Fatalf("transcript on disk changed: %s", raw)
	}
}
