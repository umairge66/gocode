package sessionstore

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// TestCommandPromptNeverTitles: a built-in command's injected prompt is
// recorded as an isMeta line, so the first prompt the user typed titles the
// session; the injected prompt is still loaded as history.
func TestCommandPromptNeverTitles(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/cmd"
	sess, err := st.Create(cwd, "s1")
	if err != nil {
		t.Fatal(err)
	}
	sess.MarkCommandTurn("statusline", "", true)
	if err := sess.AppendMessages([]agent.Message{userMsg(`Create an Agent with subagent_type "statusline-setup" and the prompt "x"`), asstMsg("done")}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessages([]agent.Message{userMsg("add a name field to calc.py"), asstMsg("ok")}); err != nil {
		t.Fatal(err)
	}
	metas, _ := st.List(cwd)
	if len(metas) != 1 || metas[0].Title != "add a name field to calc.py" {
		t.Fatalf("metas = %+v", metas)
	}
	if metas[0].MessageCount != 4 {
		t.Errorf("message count = %d, want 4", metas[0].MessageCount)
	}
	msgs, err := st.Load(cwd, "s1")
	if err != nil || len(msgs) != 4 {
		t.Fatalf("Load: %v, %d messages", err, len(msgs))
	}
}

// TestCustomCommandTitle: a custom command run with arguments titles the
// session as typed; without arguments it is skipped like a built-in.
func TestCustomCommandTitle(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/custom"
	a, _ := st.Create(cwd, "a")
	a.MarkCommandTurn("review", "the parser", false)
	_ = a.AppendMessages([]agent.Message{userMsg("Review the following code carefully..."), asstMsg("ok")})
	b, _ := st.Create(cwd, "b")
	b.MarkCommandTurn("review", "", false)
	_ = b.AppendMessages([]agent.Message{userMsg("Review the following code carefully..."), asstMsg("ok")})
	// A mark is used once: a tool-result user line does not consume it.
	c, _ := st.Create(cwd, "c")
	c.MarkCommandTurn("init", "", true)
	_ = c.AppendMessages([]agent.Message{agent.UserMessage(agent.ToolResultBlock("t1", "", false))})
	_ = c.AppendMessages([]agent.Message{userMsg("Initialize a new gocode.md file"), userMsg("typed later")})

	titles := map[string]string{}
	metas, _ := st.List(cwd)
	for _, m := range metas {
		titles[m.ID] = m.Title
	}
	if titles["a"] != "/review the parser" {
		t.Errorf("custom command with args: title %q", titles["a"])
	}
	if titles["b"] != "b" {
		t.Errorf("custom command without args: title %q, want the short id", titles["b"])
	}
	if titles["c"] != "typed later" {
		t.Errorf("built-in after a tool result: title %q", titles["c"])
	}
}
