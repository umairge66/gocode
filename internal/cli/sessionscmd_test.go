package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/sessionstore"
)

const (
	sessA = "aaaaaaaa-2222-4333-8444-555555555555"
	sessB = "bbbbbbbb-2222-4333-8444-555555555555"
)

// sessionsFixture seeds one session in f.cwd and one in another project.
func sessionsFixture(t *testing.T) (*subcmdFixture, *sessionstore.Store, string) {
	t.Helper()
	f := newSubcmdFixture(t)
	st := sessionstore.New(filepath.Join(f.home, ".config", "gocode"))
	a, _ := st.Create(f.cwd, sessA)
	if err := a.AppendMessages([]agent.Message{
		agent.UserMessage(agent.TextBlock("refactor the lexer")),
		agent.AssistantMessage(agent.TextBlock("done")),
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.SetTags([]string{"wip"}, "wip")
	other := filepath.Join(filepath.Dir(f.cwd), "other")
	b, _ := st.Create(other, sessB)
	_ = b.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("elsewhere"))})
	return f, st, other
}

func TestSessionsList(t *testing.T) {
	f, _, other := sessionsFixture(t)
	code, out, errOut := f.run(t, "sessions", "list")
	if code != 0 || !strings.Contains(out, sessA+"  refactor the lexer") || !strings.Contains(out, "#wip") || strings.Contains(out, sessB) {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	code, out, _ = f.run(t, "sessions", "list", "--json", "--all-projects")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rows []sessionEntry
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("json rows = %v (%v):\n%s", rows, err, out)
	}
	byID := map[string]sessionEntry{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if a := byID[sessA]; a.MessageCount != 2 || a.Cwd != f.cwd || len(a.Tags) != 1 || a.FirstPrompt != "refactor the lexer" || a.Created.IsZero() {
		t.Errorf("row a = %+v", a)
	}
	if b := byID[sessB]; b.Cwd != other || b.Tags == nil {
		t.Errorf("row b = %+v", b)
	}
}

func TestSessionsShowRenameDelete(t *testing.T) {
	f, st, _ := sessionsFixture(t)

	code, out, _ := f.run(t, "sessions", "show", sessA[:8], "--json")
	if code != 0 {
		t.Fatalf("show exit %d", code)
	}
	var shown struct {
		Session  sessionEntry          `json:"session"`
		Messages []sessionstore.Record `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil || len(shown.Messages) != 2 || shown.Messages[0].UUID == "" {
		t.Fatalf("show json = %+v %v\n%s", shown, err, out)
	}
	if code, out, _ := f.run(t, "sessions", "show", sessA); code != 0 || !strings.Contains(out, "refactor the lexer") || !strings.Contains(out, shown.Messages[1].UUID) {
		t.Errorf("show text: %d\n%s", code, out)
	}

	// Another project's session needs --all-projects.
	if code, _, errOut := f.run(t, "sessions", "show", sessB); code == 0 || !strings.Contains(errOut, "--all-projects") {
		t.Errorf("cross-project show without flag: %d %q", code, errOut)
	}
	if code, out, _ := f.run(t, "sessions", "show", sessB, "--all-projects"); code != 0 || !strings.Contains(out, "elsewhere") {
		t.Errorf("show --all-projects: %d %q", code, out)
	}

	// rename takes an id, never a title match.
	if code, _, _ := f.run(t, "sessions", "rename", "lexer", "x"); code == 0 {
		t.Error("rename resolved a title")
	}
	if code, out, errOut := f.run(t, "sessions", "rename", sessA[:8], "Lexer", "work"); code != 0 || !strings.Contains(out, `"Lexer work"`) {
		t.Fatalf("rename: %d %q %q", code, out, errOut)
	}
	if m, ok := st.Find(f.cwd, "lexer work"); !ok || m.ID != sessA || m.Title != "Lexer work" {
		t.Errorf("after rename: %+v %v", m, ok)
	}

	// delete asks first; "n" (or no stdin) aborts.
	f.stdin = "n\n"
	if code, _, _ := f.run(t, "sessions", "delete", sessA); code == 0 {
		t.Error("delete without confirmation succeeded")
	}
	if _, ok := st.Find(f.cwd, sessA); !ok {
		t.Fatal("declined delete removed the session")
	}
	side := filepath.Join(st.ProjectDir(f.cwd), sessA, "tool-results", "r.txt")
	subcmdWriteFile(t, side, "x")
	hist := filepath.Join(f.home, ".config", "gocode", "file-history", sessA, "f")
	subcmdWriteFile(t, hist, "x")
	f.stdin = "y\n"
	if code, out, errOut := f.run(t, "sessions", "delete", sessA); code != 0 || !strings.Contains(out, "Deleted session") {
		t.Fatalf("delete: %d %q %q", code, out, errOut)
	}
	for _, p := range []string{filepath.Dir(filepath.Dir(side)), filepath.Dir(hist)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived delete", p)
		}
	}
	if _, ok := st.Find(f.cwd, sessA); ok {
		t.Error("session still listed after delete")
	}
	f.stdin = ""
	if code, _, _ := f.run(t, "sessions", "delete", sessB, "--all-projects", "--yes"); code != 0 {
		t.Error("delete --yes --all-projects failed")
	}
}
